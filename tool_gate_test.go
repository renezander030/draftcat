package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
)

// stubApprovalChannel answers every gate with a fixed decision, so the tool
// gate's own logic is what is under test.
type stubApprovalChannel struct {
	action string
	id     int64
	delay  time.Duration
}

func (s *stubApprovalChannel) Name() string      { return "stub" }
func (s *stubApprovalChannel) Send(string) error { return nil }
func (s *stubApprovalChannel) SendForApproval(ctx context.Context, _ string, _ []int64) (OperatorDecision, error) {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return OperatorDecision{Action: "timeout"}, nil
		}
	}
	return OperatorDecision{Action: s.action, ApproverID: s.id}, nil
}
func (s *stubApprovalChannel) SendForQuorumApproval(ctx context.Context, d string, _ int, a []int64) (QuorumDecision, error) {
	dec, err := s.SendForApproval(ctx, d, a)
	return QuorumDecision{Action: dec.Action, Approvers: []int64{dec.ApproverID}}, err
}

func callToolGate(t *testing.T, cfg *config.Config, ch OperatorChannel, body string) ToolCallResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, toolGatePath, strings.NewReader(body))
	handleToolCall(cfg, ch)(rec, req)
	var resp ToolCallResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return resp
}

func gateCfg(tools ...config.ToolRule) *config.Config {
	return &config.Config{
		ToolGate: config.ToolGateConfig{Enabled: true, Tools: tools},
		Timeouts: config.TimeoutConfig{OperatorApproval: "2s"},
	}
}

// Default deny is the property that makes a forgotten tool fail closed.
func TestToolGate_UnlistedToolIsDenied(t *testing.T) {
	resp := callToolGate(t, gateCfg(config.ToolRule{Name: "read_calendar"}), &stubApprovalChannel{action: "approve"},
		`{"tool":"send_email","args":{"to":"anna@example.com"},"agent":"harness-1"}`)
	if resp.Decision != "deny" {
		t.Fatalf("decision = %q, want deny for an unlisted tool", resp.Decision)
	}
	if resp.DecidedBy != "allowlist" {
		t.Fatalf("decided_by = %q, want allowlist", resp.DecidedBy)
	}
}

// An empty allowlist denies everything rather than allowing everything.
func TestToolGate_EmptyAllowlistDeniesAll(t *testing.T) {
	resp := callToolGate(t, gateCfg(), &stubApprovalChannel{action: "approve"}, `{"tool":"anything"}`)
	if resp.Decision != "deny" {
		t.Fatalf("decision = %q, want deny with an empty allowlist", resp.Decision)
	}
}

func TestToolGate_AllowlistedToolIsAllowedWithoutHuman(t *testing.T) {
	// The channel would REJECT if consulted; the allowlist must not consult it.
	resp := callToolGate(t, gateCfg(config.ToolRule{Name: "read_calendar"}), &stubApprovalChannel{action: "skip"},
		`{"tool":"read_calendar"}`)
	if resp.Decision != "allow" || resp.DecidedBy != "allowlist" {
		t.Fatalf("got %+v, want allow via allowlist", resp)
	}
}

func TestToolGate_ApprovalRequiredAndGranted(t *testing.T) {
	resp := callToolGate(t, gateCfg(config.ToolRule{Name: "send_email", RequireApproval: true, Risk: "high"}),
		&stubApprovalChannel{action: "approve", id: 111}, `{"tool":"send_email","args":{"to":"anna@example.com"}}`)
	if resp.Decision != "allow" || resp.DecidedBy != "operator" {
		t.Fatalf("got %+v, want allow via operator", resp)
	}
}

func TestToolGate_ApprovalRequiredAndRefused(t *testing.T) {
	resp := callToolGate(t, gateCfg(config.ToolRule{Name: "send_email", RequireApproval: true}),
		&stubApprovalChannel{action: "skip"}, `{"tool":"send_email"}`)
	if resp.Decision != "deny" {
		t.Fatalf("decision = %q, want deny when the operator declines", resp.Decision)
	}
}

// A gate that timed out must deny. Assuming yes on silence is the one failure
// this whole component exists to prevent.
func TestToolGate_TimeoutDenies(t *testing.T) {
	cfg := gateCfg(config.ToolRule{Name: "send_email", RequireApproval: true})
	cfg.Timeouts.OperatorApproval = "150ms"
	resp := callToolGate(t, cfg, &stubApprovalChannel{action: "approve", delay: 2 * time.Second}, `{"tool":"send_email"}`)
	if resp.Decision != "deny" {
		t.Fatalf("decision = %q, want deny on timeout", resp.Decision)
	}
}

// With no operator channel, an approval-required tool must deny, not allow.
func TestToolGate_NoChannelDenies(t *testing.T) {
	resp := callToolGate(t, gateCfg(config.ToolRule{Name: "send_email", RequireApproval: true}), nil, `{"tool":"send_email"}`)
	if resp.Decision != "deny" {
		t.Fatalf("decision = %q, want deny with no operator channel", resp.Decision)
	}
}

func TestToolGate_MissingToolNameDenies(t *testing.T) {
	resp := callToolGate(t, gateCfg(config.ToolRule{Name: "x"}), &stubApprovalChannel{action: "approve"}, `{"args":{}}`)
	if resp.Decision != "deny" {
		t.Fatalf("decision = %q, want deny when no tool is named", resp.Decision)
	}
}

// The approval is bound to the exact arguments proposed, so a harness that
// calls with different ones leaves a trail that does not match.
func TestToolGate_ArgsHashBindsTheCall(t *testing.T) {
	cfg := gateCfg(config.ToolRule{Name: "send_email"})
	a := callToolGate(t, cfg, &stubApprovalChannel{action: "approve"}, `{"tool":"send_email","args":{"to":"anna@example.com"}}`)
	b := callToolGate(t, cfg, &stubApprovalChannel{action: "approve"}, `{"tool":"send_email","args":{"to":"mallory@example.com"}}`)
	if a.ArgsHash == "" || a.ArgsHash == b.ArgsHash {
		t.Fatalf("args hash does not distinguish calls: %q vs %q", a.ArgsHash, b.ArgsHash)
	}
	if !strings.HasPrefix(a.ArgsHash, "sha256:") {
		t.Fatalf("args hash %q lacks the sha256: prefix", a.ArgsHash)
	}
	// Same args must hash the same, or the trail cannot be checked later.
	c := callToolGate(t, cfg, &stubApprovalChannel{action: "approve"}, `{"tool":"send_email","args":{"to":"anna@example.com"}}`)
	if a.ArgsHash != c.ArgsHash {
		t.Fatal("args hash is not stable for identical arguments")
	}
}

func TestToolGate_RejectsNonPost(t *testing.T) {
	rec := httptest.NewRecorder()
	handleToolCall(gateCfg(), nil)(rec, httptest.NewRequest(http.MethodGet, toolGatePath, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET returned %d, want 405", rec.Code)
	}
}

// --- budget context (item 5) ---

func TestBudgetLine_OmittedWhenNothingConfigured(t *testing.T) {
	if got := budgetLine(0, 0, 0, 0); got != "" {
		t.Fatalf("budgetLine = %q, want empty when no budget is configured", got)
	}
}

func TestBudgetLine_ShowsRemainingAgainstCap(t *testing.T) {
	got := budgetLine(5, 20, 1, 4)
	for _, want := range []string{"today", "5.0000/20.0000", "75% left", "this run", "1.0000/4.0000"} {
		if !strings.Contains(got, want) {
			t.Fatalf("budgetLine = %q, missing %q", got, want)
		}
	}
}

func TestBudgetLine_NoCapStillReportsSpend(t *testing.T) {
	got := budgetLine(3, 0, 0, 0)
	if !strings.Contains(got, "no cap") || !strings.Contains(got, "3.0000") {
		t.Fatalf("budgetLine = %q, want spend with a no-cap note", got)
	}
}

func TestPctLeft_ClampsAtZero(t *testing.T) {
	if got := pctLeft(30, 20); got != 0 {
		t.Fatalf("pctLeft(30,20) = %v, want 0 rather than negative", got)
	}
}

// --- escalation (item 6) ---

type recordingChannel struct {
	stubApprovalChannel
	mu chan string
}

func newRecordingChannel() *recordingChannel {
	return &recordingChannel{mu: make(chan string, 8)}
}
func (r *recordingChannel) Send(text string) error { r.mu <- text; return nil }

func TestEscalation_FiresReminderBeforeTimeout(t *testing.T) {
	ch := newRecordingChannel()
	st := config.StepConfig{Name: "send", EscalateAfter: "80ms", EscalateTo: []int64{999}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stop := startEscalation(ctx, ch, st, "invoices", 1*time.Second)
	defer stop()

	select {
	case msg := <-ch.mu:
		if !strings.Contains(msg, "Still waiting on approval") || !strings.Contains(msg, "invoices") {
			t.Fatalf("reminder text unexpected: %q", msg)
		}
		if !strings.Contains(msg, "999") || !strings.Contains(msg, "not authorised to decide") {
			t.Fatalf("escalation list not reported as notify-only: %q", msg)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("no escalation reminder fired")
	}
}

// Resolving the gate must cancel the reminder, or an answered approval still
// nags the operator.
func TestEscalation_StopSuppressesReminder(t *testing.T) {
	ch := newRecordingChannel()
	st := config.StepConfig{Name: "send", EscalateAfter: "150ms"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stop := startEscalation(ctx, ch, st, "invoices", 1*time.Second)
	stop()
	select {
	case msg := <-ch.mu:
		t.Fatalf("reminder fired after the gate resolved: %q", msg)
	case <-time.After(400 * time.Millisecond):
	}
}

func TestEscalation_NotArmedWithoutConfig(t *testing.T) {
	ch := newRecordingChannel()
	stop := startEscalation(context.Background(), ch, config.StepConfig{Name: "send"}, "p", time.Second)
	defer stop()
	select {
	case msg := <-ch.mu:
		t.Fatalf("reminder fired with no escalate_after: %q", msg)
	case <-time.After(200 * time.Millisecond):
	}
}

// A reminder set at or past the window could never fire in time; it must not be
// armed and must not block.
func TestEscalation_LongerThanWindowIsNotArmed(t *testing.T) {
	ch := newRecordingChannel()
	st := config.StepConfig{Name: "send", EscalateAfter: "5s"}
	stop := startEscalation(context.Background(), ch, st, "p", 1*time.Second)
	defer stop()
	select {
	case msg := <-ch.mu:
		t.Fatalf("reminder fired despite exceeding the window: %q", msg)
	case <-time.After(200 * time.Millisecond):
	}
}
