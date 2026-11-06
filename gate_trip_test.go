package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

// tripGate is a gate with tool_gate.trip and a clock the test moves.
func tripGate(t *testing.T, trip config.GateTrip, ch OperatorChannel, tools ...config.ToolRule) (*toolGate, *time.Time) {
	t.Helper()
	cfg := gateCfg(tools...)
	cfg.Timeouts.OperatorApproval = "1h"
	cfg.ToolGate.Trip = &trip
	g := newToolGate(cfg, ch)
	clock := time.Now()
	g.now = func() time.Time { return clock }
	return g, &clock
}

var tripSeq struct {
	sync.Mutex
	n int
}

// tripCall posts one call with a fresh action id.
func tripCall(t *testing.T, g *toolGate, agent, tool, args string) ToolCallResponse {
	t.Helper()
	tripSeq.Lock()
	tripSeq.n++
	id := fmt.Sprintf("trip-%d", tripSeq.n)
	tripSeq.Unlock()
	if args == "" {
		args = "{}"
	}
	_, r := postGate(t, g, fmt.Sprintf(`{"action_id":%q,"agent":%q,"tool":%q,"args":%s}`, id, agent, tool, args))
	return r
}

func tripNotices(ch *countingChannel) []string {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	var out []string
	for _, n := range ch.notices {
		if strings.Contains(n, "TRIP") {
			out = append(out, n)
		}
	}
	return out
}

func agentFrozenInStore(t *testing.T, st *statestore.StateStore, agent string) (statestore.AgentFreeze, bool) {
	t.Helper()
	f, ok, err := st.AgentFreeze(agent)
	if err != nil {
		t.Fatal(err)
	}
	return f, ok
}

func TestTripFreezesTheAgentAtTheThresholdOnly(t *testing.T) {
	_, st := freezeStore(t)
	ch := &countingChannel{}
	g, clock := tripGate(t, config.GateTrip{Denials: 3, Within: "10m"}, ch, config.ToolRule{Name: "read_file"})

	for i := 1; i <= 2; i++ {
		if r := tripCall(t, g, "agent-1", "wire_money", ""); r.Decision != "deny" || r.DecidedBy != "allowlist" {
			t.Fatalf("denial %d: %+v", i, r)
		}
		*clock = clock.Add(time.Minute)
	}
	if _, ok := agentFrozenInStore(t, st, "agent-1"); ok || len(tripNotices(ch)) != 0 {
		t.Fatal("two denials must not trip a threshold of three")
	}
	if r := tripCall(t, g, "agent-1", "read_file", ""); r.Decision != "allow" {
		t.Fatalf("below the threshold the agent keeps working: %+v", r)
	}

	// The third denial is answered by its own reason, then the agent is frozen.
	if r := tripCall(t, g, "agent-1", "delete_repo", ""); r.Decision != "deny" || r.Rule != "not listed" {
		t.Fatalf("the tripping call keeps its own reason: %+v", r)
	}
	f, ok := agentFrozenInStore(t, st, "agent-1")
	if !ok || f.By != "trip" || f.Reason != "refused 3 times within 10m" {
		t.Fatalf("agent freeze = %+v %v", f, ok)
	}
	notices := tripNotices(ch)
	if len(notices) != 1 {
		t.Fatalf("one trip notice, got %q", notices)
	}
	for _, want := range []string{"agent agent-1 frozen after 3 denials within 10m", "last reasons:", "wire_money:", "delete_repo:", "/unfreeze agent-1"} {
		if !strings.Contains(notices[0], want) {
			t.Errorf("trip notice lacks %q:\n%s", want, notices[0])
		}
	}

	// Its further calls are denied naming the trip; another agent keeps working.
	r := tripCall(t, g, "agent-1", "read_file", "")
	if r.Decision != "deny" || r.State != "denied" || r.DecidedBy != "policy" || r.Rule != "freeze.agent" ||
		!strings.HasPrefix(r.Reason, "agent agent-1 frozen by trip: refused 3 times") {
		t.Fatalf("frozen agent's call = %+v", r)
	}
	if r := tripCall(t, g, "agent-2", "read_file", ""); r.Decision != "allow" {
		t.Fatalf("other agents keep working: %+v", r)
	}
	if line := freezeStatusLine(); !strings.Contains(line, "Agent agent-1: FROZEN by trip") || strings.Contains(line, "Tool gate: FROZEN") {
		t.Fatalf("/status line: %q", line)
	}
}

func TestTripWindowSlides(t *testing.T) {
	_, st := freezeStore(t)
	g, clock := tripGate(t, config.GateTrip{Denials: 3, Within: "10m"}, &countingChannel{}, config.ToolRule{Name: "read_file"})
	start := *clock
	at := func(m int) {
		*clock = start.Add(time.Duration(m) * time.Minute)
		tripCall(t, g, "agent-1", "wire_money", "")
	}
	at(0)
	at(4)
	at(11) // the first denial is out of the window: two inside it
	if _, ok := agentFrozenInStore(t, st, "agent-1"); ok {
		t.Fatal("denials spread wider than the window must not trip")
	}
	at(13) // 4, 11 and 13 fall inside ten minutes
	if _, ok := agentFrozenInStore(t, st, "agent-1"); !ok {
		t.Fatal("three denials inside the window must trip")
	}
}

func TestTripDoesNotCountFreezeDenials(t *testing.T) {
	_, st := freezeStore(t)
	ch := &countingChannel{}
	g, _ := tripGate(t, config.GateTrip{Denials: 3, Within: "10m"}, ch, config.ToolRule{Name: "read_file"})

	if _, err := st.SetGateFreeze("maintenance", "cli", time.Now()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if r := tripCall(t, g, "agent-1", "wire_money", ""); r.Rule != "freeze" {
			t.Fatalf("frozen gate: %+v", r)
		}
	}
	if _, err := st.ClearGateFreeze("cli", time.Now()); err != nil {
		t.Fatal(err)
	}
	tripCall(t, g, "agent-1", "wire_money", "")
	tripCall(t, g, "agent-1", "wire_money", "")
	if _, ok := agentFrozenInStore(t, st, "agent-1"); ok {
		t.Fatal("denials of a frozen gate counted toward the trip")
	}

	// Tripped, the agent's own frozen denials do not count again either.
	tripCall(t, g, "agent-1", "wire_money", "")
	for i := 0; i < 5; i++ {
		if r := tripCall(t, g, "agent-1", "read_file", ""); r.Rule != "freeze.agent" {
			t.Fatalf("frozen agent: %+v", r)
		}
	}
	g.tripMu.Lock()
	left := len(g.trips["agent-1"])
	g.tripMu.Unlock()
	if left != 0 || len(tripNotices(ch)) != 1 {
		t.Fatalf("the trip fed itself: %d counted, notices %q", left, tripNotices(ch))
	}
}

func TestTripCountsOperatorRefusalsNotTimeouts(t *testing.T) {
	_, st := freezeStore(t)
	rule := config.ToolRule{Name: "send_email", RequireApproval: true}

	timeouts, _ := tripGate(t, config.GateTrip{Denials: 2, Within: "10m"}, &stubApprovalChannel{action: "timeout"}, rule)
	for i := 0; i < 3; i++ {
		if r := tripCall(t, timeouts, "agent-t", "send_email", fmt.Sprintf(`{"n":%d}`, i)); r.Decision != "deny" || !strings.Contains(r.Reason, "timed out") {
			t.Fatalf("timeout: %+v", r)
		}
	}
	if _, ok := agentFrozenInStore(t, st, "agent-t"); ok {
		t.Fatal("approval timeouts must not trip")
	}

	skips, _ := tripGate(t, config.GateTrip{Denials: 2, Within: "10m"}, &recordingApprovalChannel{action: "skip"}, rule)
	for i := 0; i < 2; i++ {
		if r := tripCall(t, skips, "agent-s", "send_email", fmt.Sprintf(`{"n":%d}`, i)); r.Decision != "deny" || r.DecidedBy != "operator" {
			t.Fatalf("operator refusal: %+v", r)
		}
	}
	if _, ok := agentFrozenInStore(t, st, "agent-s"); !ok {
		t.Fatal("two operator refusals must trip a threshold of two")
	}
}

func TestTripCountsTheRepeatGuard(t *testing.T) {
	_, st := freezeStore(t)
	g, _ := tripGate(t, config.GateTrip{Denials: 3, Within: "10m"}, &recordingApprovalChannel{action: "skip"},
		config.ToolRule{Name: "send_email", RequireApproval: true})
	// One refusal by the operator, then the identical call twice by the repeat guard.
	for i := 0; i < 3; i++ {
		r := tripCall(t, g, "agent-1", "send_email", `{"to":"anna@example.com"}`)
		if i > 0 && r.DecidedBy != "repeat-guard" {
			t.Fatalf("call %d: %+v", i, r)
		}
	}
	if _, ok := agentFrozenInStore(t, st, "agent-1"); !ok {
		t.Fatal("repeat-guard denials must count")
	}
}

func TestTripOffByDefault(t *testing.T) {
	_, st := freezeStore(t)
	g := newToolGate(gateCfg(config.ToolRule{Name: "read_file"}), &countingChannel{})
	for i := 0; i < 20; i++ {
		tripCall(t, g, "agent-1", "wire_money", "")
	}
	if agents, err := st.AgentFreezes(); err != nil || len(agents) != 0 {
		t.Fatalf("no trip configured, no freeze: %+v %v", agents, err)
	}
	if r := tripCall(t, g, "agent-1", "read_file", ""); r.Decision != "allow" {
		t.Fatalf("%+v", r)
	}
}

func TestTripScopeGateFreezesTheWholeGate(t *testing.T) {
	_, st := freezeStore(t)
	ch := &countingChannel{}
	g, _ := tripGate(t, config.GateTrip{Denials: 2, Within: "5m", Scope: "gate"}, ch, config.ToolRule{Name: "read_file"})
	tripCall(t, g, "agent-1", "wire_money", "")
	tripCall(t, g, "agent-1", "wire_money", "")
	f, err := st.GateFreeze()
	if err != nil || !f.Frozen || f.By != "trip" || f.Reason != "agent agent-1 was refused 2 times within 5m" {
		t.Fatalf("gate freeze = %+v %v", f, err)
	}
	if agents, _ := st.AgentFreezes(); len(agents) != 0 {
		t.Fatalf("scope gate freezes no single agent: %+v", agents)
	}
	r := tripCall(t, g, "agent-2", "read_file", "")
	if r.Decision != "deny" || r.Rule != "freeze" || !strings.HasPrefix(r.Reason, "tool gate frozen by operator trip: agent agent-1") {
		t.Fatalf("every agent meets the gate freeze: %+v", r)
	}
	if n := tripNotices(ch); len(n) != 1 || !strings.Contains(n[0], "tool gate frozen after 2 denials of agent agent-1 within 5m") || !strings.Contains(n[0], "/unfreeze lifts it") {
		t.Fatalf("gate trip notice: %q", n)
	}
	if msg := freezeCommand("/unfreeze", "", 111, []int64{111}, time.Now()); msg != "Tool gate unfrozen." {
		t.Fatalf("/unfreeze: %q", msg)
	}
	if r := tripCall(t, g, "agent-2", "read_file", ""); r.Decision != "allow" {
		t.Fatalf("after /unfreeze: %+v", r)
	}
}

func TestAgentFreezeSurvivesRestartAndReachesARunningGate(t *testing.T) {
	path, st := freezeStore(t)
	g, _ := tripGate(t, config.GateTrip{Denials: 2, Within: "10m"}, &countingChannel{}, config.ToolRule{Name: "read_file"})
	tripCall(t, g, "agent-1", "wire_money", "")
	tripCall(t, g, "agent-1", "wire_money", "")

	// A second handle (a restarted engine, or the CLI) sees the freeze.
	reopened, err := statestore.OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	f, ok, err := reopened.AgentFreeze("agent-1")
	_ = reopened.Close()
	if err != nil || !ok || f.By != "trip" {
		t.Fatalf("reopened agent freeze = %+v %v %v", f, ok, err)
	}
	restarted := newToolGate(gateCfg(config.ToolRule{Name: "read_file"}), nil)
	if r := tripCall(t, restarted, "agent-1", "read_file", ""); r.Rule != "freeze.agent" {
		t.Fatalf("a restarted gate keeps the freeze, even without trip configured: %+v", r)
	}

	// A freeze written by another handle reaches the running gate at once.
	if _, err := st.SetAgentFreeze("agent-3", "by hand", "cli", time.Now()); err != nil {
		t.Fatal(err)
	}
	if r := tripCall(t, restarted, "agent-3", "read_file", ""); r.Rule != "freeze.agent" || !strings.Contains(r.Reason, "by hand") {
		t.Fatalf("running gate must see the stored freeze: %+v", r)
	}
}

func TestAgentFreezeRefusesConsumeAndIgnoresGrants(t *testing.T) {
	_, st := freezeStore(t)
	cfg := gateCfg(config.ToolRule{Name: "read_file"}, grantableEmail())
	ch := &recordingApprovalChannel{action: "approve"}
	g := newToolGate(cfg, ch)

	_, permit := postGate(t, g, `{"action_id":"p1","agent":"agent-1","tool":"read_file"}`)
	if permit.Decision != "allow" {
		t.Fatalf("setup: %+v", permit)
	}
	if msg := grantCommand("/grant", "send_email 1h 5", 7, cfg.ToolGate, time.Now()); !strings.HasPrefix(msg, "Granted g_") {
		t.Fatalf("grant: %s", msg)
	}
	if _, err := st.SetAgentFreeze("agent-1", "refused 5 times within 10m", "trip", time.Now()); err != nil {
		t.Fatal(err)
	}

	code, c := consumeGate(t, g, "p1", permit.BindingHash)
	if code != 409 || c.Permit != "" || c.Consume != "" || c.Decision != "deny" || c.Rule != "freeze.agent" {
		t.Fatalf("frozen agent's consume = %d %+v", code, c)
	}
	_, r := postGate(t, g, `{"action_id":"g1","agent":"agent-1","tool":"send_email","args":{"n":1}}`)
	if r.Decision != "deny" || r.DecidedBy == "grant" || len(ch.drafts) != 0 {
		t.Fatalf("a grant must not release a frozen agent's call: %+v %q", r, ch.drafts)
	}
	if grants, err := st.ToolGrants(time.Now(), false); err != nil || len(grants) != 1 || grants[0].Uses != 0 {
		t.Fatalf("no grant use taken: %+v %v", grants, err)
	}
	// Another agent's grant use is unaffected.
	if _, r := postGate(t, g, `{"action_id":"g2","agent":"agent-2","tool":"send_email","args":{"n":2}}`); r.DecidedBy != "grant" {
		t.Fatalf("agent-2 is not frozen: %+v", r)
	}

	if msg := freezeCommand("/unfreeze", "agent-1", 111, []int64{111}, time.Now()); msg != "Agent agent-1 unfrozen." {
		t.Fatalf("/unfreeze agent-1: %q", msg)
	}
	if code, c := consumeGate(t, g, "p1", permit.BindingHash); code != 200 || c.Permit != "execute" {
		t.Fatalf("the permit consumes once the agent is unfrozen: %d %+v", code, c)
	}
}

func TestTelegramUnfreezeAgentAndRemainingNote(t *testing.T) {
	_, st := freezeStore(t)
	ops := []int64{111}
	now := time.Now()
	if msg := freezeCommand("/unfreeze", "agent-1", 999, ops, now); !strings.HasPrefix(msg, "Not allowed") {
		t.Fatalf("non-operator: %q", msg)
	}
	if msg := freezeCommand("/unfreeze", "agent-1", 111, ops, now); msg != "Agent agent-1 was not frozen." {
		t.Fatalf("not frozen: %q", msg)
	}
	for _, a := range []string{"agent-1", "agent-2"} {
		if _, err := st.SetAgentFreeze(a, "refused 3 times within 10m", "trip", now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.SetGateFreeze("x", "cli", now); err != nil {
		t.Fatal(err)
	}
	if msg := freezeCommand("/freeze", "", 111, ops, now); !strings.Contains(msg, "Already frozen") || !strings.Contains(msg, "Agent agent-2: FROZEN by trip") {
		t.Fatalf("/freeze status lists agents: %q", msg)
	}
	msg := freezeCommand("/unfreeze", "", 111, ops, now)
	if !strings.HasPrefix(msg, "Tool gate unfrozen.") || !strings.Contains(msg, "Agents still frozen: agent-1, agent-2") {
		t.Fatalf("plain /unfreeze keeps the agents and says so: %q", msg)
	}
	if _, ok := agentFrozenInStore(t, st, "agent-1"); !ok {
		t.Fatal("plain /unfreeze lifted an agent freeze")
	}
	if msg := freezeCommand("/unfreeze", " agent-1 ", 111, ops, now); msg != "Agent agent-1 unfrozen." {
		t.Fatalf("/unfreeze agent-1: %q", msg)
	}
	if msg := freezeCommand("/unfreeze", "", 111, ops, now); msg != "The tool gate was not frozen.\nAgents still frozen: agent-2 (/unfreeze <agent> lifts one)." {
		t.Fatalf("second /unfreeze: %q", msg)
	}
}

func TestGateCLIUnfreezeAgentAndStatus(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "s.db")
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, "state:\n  path: "+dbPath+"\n")
	st, err := statestore.OpenStateStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAgentFreeze("agent-1", "refused 5 times within 10m", "trip", time.Unix(1_800_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := gateCmd(append(args, "--config", cfgPath), &out, &errOut)
		return code, out.String(), errOut.String()
	}
	if code, out, _ := run("status"); code != 0 || !strings.Contains(out, "not frozen") || !strings.Contains(out, "Agent agent-1: FROZEN by trip since 2027-01-15T08:00:00Z (refused 5 times within 10m)") {
		t.Fatalf("status: %d %q", code, out)
	}
	code, out, _ := run("status", "--json")
	var js gateStatusJSON
	if code != 0 || json.Unmarshal([]byte(out), &js) != nil || js.Frozen || len(js.Agents) != 1 ||
		js.Agents[0].Agent != "agent-1" || js.Agents[0].By != "trip" || js.Agents[0].At != "2027-01-15T08:00:00Z" {
		t.Fatalf("status --json: %d %q", code, out)
	}
	if code, out, _ := run("unfreeze"); code != 0 || !strings.Contains(out, "tool gate was not frozen") || !strings.Contains(out, "Agents still frozen: agent-1") {
		t.Fatalf("plain unfreeze: %d %q", code, out)
	}
	if code, out, _ := run("unfreeze", "--agent", "agent-1"); code != 0 || out != "agent agent-1 unfrozen\n" {
		t.Fatalf("unfreeze --agent: %d %q", code, out)
	}
	if code, out, _ := run("unfreeze", "--agent", "agent-1"); code != 0 || out != "agent agent-1 was not frozen\n" {
		t.Fatalf("second unfreeze --agent: %d %q", code, out)
	}
	if code, out, _ := run("status", "--json"); code != 0 || strings.Contains(out, "agents") {
		t.Fatalf("status after: %d %q", code, out)
	}
	for _, args := range [][]string{{"status", "--agent", "a"}, {"freeze", "--agent", "a"}, {"unfreeze", "--agent", " "}, {"check", "x", "--agent", "a"}} {
		if code, _, _ := run(args...); code != 2 {
			t.Fatalf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestBuildDigestListsFrozenAgents(t *testing.T) {
	now := time.Date(2026, 1, 5, 8, 0, 0, 0, time.UTC)
	var agents []statestore.AgentFreeze
	for i := 0; i < 7; i++ {
		agents = append(agents, statestore.AgentFreeze{Agent: fmt.Sprintf("agent-%d", i), Reason: "refused 5 times within 10m", By: "trip", At: now.Add(-time.Hour)})
	}
	text, ok := buildDigest(digestSnapshot{Now: now, Agents: agents})
	if !ok || !strings.Contains(text, "Agent agent-0: FROZEN by trip") || !strings.Contains(text, "refused 5 times within 10m") ||
		!strings.Contains(text, "… and 2 more frozen agent(s)") || strings.Contains(text, "agent-6") {
		t.Fatalf("digest: %v %q", ok, text)
	}
}

func TestDigestSnapshotReadsAgentFreezes(t *testing.T) {
	_, st := freezeStore(t)
	if _, err := st.SetAgentFreeze("agent-1", "refused 2 times within 1m", "trip", time.Now()); err != nil {
		t.Fatal(err)
	}
	snap, err := digestSnapshotAt(st, time.Now())
	if err != nil || len(snap.Agents) != 1 || snap.Agents[0].Agent != "agent-1" {
		t.Fatalf("snapshot: %+v %v", snap.Agents, err)
	}
}

func TestGateHelpListsUnfreezeAgent(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := gateCmd([]string{"--help"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "gate unfreeze [--agent name]") {
		t.Fatalf("gate --help: %d %s", code, out.String())
	}
}
