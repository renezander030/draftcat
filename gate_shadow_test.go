package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	"github.com/renezander030/draftcat/internal/obs"
	statestore "github.com/renezander030/draftcat/internal/state"
)

// shadowWith points g at a candidate made of tools.
func shadowWith(g *toolGate, tools ...config.ToolRule) *shadowCandidate {
	cand := &shadowCandidate{Path: "candidate.yaml", Gate: config.ToolGateConfig{Enabled: true, Tools: tools}, Hash: hashCanonical(tools)}
	g.shadow = func() *shadowCandidate { return cand }
	return cand
}

func shadowRows(t *testing.T, g *toolGate) []statestore.ShadowDiff {
	t.Helper()
	g.shadowWG.Wait()
	rows, err := state.ShadowDiffs(time.Time{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// metricValue reads one series from the exposition; 0 when absent.
func metricValue(t *testing.T, series string) float64 {
	t.Helper()
	var buf bytes.Buffer
	if err := obs.WriteMetrics(&buf); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatal(err)
			}
			return f
		}
	}
	return 0
}

func TestShadowStoresNoRowWhenTheCandidateAgrees(t *testing.T) {
	freezeStore(t)
	g := newToolGate(gateCfg(config.ToolRule{Name: "read_file"}), nil)
	shadowWith(g, config.ToolRule{Name: "read_file"}, config.ToolRule{Name: "other"})
	if _, r := postGate(t, g, `{"action_id":"s1","tool":"read_file","args":{"path":"/a"}}`); r.Decision != "allow" {
		t.Fatalf("%+v", r)
	}
	// Unlisted under both: the same denial, whatever the reason's detail.
	postGate(t, g, `{"action_id":"s2","tool":"wire_money"}`)
	if rows := shadowRows(t, g); len(rows) != 0 {
		t.Fatalf("no difference, no row: %+v", rows)
	}
}

func TestShadowStoresARowWhenTheCandidateDiffers(t *testing.T) {
	freezeStore(t)
	g := newToolGate(gateCfg(config.ToolRule{Name: "create_invoice"}), nil)
	cand := shadowWith(g, config.ToolRule{Name: "create_invoice", OnMismatch: "deny",
		Args: map[string]config.ArgConstraint{"amount": {Max: f64(500)}}})
	before := metricValue(t, `draftcat_gate_shadow_differences_total{tool="create_invoice",live="allow",shadow="deny"}`)

	_, r := postGate(t, g, `{"action_id":"s1","agent":"agent-1","tool":"create_invoice","args":{"customer":"anna@example.com","amount":800}}`)
	if r.Decision != "allow" || r.DecidedBy != "allowlist" {
		t.Fatalf("the live decision stands: %+v", r)
	}
	rows := shadowRows(t, g)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	d := rows[0]
	if d.Agent != "agent-1" || d.Tool != "create_invoice" || d.ArgsHash != r.ArgsHash || d.CandidateHash != cand.Hash ||
		d.LiveDecision != "allow" || d.LiveRule != "listed" || d.LiveReason != "allowlisted in tool_gate" ||
		d.ShadowDecision != "deny" || d.ShadowRule != "args.amount" || d.ShadowReason != "arguments outside the rule" {
		t.Fatalf("row = %+v", d)
	}
	// The arguments are never stored, not even inside a rule or reason.
	text := strings.Join([]string{d.Agent, d.Tool, d.LiveRule, d.LiveReason, d.ShadowRule, d.ShadowReason}, "|")
	for _, v := range []string{"anna@example.com", "800"} {
		if strings.Contains(text, v) {
			t.Fatalf("an argument value reached the shadow table: %s", text)
		}
	}
	if after := metricValue(t, `draftcat_gate_shadow_differences_total{tool="create_invoice",live="allow",shadow="deny"}`); after != before+1 {
		t.Fatalf("difference counter %v -> %v", before, after)
	}
}

func TestShadowComparesTheHumanPathAsAsk(t *testing.T) {
	freezeStore(t)
	g := newToolGate(gateCfg(config.ToolRule{Name: "send_email", RequireApproval: true}), &stubApprovalChannel{action: "approve"})
	shadowWith(g, config.ToolRule{Name: "send_email"})
	if _, r := postGate(t, g, `{"action_id":"s1","tool":"send_email","args":{"to":"x"}}`); r.Decision != "allow" || r.DecidedBy != "operator" {
		t.Fatalf("%+v", r)
	}
	rows := shadowRows(t, g)
	if len(rows) != 1 || rows[0].LiveDecision != "ask" || rows[0].LiveReason != "a human decides" || rows[0].ShadowDecision != "allow" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestShadowReadsTodaysTotalsWithoutTheCallItself(t *testing.T) {
	freezeStore(t)
	// The live rules keep no count of read_file; the candidate caps it at 1.
	g := newToolGate(gateCfg(config.ToolRule{Name: "read_file"},
		config.ToolRule{Name: "create_invoice", Args: map[string]config.ArgConstraint{"amount": {MaxPerDay: f64(1000)}}}), nil)
	one := 1
	shadowWith(g, config.ToolRule{Name: "read_file", MaxCallsPerDay: &one, OnMismatch: "deny"},
		config.ToolRule{Name: "create_invoice", OnMismatch: "deny", Args: map[string]config.ArgConstraint{"amount": {MaxPerDay: f64(500)}}})

	postGate(t, g, `{"action_id":"r1","tool":"read_file"}`)
	postGate(t, g, `{"action_id":"i1","tool":"create_invoice","args":{"amount":400}}`)
	if rows := shadowRows(t, g); len(rows) != 0 {
		t.Fatalf("the first call of the day fits under both: %+v", rows)
	}
	postGate(t, g, `{"action_id":"r2","tool":"read_file"}`)
	postGate(t, g, `{"action_id":"i2","tool":"create_invoice","args":{"amount":400}}`)
	rows := shadowRows(t, g)
	rules := map[string]bool{}
	for _, d := range rows {
		if d.LiveDecision != "allow" || d.ShadowDecision != "deny" {
			t.Fatalf("row = %+v", d)
		}
		rules[d.ShadowRule] = true
	}
	if len(rows) != 2 || !rules["max_calls_per_day"] || !rules["max_per_day.amount"] {
		t.Fatalf("the second calls pass the candidate's caps: %+v", rows)
	}
}

func TestShadowFailureNeverTouchesTheLiveDecision(t *testing.T) {
	freezeStore(t)
	for name, eval := range map[string]func(config.ToolGateConfig, string, map[string]interface{}, map[string]interface{}, toolDailyTotal) (toolPolicyVerdict, error){
		"panic": func(config.ToolGateConfig, string, map[string]interface{}, map[string]interface{}, toolDailyTotal) (toolPolicyVerdict, error) {
			panic("candidate exploded")
		},
		"error": func(config.ToolGateConfig, string, map[string]interface{}, map[string]interface{}, toolDailyTotal) (toolPolicyVerdict, error) {
			return toolPolicyVerdict{}, errors.New("total unreadable")
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newToolGate(gateCfg(config.ToolRule{Name: "read_file"}), nil)
			shadowWith(g, config.ToolRule{Name: "read_file", RequireApproval: true})
			g.shadowEval = eval
			before := metricValue(t, "draftcat_gate_shadow_errors_total")
			code, r := postGate(t, g, `{"action_id":"f-`+name+`","tool":"read_file"}`)
			if code != 200 || r.Decision != "allow" || r.DecidedBy != "allowlist" {
				t.Fatalf("live decision = %d %+v", code, r)
			}
			if rows := shadowRows(t, g); len(rows) != 0 {
				t.Fatalf("rows = %+v", rows)
			}
			if after := metricValue(t, "draftcat_gate_shadow_errors_total"); after != before+1 {
				t.Fatalf("error counter %v -> %v", before, after)
			}
		})
	}
}

func TestShadowOffAndFrozenCallsAreNotCompared(t *testing.T) {
	_, st := freezeStore(t)
	g := newToolGate(gateCfg(config.ToolRule{Name: "read_file"}), nil)
	evals := 0
	g.shadowEval = func(gate config.ToolGateConfig, tool string, args, def map[string]interface{}, total toolDailyTotal) (toolPolicyVerdict, error) {
		evals++
		return evaluateToolPolicy(gate, tool, args, def, total)
	}
	g.shadow = func() *shadowCandidate { return nil }
	postGate(t, g, `{"action_id":"o1","tool":"read_file"}`)
	g.shadowWG.Wait()
	if evals != 0 {
		t.Fatal("an absent candidate is never evaluated")
	}
	shadowWith(g) // a candidate that lists nothing would deny every call
	if _, err := st.SetGateFreeze("x", "cli", time.Now()); err != nil {
		t.Fatal(err)
	}
	postGate(t, g, `{"action_id":"o2","tool":"read_file"}`)
	if rows := shadowRows(t, g); len(rows) != 0 || evals != 0 {
		t.Fatalf("a frozen call has no policy verdict to compare: %d %+v", evals, rows)
	}
}

func TestGateShadowCommandGroupsAndLists(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "s.db")
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, "state:\n  path: "+dbPath+"\n")
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := gateCmd(append(args, "--config", cfgPath), &out, &errOut)
		return code, out.String(), errOut.String()
	}
	if code, out, _ := run("shadow"); code != 0 || !strings.HasPrefix(out, "no shadow differences since ") {
		t.Fatalf("no store yet: %d %q", code, out)
	}
	if _, err := os.Stat(dbPath); err == nil {
		t.Fatal("gate shadow created a store")
	}

	st, err := statestore.OpenStateStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	add := func(ago time.Duration, tool, live, shadow, rule string) {
		t.Helper()
		if err := st.RecordShadowDiff(statestore.ShadowDiff{At: now.Add(-ago), Agent: "agent-1", Tool: tool, ArgsHash: "sha256:aa",
			LiveDecision: live, LiveRule: "listed", ShadowDecision: shadow, ShadowRule: rule, CandidateHash: "sha256:cc"},
			now.Add(-shadowRetention)); err != nil {
			t.Fatal(err)
		}
	}
	add(3*time.Hour, "send_email", "allow", "deny", "max_calls_per_day")
	add(2*time.Hour, "send_email", "allow", "deny", "max_calls_per_day")
	add(1*time.Hour, "send_email", "allow", "deny", "max_calls_per_day")
	add(30*time.Minute, "create_invoice", "ask", "allow", "")
	add(10*24*time.Hour, "send_email", "deny", "allow", "listed") // outside the default 7d
	_ = st.Close()

	code, out, _ := run("shadow")
	if code != 0 || !strings.Contains(out, "4 call(s) decided differently") ||
		!strings.Contains(out, "      3  send_email: allow -> deny") || !strings.Contains(out, "      1  create_invoice: ask -> allow") ||
		strings.Contains(out, "deny -> allow") || strings.Index(out, "      3  send_email") > strings.Index(out, "      1  create_invoice") {
		t.Fatalf("text: %d\n%s", code, out)
	}
	_, latest, found := strings.Cut(out, "latest:")
	if !found {
		t.Fatalf("no latest rows: %s", out)
	}
	if strings.Count(latest, "\n") != 5 || !strings.Contains(latest, "agent-1  create_invoice: ask (listed) -> allow  args sha256:aa  candidate sha256:cc") ||
		!strings.Contains(latest, "send_email: allow (listed) -> deny (max_calls_per_day)") || strings.Index(latest, "create_invoice") > strings.Index(latest, "send_email") {
		t.Fatalf("latest rows, newest first: %s", latest)
	}

	code, out, _ = run("shadow", "--since", "30d", "--json")
	var rep shadowReportJSON
	if code != 0 || json.Unmarshal([]byte(out), &rep) != nil {
		t.Fatalf("json: %d %q", code, out)
	}
	if rep.Total != 5 || len(rep.Groups) != 3 || rep.Groups[0].Tool != "send_email" || rep.Groups[0].Count != 3 ||
		len(rep.Latest) != 5 || rep.Latest[0].Tool != "create_invoice" || rep.Latest[0].ArgsHash != "sha256:aa" || rep.Latest[0].CandidateHash != "sha256:cc" {
		t.Fatalf("json report: %+v", rep)
	}

	for _, args := range [][]string{{"shadow", "extra"}, {"shadow", "--since", "soon"}, {"shadow", "--agent", "a"}, {"status", "--since", "7d"}} {
		if code, _, _ := run(args...); code != 2 {
			t.Fatalf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestShadowCandidateReloads(t *testing.T) {
	resetLive(t)
	liveShadow.Store(nil)
	t.Cleanup(func() { liveShadow.Store(nil) })
	t.Setenv("RL_TG", "x")
	t.Setenv("RL_KEY", "y")
	t.Setenv("RL_HOOK", "s3cret")
	_, st := freezeStore(t)
	_ = st
	dir := t.TempDir()
	cand := filepath.Join(dir, "candidate.yaml")
	writeFile(t, cand, "tool_gate:\n  tools:\n    - name: read_file\n      require_approval: true\n")
	gateSection := "webhook:\n  enabled: true\n  secret_env: RL_HOOK\ntool_gate:\n  enabled: true\n  tools:\n    - name: read_file\n"
	withShadow := reloadBase + gateSection + "  shadow:\n    config: " + cand + "\n"
	running, boot := bootFrom(t, withShadow)
	sched := newScheduler(running.Pipelines)
	skills := filepath.Join(dir, "skills")
	path := writeReloadConfig(t, dir, withShadow)

	startShadow(running, func(string) { t.Fatal("a valid candidate sends no notice") })
	if c := liveShadow.Load(); c == nil || c.Path != cand {
		t.Fatalf("boot candidate: %+v", c)
	}
	g := newToolGate(running, nil)

	// The running gate picks the reloaded candidate up on its next call.
	writeFile(t, cand, "tool_gate:\n  tools:\n    - name: other\n")
	msg := reloadEngine(path, skills, boot, running, sched, nil, nil)
	if !strings.Contains(msg, "Reloaded") || !strings.Contains(msg, "Shadow policy: "+cand) || strings.Contains(msg, "tool_gate") {
		t.Fatalf("reload: %q", msg)
	}
	if _, r := postGate(t, g, `{"action_id":"rl1","tool":"read_file"}`); r.Decision != "allow" {
		t.Fatalf("%+v", r)
	}
	if rows := shadowRows(t, g); len(rows) != 1 || rows[0].ShadowRule != "not listed" {
		t.Fatalf("the reloaded candidate decides: %+v", rows)
	}

	// A broken candidate is refused: the reload still applies, the shadow is off.
	writeFile(t, cand, "tool_gate:\n  tools:\n    - name: read_file\n      args:\n        path: {regex: \"(\"}\n")
	msg = reloadEngine(path, skills, boot, running, sched, nil, nil)
	if !strings.Contains(msg, "Reloaded") || !strings.Contains(msg, "Shadow policy refused, shadow off") || !strings.Contains(msg, "regex") {
		t.Fatalf("broken candidate: %q", msg)
	}
	if liveShadow.Load() != nil {
		t.Fatal("a refused candidate must switch the shadow off")
	}
	_ = os.Remove(cand)
	msg = reloadEngine(path, skills, boot, running, sched, nil, nil)
	if !strings.Contains(msg, "Shadow policy refused") || !strings.Contains(msg, "candidate.yaml") {
		t.Fatalf("missing candidate: %q", msg)
	}

	// Fixed, then removed from the config: on again, then off; never a restart.
	writeFile(t, cand, "tool_gate:\n  tools:\n    - name: other\n")
	if msg := reloadEngine(path, skills, boot, running, sched, nil, nil); !strings.Contains(msg, "Shadow policy: ") || liveShadow.Load() == nil {
		t.Fatalf("fixed candidate: %q", msg)
	}
	path = writeReloadConfig(t, dir, reloadBase+gateSection)
	msg = reloadEngine(path, skills, boot, running, sched, nil, nil)
	if !strings.Contains(msg, "Shadow policy off.") || strings.Contains(msg, "tool_gate") || liveShadow.Load() != nil {
		t.Fatalf("shadow removed: %q", msg)
	}
}

func TestStartShadowRefusesABrokenCandidateOnce(t *testing.T) {
	liveShadow.Store(nil)
	t.Cleanup(func() { liveShadow.Store(nil) })
	cfg := gateCfg(config.ToolRule{Name: "read_file"})
	cfg.ToolGate.Shadow = &config.GateShadow{Config: filepath.Join(t.TempDir(), "missing.yaml")}
	var notices []string
	startShadow(cfg, func(m string) { notices = append(notices, m) })
	if len(notices) != 1 || !strings.Contains(notices[0], "Shadow policy refused, shadow off") || liveShadow.Load() != nil {
		t.Fatalf("notices = %q", notices)
	}
}

func TestGateHelpListsShadow(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := gateCmd([]string{"--help"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "gate shadow [--since") {
		t.Fatalf("gate --help: %d %s", code, out.String())
	}
}

// A candidate rule can pin its definition with definition_file; the shadow
// reads the file the way the live gate does, and refuses a candidate whose
// file it cannot read.
func TestShadowCandidateReadsDefinitionFiles(t *testing.T) {
	dir := t.TempDir()
	def := filepath.Join(dir, "send_email.json")
	writeFile(t, def, `{"name":"send_email","description":"Send one email"}`)
	cand := filepath.Join(dir, "candidate.yaml")
	writeFile(t, cand, "tool_gate:\n  tools:\n    - name: send_email\n      definition_file: "+def+"\n")
	cfg := gateCfg(config.ToolRule{Name: "send_email"})
	cfg.ToolGate.Shadow = &config.GateShadow{Config: cand}
	c, err := loadShadowCandidate(cfg)
	if err != nil || c == nil {
		t.Fatalf("candidate: %+v, %v", c, err)
	}
	if p := c.Gate.Tools[0].Pinned(); p == nil || p.SHA256 == "" {
		t.Fatalf("candidate rule is not pinned to its file: %+v", p)
	}
	writeFile(t, def, "{")
	if _, err := loadShadowCandidate(cfg); err == nil {
		t.Fatal("an unreadable definition_file must refuse the candidate")
	}
}
