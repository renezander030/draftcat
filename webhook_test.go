package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

func TestSchedulerTryStart(t *testing.T) {
	sched := newScheduler([]config.PipelineConfig{
		{Name: "p1", Schedule: "webhook"},
	})

	if ok, reason := sched.TryStart("p1"); !ok {
		t.Fatalf("first TryStart should succeed, got reason %q", reason)
	}
	if ok, reason := sched.TryStart("p1"); ok || reason != "pipeline is already running" {
		t.Errorf("second TryStart should report already-running, got ok=%v reason=%q", ok, reason)
	}

	sched.SetRunning("p1", false)
	if ok, _ := sched.TryStart("p1"); !ok {
		t.Error("TryStart should succeed after SetRunning(false)")
	}
	sched.SetRunning("p1", false)

	sched.Pause("p1")
	if ok, reason := sched.TryStart("p1"); ok || reason != "pipeline is paused" {
		t.Errorf("paused pipeline should not start, got ok=%v reason=%q", ok, reason)
	}

	if ok, reason := sched.TryStart("nope"); ok || reason != "unknown pipeline" {
		t.Errorf("unknown pipeline should report unknown, got ok=%v reason=%q", ok, reason)
	}
}

func TestWebhookPipelineNotAutoScheduled(t *testing.T) {
	sched := newScheduler([]config.PipelineConfig{
		{Name: "hooked", Schedule: "webhook"},
		{Name: "manualp", Schedule: "manual"},
	})
	// A webhook/manual pipeline must never be returned as timer-due.
	if due := sched.GetDue(); len(due) != 0 {
		t.Errorf("expected no auto-due pipelines, got %v", due)
	}
	if isAutoSchedule("webhook") {
		t.Error("'webhook' must not be an auto schedule")
	}
}

// testHandler builds a webhook handler with one zero-step pipeline ("ping")
// that is webhook-triggerable. A zero-step pipeline lets the 202 path run to
// completion without any LLM or operator-channel call.
func testHandler(t *testing.T) (http.Handler, *Scheduler) {
	t.Helper()
	cfg := &config.Config{
		Pipelines: []config.PipelineConfig{
			{Name: "ping", Schedule: "webhook"},
		},
	}
	cfg.Webhook.Enabled = true
	cfg.Webhook.SetSecret("s3cret")
	sched := newScheduler(cfg.Pipelines)
	budget := &BudgetTracker{dayStart: time.Now()}
	h := newWebhookHandler(cfg, sched, budget, &TGBot{}, nil)
	return h, sched
}

func post(h http.Handler, path, auth, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestWebhookRejectsBadMethod(t *testing.T) {
	h, _ := testHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/hooks/ping", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET should be 405, got %d", rr.Code)
	}
}

func TestWebhookRejectsBadAuth(t *testing.T) {
	h, _ := testHandler(t)
	if rr := post(h, "/hooks/ping", "", ""); rr.Code != http.StatusUnauthorized {
		t.Errorf("missing auth should be 401, got %d", rr.Code)
	}
	if rr := post(h, "/hooks/ping", "Bearer wrong", ""); rr.Code != http.StatusUnauthorized {
		t.Errorf("wrong token should be 401, got %d", rr.Code)
	}
}

func TestWebhookUnknownPipeline(t *testing.T) {
	h, _ := testHandler(t)
	if rr := post(h, "/hooks/missing", "Bearer s3cret", ""); rr.Code != http.StatusNotFound {
		t.Errorf("unknown pipeline should be 404, got %d", rr.Code)
	}
}

func TestWebhookConflictWhenRunning(t *testing.T) {
	h, sched := testHandler(t)
	// Claim the pipeline so the next trigger sees it as already running.
	if ok, _ := sched.TryStart("ping"); !ok {
		t.Fatal("could not pre-claim pipeline")
	}
	if rr := post(h, "/hooks/ping", "Bearer s3cret", ""); rr.Code != http.StatusConflict {
		t.Errorf("running pipeline should be 409, got %d", rr.Code)
	}
}

func TestWebhookAcceptsValid(t *testing.T) {
	h, sched := testHandler(t)
	rr := post(h, "/hooks/ping", "Bearer s3cret", `{"hello":"world"}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("valid trigger should be 202, got %d (%s)", rr.Code, rr.Body.String())
	}
	// The zero-step pipeline runs in a goroutine; wait for it to release.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ok, _ := sched.TryStart("ping"); ok {
			sched.SetRunning("ping", false)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("pipeline did not finish / release running flag within timeout")
}

func TestListenerHealthAndReadiness(t *testing.T) {
	prev := state
	state = nil
	t.Cleanup(func() { state = prev })
	h, _ := testHandler(t)
	for path, want := range map[string]int{"/healthz": http.StatusOK, "/readyz": http.StatusServiceUnavailable} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != want {
			t.Fatalf("%s = %d, want %d", path, rr.Code, want)
		}
	}
	st, err := statestore.OpenStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	state = st
	t.Cleanup(func() { _ = st.Close() })
	h, _ = testHandler(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("readyz = %d, want 200", rr.Code)
	}
}

func TestToolGateRoutesRequireBearer(t *testing.T) {
	prev := state
	state = nil
	t.Cleanup(func() { state = prev })
	cfg := &config.Config{Timeouts: config.TimeoutConfig{OperatorApproval: "1m"}}
	cfg.Webhook.Enabled = true
	cfg.Webhook.SetSecret("s3cret")
	cfg.ToolGate = config.ToolGateConfig{Enabled: true, Tools: []config.ToolRule{{Name: "read_calendar"}}}
	h := newWebhookHandler(cfg, newScheduler(nil), &BudgetTracker{dayStart: time.Now()}, &TGBot{}, nil)
	body := `{"action_id":"read-1","tool":"read_calendar"}`
	if rr := post(h, toolGatePath, "", body); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated gate = %d, want 401", rr.Code)
	}
	if rr := post(h, toolGatePath, "Bearer s3cret", body); rr.Code != http.StatusOK {
		t.Fatalf("authenticated gate = %d (%s), want 200", rr.Code, rr.Body.String())
	}
}

func TestToolGatePostUsesConfiguredBodySignature(t *testing.T) {
	st := newTempStateStore(t)
	prev := state
	state = st
	t.Cleanup(func() { state = prev })
	cfg := &config.Config{Timeouts: config.TimeoutConfig{OperatorApproval: "1m"}}
	cfg.Webhook.Enabled = true
	cfg.Webhook.RequireSignature = true
	cfg.Webhook.SetSecret("s3cret")
	cfg.ToolGate = config.ToolGateConfig{Enabled: true, Tools: []config.ToolRule{{Name: "read_calendar"}}}
	h := newWebhookHandler(cfg, newScheduler(nil), &BudgetTracker{dayStart: time.Now()}, &TGBot{}, nil)
	body := `{"action_id":"read-signed","tool":"read_calendar"}`
	if rr := post(h, toolGatePath, "Bearer s3cret", body); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned gate POST = %d, want 401", rr.Code)
	}
	req := httptest.NewRequest(http.MethodPost, toolGatePath, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer s3cret")
	req.Header.Set(webhookSigHeader, signBody([]byte("s3cret"), time.Now().Unix(), []byte(body)))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("signed gate POST = %d (%s), want 200", rr.Code, rr.Body.String())
	}
}

func TestWebhookWritesAdmissionBeforeAccepted(t *testing.T) {
	st, err := statestore.OpenStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	prev := state
	state = st
	t.Cleanup(func() { state = prev; _ = st.Close() })
	h, _ := testHandler(t)
	rr := post(h, "/hooks/ping", "Bearer s3cret", `{"hello":"world"}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		AdmissionID string `json:"admission_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body.AdmissionID == "" {
		t.Fatalf("response=%q err=%v", rr.Body.String(), err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		a, err := st.WebhookAdmission(body.AdmissionID)
		if err != nil || a.BodyHash == "" {
			t.Fatalf("admission=%+v err=%v", a, err)
		}
		if a.Status == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("admission stayed %q", a.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
