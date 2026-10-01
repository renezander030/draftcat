package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

func useReliabilityStore(t *testing.T) *statestore.StateStore {
	t.Helper()
	st := newTempStateStore(t)
	prev := state
	state = st
	t.Cleanup(func() { state = prev })
	return st
}

func TestWebhookKeyRetriesReturnSameAdmission(t *testing.T) {
	st := useReliabilityStore(t)
	h, sched := testHandler(t)
	request := func(body, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/hooks/ping", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer s3cret")
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set(webhookSigHeader, signBody([]byte("s3cret"), time.Now().Unix(), []byte(body)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	first := request(`{"amount":1}`, "invoice-1")
	if first.Code != 202 {
		t.Fatalf("first=%d %s", first.Code, first.Body)
	}
	var initial map[string]string
	if err := json.Unmarshal(first.Body.Bytes(), &initial); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			retry := request(`{"amount":1}`, "invoice-1")
			var v map[string]string
			if err := json.Unmarshal(retry.Body.Bytes(), &v); err != nil {
				t.Error(err)
			}
			if retry.Code != 202 || v["admission_id"] != initial["admission_id"] {
				t.Errorf("retry=%d %s", retry.Code, retry.Body)
			}
		}()
	}
	wg.Wait()
	if changed := request(`{"amount":2}`, "invoice-1"); changed.Code != 409 {
		t.Fatalf("changed=%d", changed.Code)
	}
	waitFor(t, "pipeline completion", func() bool {
		for _, p := range sched.GetAll() {
			if p.Name == "ping" {
				return !p.Running
			}
		}
		return false
	})
	runs, err := st.AllRecentRuns(100)
	if err != nil || len(runs) != 1 {
		t.Fatalf("executed runs=%d err=%v", len(runs), err)
	}
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (brokenBody) Close() error             { return nil }

func TestRequestBoundariesDoNotAdmitTruncatedData(t *testing.T) {
	useReliabilityStore(t)
	cfg := gateCfg(config.ToolRule{Name: "send"})
	cfg.Webhook.MaxBodyBytes = 32
	cfg.Webhook.SetSecret("s3cret")
	cfg.Pipelines = []config.PipelineConfig{{Name: "ping", Schedule: "webhook"}}
	h := newWebhookHandler(cfg, newScheduler(cfg.Pipelines), &BudgetTracker{}, &TGBot{}, nil)
	for _, route := range []string{"/hooks/ping", toolGatePath, toolGatePath + "/a/consume"} {
		req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(strings.Repeat("x", 33)))
		req.Header.Set("Authorization", "Bearer s3cret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 413 {
			t.Errorf("%s oversize=%d", route, rec.Code)
		}
		req = httptest.NewRequest(http.MethodPost, route, nil)
		req.Body = brokenBody{}
		req.Header.Set("Authorization", "Bearer s3cret")
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Errorf("%s unreadable=%d", route, rec.Code)
		}
	}
	// Direct gate use has its own independent bound as well.
	g := newToolGate(cfg, nil)
	rec := httptest.NewRecorder()
	g.HandleCall(rec, httptest.NewRequest(http.MethodPost, toolGatePath, strings.NewReader(strings.Repeat("x", (1<<20)+1))))
	if rec.Code != 413 {
		t.Fatalf("direct gate oversize=%d", rec.Code)
	}
}

func TestStrictToolRequestDoesNotSpendPermitOnMalformedConsume(t *testing.T) {
	useReliabilityStore(t)
	g := newToolGate(gateCfg(config.ToolRule{Name: "send"}), nil)
	for _, body := range []string{`{"tool":"send","typo":true}`, `{"tool":"send"} {}`, `{"tool":"send","args":{"id":1,"id":2}}`, `{"tool":"send","tool":"other"}`} {
		rec := httptest.NewRecorder()
		g.HandleCall(rec, httptest.NewRequest(http.MethodPost, toolGatePath, strings.NewReader(body)))
		if rec.Code != 400 {
			t.Errorf("malformed request %s accepted: %d", body, rec.Code)
		}
	}
	_, allowed := postGate(t, g, `{"action_id":"a","tool":"send"}`)
	for _, suffix := range []string{` {}`, `,"extra":true}`, `,"binding_hash":"other"}`} {
		body := fmt.Sprintf(`{"binding_hash":%q}`, allowed.BindingHash)
		if strings.HasPrefix(suffix, ",") {
			body = strings.TrimSuffix(body, "}") + suffix
		} else {
			body += suffix
		}
		rec := httptest.NewRecorder()
		g.HandleStatus(rec, httptest.NewRequest(http.MethodPost, toolGatePath+"/a/consume", strings.NewReader(body)))
		if rec.Code != 400 {
			t.Errorf("malformed consume %s accepted: %d", body, rec.Code)
		}
	}
	if code, resp := consumeGate(t, g, "a", allowed.BindingHash); code != 200 || resp.Permit != "execute" {
		t.Fatalf("valid permit lost: %d %+v", code, resp)
	}
}

func TestLargeToolArgumentsHaveDifferentIdentities(t *testing.T) {
	useReliabilityStore(t)
	g := newToolGate(gateCfg(config.ToolRule{Name: "send"}), nil)
	_, a := postGate(t, g, `{"action_id":"same","tool":"send","args":{"id":9007199254740992}}`)
	code, _ := postGate(t, g, `{"action_id":"same","tool":"send","args":{"id":9007199254740993}}`)
	if code != 409 {
		t.Fatalf("large integer collision accepted: %d", code)
	}
	_, b := postGate(t, g, `{"action_id":"different","tool":"send","args":{"id":9007199254740993}}`)
	if a.ArgsHash == b.ArgsHash {
		t.Fatal("different exact integers share a hash")
	}
}

func TestPermitRejectsChangedPolicyAfterRestart(t *testing.T) {
	useReliabilityStore(t)
	for _, change := range []string{"rule", "operators", "channel", "window"} {
		t.Run(change, func(t *testing.T) {
			cfg := gateCfg(config.ToolRule{Name: "send"})
			g := newToolGate(cfg, nil)
			_, a := postGate(t, g, fmt.Sprintf(`{"action_id":%q,"tool":"send"}`, change))
			switch change {
			case "rule":
				cfg.ToolGate.Tools[0].RequireApproval = true
			case "operators":
				cfg.Telegram.Security.AllowedUsers = []int64{42}
			case "channel":
				cfg.Relay.URL = "https://relay.example/dispatch"
			case "window":
				cfg.Timeouts.OperatorApproval = "1s"
			}
			restarted := newToolGate(cfg, nil)
			if code, resp := consumeGate(t, restarted, a.ActionID, a.BindingHash); code != 409 || resp.Permit != "" || resp.Decision != "deny" {
				t.Fatalf("changed policy consumed: %d %+v", code, resp)
			}
		})
	}
}

func TestEnginePersistsRunsAndExactApprovalJoin(t *testing.T) {
	st := useReliabilityStore(t)
	cfg := &config.Config{}
	if err := runPipeline(cfg, config.PipelineConfig{Name: "zero"}, &BudgetTracker{}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	runs, err := st.AllRecentRuns(10)
	if err != nil || len(runs) != 1 || runs[0].RunID == "" {
		t.Fatalf("engine run=%+v err=%v", runs, err)
	}
	at := runs[0].StartedAt
	for _, id := range []string{runs[0].RunID, "different-run", ""} {
		if err := st.RecordApprovalForRun(id, "zero", id, at, "approve", 7, "hash", 1, 1, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	joined := approvalsDuring(st, runs[0])
	if len(joined) != 1 || joined[0].Step != runs[0].RunID {
		t.Fatalf("inexact correlation=%+v", joined)
	}
	// A budget-blocked run must leave a terminal error record too.
	cfg.Budgets.PerStepTokens = 1
	err = runPipeline(cfg, config.PipelineConfig{Name: "failure", Steps: []config.StepConfig{{Name: "ai", Type: "ai", Skill: "missing"}}}, &BudgetTracker{}, nil, nil, nil)
	if err == nil {
		t.Fatal("expected budget failure")
	}
	failures, err := st.RecentRuns("failure", 10)
	if err != nil || len(failures) != 1 || failures[0].Status != "error" {
		t.Fatalf("failed run=%+v err=%v", failures, err)
	}
}

func TestOfflineReceiptVerificationDoesNotTrustExportVerdict(t *testing.T) {
	st := useReliabilityStore(t)
	secret := []byte("secret")
	now := time.Now()
	e := newApprovalEnvelope(secret, "r", "p", config.StepConfig{Name: "send"}, now, now.Add(time.Hour), "approve", 42, "hash", 1, 1, "human")
	if err := st.RecordApprovalV2(e); err != nil {
		t.Fatal(err)
	}
	rows, err := st.AllApprovals(10)
	if err != nil {
		t.Fatal(err)
	}
	v := receiptView(rows[0], nil)
	v.Verification = "tampered"
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	checks, err := verifyReceiptStream(bytes.NewReader(raw), secret)
	if err != nil || len(checks) != 1 || checks[0].Verification != "ok" {
		t.Fatalf("intact=%+v err=%v", checks, err)
	}
	v.PayloadHash = "edited"
	v.Verification = "ok"
	raw, _ = json.Marshal(v)
	checks, err = verifyReceiptStream(bytes.NewReader(raw), secret)
	if err != nil || checks[0].Verification != "tampered" {
		t.Fatalf("edited=%+v err=%v", checks, err)
	}
	v.Signature = ""
	raw, _ = json.Marshal(v)
	checks, err = verifyReceiptStream(bytes.NewReader(raw), secret)
	if err != nil || checks[0].Verification != "unsigned" {
		t.Fatalf("unsigned=%+v err=%v", checks, err)
	}
	for _, bad := range []string{"", string(raw) + " {}", `{"version":99,"decided_at":"2026-10-01T00:00:00Z"}`} {
		if _, err := verifyReceiptStream(strings.NewReader(bad), secret); err == nil {
			t.Errorf("bad input accepted: %q", bad)
		}
	}
	// The CLI must work with no decision database and return nonzero on unsigned rows.
	t.Setenv("DRAFTCAT_STATE_PATH", filepath.Join(t.TempDir(), "absent.db"))
	t.Setenv("DRAFTCAT_APPROVAL_SECRET", string(secret))
	path := filepath.Join(t.TempDir(), "receipts.jsonl")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if code := runReceiptsVerify([]string{path, "--json"}); code != 1 {
		t.Fatalf("unsigned CLI exit=%d", code)
	}
	if _, err := os.Stat(os.Getenv("DRAFTCAT_STATE_PATH")); !os.IsNotExist(err) {
		t.Fatal("offline verifier touched database")
	}
}

func TestStrictJSONDepthBound(t *testing.T) {
	body := []byte(`{"tool":"send","args":{"nested":` + strings.Repeat("[", 150) + "0" + strings.Repeat("]", 150) + "}}")
	var req ToolCallRequest
	if err := decodeStrictJSON(body, &req); err == nil {
		t.Fatal("unbounded nesting accepted")
	}
}

func TestUnknownConsumeRemainsStructured(t *testing.T) {
	useReliabilityStore(t)
	g := newToolGate(gateCfg(config.ToolRule{Name: "send"}), nil)
	if code, resp := consumeGate(t, g, "absent", "binding"); code != 404 || resp.Decision != "deny" {
		t.Fatalf("unknown consume=%d %+v", code, resp)
	}
}
