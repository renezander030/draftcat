package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
)

func consumeGateWith(t *testing.T, g *toolGate, id, body string) (int, ToolCallResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	g.HandleStatus(rec, httptest.NewRequest(http.MethodPost, toolGatePath+"/"+id+"/consume", strings.NewReader(body)))
	var resp ToolCallResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return rec.Code, resp
}

// A call without a precondition keeps the binding it had before preconditions
// existed, so permits issued before an upgrade still consume after it.
func TestToolBindingWithoutPreconditionUnchanged(t *testing.T) {
	req := ToolCallRequest{ActionID: "send-1", Agent: "a", Tool: "send_email", RunID: "r1"}
	exp := time.Unix(1_800_000_000, 0)
	if got := hashToolBinding(req, "sha256:aa", "sha256:bb", exp); got != expectedV2Binding(req, exp) {
		t.Fatalf("v2 binding changed: %s", got)
	}
	req.Precondition = "etag:7"
	if hashToolBinding(req, "sha256:aa", "sha256:bb", exp) == expectedV2Binding(ToolCallRequest{ActionID: "send-1", Agent: "a", Tool: "send_email", RunID: "r1"}, exp) {
		t.Fatal("a precondition must change the binding")
	}
}

// expectedV2Binding restates the version 2 binding independently of
// toolBindingHash.
func expectedV2Binding(req ToolCallRequest, exp time.Time) string {
	b, _ := json.Marshal(struct {
		Version    int    `json:"version"`
		ActionID   string `json:"action_id"`
		Agent      string `json:"agent"`
		Tool       string `json:"tool"`
		RunID      string `json:"run_id"`
		ArgsHash   string `json:"args_hash"`
		PolicyHash string `json:"policy_hash"`
		ExpiresAt  int64  `json:"expires_at"`
	}{2, req.ActionID, req.Agent, req.Tool, req.RunID, "sha256:aa", "sha256:bb", exp.Unix()})
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestToolGatePreconditionGuardsConsume(t *testing.T) {
	useReliabilityStore(t)
	ch := &recordingApprovalChannel{action: "approve"}
	g := newToolGate(gateCfg(config.ToolRule{Name: "update_record", RequireApproval: true}), ch)
	_, r := postGate(t, g, `{"action_id":"upd-1","tool":"update_record","args":{"id":42},"precondition":"etag:\"v7\""}`)
	if r.Decision != "allow" {
		t.Fatalf("operator approved: %+v", r)
	}
	if len(ch.drafts) != 1 || !strings.Contains(ch.drafts[0], `target: etag:"v7"`) {
		t.Fatalf("the prompt shows the target state: %q", ch.drafts)
	}
	consume := func(pre string) (int, ToolCallResponse) {
		body := `{"binding_hash":` + strconv.Quote(r.BindingHash)
		if pre != "" {
			body += `,"precondition":` + strconv.Quote(pre)
		}
		return consumeGateWith(t, g, "upd-1", body+"}")
	}
	if code, c := consume(""); code != http.StatusConflict || c.Rule != "precondition_failed" || c.Permit != "" {
		t.Fatalf("a bound permit needs the current state: %d %+v", code, c)
	}
	if code, c := consume(`etag:"v8"`); code != http.StatusConflict || c.Rule != "precondition_failed" || !strings.Contains(c.Reason, "target changed") {
		t.Fatalf("a changed target is not acted on: %d %+v", code, c)
	}
	if code, c := consume(`etag:"v7"`); code != http.StatusOK || c.Permit != "execute" {
		t.Fatalf("the unchanged target consumes: %d %+v", code, c)
	}
}

func TestToolGatePermitWithoutPreconditionIgnoresOne(t *testing.T) {
	useReliabilityStore(t)
	g := newToolGate(gateCfg(config.ToolRule{Name: "send_email"}), nil)
	_, r := postGate(t, g, `{"action_id":"s-1","tool":"send_email","args":{}}`)
	if code, c := consumeGateWith(t, g, "s-1", `{"binding_hash":`+strconv.Quote(r.BindingHash)+`,"precondition":"anything"}`); code != http.StatusOK || c.Permit != "execute" {
		t.Fatalf("an unbound permit consumes as before: %d %+v", code, c)
	}
}

func TestToolGatePreconditionMustBePrintable(t *testing.T) {
	useReliabilityStore(t)
	g := newToolGate(gateCfg(config.ToolRule{Name: "send_email"}), nil)
	if code, _ := postGate(t, g, `{"tool":"send_email","precondition":"a\u0007b"}`); code != http.StatusBadRequest {
		t.Fatalf("a control character is refused, got %d", code)
	}
	if code, _ := postGate(t, g, `{"tool":"send_email","precondition":"`+strings.Repeat("x", 513)+`"}`); code != http.StatusBadRequest {
		t.Fatalf("an oversized precondition is refused, got %d", code)
	}
}

func TestExecutionGrantIssuedAndVerified(t *testing.T) {
	useReliabilityStore(t)
	t.Setenv("TEST_GRANT_KEY", "grant-key")
	cfg := gateCfg(config.ToolRule{Name: "send_email"})
	cfg.ToolGate.ExecutionGrant.SecretEnv = "TEST_GRANT_KEY"
	g := newToolGate(cfg, nil)
	_, r := postGate(t, g, `{"action_id":"g-1","tool":"send_email","agent":"inbox","args":{"to":"anna@example.com"}}`)
	if r.ExecutionGrant != "" {
		t.Fatalf("a decision carries no grant, only a consume does: %+v", r)
	}
	code, c := consumeGate(t, g, "g-1", r.BindingHash)
	if code != http.StatusOK || !strings.HasPrefix(c.ExecutionGrant, "dcg1.") {
		t.Fatalf("consume returns a grant: %d %+v", code, c)
	}
	args := map[string]interface{}{"to": "anna@example.com"}
	now := time.Now()
	got, err := verifyExecutionGrant([]byte("grant-key"), c.ExecutionGrant, "send_email", args, time.Minute, now)
	if err != nil || got.ActionID != "g-1" || got.Agent != "inbox" || got.BindingHash != r.BindingHash {
		t.Fatalf("grant verifies: %+v %v", got, err)
	}
	for name, check := range map[string]func() error{
		"wrong key": func() error {
			_, err := verifyExecutionGrant([]byte("other"), c.ExecutionGrant, "send_email", args, 0, now)
			return err
		},
		"other tool": func() error {
			_, err := verifyExecutionGrant([]byte("grant-key"), c.ExecutionGrant, "delete_all", args, 0, now)
			return err
		},
		"other args": func() error {
			_, err := verifyExecutionGrant([]byte("grant-key"), c.ExecutionGrant, "send_email", map[string]interface{}{"to": "mallory@example.com"}, 0, now)
			return err
		},
		"too old": func() error {
			_, err := verifyExecutionGrant([]byte("grant-key"), c.ExecutionGrant, "send_email", args, time.Minute, now.Add(2*time.Minute))
			return err
		},
		"edited payload": func() error {
			parts := strings.Split(c.ExecutionGrant, ".")
			parts[1] = parts[1][:len(parts[1])-2] + "AA"
			_, err := verifyExecutionGrant([]byte("grant-key"), strings.Join(parts, "."), "send_email", args, 0, now)
			return err
		},
	} {
		if check() == nil {
			t.Errorf("%s: grant accepted", name)
		}
	}
}

func TestExecutionGrantOffByDefault(t *testing.T) {
	useReliabilityStore(t)
	g := newToolGate(gateCfg(config.ToolRule{Name: "send_email"}), nil)
	_, r := postGate(t, g, `{"action_id":"g-2","tool":"send_email","args":{}}`)
	if _, c := consumeGate(t, g, "g-2", r.BindingHash); c.ExecutionGrant != "" {
		t.Fatalf("no key, no grant: %+v", c)
	}
}

func TestGateCommandVerifyGrant(t *testing.T) {
	t.Setenv("TEST_GRANT_KEY", "grant-key")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, `tool_gate:
  enabled: true
  execution_grant: {secret_env: TEST_GRANT_KEY}
  tools:
    - name: send_email
`)
	grant := signExecutionGrant([]byte("grant-key"), executionGrant{ActionID: "a1", Tool: "send_email",
		ArgsHash: hashToolArgs(map[string]interface{}{"to": "x@example.com"}), ConsumedAt: time.Now().UTC().Format(time.RFC3339)})
	var out, errOut bytes.Buffer
	if code := gateCmd([]string{"verify-grant", grant, "--tool", "send_email", "--args", `{"to":"x@example.com"}`, "--max-age", "5m", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("valid grant exit %d: %s%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "VALID  send_email") {
		t.Fatalf("output: %s", out.String())
	}
	out.Reset()
	if code := gateCmd([]string{"verify-grant", grant, "--tool", "send_email", "--args", `{"to":"y@example.com"}`, "--config", cfgPath, "--json"}, &out, &errOut); code != 1 {
		t.Fatalf("other arguments must exit 1, got %d", code)
	}
	if code := gateCmd([]string{"verify-grant", grant, "--config", cfgPath}, &out, &errOut); code != 2 {
		t.Fatalf("missing --tool is a usage error, got %d", code)
	}
}
