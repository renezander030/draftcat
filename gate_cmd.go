package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/renezander030/draftcat/internal/config"
)

const gateUsage = `Usage:
  draftcat gate check <tool> [--args JSON] [--definition file.json] [--spent arg=N]... [--calls N] [--config path] [--json]
  draftcat gate test <cases.yaml> [--config path] [--compare old.yaml] [--json]
  draftcat gate hash-definition <file.json|->
  draftcat gate diff-definition <pinned.json> <current.json> [--json]
  draftcat gate verify-grant <grant> --tool <tool> [--args JSON] [--max-age 5m] [--config path] [--json]
  draftcat gate freeze [--reason text] [--config path]
  draftcat gate unfreeze [--agent name] [--config path]
  draftcat gate status [--json] [--config path]
  draftcat gate hook --format claude|codex|copilot|gemini [--url URL] [--token-env VAR] [--agent NAME] [--wait 5m]
  draftcat gate install claude-code|codex|copilot-cli|gemini-cli [--matcher REGEX] [--dir DIR | --user] [--dry-run] [--remove]
  draftcat gate shadow [--since 7d|YYYY-MM-DD] [--json] [--config path]

check         decide one call against tool_gate without running the engine
test          decide every case in a file and fail when one differs from its expect;
              with --compare, run the cases against --config and the older file
              and print only the cases that decide differently (exit 1 when any
              does; expect is not checked)
hash-definition  print the sha256 value to paste into a rule's definition_sha256
diff-definition  name what changed between a pinned tool definition and a
              current one, field by field: exit 0 when they hash alike, 1 when
              they differ (--json lists every change)
verify-grant  check an execution grant before running the tool: exit 0 valid, 1 not

A decision is allow, deny, or ask (a human decides). --spent sets an
argument's total already allowed today, for rules with max_per_day, and
--calls the tool's calls already allowed today, for max_calls_per_day (both
default 0). verify-grant reads the key named by
tool_gate.execution_grant.secret_env. check, test, hash-definition,
diff-definition and verify-grant do not read or write the state store.

freeze        deny every gated tool call until unfreeze; a running engine sees it
              on its next call (also /freeze and /unfreeze in Telegram)
unfreeze      lift the freeze; with --agent, lift that one agent's freeze
              (set by tool_gate.trip) instead
status        show whether the gate is frozen, by whom, since when and why, and
              which agents are frozen
freeze, unfreeze and status use the state store of --config.

hook          an agent harness's pre-tool hook: ask the gate about the call on
              stdin, consume an allowed permit, answer allow or deny in the
              harness's format; anything else blocks the call
install       write that hook into the harness's settings file (project, or
              --user); see 'draftcat gate install --help'
shadow        list the live calls the tool_gate.shadow candidate decided
              differently, grouped by tool and decision pair, with the latest
              rows (default --since 7d); reads the state store of --config`

// gateCase is one entry of a `draftcat gate test` file.
type gateCase struct {
	Name  string                 `yaml:"name" json:"name"`
	Tool  string                 `yaml:"tool" json:"tool"`
	Args  map[string]interface{} `yaml:"args" json:"args,omitempty"`
	Spent map[string]string      `yaml:"spent" json:"spent,omitempty"`
	// Calls is how many calls of the tool were already allowed today, for
	// max_calls_per_day.
	Calls  int    `yaml:"calls" json:"calls,omitempty"`
	Expect string `yaml:"expect" json:"expect"`
	// Definition is the tool definition the harness would send with the call,
	// for rules with definition_sha256 or definition_file.
	Definition map[string]interface{} `yaml:"definition" json:"definition,omitempty"`
	// Rule optionally pins the rule that decides (listed, not listed,
	// args.<why>, max_per_day.<arg>, max_calls_per_day), for a deny or allow.
	Rule string `yaml:"rule" json:"rule,omitempty"`
}

type gateResult struct {
	Case     string   `json:"case,omitempty"`
	Tool     string   `json:"tool"`
	Decision string   `json:"decision"`
	Rule     string   `json:"rule,omitempty"`
	Reason   string   `json:"reason"`
	Why      string   `json:"why,omitempty"`
	Totals   []string `json:"totals,omitempty"`
	Expect   string   `json:"expect,omitempty"`
	Pass     *bool    `json:"pass,omitempty"`
	Problem  string   `json:"problem,omitempty"`
}

func runGateCmd(args []string) int { return gateCmd(args, os.Stdout, os.Stderr) }

func gateCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		_, _ = fmt.Fprintln(stdout, gateUsage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "hook":
		return gateHookCmd(rest, os.Stdin, stdout, stderr)
	case "install":
		return gateInstallCmd(rest, stdout, stderr)
	}
	configPath, asJSON, argsJSON := "config.yaml", false, ""
	definitionPath, comparePath := "", ""
	spent := map[string]string{}
	calls := 0
	tool, maxAge, reason, agent, since := "", time.Duration(0), "", "", ""
	var positional []string
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		needValue := func() (string, bool) {
			if i+1 >= len(rest) {
				_, _ = fmt.Fprintf(stderr, "gate: %s requires a value\n", a)
				return "", false
			}
			i++
			return rest[i], true
		}
		switch a {
		case "--config", "-config":
			v, ok := needValue()
			if !ok {
				return 2
			}
			configPath = v
		case "--args":
			v, ok := needValue()
			if !ok {
				return 2
			}
			argsJSON = v
		case "--definition":
			v, ok := needValue()
			if !ok {
				return 2
			}
			definitionPath = v
		case "--compare":
			v, ok := needValue()
			if !ok {
				return 2
			}
			comparePath = v
		case "--spent":
			v, ok := needValue()
			if !ok {
				return 2
			}
			name, amount, found := strings.Cut(v, "=")
			if !found || strings.TrimSpace(name) == "" {
				_, _ = fmt.Fprintf(stderr, "gate: --spent wants arg=N (got %q)\n", v)
				return 2
			}
			spent[strings.TrimSpace(name)] = strings.TrimSpace(amount)
		case "--calls":
			v, ok := needValue()
			if !ok {
				return 2
			}
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 0 {
				_, _ = fmt.Fprintf(stderr, "gate: --calls wants a count of 0 or more (got %q)\n", v)
				return 2
			}
			calls = n
		case "--tool":
			v, ok := needValue()
			if !ok {
				return 2
			}
			tool = v
		case "--max-age":
			v, ok := needValue()
			if !ok {
				return 2
			}
			d, err := time.ParseDuration(v)
			if err != nil || d < 0 {
				_, _ = fmt.Fprintf(stderr, "gate: --max-age wants a duration like 5m (got %q)\n", v)
				return 2
			}
			maxAge = d
		case "--reason":
			v, ok := needValue()
			if !ok {
				return 2
			}
			reason = strings.TrimSpace(v)
		case "--since":
			v, ok := needValue()
			if !ok {
				return 2
			}
			since = strings.TrimSpace(v)
		case "--agent":
			v, ok := needValue()
			if !ok {
				return 2
			}
			if agent = strings.TrimSpace(v); agent == "" {
				_, _ = fmt.Fprintln(stderr, "gate: --agent wants an agent name")
				return 2
			}
		case "--json":
			asJSON = true
		default:
			if strings.HasPrefix(a, "-") {
				_, _ = fmt.Fprintf(stderr, "gate: unknown option %q\n", a)
				return 2
			}
			positional = append(positional, a)
		}
	}
	if sub == "shadow" {
		if len(positional) != 0 || argsJSON != "" || definitionPath != "" || comparePath != "" || len(spent) > 0 ||
			calls > 0 || tool != "" || maxAge != 0 || reason != "" || agent != "" {
			_, _ = fmt.Fprintln(stderr, "gate: shadow takes only --since, --json and --config")
			return 2
		}
		return gateShadowCmd(configPath, since, asJSON, time.Now(), stdout, stderr)
	}
	if since != "" {
		_, _ = fmt.Fprintln(stderr, "gate: --since belongs to shadow")
		return 2
	}
	if sub == "freeze" || sub == "unfreeze" || sub == "status" {
		if len(positional) != 0 || argsJSON != "" || len(spent) > 0 || calls > 0 || tool != "" || maxAge != 0 {
			_, _ = fmt.Fprintf(stderr, "gate: %s takes only --config (and --reason for freeze, --agent for unfreeze, --json for status)\n", sub)
			return 2
		}
		if reason != "" && sub != "freeze" {
			_, _ = fmt.Fprintf(stderr, "gate: --reason belongs to freeze\n")
			return 2
		}
		if asJSON && sub != "status" {
			_, _ = fmt.Fprintf(stderr, "gate: --json belongs to status\n")
			return 2
		}
		if agent != "" && sub != "unfreeze" {
			_, _ = fmt.Fprintf(stderr, "gate: --agent belongs to unfreeze\n")
			return 2
		}
		return gateFreezeCmd(sub, configPath, reason, agent, asJSON, stdout, stderr)
	}
	if agent != "" {
		_, _ = fmt.Fprintln(stderr, "gate: --agent belongs to unfreeze")
		return 2
	}
	if sub == "diff-definition" {
		if len(positional) != 2 {
			_, _ = fmt.Fprintln(stderr, "gate: diff-definition wants two files: <pinned.json> <current.json>")
			return 2
		}
		return gateDiffDefinition(positional[0], positional[1], asJSON, stdout, stderr)
	}
	if len(positional) != 1 {
		_, _ = fmt.Fprintln(stderr, gateUsage)
		return 2
	}
	if sub == "hash-definition" {
		return gateHashDefinition(positional[0], stdout, stderr)
	}
	if definitionPath != "" && sub != "check" {
		_, _ = fmt.Fprintln(stderr, "gate: --definition belongs to check; test reads definition: from the file")
		return 2
	}
	if comparePath != "" && sub != "test" {
		_, _ = fmt.Fprintln(stderr, "gate: --compare belongs to test")
		return 2
	}
	cfg, err := loadGateConfig(configPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gate: %v\n", err)
		return 1
	}

	switch sub {
	case "check":
		c := gateCase{Tool: positional[0], Spent: spent, Calls: calls}
		if argsJSON != "" {
			if err := decodeStrictJSON([]byte(argsJSON), &c.Args); err != nil {
				_, _ = fmt.Fprintf(stderr, "gate: --args is not a JSON object: %v\n", err)
				return 2
			}
		}
		if definitionPath != "" {
			def, err := readDefinitionFile(definitionPath)
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "gate: %v\n", err)
				return 2
			}
			c.Definition = def
		}
		r, err := decideGateCase(cfg.ToolGate, c)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "gate: %v\n", err)
			return 2
		}
		printGateResults(stdout, []gateResult{r}, asJSON)
		return 0
	case "test":
		if argsJSON != "" || len(spent) > 0 || calls > 0 {
			_, _ = fmt.Fprintln(stderr, "gate: --args, --spent and --calls belong to check; test reads them from the file")
			return 2
		}
		cases, code := readGateCases(positional[0], stderr)
		if code != 0 {
			return code
		}
		if comparePath != "" {
			older, err := loadGateConfig(comparePath)
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "gate: %v\n", err)
				return 1
			}
			return gateCompare(cases, older.ToolGate, cfg.ToolGate, asJSON, stdout, stderr)
		}
		return gateTest(cases, cfg.ToolGate, asJSON, stdout, stderr)
	case "verify-grant":
		if tool == "" {
			_, _ = fmt.Fprintln(stderr, "gate: verify-grant needs --tool")
			return 2
		}
		var args map[string]interface{}
		if argsJSON != "" {
			if err := decodeStrictJSON([]byte(argsJSON), &args); err != nil {
				_, _ = fmt.Fprintf(stderr, "gate: --args is not a JSON object: %v\n", err)
				return 2
			}
		}
		key := cfg.ToolGate.ExecutionGrant.Secret()
		if len(key) == 0 {
			_, _ = fmt.Fprintln(stderr, "gate: tool_gate.execution_grant.secret_env is not set or its variable is empty")
			return 2
		}
		g, err := verifyExecutionGrant(key, positional[0], tool, args, maxAge, time.Now())
		switch {
		case asJSON:
			out := map[string]interface{}{"valid": err == nil, "grant": g}
			if err != nil {
				out["problem"] = err.Error()
			}
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(out)
		case err == nil:
			_, _ = fmt.Fprintf(stdout, "VALID  %s\n  action: %s\n  consumed: %s\n", g.Tool, g.ActionID, g.ConsumedAt)
		default:
			_, _ = fmt.Fprintf(stdout, "INVALID  %s\n  problem: %v\n", tool, err)
		}
		if err != nil {
			return 1
		}
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "gate: unknown command %q\n%s\n", sub, gateUsage)
		return 2
	}
}

// loadGateConfig reads a config for the offline checks, with every rule's
// definition_file read the way the engine reads it.
func loadGateConfig(path string) (*config.Config, error) {
	cfg, err := loadConfigFile(path)
	if err != nil {
		return nil, err
	}
	if err := loadToolDefinitions(&cfg.ToolGate); err != nil {
		return nil, err
	}
	return cfg, nil
}

// readDefinitionFile reads a tool definition (a JSON object) from a file, or
// from standard input when the path is "-".
func readDefinitionFile(path string) (map[string]interface{}, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		// #nosec G304 -- the operator's own definition file.
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	def, err := config.ParseToolDefinition(raw)
	if err != nil {
		return nil, fmt.Errorf("%s is not a JSON object with a tool definition: %w", path, err)
	}
	return def, nil
}

// gateHashDefinition prints the value a rule's definition_sha256 takes for a
// definition. The hash is over canonical JSON, so key order does not matter.
func gateHashDefinition(path string, stdout, stderr io.Writer) int {
	def, err := readDefinitionFile(path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gate: %v\n", err)
		return 2
	}
	_, _ = fmt.Fprintln(stdout, hashCanonical(def))
	return 0
}

// gateDiffDefinition compares a pinned tool definition with a current one the
// way the gate compares a call's definition with a rule's definition_file:
// exit 0 when they hash alike, 1 when they differ, 2 when either cannot be
// read. The text form is the summary the gate shows; --json lists every
// change.
func gateDiffDefinition(pinnedPath, currentPath string, asJSON bool, stdout, stderr io.Writer) int {
	pinned, err := readDefinitionFile(pinnedPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gate: %v\n", err)
		return 2
	}
	current, err := readDefinitionFile(currentPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gate: %v\n", err)
		return 2
	}
	pinnedHash, currentHash := hashCanonical(pinned), hashCanonical(current)
	changes := []string{}
	if pinnedHash != currentHash {
		changes = definitionChanges(pinned, current)
	}
	switch {
	case asJSON:
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(struct {
			Changed bool     `json:"changed"`
			Pinned  string   `json:"pinned_sha256"`
			Current string   `json:"current_sha256"`
			Changes []string `json:"changes"`
		}{pinnedHash != currentHash, pinnedHash, currentHash, changes})
	case pinnedHash == currentHash:
		_, _ = fmt.Fprintf(stdout, "definition unchanged: %s\n", pinnedHash)
	default:
		_, _ = fmt.Fprintln(stdout, summarizeDefinitionChanges(changes))
		_, _ = fmt.Fprintf(stdout, "  pinned:  %s\n  current: %s\n", pinnedHash, currentHash)
	}
	if pinnedHash != currentHash {
		return 1
	}
	return 0
}

// readGateCases reads a `draftcat gate test` file: strict YAML with at least
// one case, every unnamed case named "case N" by its position. A file it
// cannot use is reported on stderr with the exit code to return (1 unreadable,
// 2 invalid or empty).
func readGateCases(path string, stderr io.Writer) ([]gateCase, int) {
	// #nosec G304 -- the operator's own cases file.
	raw, err := os.ReadFile(path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gate: %v\n", err)
		return nil, 1
	}
	var file struct {
		Cases []gateCase `yaml:"cases"`
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		_, _ = fmt.Fprintf(stderr, "gate: %s: %v\n", path, err)
		return nil, 2
	}
	if len(file.Cases) == 0 {
		_, _ = fmt.Fprintf(stderr, "gate: %s has no cases\n", path)
		return nil, 2
	}
	for i := range file.Cases {
		if file.Cases[i].Name == "" {
			file.Cases[i].Name = fmt.Sprintf("case %d", i+1)
		}
	}
	return file.Cases, 0
}

// gateTest decides every case against gate and checks it against its expect
// (and its rule, when the case pins one). It exits 1 when any case fails.
func gateTest(cases []gateCase, gate config.ToolGateConfig, asJSON bool, stdout, stderr io.Writer) int {
	results, failed := make([]gateResult, 0, len(cases)), 0
	for _, c := range cases {
		expect := strings.ToLower(strings.TrimSpace(c.Expect))
		if expect != "allow" && expect != "deny" && expect != "ask" {
			_, _ = fmt.Fprintf(stderr, "gate: %s: expect must be allow, deny or ask (got %q)\n", c.Name, c.Expect)
			return 2
		}
		r, err := decideGateCase(gate, c)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "gate: %s: %v\n", c.Name, err)
			return 2
		}
		r.Expect = expect
		pass := r.Decision == expect
		if !pass {
			r.Problem = fmt.Sprintf("decided %s, expected %s", r.Decision, expect)
		} else if c.Rule != "" && r.Rule != c.Rule {
			pass, r.Problem = false, fmt.Sprintf("decided by rule %q, expected %q", r.Rule, c.Rule)
		}
		r.Pass = &pass
		if !pass {
			failed++
		}
		results = append(results, r)
	}
	printGateResults(stdout, results, asJSON)
	if !asJSON {
		_, _ = fmt.Fprintf(stdout, "%d case(s), %d failed\n", len(results), failed)
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// gateDiff is one case that decides differently under the two configs.
type gateDiff struct {
	Name string      `json:"name"`
	Tool string      `json:"tool"`
	Old  gateVerdict `json:"old"`
	New  gateVerdict `json:"new"`
}

type gateVerdict struct {
	Decision string `json:"decision"`
	Rule     string `json:"rule,omitempty"`
	Reason   string `json:"reason"`
}

// reasonClass is the part of a reason before its detail ("arguments outside
// the rule", "above the review threshold"), so two denials by the same cause
// with different values are not a difference.
func reasonClass(r gateResult) string {
	s := r.Why
	if s == "" {
		s = r.Reason
	}
	class, _, _ := strings.Cut(s, " — ")
	return class
}

// gateCompare runs every case (named by readGateCases) against both configs
// through decideGateCase and reports the ones whose decision, rule or reason
// class differ. Expectations are not checked. It exits 1 when any case
// differs, like diff.
func gateCompare(cases []gateCase, older, newer config.ToolGateConfig, asJSON bool, stdout, stderr io.Writer) int {
	diffs := []gateDiff{}
	for _, c := range cases {
		ro, err := decideGateCase(older, c)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "gate: %s: %v\n", c.Name, err)
			return 2
		}
		rn, err := decideGateCase(newer, c)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "gate: %s: %v\n", c.Name, err)
			return 2
		}
		if ro.Decision == rn.Decision && ro.Rule == rn.Rule && reasonClass(ro) == reasonClass(rn) {
			continue
		}
		diffs = append(diffs, gateDiff{Name: c.Name, Tool: c.Tool,
			Old: gateVerdict{ro.Decision, ro.Rule, ro.Reason}, New: gateVerdict{rn.Decision, rn.Rule, rn.Reason}})
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(diffs)
	} else {
		for _, d := range diffs {
			rule := d.New.Rule
			if d.Old.Decision == d.New.Decision && d.Old.Rule != d.New.Rule {
				rule = d.Old.Rule + " -> " + d.New.Rule
			}
			line := fmt.Sprintf("%s: %s -> %s", d.Name, d.Old.Decision, d.New.Decision)
			if rule != "" {
				line += " (" + rule + ")"
			}
			_, _ = fmt.Fprintln(stdout, line)
		}
		_, _ = fmt.Fprintf(stdout, "%d of %d cases decide differently\n", len(diffs), len(cases))
	}
	if len(diffs) > 0 {
		return 1
	}
	return 0
}

// decideGateCase runs one case through the same evaluation the gate endpoint
// uses, with today's totals taken from the case instead of the state store.
func decideGateCase(gate config.ToolGateConfig, c gateCase) (gateResult, error) {
	if strings.TrimSpace(c.Tool) == "" {
		return gateResult{}, fmt.Errorf("no tool named")
	}
	totals := map[string]*big.Rat{}
	for arg, amount := range c.Spent {
		v, ok := new(big.Rat).SetString(amount)
		if !ok || v.Sign() < 0 {
			return gateResult{}, fmt.Errorf("spent %s=%q is not a number of 0 or more", arg, amount)
		}
		totals[arg] = v
	}
	if c.Calls < 0 {
		return gateResult{}, fmt.Errorf("calls %d is not a count of 0 or more", c.Calls)
	}
	totals[config.CallCountKey] = big.NewRat(int64(c.Calls), 1)
	args := normalizeGateArgs(c.Args)
	v, err := evaluateToolPolicy(gate, c.Tool, args, normalizeGateArgs(c.Definition), func(_, arg string) (*big.Rat, error) {
		if t, ok := totals[arg]; ok {
			return t, nil
		}
		return new(big.Rat), nil
	})
	if err != nil {
		return gateResult{}, err
	}
	return gateResult{Case: c.Name, Tool: c.Tool, Decision: v.Decision, Rule: v.Rule, Reason: v.Reason, Why: v.Why, Totals: v.Totals}, nil
}

// normalizeGateArgs gives YAML-decoded arguments the shapes a JSON request
// body has at the gate (integers as numbers, nested maps keyed by string), so
// a case compares exactly as the call would.
func normalizeGateArgs(args map[string]interface{}) map[string]interface{} {
	if args == nil {
		return nil
	}
	out := make(map[string]interface{}, len(args))
	for k, v := range args {
		out[k] = normalizeGateValue(v)
	}
	return out
}

func normalizeGateValue(v interface{}) interface{} {
	switch x := v.(type) {
	case int:
		return json.Number(fmt.Sprint(x))
	case int64:
		return json.Number(fmt.Sprint(x))
	case uint64:
		return json.Number(fmt.Sprint(x))
	case map[string]interface{}:
		return normalizeGateArgs(x)
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, e := range x {
			out[i] = normalizeGateValue(e)
		}
		return out
	default:
		return v
	}
}

func printGateResults(w io.Writer, results []gateResult, asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if len(results) == 1 && results[0].Pass == nil {
			_ = enc.Encode(results[0])
			return
		}
		_ = enc.Encode(results)
		return
	}
	for _, r := range results {
		head := strings.ToUpper(r.Decision)
		if r.Pass != nil {
			mark := "PASS"
			if !*r.Pass {
				mark = "FAIL"
			}
			head = mark + "  " + r.Case + ": " + head
		}
		_, _ = fmt.Fprintf(w, "%s  %s\n", head, r.Tool)
		if r.Rule != "" {
			_, _ = fmt.Fprintf(w, "  rule:   %s\n", r.Rule)
		}
		_, _ = fmt.Fprintf(w, "  reason: %s\n", r.Reason)
		if r.Why != "" {
			_, _ = fmt.Fprintf(w, "  why:    %s\n", r.Why)
		}
		for _, t := range r.Totals {
			_, _ = fmt.Fprintf(w, "  today:  %s\n", t)
		}
		if r.Problem != "" {
			_, _ = fmt.Fprintf(w, "  %s\n", r.Problem)
		}
	}
}
