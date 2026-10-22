package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	"github.com/renezander030/draftcat/internal/outputschema"
)

// schemaHandler builds a webhook handler whose one zero-step pipeline ("order")
// declares an input_schema with an exact decimal bound.
func schemaHandler(t *testing.T) (http.Handler, *Scheduler) {
	t.Helper()
	cfg := &config.Config{Pipelines: []config.PipelineConfig{{
		Name: "order", Schedule: "webhook",
		InputSchema: outputschema.Schema{
			"amount": map[string]interface{}{"type": "number", "min": json.Number("0"), "max": json.Number("100.10")},
			"kind":   map[string]interface{}{"enum": []interface{}{"new", "refund"}},
		},
	}}}
	cfg.Webhook.Enabled = true
	cfg.Webhook.SetSecret("s3cret")
	sched := newScheduler(cfg.Pipelines)
	return newWebhookHandler(cfg, sched, &BudgetTracker{dayStart: time.Now()}, &TGBot{}, nil), sched
}

// hashKey mirrors how the handler stores an Idempotency-Key.
func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func waitReleased(t *testing.T, sched *Scheduler, name string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ok, _ := sched.TryStart(name); ok {
			sched.Release(name)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pipeline %s did not release its running flag", name)
}

func TestWebhookInputSchemaRejectsBeforeAdmission(t *testing.T) {
	prev := state
	state = newTempStateStore(t)
	t.Cleanup(func() { state = prev })
	h, sched := schemaHandler(t)

	cases := map[string]struct{ body, path string }{
		"above exact max": {`{"amount": 100.1000000000000000001, "kind": "new"}`, "amount"},
		"missing field":   {`{"amount": 5}`, "kind"},
		"enum":            {`{"amount": 5, "kind": "other"}`, "kind"},
		"wrong type":      {`{"amount": "5", "kind": "new"}`, "amount"},
		"not json":        {`amount=5`, ""},
		"duplicate key":   {`{"amount": 5, "amount": 6, "kind": "new"}`, ""},
	}
	for name, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/hooks/order", strings.NewReader(c.body))
		req.Header.Set("Authorization", "Bearer s3cret")
		req.Header.Set("Idempotency-Key", "key-"+strings.ReplaceAll(name, " ", "-"))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: code=%d body=%s, want 422", name, rr.Code, rr.Body.String())
			continue
		}
		if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s: content type %q", name, ct)
		}
		var reply map[string]string
		if err := json.Unmarshal(rr.Body.Bytes(), &reply); err != nil {
			t.Errorf("%s: reply is not JSON: %v", name, err)
			continue
		}
		if reply["path"] != c.path || reply["error"] == "" || reply["detail"] == "" {
			t.Errorf("%s: reply=%v, want path %q", name, reply, c.path)
		}
		if strings.Contains(reply["detail"], "other") || strings.Contains(reply["detail"], "amount=5") {
			t.Errorf("%s: body content leaked into the reply: %v", name, reply)
		}
		// Nothing was admitted: no idempotency record, and the scheduler slot
		// was never taken.
		if _, err := state.WebhookAdmissionForKey("order", hashKey("key-"+strings.ReplaceAll(name, " ", "-"))); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("%s: an admission was recorded for a rejected body (err=%v)", name, err)
		}
		if ok, reason := sched.TryStart("order"); !ok {
			t.Errorf("%s: pipeline left running after a rejected body: %s", name, reason)
		} else {
			sched.Release("order")
		}
	}
}

func TestWebhookInputSchemaAuthenticatesFirst(t *testing.T) {
	h, _ := schemaHandler(t)
	if rr := post(h, "/hooks/order", "Bearer wrong", `{}`); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad bearer with a bad body = %d, want 401", rr.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/hooks/order", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer s3cret")
	req.Header.Set(webhookSigHeader, "t=1,v1=00")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature with a bad body = %d, want 401", rr.Code)
	}
}

func TestWebhookInputSchemaAcceptsMatchingBody(t *testing.T) {
	h, sched := schemaHandler(t)
	rr := post(h, "/hooks/order", "Bearer s3cret", `{"amount": 100.10, "kind": "refund", "note": "extra fields pass"}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("matching body = %d (%s), want 202", rr.Code, rr.Body.String())
	}
	waitReleased(t, sched, "order")
}

// Without input_schema a body that is not JSON is still admitted.
func TestWebhookWithoutInputSchemaAcceptsAnyBody(t *testing.T) {
	h, sched := testHandler(t)
	if rr := post(h, "/hooks/ping", "Bearer s3cret", "plain text"); rr.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s, want 202", rr.Code, rr.Body.String())
	}
	waitReleased(t, sched, "ping")
}

// `draftcat test` checks a seeded webhook_body against input_schema.
func TestFixtureWebhookBodyMeetsInputSchema(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, "pipelines:\n  - name: order\n    schedule: webhook\n    input_schema:\n      amount: {type: number, max: 100.10}\n    steps: []\n")
	fixtures := filepath.Join(dir, "fixtures")
	if err := os.MkdirAll(filepath.Join(fixtures, "order"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(body string) int {
		writeFile(t, filepath.Join(fixtures, "order", "_input.json"), body)
		return runTestCmd([]string{"order", "--config", cfgPath, "--skills", filepath.Join(dir, "skills"), "--fixtures", fixtures})
	}
	if code := run(`{"webhook_body": "{\"amount\": 100.10}"}`); code != 0 {
		t.Errorf("a matching webhook_body must pass, exit %d", code)
	}
	if code := run(`{"webhook_body": "{\"amount\": 100.100000000000000001}"}`); code != 1 {
		t.Errorf("a webhook_body above max must fail, exit %d", code)
	}
	if code := run(`{"input": "no webhook body"}`); code != 0 {
		t.Errorf("a fixture without webhook_body is not checked, exit %d", code)
	}
}
