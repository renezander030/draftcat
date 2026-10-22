package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
)

// callersFixture serves the gate behind the same authenticator the engine
// mounts, with one shared token and two named callers.
func callersFixture(t *testing.T, requireCaller bool) http.Handler {
	t.Helper()
	t.Setenv("GATE_TOKEN_BILLING", "billing-token")
	t.Setenv("GATE_TOKEN_MAIL", "mail-token")
	cfg := gateCfg(config.ToolRule{Name: "create_invoice"}, config.ToolRule{Name: "send_email"})
	cfg.ToolGate.RequireCaller = requireCaller
	cfg.ToolGate.Callers = []config.GateCaller{
		{Name: "billing-agent", TokenEnv: "GATE_TOKEN_BILLING", Tools: []string{"create_invoice"}},
		{Name: "mail-agent", TokenEnv: "GATE_TOKEN_MAIL"},
	}
	gate := newToolGate(cfg, nil)
	wrap := func(next http.HandlerFunc) http.Handler {
		return newGateAuthenticator([]byte("shared-token"), cfg.ToolGate).middleware(1<<20, 300, false)(next)
	}
	mux := http.NewServeMux()
	mux.Handle(toolGatePath, wrap(gate.HandleCall))
	mux.Handle(toolGatePath+"/", wrap(gate.HandleStatus))
	return mux
}

func gateRequest(t *testing.T, h http.Handler, method, path, token, body string) (int, ToolCallResponse) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp ToolCallResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

func TestGateCallers_TokenFixesTheAgentAndLimitsTools(t *testing.T) {
	useReliabilityStore(t)
	h := callersFixture(t, false)

	code, r := gateRequest(t, h, http.MethodPost, toolGatePath, "billing-token", `{"action_id":"inv-1","tool":"create_invoice","args":{"amount":10}}`)
	if code != http.StatusOK || r.Decision != "allow" {
		t.Fatalf("billing caller on its own tool: %d %+v", code, r)
	}
	a, err := state.ToolAction("inv-1")
	if err != nil || a.Agent != "billing-agent" {
		t.Fatalf("the action is recorded under the caller's name: %+v %v", a, err)
	}

	_, r = gateRequest(t, h, http.MethodPost, toolGatePath, "billing-token", `{"action_id":"mail-1","tool":"send_email","args":{}}`)
	if r.Decision != "deny" || r.Rule != "caller.tools" || r.DecidedBy != "allowlist" {
		t.Fatalf("a tool outside the caller's list is refused: %+v", r)
	}

	if code, _ := gateRequest(t, h, http.MethodPost, toolGatePath, "billing-token", `{"tool":"create_invoice","agent":"mail-agent","args":{}}`); code != http.StatusForbidden {
		t.Fatalf("a caller cannot claim another agent name: %d", code)
	}

	// A caller without a tools list may call every listed tool.
	if _, r := gateRequest(t, h, http.MethodPost, toolGatePath, "mail-token", `{"action_id":"mail-2","tool":"send_email","args":{}}`); r.Decision != "allow" {
		t.Fatalf("mail caller: %+v", r)
	}
}

func TestGateCallers_SeeOnlyTheirOwnActions(t *testing.T) {
	useReliabilityStore(t)
	h := callersFixture(t, false)
	_, r := gateRequest(t, h, http.MethodPost, toolGatePath, "billing-token", `{"action_id":"inv-7","tool":"create_invoice","args":{}}`)
	if r.Decision != "allow" {
		t.Fatalf("setup: %+v", r)
	}
	if code, _ := gateRequest(t, h, http.MethodGet, toolGatePath+"/inv-7", "mail-token", ""); code != http.StatusNotFound {
		t.Fatalf("another caller's action must look unknown, got %d", code)
	}
	if code, _ := gateRequest(t, h, http.MethodPost, toolGatePath+"/inv-7/consume", "mail-token", `{"binding_hash":"`+r.BindingHash+`"}`); code != http.StatusNotFound {
		t.Fatalf("another caller cannot consume the permit, got %d", code)
	}
	if code, _ := gateRequest(t, h, http.MethodGet, toolGatePath+"/inv-7", "billing-token", ""); code != http.StatusOK {
		t.Fatalf("the owner reads its action, got %d", code)
	}
	if code, _ := gateRequest(t, h, http.MethodGet, toolGatePath+"/inv-7", "shared-token", ""); code != http.StatusOK {
		t.Fatalf("the shared token keeps reaching every action, got %d", code)
	}
}

func TestGateCallers_SharedTokenAndRequireCaller(t *testing.T) {
	useReliabilityStore(t)
	open := callersFixture(t, false)
	if code, r := gateRequest(t, open, http.MethodPost, toolGatePath, "shared-token", `{"tool":"send_email","agent":"anything","args":{}}`); code != http.StatusOK || r.Decision != "allow" {
		t.Fatalf("without require_caller the shared token works as before: %d %+v", code, r)
	}
	if code, _ := gateRequest(t, open, http.MethodPost, toolGatePath, "nope", `{"tool":"send_email"}`); code != http.StatusUnauthorized {
		t.Fatalf("an unknown token is refused, got %d", code)
	}
	strict := callersFixture(t, true)
	if code, _ := gateRequest(t, strict, http.MethodPost, toolGatePath, "shared-token", `{"tool":"send_email"}`); code != http.StatusUnauthorized {
		t.Fatalf("require_caller refuses the shared token, got %d", code)
	}
	if code, _ := gateRequest(t, strict, http.MethodPost, toolGatePath, "mail-token", `{"tool":"send_email","args":{}}`); code != http.StatusOK {
		t.Fatalf("require_caller still accepts a caller token, got %d", code)
	}
}

// A caller whose variable is empty cannot authenticate with an empty token.
func TestGateCallers_EmptyTokenNeverMatches(t *testing.T) {
	t.Setenv("GATE_TOKEN_EMPTY", "")
	auth := newGateAuthenticator([]byte("shared"), config.ToolGateConfig{
		Callers: []config.GateCaller{{Name: "x", TokenEnv: "GATE_TOKEN_EMPTY"}}})
	if _, _, ok := auth.identify("Bearer "); ok {
		t.Fatal("an empty bearer must never match")
	}
}

// A signed request is checked with the caller's own token, not the shared one.
func TestGateCallers_SignatureUsesTheCallerKey(t *testing.T) {
	useReliabilityStore(t)
	h := callersFixture(t, false)
	body := []byte(`{"action_id":"sig-1","tool":"send_email","args":{}}`)
	send := func(key string) int {
		req := httptest.NewRequest(http.MethodPost, toolGatePath, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer mail-token")
		req.Header.Set(webhookSigHeader, signBody([]byte(key), time.Now().Unix(), body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := send("shared-token"); code != http.StatusUnauthorized {
		t.Fatalf("a body signed with the shared token under a caller token is refused, got %d", code)
	}
	if code := send("mail-token"); code != http.StatusOK {
		t.Fatalf("a body signed with the caller's token is accepted, got %d", code)
	}
}
