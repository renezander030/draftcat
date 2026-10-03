package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

func lifecycleHTTPStore(t *testing.T) (string, *statestore.StateStore) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := statestore.OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	old := state
	state = st
	t.Cleanup(func() { _ = state.Close(); state = old })
	return path, st
}
func lifecyclePost(t *testing.T, g *toolGate, id, operation, body string) (int, ToolCallResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	g.HandleStatus(rec, httptest.NewRequest(http.MethodPost, toolGatePath+"/"+id+"/"+operation, strings.NewReader(body)))
	var resp ToolCallResponse
	if rec.Code != http.StatusBadRequest {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, resp
}
func revokeBody(binding string) string { return fmt.Sprintf(`{"binding_hash":%q}`, binding) }
func completeBody(binding, status, hash string) string {
	return fmt.Sprintf(`{"binding_hash":%q,"status":%q,"result_hash":%q}`, binding, status, hash)
}

func TestToolLifecycleHTTPCompletionAndRestart(t *testing.T) {
	path, st := lifecycleHTTPStore(t)
	cfg := gateCfg(config.ToolRule{Name: "send"})
	g := newToolGate(cfg, nil)
	_, allowed := postGate(t, g, `{"action_id":"result","tool":"send"}`)
	hash := "sha256:" + strings.Repeat("a", 64)
	body := completeBody(allowed.BindingHash, "succeeded", hash)
	if code, _ := lifecyclePost(t, g, "result", "complete", body); code != 409 {
		t.Fatalf("unconsumed result accepted: %d", code)
	}
	if code, resp := consumeGate(t, g, "result", allowed.BindingHash); code != 200 || resp.Permit != "execute" || resp.ExecutionStatus != "unreported" {
		t.Fatalf("consume=%d %+v", code, resp)
	}
	code, completed := lifecyclePost(t, g, "result", "complete", body)
	if code != 200 || completed.State != "consumed" || completed.ExecutionStatus != "succeeded" || completed.ResultHash != hash || completed.Permit != "" || completed.ExecutionEvidence != "caller_attestation" {
		t.Fatalf("completion=%d %+v", code, completed)
	}
	if code, _ := lifecyclePost(t, g, "result", "complete", completeBody(allowed.BindingHash, "failed", hash)); code != 409 {
		t.Fatalf("conflicting outcome accepted: %d", code)
	}
	if code, _ := lifecyclePost(t, g, "result", "complete", completeBody("wrong", "succeeded", hash)); code != 409 {
		t.Fatalf("changed binding accepted: %d", code)
	}
	if code, _ := lifecyclePost(t, g, "result", "complete", `{"binding_hash":"x","status":"succeeded","result":"private payload"}`); code != 400 {
		t.Fatalf("payload accepted: %d", code)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	state, err = statestore.OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	g = newToolGate(cfg, nil)
	if code, resp := getGate(t, g, "result", ""); code != 200 || resp.ExecutionStatus != "succeeded" || resp.ResultHash != hash || resp.CompletedAt != completed.CompletedAt || resp.Permit != "" || resp.Consume != "" {
		t.Fatalf("restored=%d %+v", code, resp)
	}
	if code, resp := lifecyclePost(t, g, "result", "complete", body); code != 200 || resp.CompletedAt != completed.CompletedAt {
		t.Fatalf("retry=%d %+v", code, resp)
	}
	if code, resp := consumeGate(t, g, "result", allowed.BindingHash); code != 409 || resp.Permit != "" {
		t.Fatalf("result reopened permit: %d %+v", code, resp)
	}
	if code, resp := lifecyclePost(t, g, "result", "revoke", revokeBody(allowed.BindingHash)); code != 409 || resp.State != "consumed" {
		t.Fatalf("consumed revoke=%d %+v", code, resp)
	}
}

func TestToolLifecycleHTTPPollNormalizesLiveAndRecovered(t *testing.T) {
	lifecycleHTTPStore(t)
	for _, change := range []string{"expiry", "policy"} {
		for _, recovered := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/recovered=%v", change, recovered), func(t *testing.T) {
				cfg := gateCfg(config.ToolRule{Name: "send"})
				g := newToolGate(cfg, nil)
				_, allowed := postGate(t, g, fmt.Sprintf(`{"action_id":%q,"tool":"send"}`, fmt.Sprintf("%s-%v", change, recovered)))
				expires, err := time.Parse(time.RFC3339, allowed.ExpiresAt)
				if err != nil {
					t.Fatal(err)
				}
				if change == "policy" {
					cfg.ToolGate.Tools[0].RequireApproval = true
				}
				if recovered {
					g = newToolGate(cfg, nil)
				}
				if change == "expiry" {
					g.now = func() time.Time { return expires }
				}
				code, resp := getGate(t, g, allowed.ActionID, "")
				if code != 200 || resp.Decision != "deny" || resp.State != "expired" || resp.Consume != "" || resp.Permit != "" {
					t.Fatalf("invalid poll=%d %+v", code, resp)
				}
				if code, resp := consumeGate(t, g, allowed.ActionID, allowed.BindingHash); code != 409 || resp.Permit != "" {
					t.Fatalf("invalid consume=%d %+v", code, resp)
				}
			})
		}
	}
}

type revokedPromptChannel struct {
	stubApprovalChannel
	began     chan struct{}
	cancelled chan struct{}
	release   chan struct{}
	returned  chan struct{}
}

func (ch *revokedPromptChannel) SendForApproval(ctx context.Context, _ string, _ []int64) (OperatorDecision, error) {
	close(ch.began)
	<-ctx.Done()
	close(ch.cancelled)
	<-ch.release
	close(ch.returned)
	return OperatorDecision{Action: "approve", ApproverID: 42}, nil
}

func TestToolLifecycleHTTPRevokeWakesPollAndLateApprovalCannotResurrect(t *testing.T) {
	_, st := lifecycleHTTPStore(t)
	ch := &revokedPromptChannel{began: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{})}
	cfg := gateCfg(config.ToolRule{Name: "send", RequireApproval: true})
	cfg.Timeouts.OperatorApproval = "30s"
	g := newToolGate(cfg, ch)
	_, pending := postGate(t, g, `{"action_id":"cancel","tool":"send","mode":"async"}`)
	select {
	case <-ch.began:
	case <-time.After(time.Second):
		t.Fatal("prompt never started")
	}
	poll := make(chan ToolCallResponse, 1)
	go func() {
		rec := httptest.NewRecorder()
		g.HandleStatus(rec, httptest.NewRequest(http.MethodGet, toolGatePath+"/cancel?wait=20s", nil))
		var resp ToolCallResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		poll <- resp
	}()
	if code, resp := lifecyclePost(t, g, "cancel", "revoke", revokeBody(pending.BindingHash)); code != 200 || resp.State != "revoked" || resp.Decision != "deny" {
		t.Fatalf("revoke=%d %+v", code, resp)
	}
	select {
	case resp := <-poll:
		if resp.State != "revoked" {
			t.Fatalf("poll=%+v", resp)
		}
	case <-time.After(time.Second):
		t.Fatal("revocation did not wake poll")
	}
	select {
	case <-ch.cancelled:
	case <-time.After(time.Second):
		t.Fatal("prompt was not canceled")
	}
	close(ch.release)
	<-ch.returned
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		open, err := st.OpenApprovals()
		if err != nil {
			t.Fatal(err)
		}
		if len(open) == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if code, resp := getGate(t, g, "cancel", ""); code != 200 || resp.State != "revoked" || resp.Decision != "deny" {
		t.Fatalf("late approval resurrected: %d %+v", code, resp)
	}
	if code, _ := lifecyclePost(t, g, "cancel", "revoke", revokeBody(pending.BindingHash)); code != 200 {
		t.Fatalf("revocation retry=%d", code)
	}
	if code, _ := lifecyclePost(t, g, "cancel", "revoke", revokeBody("different")); code != 409 {
		t.Fatalf("changed binding accepted=%d", code)
	}
	if code, resp := consumeGate(t, g, "cancel", pending.BindingHash); code != 409 || resp.Permit != "" {
		t.Fatalf("revoked execution=%d %+v", code, resp)
	}
}

func TestToolLifecycleHTTPAuditAndStoreFailuresDeny(t *testing.T) {
	path, st := lifecycleHTTPStore(t)
	g := newToolGate(gateCfg(config.ToolRule{Name: "send"}), nil)
	_, allowed := postGate(t, g, `{"action_id":"consume-audit","tool":"send"}`)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), `CREATE TRIGGER reject_tool_audit BEFORE INSERT ON action_approvals BEGIN SELECT RAISE(FAIL,'receipt unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if code, resp := postGate(t, g, `{"action_id":"decision-audit","tool":"send"}`); code != 503 || resp.Decision != "deny" || resp.Permit != "" {
		t.Fatalf("audit decision=%d %+v", code, resp)
	}
	if got, err := st.ToolAction("decision-audit"); err != nil || got.Status != "pending" {
		t.Fatalf("audit decision persisted=%+v %v", got, err)
	}
	if code, resp := getGate(t, g, "decision-audit", ""); code != 503 || resp.Decision != "deny" {
		t.Fatalf("failed audit was reported pending: %d %+v", code, resp)
	}
	if code, resp := consumeGate(t, g, allowed.ActionID, allowed.BindingHash); code != 503 || resp.Permit != "" {
		t.Fatalf("audit consume=%d %+v", code, resp)
	}
	if got, err := st.ToolAction(allowed.ActionID); err != nil || got.Status != "allowed" {
		t.Fatalf("audit consumed permit=%+v %v", got, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if code, resp := getGate(t, g, allowed.ActionID, ""); code != 503 || resp.Decision != "deny" {
		t.Fatalf("cached allow escaped store failure=%d %+v", code, resp)
	}
	if code, resp := consumeGate(t, g, allowed.ActionID, allowed.BindingHash); code != 503 || resp.Permit != "" {
		t.Fatalf("closed store execution=%d %+v", code, resp)
	}
}

func TestToolLifecycleHTTPRequiresDurableStoreAndExistingAuth(t *testing.T) {
	old := state
	state = nil
	t.Cleanup(func() { state = old })
	cfg := gateCfg(config.ToolRule{Name: "send"})
	g := newToolGate(cfg, nil)
	for _, operation := range []string{"consume", "revoke", "complete"} {
		body := revokeBody("binding")
		if operation == "complete" {
			body = completeBody("binding", "succeeded", "")
		}
		if code, resp := lifecyclePost(t, g, "missing", operation, body); code != 503 || resp.Permit != "" {
			t.Fatalf("%s without store=%d %+v", operation, code, resp)
		}
	}
	cfg.Webhook.SetSecret("secret")
	h := newWebhookHandler(cfg, newScheduler(nil), &BudgetTracker{}, &TGBot{}, nil)
	for _, operation := range []string{"consume", "revoke", "complete"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, toolGatePath+"/missing/"+operation, strings.NewReader(revokeBody("binding"))))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s bypassed authentication: %d", operation, rec.Code)
		}
	}
}

func TestToolLifecycleHTTPPendingWriteFailureDoesNotPrompt(t *testing.T) {
	path, _ := lifecycleHTTPStore(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), `CREATE TRIGGER reject_tool_pending BEFORE INSERT ON pending_approvals BEGIN SELECT RAISE(FAIL,'pending unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	ch := &countingChannel{stubApprovalChannel: stubApprovalChannel{action: "approve"}}
	g := newToolGate(gateCfg(config.ToolRule{Name: "send", RequireApproval: true}), ch)
	_, pending := postGate(t, g, `{"action_id":"pending-write","tool":"send","mode":"async"}`)
	code, resp := getGate(t, g, pending.ActionID, "?wait=1s")
	if code != 503 || resp.Decision != "deny" || ch.askCount() != 0 {
		t.Fatalf("pending failure=%d %+v prompts=%d", code, resp, ch.askCount())
	}
}
