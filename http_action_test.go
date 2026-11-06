package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	"github.com/renezander030/draftcat/internal/redact"
	statestore "github.com/renezander030/draftcat/internal/state"
)

const testHTTPToken = "tok-http-0123456789abcdef"

// draftChannel records every draft shown for approval and answers action.
type draftChannel struct {
	stubApprovalChannel
	mu     sync.Mutex
	drafts []string
}

func (c *draftChannel) SendForApproval(ctx context.Context, draft string, approvers []int64) (OperatorDecision, error) {
	c.mu.Lock()
	c.drafts = append(c.drafts, draft)
	c.mu.Unlock()
	return c.stubApprovalChannel.SendForApproval(ctx, draft, approvers)
}

func (c *draftChannel) SendForQuorumApproval(ctx context.Context, d string, _ int, a []int64) (QuorumDecision, error) {
	dec, err := c.SendForApproval(ctx, d, a)
	return QuorumDecision{Action: dec.Action, Approvers: []int64{dec.ApproverID}}, err
}

// receiver is a test endpoint that records what reached it.
type receiver struct {
	mu       sync.Mutex
	hits     int
	auth     string
	key      string
	body     string
	status   int
	response string
}

func (r *receiver) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.hits++
		r.auth, r.key, r.body = req.Header.Get("Authorization"), req.Header.Get("Idempotency-Key"), string(b)
		status, resp := r.status, r.response
		r.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func useTempState(t *testing.T) {
	t.Helper()
	st, err := statestore.OpenStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	old := state
	state = st
	t.Cleanup(func() { state = old; _ = st.Close() })
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func httpPipeline(url string, steps ...config.StepConfig) config.PipelineConfig {
	send := config.StepConfig{Name: "send", Type: "deterministic", Action: "http_request",
		Vars: map[string]string{"url": url, "auth_env": "TEST_HTTP_TOKEN", "headers": "X-Source: agent-1"}}
	return config.PipelineConfig{Name: "outbound", Steps: append(steps, send)}
}

func reviewStep() config.StepConfig { return config.StepConfig{Name: "review", Type: "approval"} }

func TestHTTPRequestSendsTheApprovedRequest(t *testing.T) {
	redact.Reset()
	defer redact.Reset()
	useTempState(t)
	logs := captureLog(t)
	t.Setenv("TEST_HTTP_TOKEN", testHTTPToken)
	rcv := &receiver{status: http.StatusCreated, response: `{"id":"o1"}`}
	srv := rcv.server(t)
	ch := &draftChannel{stubApprovalChannel: stubApprovalChannel{action: "approve", id: 7}}
	seed := map[string]interface{}{"ai_output": map[string]interface{}{"to": "agent-1", "text": "hello"}}

	if err := runPipeline(&config.Config{}, httpPipeline(srv.URL+"/orders", reviewStep()), &BudgetTracker{dayStart: time.Now()}, ch, nil, seed); err != nil {
		t.Fatal(err)
	}
	if rcv.hits != 1 || rcv.auth != "Bearer "+testHTTPToken || rcv.key == "" || !strings.Contains(rcv.body, `"to": "agent-1"`) {
		t.Fatalf("receiver saw hits=%d auth=%q key=%q body=%q", rcv.hits, rcv.auth, rcv.key, rcv.body)
	}
	if len(ch.drafts) != 1 {
		t.Fatalf("drafts = %d", len(ch.drafts))
	}
	draft := ch.drafts[0]
	for _, want := range []string{"Outbound request for review", "POST " + srv.URL + "/orders", "<credential from TEST_HTTP_TOKEN>", "X-Source: agent-1", "Idempotency-Key: " + rcv.key, rcv.body} {
		if !strings.Contains(draft, want) {
			t.Errorf("draft lacks %q:\n%s", want, draft)
		}
	}
	if strings.Contains(draft, testHTTPToken) || strings.Contains(logs.String(), testHTTPToken) {
		t.Fatal("credential shown to the operator or logged")
	}
}

func TestHTTPRequestResponseGoesIntoRunData(t *testing.T) {
	t.Setenv("TEST_HTTP_TOKEN", testHTTPToken)
	rcv := &receiver{status: http.StatusCreated, response: `{"id":"o1","n":2}`}
	srv := rcv.server(t)
	step := httpPipeline(srv.URL).Steps[0]
	data := map[string]interface{}{"ai_output": map[string]interface{}{"to": "agent-1"}}
	_, digest, err := outboundDraft(step, "run-1", data)
	if err != nil {
		t.Fatal(err)
	}
	if err := runHTTPRequest(context.Background(), "run-1", "outbound", step, data, digest); err != nil {
		t.Fatal(err)
	}
	resp, ok := data["http_response"].(map[string]interface{})
	if !ok || data["http_status"] != "201" || resp["status"] != 201 {
		t.Fatalf("run data: %v", data)
	}
	body, ok := resp["body"].(map[string]interface{})
	if !ok || body["id"] != "o1" || !strings.Contains(data["http_response_body"].(string), `"n":2`) {
		t.Fatalf("response body: %v", resp["body"])
	}
}

func TestHTTPRequestNeedsItsApproval(t *testing.T) {
	useTempState(t)
	t.Setenv("TEST_HTTP_TOKEN", testHTTPToken)
	rcv := &receiver{}
	srv := rcv.server(t)
	seed := map[string]interface{}{"ai_output": map[string]interface{}{"to": "agent-1"}}

	err := runPipeline(&config.Config{}, httpPipeline(srv.URL), &BudgetTracker{dayStart: time.Now()}, nil, nil, seed)
	if err == nil || !strings.Contains(err.Error(), "approval step") {
		t.Fatalf("an unreleased request must fail: %v", err)
	}

	skip := &draftChannel{stubApprovalChannel: stubApprovalChannel{action: "skip", id: 7}}
	if err := runPipeline(&config.Config{}, httpPipeline(srv.URL, reviewStep()), &BudgetTracker{dayStart: time.Now()}, skip, nil, seed); err != nil {
		t.Fatal(err)
	}
	if rcv.hits != 0 {
		t.Fatalf("a request was sent without approval: %d", rcv.hits)
	}
}

func TestHTTPRequestChangedAfterApprovalIsNotSent(t *testing.T) {
	t.Setenv("TEST_HTTP_TOKEN", testHTTPToken)
	rcv := &receiver{}
	srv := rcv.server(t)
	step := httpPipeline(srv.URL).Steps[0]
	data := map[string]interface{}{"ai_output": map[string]interface{}{"amount": 10}}
	_, digest, err := outboundDraft(step, "run-1", data)
	if err != nil {
		t.Fatal(err)
	}
	data["ai_output"] = map[string]interface{}{"amount": 1000}
	if err := runHTTPRequest(context.Background(), "run-1", "outbound", step, data, digest); err == nil || !strings.Contains(err.Error(), "changed after it was approved") {
		t.Fatalf("a changed request must not be sent: %v", err)
	}
	// The digest also binds the run: the same body in another run is a new request.
	data["ai_output"] = map[string]interface{}{"amount": 10}
	if err := runHTTPRequest(context.Background(), "run-2", "outbound", step, data, digest); err == nil {
		t.Fatal("an approval from another run must not release this one")
	}
	if rcv.hits != 0 {
		t.Fatalf("hits = %d", rcv.hits)
	}
}

func TestHTTPRequestEmptyCredentialIsNotSent(t *testing.T) {
	t.Setenv("TEST_HTTP_TOKEN", "")
	rcv := &receiver{}
	srv := rcv.server(t)
	step := httpPipeline(srv.URL).Steps[0]
	data := map[string]interface{}{"ai_output": "x"}
	_, digest, _ := outboundDraft(step, "run-1", data)
	if err := runHTTPRequest(context.Background(), "run-1", "outbound", step, data, digest); err == nil || rcv.hits != 0 {
		t.Fatalf("an empty credential must refuse: err=%v hits=%d", err, rcv.hits)
	}
}

func TestHTTPRequestFailureIsRedactedEverywhere(t *testing.T) {
	redact.Reset()
	defer redact.Reset()
	useTempState(t)
	logs := captureLog(t)
	t.Setenv("TEST_HTTP_TOKEN", testHTTPToken)
	rcv := &receiver{status: http.StatusForbidden, response: `{"error":"token ` + testHTTPToken + ` refused"}`}
	srv := rcv.server(t)
	ch := &draftChannel{stubApprovalChannel: stubApprovalChannel{action: "approve", id: 7}}
	seed := map[string]interface{}{"ai_output": map[string]interface{}{"to": "agent-1"}}

	err := runPipeline(&config.Config{}, httpPipeline(srv.URL, reviewStep()), &BudgetTracker{dayStart: time.Now()}, ch, nil, seed)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || strings.Contains(err.Error(), testHTTPToken) {
		t.Fatalf("error = %v", err)
	}
	if rcv.hits != 1 {
		t.Fatalf("a failed request must not be retried: hits=%d", rcv.hits)
	}
	if redact.String("x "+testHTTPToken) == "x "+testHTTPToken {
		t.Fatal("the credential is not in the redaction set")
	}
	runs, rerr := state.RecentRuns("outbound", 1)
	if rerr != nil || len(runs) != 1 || strings.Contains(runs[0].Error, testHTTPToken) {
		t.Fatalf("stored run error leaks the credential: %+v %v", runs, rerr)
	}
	if strings.Contains(logs.String(), testHTTPToken) {
		t.Fatal("credential logged")
	}
}

func TestSecretEnvNamesIncludeHTTPAuthEnv(t *testing.T) {
	cfg := &config.Config{Pipelines: []config.PipelineConfig{httpPipeline("https://example.com", reviewStep())}}
	found := false
	for _, n := range secretEnvNames(cfg) {
		found = found || n == "TEST_HTTP_TOKEN"
	}
	if !found {
		t.Fatal("auth_env is not registered for redaction")
	}
}
