package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

// countingChannel is a stub channel that also counts how often a human was
// asked and keeps every plain notice, so a test can assert "no prompt went
// out" and "the operator was told" directly.
type countingChannel struct {
	stubApprovalChannel
	mu      sync.Mutex
	asks    int
	notices []string
}

func (c *countingChannel) SendForApproval(ctx context.Context, d string, a []int64) (OperatorDecision, error) {
	c.mu.Lock()
	c.asks++
	c.mu.Unlock()
	return c.stubApprovalChannel.SendForApproval(ctx, d, a)
}

func (c *countingChannel) Send(text string) error {
	c.mu.Lock()
	c.notices = append(c.notices, text)
	c.mu.Unlock()
	return nil
}

func (c *countingChannel) askCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.asks
}

func (c *countingChannel) noticeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.notices)
}

func postGate(t *testing.T, g *toolGate, body string) (int, ToolCallResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	g.HandleCall(rec, httptest.NewRequest(http.MethodPost, toolGatePath, strings.NewReader(body)))
	var resp ToolCallResponse
	if rec.Code != http.StatusBadRequest {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, resp
}

func getGate(t *testing.T, g *toolGate, id, query string) (int, ToolCallResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	g.HandleStatus(rec, httptest.NewRequest(http.MethodGet, toolGatePath+"/"+id+query, nil))
	var resp ToolCallResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return rec.Code, resp
}

func glob(pattern string) config.ArgConstraint { return config.ArgConstraint{Glob: pattern} }

// --- argument constraints ---

func TestToolGate_ArgsInsideRuleAllowWithoutHuman(t *testing.T) {
	ch := &countingChannel{stubApprovalChannel: stubApprovalChannel{action: "skip"}}
	g := newToolGate(gateCfg(config.ToolRule{Name: "send_email", Args: map[string]config.ArgConstraint{"to": glob("*@example.com")}}), ch)
	_, resp := postGate(t, g, `{"tool":"send_email","args":{"to":"anna@example.com"}}`)
	if resp.Decision != "allow" || resp.DecidedBy != "allowlist" {
		t.Fatalf("got %+v, want allow via allowlist for an argument inside the rule", resp)
	}
	if ch.askCount() != 0 {
		t.Fatalf("human was asked %d time(s) for a call the rule allows", ch.askCount())
	}
}

func TestToolGate_ArgsOutsideRuleEscalateToHuman(t *testing.T) {
	rule := config.ToolRule{Name: "send_email", Args: map[string]config.ArgConstraint{"to": glob("*@example.com")}}
	// Human says yes → allow, but only because the human said so.
	ch := &countingChannel{stubApprovalChannel: stubApprovalChannel{action: "approve", id: 111}}
	_, resp := postGate(t, newToolGate(gateCfg(rule), ch), `{"tool":"send_email","args":{"to":"mallory@evil.example"}}`)
	if resp.Decision != "allow" || resp.DecidedBy != "operator" {
		t.Fatalf("got %+v, want allow via operator on a mismatch", resp)
	}
	if ch.askCount() != 1 {
		t.Fatalf("human asked %d time(s), want 1", ch.askCount())
	}
	// Human says no → deny.
	ch = &countingChannel{stubApprovalChannel: stubApprovalChannel{action: "skip"}}
	_, resp = postGate(t, newToolGate(gateCfg(rule), ch), `{"tool":"send_email","args":{"to":"mallory@evil.example"}}`)
	if resp.Decision != "deny" {
		t.Fatalf("got %+v, want deny when the human declines a mismatch", resp)
	}
}

func TestToolGate_MismatchDeniesWhenRuleSaysSo(t *testing.T) {
	ch := &countingChannel{stubApprovalChannel: stubApprovalChannel{action: "approve"}}
	rule := config.ToolRule{Name: "send_email", OnMismatch: "deny",
		Args: map[string]config.ArgConstraint{"to": glob("*@example.com")}}
	_, resp := postGate(t, newToolGate(gateCfg(rule), ch), `{"tool":"send_email","args":{"to":"mallory@evil.example"}}`)
	if resp.Decision != "deny" || resp.DecidedBy != "policy" {
		t.Fatalf("got %+v, want deny via policy", resp)
	}
	if !strings.HasPrefix(resp.Rule, "args.to") {
		t.Fatalf("rule = %q, want it to name the failing argument", resp.Rule)
	}
	if ch.askCount() != 0 {
		t.Fatal("on_mismatch: deny must not consult the human")
	}
	if ch.noticeCount() != 1 || !strings.Contains(ch.notices[0], "DENIED send_email") {
		t.Fatalf("operator notices = %v, want one denial notice", ch.notices)
	}
}

func TestToolGate_MissingArgIsAMismatch(t *testing.T) {
	ch := &countingChannel{stubApprovalChannel: stubApprovalChannel{action: "skip"}}
	rule := config.ToolRule{Name: "send_email", Args: map[string]config.ArgConstraint{"to": glob("*@example.com")}}
	_, resp := postGate(t, newToolGate(gateCfg(rule), ch), `{"tool":"send_email","args":{"subject":"hi"}}`)
	if resp.Decision != "deny" || ch.askCount() != 1 {
		t.Fatalf("got %+v after %d ask(s); a missing constrained argument must go to the human", resp, ch.askCount())
	}
}

func TestToolGate_OptionalArgMayBeAbsent(t *testing.T) {
	ch := &countingChannel{stubApprovalChannel: stubApprovalChannel{action: "skip"}}
	rule := config.ToolRule{Name: "send_email", Args: map[string]config.ArgConstraint{"cc": {Glob: "*@example.com", Optional: true}}}
	_, resp := postGate(t, newToolGate(gateCfg(rule), ch), `{"tool":"send_email","args":{"to":"x"}}`)
	if resp.Decision != "allow" || ch.askCount() != 0 {
		t.Fatalf("got %+v after %d ask(s); an optional absent argument is not a mismatch", resp, ch.askCount())
	}
}

// A match never loosens a rule: require_approval still asks.
func TestToolGate_MatchDoesNotSkipARequiredApproval(t *testing.T) {
	ch := &countingChannel{stubApprovalChannel: stubApprovalChannel{action: "approve"}}
	rule := config.ToolRule{Name: "send_email", RequireApproval: true, Args: map[string]config.ArgConstraint{"to": glob("*@example.com")}}
	_, resp := postGate(t, newToolGate(gateCfg(rule), ch), `{"tool":"send_email","args":{"to":"anna@example.com"}}`)
	if resp.Decision != "allow" || resp.DecidedBy != "operator" || ch.askCount() != 1 {
		t.Fatalf("got %+v after %d ask(s); a matching call still needs the human when the rule requires one", resp, ch.askCount())
	}
}

// --- denial notices ---

func TestToolGate_UnlistedToolIsReportedOncePerWindow(t *testing.T) {
	ch := &countingChannel{stubApprovalChannel: stubApprovalChannel{action: "approve"}}
	g := newToolGate(gateCfg(config.ToolRule{Name: "read_calendar"}), ch)
	for i := 0; i < 3; i++ {
		_, resp := postGate(t, g, `{"tool":"send_email","agent":"h1"}`)
		if resp.Decision != "deny" || resp.Rule != "not listed" {
			t.Fatalf("got %+v, want deny with rule 'not listed'", resp)
		}
	}
	if ch.noticeCount() != 1 {
		t.Fatalf("operator got %d notice(s) for a looping agent, want 1", ch.noticeCount())
	}
	if !strings.Contains(ch.notices[0], "send_email") || !strings.Contains(ch.notices[0], "h1") {
		t.Fatalf("notice %q does not name the tool and agent", ch.notices[0])
	}
	// A different tool is a different fact.
	postGate(t, g, `{"tool":"delete_everything","agent":"h1"}`)
	if ch.noticeCount() != 2 {
		t.Fatalf("operator got %d notice(s), want 2 after a second tool", ch.noticeCount())
	}
}

func TestToolGate_DenialNoticesCanBeTurnedOff(t *testing.T) {
	ch := &countingChannel{stubApprovalChannel: stubApprovalChannel{action: "approve"}}
	cfg := gateCfg(config.ToolRule{Name: "read_calendar"})
	off := false
	cfg.ToolGate.NotifyDenials = &off
	postGate(t, newToolGate(cfg, ch), `{"tool":"send_email"}`)
	if ch.noticeCount() != 0 {
		t.Fatalf("operator got %d notice(s) with notify_denials: false", ch.noticeCount())
	}
}

// --- repeat guard ---

func TestToolGate_RepeatOfDeniedCallIsDeniedWithoutAsking(t *testing.T) {
	ch := &countingChannel{stubApprovalChannel: stubApprovalChannel{action: "skip"}}
	g := newToolGate(gateCfg(config.ToolRule{Name: "send_email", RequireApproval: true}), ch)
	body := `{"tool":"send_email","args":{"to":"anna@example.com"},"agent":"h1"}`
	postGate(t, g, body)
	_, resp := postGate(t, g, body)
	if resp.Decision != "deny" || resp.DecidedBy != "repeat-guard" || resp.Rule != "repeat_window" {
		t.Fatalf("got %+v, want deny via repeat-guard", resp)
	}
	if ch.askCount() != 1 {
		t.Fatalf("human asked %d time(s), want 1 — the repeat must not re-prompt", ch.askCount())
	}
	if ch.noticeCount() != 1 || !strings.Contains(ch.notices[0], "repeat") {
		t.Fatalf("notices = %v, want one repeat-guard notice", ch.notices)
	}
	// Different arguments are a different call.
	_, resp = postGate(t, g, `{"tool":"send_email","args":{"to":"bob@example.com"},"agent":"h1"}`)
	if resp.DecidedBy != "operator" || ch.askCount() != 2 {
		t.Fatalf("got %+v after %d ask(s); changed arguments must reach the human", resp, ch.askCount())
	}
}

func TestToolGate_RepeatGuardOffAsksEveryTime(t *testing.T) {
	ch := &countingChannel{stubApprovalChannel: stubApprovalChannel{action: "skip"}}
	cfg := gateCfg(config.ToolRule{Name: "send_email", RequireApproval: true})
	cfg.ToolGate.RepeatWindow = "0"
	g := newToolGate(cfg, ch)
	body := `{"tool":"send_email","args":{"to":"anna@example.com"}}`
	postGate(t, g, body)
	postGate(t, g, body)
	if ch.askCount() != 2 {
		t.Fatalf("human asked %d time(s), want 2 with repeat_window: 0", ch.askCount())
	}
}

func TestToolGate_ApprovalIsNotRememberedByDefault(t *testing.T) {
	ch := &countingChannel{stubApprovalChannel: stubApprovalChannel{action: "approve", id: 111}}
	g := newToolGate(gateCfg(config.ToolRule{Name: "send_email", RequireApproval: true}), ch)
	body := `{"tool":"send_email","args":{"to":"anna@example.com"}}`
	postGate(t, g, body)
	_, resp := postGate(t, g, body)
	if resp.DecidedBy != "operator" || ch.askCount() != 2 {
		t.Fatalf("got %+v after %d ask(s); approving one send must not approve the next", resp, ch.askCount())
	}
}

func TestToolGate_RememberApprovalReusesIt(t *testing.T) {
	ch := &countingChannel{stubApprovalChannel: stubApprovalChannel{action: "approve", id: 111}}
	g := newToolGate(gateCfg(config.ToolRule{Name: "read_crm", RequireApproval: true, RememberApproval: true}), ch)
	body := `{"tool":"read_crm","args":{"q":"anna"}}`
	postGate(t, g, body)
	_, resp := postGate(t, g, body)
	if resp.Decision != "allow" || resp.DecidedBy != "repeat-guard" || resp.Rule != "remember_approval" {
		t.Fatalf("got %+v, want allow via repeat-guard", resp)
	}
	if ch.askCount() != 1 {
		t.Fatalf("human asked %d time(s), want 1", ch.askCount())
	}
}

func TestToolGate_MaxRepeatsStopsAsking(t *testing.T) {
	ch := &countingChannel{stubApprovalChannel: stubApprovalChannel{action: "approve", id: 111}}
	cfg := gateCfg(config.ToolRule{Name: "send_email", RequireApproval: true})
	cfg.ToolGate.MaxRepeats = 2
	g := newToolGate(cfg, ch)
	body := `{"tool":"send_email","args":{"to":"anna@example.com"},"agent":"looper"}`
	postGate(t, g, body)
	postGate(t, g, body)
	_, resp := postGate(t, g, body)
	if resp.Decision != "deny" || resp.Rule != "max_repeats" {
		t.Fatalf("got %+v, want deny via max_repeats on the third identical ask", resp)
	}
	if ch.askCount() != 2 {
		t.Fatalf("human asked %d time(s), want exactly max_repeats=2", ch.askCount())
	}
}

// --- async tickets ---

func TestToolGate_AsyncReturnsTicketThenDecision(t *testing.T) {
	ch := &countingChannel{stubApprovalChannel: stubApprovalChannel{action: "approve", id: 111, delay: 150 * time.Millisecond}}
	g := newToolGate(gateCfg(config.ToolRule{Name: "send_email", RequireApproval: true}), ch)
	code, resp := postGate(t, g, `{"tool":"send_email","mode":"async"}`)
	if code != http.StatusAccepted || resp.Decision != "pending" || resp.ApprovalID == "" || resp.Poll == "" {
		t.Fatalf("async POST returned %d %+v, want 202 pending with an id and a poll path", code, resp)
	}
	if code, r := getGate(t, g, resp.ApprovalID, ""); code != http.StatusOK || r.Decision != "pending" {
		t.Fatalf("immediate GET returned %d %+v, want 200 pending", code, r)
	}
	code, final := getGate(t, g, resp.ApprovalID, "?wait=2s")
	if code != http.StatusOK || final.Decision != "allow" || final.DecidedBy != "operator" || final.ApprovalID != resp.ApprovalID {
		t.Fatalf("long-poll GET returned %d %+v, want the operator's allow", code, final)
	}
}

func TestToolGate_WaitBoundedHoldFallsBackToTicket(t *testing.T) {
	ch := &countingChannel{stubApprovalChannel: stubApprovalChannel{action: "approve", id: 111, delay: 300 * time.Millisecond}}
	g := newToolGate(gateCfg(config.ToolRule{Name: "send_email", RequireApproval: true}), ch)
	start := time.Now()
	code, resp := postGate(t, g, `{"tool":"send_email","wait":"40ms"}`)
	if code != http.StatusAccepted || resp.Decision != "pending" {
		t.Fatalf("bounded hold returned %d %+v, want 202 pending", code, resp)
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Fatalf("bounded hold took %s, want ~40ms", time.Since(start))
	}
	if _, final := getGate(t, g, resp.ApprovalID, "?wait=2s"); final.Decision != "allow" {
		t.Fatalf("decision after the hold = %+v, want allow", final)
	}
}

func TestToolGate_SyncCallStillHoldsForTheDecision(t *testing.T) {
	ch := &countingChannel{stubApprovalChannel: stubApprovalChannel{action: "approve", id: 111, delay: 100 * time.Millisecond}}
	g := newToolGate(gateCfg(config.ToolRule{Name: "send_email", RequireApproval: true}), ch)
	code, resp := postGate(t, g, `{"tool":"send_email"}`)
	if code != http.StatusOK || resp.Decision != "allow" || resp.ApprovalID == "" {
		t.Fatalf("sync POST returned %d %+v, want 200 allow carrying the approval id", code, resp)
	}
}

func TestToolGate_UnknownTicketDenies(t *testing.T) {
	g := newToolGate(gateCfg(config.ToolRule{Name: "x"}), nil)
	code, resp := getGate(t, g, "tc_nope", "")
	if code != http.StatusNotFound || resp.Decision != "deny" {
		t.Fatalf("unknown id returned %d %+v, want 404 deny", code, resp)
	}
}

func TestToolGate_IdenticalInFlightCallJoinsTheOpenPrompt(t *testing.T) {
	ch := &countingChannel{stubApprovalChannel: stubApprovalChannel{action: "approve", id: 111, delay: 200 * time.Millisecond}}
	g := newToolGate(gateCfg(config.ToolRule{Name: "send_email", RequireApproval: true}), ch)
	body := `{"tool":"send_email","args":{"to":"anna@example.com"},"agent":"h1","mode":"async"}`
	_, a := postGate(t, g, body)
	_, b := postGate(t, g, body)
	if a.ApprovalID == "" || a.ApprovalID != b.ApprovalID {
		t.Fatalf("identical in-flight calls got ids %q and %q, want the same ticket", a.ApprovalID, b.ApprovalID)
	}
	// The prompt goes out on the asker goroutine; give it a moment, then make
	// sure the second call did not add a prompt of its own.
	waitFor(t, "the single prompt", func() bool { return ch.askCount() >= 1 })
	if _, final := getGate(t, g, a.ApprovalID, "?wait=2s"); final.Decision != "allow" {
		t.Fatalf("joined decision = %+v, want allow", final)
	}
	if ch.askCount() != 1 {
		t.Fatalf("human asked %d time(s), want 1", ch.askCount())
	}
}

func TestToolGate_BadModeIsRejected(t *testing.T) {
	g := newToolGate(gateCfg(config.ToolRule{Name: "x"}), nil)
	if code, _ := postGate(t, g, `{"tool":"x","mode":"later"}`); code != http.StatusBadRequest {
		t.Fatalf("mode=later returned %d, want 400", code)
	}
}

// --- durable rows ---

// A tool-call gate waiting on a human is written to pending_approvals like a
// pipeline gate, so a crash mid-wait is reconciled instead of vanishing, and
// /pending can list it.
func TestToolGate_HumanPathWritesAPendingRowAndResolvesIt(t *testing.T) {
	st, err := statestore.OpenStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	prev := state
	state = st
	t.Cleanup(func() { state = prev; _ = st.Close() })

	ch := &countingChannel{stubApprovalChannel: stubApprovalChannel{action: "approve", id: 111, delay: 300 * time.Millisecond}}
	g := newToolGate(gateCfg(config.ToolRule{Name: "send_email", RequireApproval: true}), ch)
	_, resp := postGate(t, g, `{"tool":"send_email","args":{"to":"anna@example.com"},"mode":"async"}`)

	deadline := time.Now().Add(time.Second)
	var open []statestore.PendingApproval
	for time.Now().Before(deadline) {
		open, _ = st.OpenApprovals()
		if len(open) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(open) != 1 || open[0].Pipeline != "tool-gate" || open[0].Step != "send_email" {
		t.Fatalf("open gates while waiting = %+v, want one tool-gate/send_email row", open)
	}
	if open[0].PayloadHash != resp.ArgsHash {
		t.Fatalf("pending row hash %q != args hash %q", open[0].PayloadHash, resp.ArgsHash)
	}

	if _, final := getGate(t, g, resp.ApprovalID, "?wait=2s"); final.Decision != "allow" {
		t.Fatalf("decision = %+v, want allow", final)
	}
	if open, _ = st.OpenApprovals(); len(open) != 0 {
		t.Fatalf("open gates after the decision = %d, want 0", len(open))
	}
	rows, err := st.ApprovalsForPipeline("tool-gate", 10)
	if err != nil || len(rows) != 1 || rows[0].Decision != "approve" || rows[0].OperatorID != 111 {
		t.Fatalf("audit rows = %+v (err %v), want one approve by 111", rows, err)
	}
}
