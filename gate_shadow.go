package main

// Shadow policy: a candidate tool_gate decides every live call a second time.
//
// `draftcat gate test --compare` shows how a policy change moves hand-written
// cases; tool_gate.shadow shows how it moves the calls agents actually make.
// After the live decision is made, the candidate's rules decide the same call
// the way `draftcat gate check` does: a pure evaluation, with today's totals
// read from the live store, no prompt, no permit and no state written for
// the candidate. Where the two decide differently one row is stored (time,
// agent, tool, argument hash, both verdicts, the candidate's hash), and
// `draftcat gate shadow` reads them back.
//
// The candidate never has a say: it runs after the live answer, in the
// background, under recover, and a failure is logged and counted, never
// returned to the caller.

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	"github.com/renezander030/draftcat/internal/obs"
	statestore "github.com/renezander030/draftcat/internal/state"
	"github.com/renezander030/draftcat/internal/validate"
)

// shadowRetention is how long a stored difference is kept; older rows are
// dropped as new ones are written.
const shadowRetention = 30 * 24 * time.Hour

// shadowListed is how many of the latest differences `gate shadow` prints.
const shadowListed = 10

// shadowCandidate is the loaded candidate policy.
type shadowCandidate struct {
	Path string
	Gate config.ToolGateConfig
	// Hash identifies the candidate's tool rules in every stored row.
	Hash string
}

// liveShadow is the candidate the running gate compares against; nil = off.
// Set at start and on every reload.
var liveShadow atomic.Pointer[shadowCandidate]

// loadShadowCandidate reads and checks the candidate tool_gate.shadow names.
// nil, nil when the shadow is off. A candidate that fails validation is an
// error, and the caller runs without a shadow.
func loadShadowCandidate(cfg *config.Config) (*shadowCandidate, error) {
	s := cfg.ToolGate.Shadow
	if s == nil || !cfg.ToolGate.Enabled {
		return nil, nil
	}
	if v := validate.Summarize(validate.CheckToolGateShadow(cfg)); v.Errors > 0 {
		return nil, fmt.Errorf("%d error(s); first: %s", v.Errors, v.FirstError)
	}
	path := strings.TrimSpace(s.Config)
	gate, err := config.LoadToolGateFile(path)
	if err != nil {
		return nil, err
	}
	return &shadowCandidate{Path: path, Gate: gate, Hash: hashCanonical(gate.Tools)}, nil
}

// startShadow loads the candidate at engine start. Startup validation has
// already refused a broken candidate unless it was skipped; then the engine
// runs without a shadow and the operator is told once.
func startShadow(cfg *config.Config, notify func(string)) {
	cand, err := loadShadowCandidate(cfg)
	if err != nil {
		msg := "[tool-gate] Shadow policy refused, shadow off: " + err.Error()
		log.Printf("%s", msg)
		notify(msg)
		return
	}
	liveShadow.Store(cand)
	if cand != nil {
		log.Printf("[tool-gate] shadow policy %s (%s) decides beside the live rules", cand.Path, cand.Hash)
	}
}

// shadowReloadLine is the reload summary's line about the shadow.
func shadowReloadLine(was, now *shadowCandidate, err error) string {
	switch {
	case err != nil:
		return "\nShadow policy refused, shadow off: " + err.Error()
	case now != nil:
		return "\nShadow policy: " + now.Path + " (" + now.Hash + ")"
	case was != nil:
		return "\nShadow policy off."
	}
	return ""
}

// shadowVerdict is a policy verdict without argument values: the decision,
// the deciding rule's name and the reason's class. Two verdicts that differ
// only in a value (the amount in "amount: 800 exceeds max 500") are the same.
type shadowVerdict struct {
	Decision, Rule, Reason string
}

func shadowVerdictOf(v toolPolicyVerdict) shadowVerdict {
	rule := v.Rule
	if strings.HasPrefix(rule, "args.") {
		rule, _, _ = strings.Cut(rule, ": ")
	}
	reason := v.Why
	if reason == "" {
		reason = v.Reason
	}
	class, _, _ := strings.Cut(reason, " — ")
	return shadowVerdict{Decision: v.Decision, Rule: rule, Reason: class}
}

// shadowAfter returns the function that compares this call under the
// candidate: once, in the background. HandleCall runs it after the live
// answer is written, or once a human is being asked, so the candidate never
// changes or delays the live decision. A no-op when the shadow is off.
func (g *toolGate) shadowAfter(req ToolCallRequest, live toolPolicyVerdict) func() {
	cand := g.shadow()
	if cand == nil {
		return func() {}
	}
	at := g.now()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.shadowWG.Add(1)
			go func() {
				defer g.shadowWG.Done()
				g.compareShadow(cand, req, live, at)
			}()
		})
	}
}

// compareShadow decides req under the candidate and stores the difference,
// if any. Today's totals come from the live store without this call's own
// share, so the candidate sees what the live rules saw.
func (g *toolGate) compareShadow(cand *shadowCandidate, req ToolCallRequest, live toolPolicyVerdict, at time.Time) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[tool-gate] shadow evaluation of %s panicked: %v", req.Tool, r)
			obs.RecordShadowError()
		}
	}()
	day := statestore.UTCDay(at)
	v, err := g.shadowEval(cand.Gate, req.Tool, req.Args, req.Definition, func(tool, arg string) (*big.Rat, error) {
		if arg == config.CallCountKey {
			// Counted from the actions themselves: the live rules keep a
			// call count only for tools that have max_calls_per_day.
			n, err := state.ToolCallsAllowedOn(tool, day, req.ActionID)
			if err != nil {
				return nil, err
			}
			return new(big.Rat).SetInt64(n), nil
		}
		return state.ToolArgTotalExcept(tool, arg, day, req.ActionID)
	})
	if err != nil {
		log.Printf("[tool-gate] shadow evaluation of %s failed: %v", req.Tool, err)
		obs.RecordShadowError()
		return
	}
	lv, sv := shadowVerdictOf(live), shadowVerdictOf(v)
	if lv == sv {
		return
	}
	obs.RecordShadowDifference(req.Tool, lv.Decision, sv.Decision)
	if err := state.RecordShadowDiff(statestore.ShadowDiff{
		At: at, Agent: req.Agent, Tool: req.Tool, ArgsHash: req.argsHash,
		LiveDecision: lv.Decision, LiveRule: lv.Rule, LiveReason: lv.Reason,
		ShadowDecision: sv.Decision, ShadowRule: sv.Rule, ShadowReason: sv.Reason,
		CandidateHash: cand.Hash,
	}, at.Add(-shadowRetention)); err != nil {
		log.Printf("[tool-gate] shadow difference for %s not stored: %v", req.Tool, err)
		obs.RecordShadowError()
	}
}

type shadowGroupJSON struct {
	Tool   string `json:"tool"`
	Live   string `json:"live"`
	Shadow string `json:"shadow"`
	Count  int    `json:"count"`
	Latest string `json:"latest"`
}

type shadowRowJSON struct {
	At            string      `json:"at"`
	Agent         string      `json:"agent,omitempty"`
	Tool          string      `json:"tool"`
	ArgsHash      string      `json:"args_hash"`
	Live          gateVerdict `json:"live"`
	Shadow        gateVerdict `json:"shadow"`
	CandidateHash string      `json:"candidate_hash"`
}

type shadowReportJSON struct {
	Since  string            `json:"since"`
	Total  int               `json:"total"`
	Groups []shadowGroupJSON `json:"groups"`
	Latest []shadowRowJSON   `json:"latest"`
}

// gateShadowCmd runs `draftcat gate shadow`: the stored differences since a
// day, grouped by tool and decision pair, and the latest rows. It opens the
// store read-only and never creates one.
func gateShadowCmd(configPath, since string, asJSON bool, now time.Time, stdout, stderr io.Writer) int {
	if since == "" {
		since = "7d"
	}
	from, _, err := reportWindow(since, "", now)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gate: %v\n", err)
		return 2
	}
	rep := shadowReportJSON{Since: from.UTC().Format(time.RFC3339), Groups: []shadowGroupJSON{}, Latest: []shadowRowJSON{}}
	path := statePathFromConfigFile(configPath)
	if _, err := os.Stat(path); err == nil {
		st, err := statestore.OpenStateStoreReadOnly(path)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "gate: %v\n", err)
			return 1
		}
		defer func() { _ = st.Close() }()
		groups, err := st.ShadowDiffGroups(from)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "gate: %v\n", err)
			return 1
		}
		rows, err := st.ShadowDiffs(from, shadowListed)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "gate: %v\n", err)
			return 1
		}
		for _, g := range groups {
			rep.Total += g.Count
			rep.Groups = append(rep.Groups, shadowGroupJSON{Tool: g.Tool, Live: g.LiveDecision, Shadow: g.ShadowDecision,
				Count: g.Count, Latest: g.Latest.UTC().Format(time.RFC3339)})
		}
		for _, d := range rows {
			rep.Latest = append(rep.Latest, shadowRowJSON{At: d.At.UTC().Format(time.RFC3339), Agent: d.Agent, Tool: d.Tool,
				ArgsHash: d.ArgsHash, CandidateHash: d.CandidateHash,
				Live:   gateVerdict{Decision: d.LiveDecision, Rule: d.LiveRule, Reason: d.LiveReason},
				Shadow: gateVerdict{Decision: d.ShadowDecision, Rule: d.ShadowRule, Reason: d.ShadowReason}})
		}
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
		return 0
	}
	if rep.Total == 0 {
		_, _ = fmt.Fprintf(stdout, "no shadow differences since %s\n", rep.Since)
		return 0
	}
	_, _ = fmt.Fprintf(stdout, "%d call(s) decided differently under the shadow policy since %s\n", rep.Total, rep.Since)
	for _, g := range rep.Groups {
		_, _ = fmt.Fprintf(stdout, "  %5d  %s: %s -> %s  (latest %s)\n", g.Count, g.Tool, g.Live, g.Shadow, g.Latest)
	}
	_, _ = fmt.Fprintln(stdout, "latest:")
	for _, d := range rep.Latest {
		_, _ = fmt.Fprintf(stdout, "  %s  %s  %s: %s -> %s  args %s  candidate %s\n", d.At, orDash(d.Agent), d.Tool,
			shadowVerdictText(d.Live), shadowVerdictText(d.Shadow), d.ArgsHash, d.CandidateHash)
	}
	return 0
}

// shadowVerdictText is "deny (rule)", or "ask (reason)" when no rule decided.
func shadowVerdictText(v gateVerdict) string {
	switch {
	case v.Rule != "":
		return v.Decision + " (" + v.Rule + ")"
	case v.Reason != "":
		return v.Decision + " (" + v.Reason + ")"
	}
	return v.Decision
}
