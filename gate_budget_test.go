package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

const budgetFixtureConfig = `budgets:
  per_day_tokens: 1000
  per_day_cost: 2
pipelines:
  - name: replies
    budget: {per_day_tokens: 300, per_day_cost: 1}
  - name: digest
tool_gate:
  enabled: true
  callers:
    - name: billing-agent
      token_env: GATE_TOKEN_BILLING
      tools: [create_invoice]
    - name: mail-agent
      token_env: GATE_TOKEN_MAIL
      tools: [send_email, read_calendar]
  tools:
    - name: create_invoice
      max_calls_per_day: 5
      args:
        amount: {max: 1000, max_per_day: 2000}
    - name: send_email
      max_calls_per_day: 50
    - name: read_calendar
`

// budgetFixture serves the gate and the budget route behind the engine's
// authenticator, on a store at a path the CLI can open too.
func budgetFixture(t *testing.T, requireCaller, requireSignature bool) (http.Handler, *config.Config, string, *statestore.StateStore) {
	t.Helper()
	t.Setenv("GATE_TOKEN_BILLING", "billing-token")
	t.Setenv("GATE_TOKEN_MAIL", "mail-token")
	statePath, st := lifecycleHTTPStore(t)
	t.Setenv("DRAFTCAT_STATE_PATH", statePath)
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(budgetFixtureConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfigFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ToolGate.RequireCaller = requireCaller
	gate := newToolGate(cfg, nil)
	wrap := func(next http.HandlerFunc) http.Handler {
		return newGateAuthenticator([]byte("shared-token"), cfg.ToolGate).middleware(1<<20, 300, requireSignature)(next)
	}
	mux := http.NewServeMux()
	mux.Handle(toolGatePath, wrap(gate.HandleCall))
	mux.Handle(toolGateBudgetPath, wrap(gate.HandleBudget))
	return mux, cfg, cfgPath, st
}

func getBudget(t *testing.T, h http.Handler, token string) (*httptest.ResponseRecorder, gateBudget) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, toolGateBudgetPath, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var b gateBudget
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
	}
	return rec, b
}

func toolNames(b gateBudget) string {
	var names []string
	for _, tb := range b.Tools {
		names = append(names, tb.Tool)
	}
	return strings.Join(names, ",")
}

func TestGateBudget_RefusesWithoutAGateToken(t *testing.T) {
	h, _, _, _ := budgetFixture(t, false, false)
	for _, token := range []string{"", "nope"} {
		if rec, _ := getBudget(t, h, token); rec.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: %d", token, rec.Code)
		}
	}
	strict, _, _, _ := budgetFixture(t, true, false)
	if rec, _ := getBudget(t, strict, "shared-token"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("require_caller refuses the shared token: %d", rec.Code)
	}
	if rec, _ := getBudget(t, strict, "mail-token"); rec.Code != http.StatusOK {
		t.Fatalf("require_caller still accepts a caller token: %d", rec.Code)
	}
	// Like the poll route, a GET carries no body to sign.
	signed, _, _, _ := budgetFixture(t, false, true)
	if rec, _ := getBudget(t, signed, "shared-token"); rec.Code != http.StatusOK {
		t.Fatalf("require_signature: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, toolGateBudgetPath, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer shared-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rec.Code)
	}
}

func TestGateBudget_CallerSeesOnlyItsTools(t *testing.T) {
	h, _, _, _ := budgetFixture(t, false, false)
	for i, amount := range []string{"120.50", "79.5"} {
		code, r := gateRequest(t, h, http.MethodPost, toolGatePath, "billing-token",
			`{"action_id":"inv-`+string(rune('a'+i))+`","tool":"create_invoice","args":{"amount":`+amount+`}}`)
		if code != http.StatusOK || r.Decision != "allow" {
			t.Fatalf("setup: %d %+v", code, r)
		}
	}
	if _, r := gateRequest(t, h, http.MethodPost, toolGatePath, "mail-token", `{"action_id":"m-1","tool":"send_email","args":{"to":"anna@example.com"}}`); r.Decision != "allow" {
		t.Fatalf("setup: %+v", r)
	}

	rec, shared := getBudget(t, h, "shared-token")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("shared: %d %v", rec.Code, rec.Header())
	}
	if toolNames(shared) != "create_invoice,send_email" {
		t.Fatalf("the shared token sees every capped tool: %s", toolNames(shared))
	}
	inv := shared.Tools[0]
	if inv.Calls == nil || *inv.Calls != 2 || inv.MaxCallsPerDay == nil || *inv.MaxCallsPerDay != 5 ||
		len(inv.Args) != 1 || inv.Args[0].Arg != "amount" || inv.Args[0].Total.String() != "200" || inv.Args[0].MaxPerDay != 2000 {
		t.Fatalf("create_invoice totals: %+v", inv)
	}
	if mail := shared.Tools[1]; mail.Calls == nil || *mail.Calls != 1 || *mail.MaxCallsPerDay != 50 || len(mail.Args) != 0 {
		t.Fatalf("send_email totals: %+v", mail)
	}

	rec, billing := getBudget(t, h, "billing-token")
	if rec.Code != http.StatusOK || toolNames(billing) != "create_invoice" {
		t.Fatalf("billing caller: %d %s", rec.Code, toolNames(billing))
	}
	rec, mail := getBudget(t, h, "mail-token")
	if rec.Code != http.StatusOK || toolNames(mail) != "send_email" {
		t.Fatalf("mail caller sees its capped tool only: %s", toolNames(mail))
	}
	body := rec.Body.String()
	for _, leak := range []string{"create_invoice", "anna@example.com", "billing-agent", "mail-agent", "inv-a", "m-1"} {
		if strings.Contains(body, leak) {
			t.Fatalf("the mail caller's budget carries %q: %s", leak, body)
		}
	}
}

func TestGateBudget_MatchesBudgetStatus(t *testing.T) {
	h, _, cfgPath, st := budgetFixture(t, false, false)
	ctx := context.Background()
	now := time.Now()
	for i, c := range []struct {
		pipeline string
		tokens   int
		cost     float64
	}{{"replies", 120, .3}, {"replies", 200, .45}, {"digest", 70, .05}} {
		id := "call-" + string(rune('a'+i))
		if err := st.BeginBudgetCall(ctx, id, now, 1, 0, 0, statestore.UsageLabel{Pipeline: c.pipeline}); err != nil {
			t.Fatal(err)
		}
		if err := st.SettleBudgetCall(ctx, id, c.tokens, c.cost); err != nil {
			t.Fatal(err)
		}
	}
	out := captureStdout(t, func() {
		if code := runBudgetCmd([]string{"status", "--json", "--config", cfgPath}); code != 0 {
			t.Errorf("status exit %d", code)
		}
	})
	var status struct {
		Day          string           `json:"day"`
		Tokens       int              `json:"tokens"`
		Cost         float64          `json:"cost"`
		PipelineCaps []pipelineDayCap `json:"pipeline_caps"`
	}
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	rec, b := getBudget(t, h, "shared-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("budget: %d %s", rec.Code, rec.Body.String())
	}
	if b.Day != status.Day || b.Tokens != status.Tokens || b.Cost != status.Cost || b.Tokens != 390 {
		t.Fatalf("budget %+v, status %+v", b, status)
	}
	if b.PerDayTokens != 1000 || b.PerDayCost != 2 || b.ReservedCost != nil || b.Frozen {
		t.Fatalf("caps: %+v", b)
	}
	if len(b.PipelineCaps) != 1 || len(status.PipelineCaps) != 1 || b.PipelineCaps[0] != status.PipelineCaps[0] ||
		b.PipelineCaps[0].Tokens != 320 || !b.PipelineCaps[0].Reached {
		t.Fatalf("pipeline caps: %+v vs %+v", b.PipelineCaps, status.PipelineCaps)
	}
	if strings.Contains(rec.Body.String(), "reserved_cost") {
		t.Fatalf("no reservation without reserve_max_cost: %s", rec.Body.String())
	}
}

func TestGateBudget_ReservedCostAndFreeze(t *testing.T) {
	h, cfg, _, st := budgetFixture(t, false, false)
	cfg.Budgets.ReserveMaxCost = true
	if err := st.BeginBudgetCall(context.Background(), "pending-1", time.Now(), 1, 0, 2, statestore.UsageLabel{ReserveCost: .4}); err != nil {
		t.Fatal(err)
	}
	_, b := getBudget(t, h, "mail-token")
	if b.ReservedCost == nil || *b.ReservedCost != .4 || b.Frozen {
		t.Fatalf("reserved: %+v", b)
	}
	if _, err := st.SetGateFreeze("maintenance", "cli", time.Now()); err != nil {
		t.Fatal(err)
	}
	rec, b := getBudget(t, h, "mail-token")
	if !b.Frozen || strings.Contains(rec.Body.String(), "maintenance") || strings.Contains(rec.Body.String(), "cli") {
		t.Fatalf("frozen, without who or why: %s", rec.Body.String())
	}
}

func TestGateBudget_NoStoreIsUnavailable(t *testing.T) {
	prev := state
	state = nil
	t.Cleanup(func() { state = prev })
	g := newToolGate(gateCfg(config.ToolRule{Name: "send_email"}), nil)
	rec := httptest.NewRecorder()
	g.HandleBudget(rec, httptest.NewRequest(http.MethodGet, toolGateBudgetPath, nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("no store: %d %v", rec.Code, rec.Header())
	}
}
