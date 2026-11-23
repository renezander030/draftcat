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
	statestore "github.com/renezander030/draftcat/internal/state"
)

// Every attempt the gate refuses without a human leaves a signed receipt that
// names the reason: a request it could not read or would not accept, an
// approval that timed out, a permit it would not let be consumed. An audit
// trail that kept only the calls that got through would lose exactly these.
func TestToolGate_RefusalsLeaveSignedReceipts(t *testing.T) {
	const secret = "refusal-receipts-key"
	t.Setenv("DRAFTCAT_APPROVAL_SECRET", secret)
	_, st := lifecycleHTTPStore(t)
	cfg := gateCfg(config.ToolRule{Name: "read_calendar"}, config.ToolRule{Name: "send_email", RequireApproval: true})
	cfg.Timeouts.OperatorApproval = "1m"
	g := newToolGate(cfg, &stubApprovalChannel{action: "approve", delay: time.Minute})
	caller := &config.GateCaller{Name: "harness-1"}
	post := func(path, body string) (int, ToolCallResponse) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req = req.WithContext(withGateCaller(req.Context(), caller))
		if path == toolGatePath {
			g.HandleCall(rec, req)
		} else {
			g.HandleStatus(rec, req)
		}
		var resp ToolCallResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return rec.Code, resp
	}
	consume := func(binding string) int {
		code, _ := post(toolGatePath+"/r-permit/consume", `{"binding_hash":"`+binding+`"}`)
		return code
	}

	steps := []struct {
		name, body string
		code       int
	}{
		{"malformed json", `{"action_id":"r-malformed","tool":`, http.StatusBadRequest},
		{"unknown field", `{"action_id":"r-field","tool":"read_calendar","argz":{}}`, http.StatusBadRequest},
		{"invalid action_id", `{"action_id":"bad id!","tool":"read_calendar"}`, http.StatusBadRequest},
		{"no tool", `{"action_id":"r-notool","args":{}}`, http.StatusOK},
		{"agent not the caller", `{"action_id":"r-agent","tool":"read_calendar","agent":"other-agent"}`, http.StatusForbidden},
	}
	for _, s := range steps {
		if code, _ := post(toolGatePath, s.body); code != s.code {
			t.Fatalf("%s: code %d, want %d", s.name, code, s.code)
		}
	}
	code, permit := post(toolGatePath, `{"action_id":"r-permit","tool":"read_calendar","args":{"day":"mon"}}`)
	if code != http.StatusOK || permit.State != "allowed" {
		t.Fatalf("permit: code %d %+v", code, permit)
	}
	if code, _ := post(toolGatePath, `{"action_id":"r-permit","tool":"read_calendar","args":{"day":"tue"}}`); code != http.StatusConflict {
		t.Fatalf("reused action_id: code %d, want 409", code)
	}
	if _, err := st.SetGateFreeze("drill", "cli", time.Now()); err != nil {
		t.Fatal(err)
	}
	if code := consume(permit.BindingHash); code != http.StatusConflict {
		t.Fatalf("consume while frozen: code %d, want 409", code)
	}
	if _, err := st.ClearGateFreeze("cli", time.Now()); err != nil {
		t.Fatal(err)
	}
	if code := consume("sha256:00"); code != http.StatusConflict {
		t.Fatalf("consume with another binding: code %d, want 409", code)
	}
	if code := consume(permit.BindingHash); code != http.StatusOK {
		t.Fatalf("consume: code %d, want 200", code)
	}
	if code := consume(permit.BindingHash); code != http.StatusConflict {
		t.Fatalf("second consume: code %d, want 409", code)
	}
	// The approval window closes with no answer from the operator.
	expires := time.Now().Add(2 * time.Second).UTC().Format(time.RFC3339)
	if code, _ := post(toolGatePath, `{"action_id":"r-timeout","tool":"send_email","mode":"async","expires_at":"`+expires+`"}`); code != http.StatusAccepted {
		t.Fatalf("timeout ask: code %d, want 202", code)
	}
	if _, final := getGate(t, g, "r-timeout", "?wait=5s"); final.Decision != "deny" {
		t.Fatalf("timed out approval: %+v, want deny", final)
	}

	rows, err := st.ApprovalsForPipeline("tool-gate", 100)
	if err != nil {
		t.Fatal(err)
	}
	var export bytes.Buffer
	for _, r := range rows {
		b, _ := json.Marshal(receiptView(r, []byte(secret)))
		export.Write(append(b, '\n'))
	}
	want := []struct{ actionID, step, policy string }{
		{"", "-", "malformed json"},
		{"", "-", "malformed json"},
		{"", "read_calendar", "action_id must be 1-128 URL-safe characters"},
		{"r-notool", "-", "no tool named"},
		{"r-agent", "read_calendar", `agent "other-agent" does not match the caller token (caller harness-1)`},
		{"r-permit", "read_calendar", "action_id is already bound to different action data"},
		{"r-permit", "read_calendar", "permit not consumed — tool gate frozen by operator cli: drill"},
		{"r-permit", "read_calendar", "permit not consumed — binding_hash does not match this action"},
		{"r-permit", "read_calendar", "permit not consumed — permit is consumed"},
		{"r-timeout", "send_email", "approval timed out"},
	}
	used := map[int]bool{}
	for _, w := range want {
		found := false
		for i, r := range rows {
			if used[i] || r.ActionID != w.actionID || r.Step != w.step || !strings.Contains(r.Policy, w.policy) {
				continue
			}
			used[i], found = true, true
			if r.Decision != "deny" || r.OperatorID != 0 || r.QuorumGot != 0 {
				t.Errorf("receipt %q: decision=%s operator=%d quorum_got=%d, want a deny no human made", w.policy, r.Decision, r.OperatorID, r.QuorumGot)
			}
			if v := statestore.VerifyApprovalRecord([]byte(secret), r); v != "ok" {
				t.Errorf("receipt %q: verification %s, want ok", w.policy, v)
			}
			break
		}
		if !found {
			t.Errorf("no receipt for %s/%s with policy %q", orDash(w.actionID), w.step, w.policy)
		}
	}
	checks, err := verifyReceiptStream(&export, []byte(secret))
	if err != nil {
		t.Fatalf("offline verify: %v", err)
	}
	for _, c := range checks {
		if c.Verification != "ok" {
			t.Errorf("offline verify line %d %s: %s", c.Line, c.ReceiptID, c.Verification)
		}
	}
	if t.Failed() {
		for _, r := range rows {
			t.Logf("receipt: %s %s %s %q", orDash(r.ActionID), r.Step, r.Decision, r.Policy)
		}
	}
}
