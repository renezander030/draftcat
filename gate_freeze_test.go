package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

func freezeStore(t *testing.T) (string, *statestore.StateStore) {
	t.Helper()
	return lifecycleHTTPStore(t)
}

func TestFreezeDeniesDecisionRefusesConsumeAndIgnoresGrants(t *testing.T) {
	_, st := freezeStore(t)
	ch := &recordingApprovalChannel{action: "approve"}
	cfg := gateCfg(config.ToolRule{Name: "read_file"}, grantableEmail())
	g := newToolGate(cfg, ch)

	// An allowed permit issued before the freeze, and a live grant.
	_, permit := postGate(t, g, `{"action_id":"p1","tool":"read_file"}`)
	if permit.Decision != "allow" {
		t.Fatalf("setup: %+v", permit)
	}
	if msg := grantCommand("/grant", "send_email 1h 5", 7, cfg.ToolGate, time.Now()); !strings.HasPrefix(msg, "Granted g_") {
		t.Fatalf("grant: %s", msg)
	}

	if _, err := st.SetGateFreeze("maintenance", "cli", time.Now()); err != nil {
		t.Fatal(err)
	}

	// Decision: deny with the same shape as a policy deny.
	code, r := postGate(t, g, `{"action_id":"d1","tool":"read_file"}`)
	if code != 200 || r.Decision != "deny" || r.State != "denied" || r.DecidedBy != "policy" || r.Rule != "freeze" ||
		!strings.HasPrefix(r.Reason, "tool gate frozen by operator") || !strings.Contains(r.Reason, "maintenance") ||
		r.BindingHash == "" || r.PolicyHash == "" {
		t.Fatalf("frozen decision = %d %+v", code, r)
	}
	// Unlisted tools are frozen too, with the freeze reason.
	if _, r := postGate(t, g, `{"action_id":"d2","tool":"unknown_tool"}`); !strings.HasPrefix(r.Reason, "tool gate frozen by operator") {
		t.Fatalf("unlisted while frozen: %+v", r)
	}

	// The standing grant does not answer the ask, and no prompt goes out.
	_, r = postGate(t, g, `{"action_id":"d3","tool":"send_email","args":{"n":1}}`)
	if r.Decision != "deny" || r.DecidedBy == "grant" || !strings.HasPrefix(r.Reason, "tool gate frozen by operator") {
		t.Fatalf("grant must not answer while frozen: %+v", r)
	}
	if len(ch.drafts) != 0 {
		t.Fatalf("frozen calls send no prompt: %q", ch.drafts)
	}
	grants, err := st.ToolGrants(time.Now(), false)
	if err != nil || len(grants) != 1 || grants[0].Uses != 0 {
		t.Fatalf("a frozen call must not use the grant: %v %+v", err, grants)
	}

	// Consume of the earlier permit is refused like a denied permit.
	code, c := consumeGate(t, g, "p1", permit.BindingHash)
	if code != 409 || c.Permit != "" || c.Consume != "" || c.Decision != "deny" || !strings.HasPrefix(c.Reason, "tool gate frozen by operator") {
		t.Fatalf("frozen consume = %d %+v", code, c)
	}

	// Lifted: the same permit consumes, a new call is allowed, the grant answers.
	if changed, err := st.ClearGateFreeze("cli", time.Now()); err != nil || !changed {
		t.Fatalf("clear: %v %v", changed, err)
	}
	if code, c := consumeGate(t, g, "p1", permit.BindingHash); code != 200 || c.Permit != "execute" {
		t.Fatalf("consume after unfreeze = %d %+v", code, c)
	}
	if _, r := postGate(t, g, `{"action_id":"d4","tool":"read_file"}`); r.Decision != "allow" {
		t.Fatalf("allowed again after unfreeze: %+v", r)
	}
	if _, r := postGate(t, g, `{"action_id":"d5","tool":"send_email","args":{"n":2}}`); r.DecidedBy != "grant" {
		t.Fatalf("grant answers again after unfreeze: %+v", r)
	}
}

func TestFreezeWrittenByAnotherHandleIsSeenOnNextDecision(t *testing.T) {
	path, _ := freezeStore(t)
	g := newToolGate(gateCfg(config.ToolRule{Name: "read_file"}), nil)
	if _, r := postGate(t, g, `{"action_id":"a1","tool":"read_file"}`); r.Decision != "allow" {
		t.Fatalf("not frozen: %+v", r)
	}
	var out, errOut bytes.Buffer
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, cfgPath, "state:\n  path: "+path+"\n")
	if code := gateCmd([]string{"freeze", "--reason", "stop", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("freeze exit %d: %s", code, errOut.String())
	}
	if _, r := postGate(t, g, `{"action_id":"a2","tool":"read_file"}`); r.Decision != "deny" || !strings.Contains(r.Reason, "stop") {
		t.Fatalf("running engine must see the CLI freeze: %+v", r)
	}
}

func TestFreezeSurvivesReopeningTheStore(t *testing.T) {
	path, st := freezeStore(t)
	at := time.Unix(1_800_000_000, 0)
	if changed, err := st.SetGateFreeze("deploy window", "telegram:1", at); err != nil || !changed {
		t.Fatalf("freeze: %v %v", changed, err)
	}
	if changed, _ := st.SetGateFreeze("second", "cli", at.Add(time.Hour)); changed {
		t.Fatal("a second freeze keeps the first")
	}
	reopened, err := statestore.OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	f, err := reopened.GateFreeze()
	if err != nil || !f.Frozen || f.Reason != "deploy window" || f.By != "telegram:1" || !f.At.Equal(at) {
		t.Fatalf("reopened freeze = %+v %v", f, err)
	}
}

func TestFreezeNeverSetLeavesTheGateAlone(t *testing.T) {
	_, st := freezeStore(t)
	if f, err := st.GateFreeze(); err != nil || f.Frozen {
		t.Fatalf("fresh store: %+v %v", f, err)
	}
	if line := freezeStatusLine(); line != "" {
		t.Fatalf("a never-frozen /status adds no line: %q", line)
	}
}

func TestTelegramFreezeNeedsAnOperator(t *testing.T) {
	_, st := freezeStore(t)
	now := time.Now()
	operators := []int64{111}
	if msg := freezeCommand("/freeze", "nope", 999, operators, now); !strings.HasPrefix(msg, "Not allowed") {
		t.Fatalf("non-operator /freeze: %q", msg)
	}
	if f, _ := st.GateFreeze(); f.Frozen {
		t.Fatal("a non-operator froze the gate")
	}
	if msg := freezeCommand("/freeze", "incident 12", 111, operators, now); !strings.HasPrefix(msg, "Tool gate frozen") {
		t.Fatalf("operator /freeze: %q", msg)
	}
	f, _ := st.GateFreeze()
	if !f.Frozen || f.Reason != "incident 12" || f.By != "telegram:111" {
		t.Fatalf("stored freeze: %+v", f)
	}
	if line := freezeStatusLine(); !strings.Contains(line, "FROZEN by telegram:111") || !strings.Contains(line, "incident 12") {
		t.Fatalf("/status line: %q", line)
	}
	if msg := freezeCommand("/unfreeze", "", 999, operators, now); !strings.HasPrefix(msg, "Not allowed") {
		t.Fatalf("non-operator /unfreeze: %q", msg)
	}
	if f, _ := st.GateFreeze(); !f.Frozen {
		t.Fatal("a non-operator lifted the freeze")
	}
	if msg := freezeCommand("/unfreeze", "", 111, operators, now); msg != "Tool gate unfrozen." {
		t.Fatalf("operator /unfreeze: %q", msg)
	}
	if msg := freezeCommand("/unfreeze", "", 111, operators, now); !strings.Contains(msg, "not frozen") {
		t.Fatalf("second /unfreeze: %q", msg)
	}
}

func TestGateCLIFreezeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, "state:\n  path: "+filepath.Join(dir, "s.db")+"\n")
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := gateCmd(append(args, "--config", cfgPath), &out, &errOut)
		return code, out.String(), errOut.String()
	}
	// Status before any store exists does not create one.
	if code, out, _ := run("status"); code != 0 || !strings.Contains(out, "not frozen") {
		t.Fatalf("status of a missing store: %d %q", code, out)
	}
	if code, out, e := run("freeze", "--reason", "rollout"); code != 0 || !strings.Contains(out, "frozen") {
		t.Fatalf("freeze: %d %q %q", code, out, e)
	}
	code, out, _ := run("status", "--json")
	var js gateStatusJSON
	if code != 0 || json.Unmarshal([]byte(out), &js) != nil || !js.Frozen || js.Reason != "rollout" || js.By != "cli" || js.At == "" {
		t.Fatalf("status --json: %d %q", code, out)
	}
	if code, out, _ := run("status"); code != 0 || !strings.Contains(out, "FROZEN") || !strings.Contains(out, "rollout") {
		t.Fatalf("status: %d %q", code, out)
	}
	if code, out, _ := run("freeze"); code != 0 || !strings.Contains(out, "already frozen") {
		t.Fatalf("second freeze: %d %q", code, out)
	}
	if code, out, _ := run("unfreeze"); code != 0 || !strings.Contains(out, "unfrozen") {
		t.Fatalf("unfreeze: %d %q", code, out)
	}
	code, out, _ = run("status", "--json")
	js = gateStatusJSON{}
	if code != 0 || json.Unmarshal([]byte(out), &js) != nil || js.Frozen {
		t.Fatalf("status after unfreeze: %d %q", code, out)
	}
	if code, _, e := run("status", "extra"); code != 2 {
		t.Fatalf("stray argument: %d %q", code, e)
	}
	if code, _, _ := run("unfreeze", "--reason", "x"); code != 2 {
		t.Fatalf("--reason on unfreeze: %d", code)
	}
}

func TestGateHelpListsFreezeCommands(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := gateCmd([]string{"--help"}, &out, &errOut); code != 0 {
		t.Fatal(code)
	}
	for _, want := range []string{"gate freeze", "gate unfreeze", "gate status"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("gate --help lacks %q:\n%s", want, out.String())
		}
	}
}
