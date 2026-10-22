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
  draftcat gate check <tool> [--args JSON] [--spent arg=N]... [--calls N] [--config path] [--json]
  draftcat gate test <cases.yaml> [--config path] [--json]
  draftcat gate verify-grant <grant> --tool <tool> [--args JSON] [--max-age 5m] [--config path] [--json]

check         decide one call against tool_gate without running the engine
test          decide every case in a file and fail when one differs from its expect
verify-grant  check an execution grant before running the tool: exit 0 valid, 1 not

A decision is allow, deny, or ask (a human decides). --spent sets an
argument's total already allowed today, for rules with max_per_day, and
--calls the tool's calls already allowed today, for max_calls_per_day (both
default 0). verify-grant reads the key named by
tool_gate.execution_grant.secret_env. None of them reads or writes the state
store.`

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
	configPath, asJSON, argsJSON := "config.yaml", false, ""
	spent := map[string]string{}
	calls := 0
	tool, maxAge := "", time.Duration(0)
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
	if len(positional) != 1 {
		_, _ = fmt.Fprintln(stderr, gateUsage)
		return 2
	}
	cfg, err := loadConfigFile(configPath)
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
		// #nosec G304 -- the operator's own cases file.
		raw, err := os.ReadFile(positional[0])
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "gate: %v\n", err)
			return 1
		}
		var file struct {
			Cases []gateCase `yaml:"cases"`
		}
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)
		if err := dec.Decode(&file); err != nil {
			_, _ = fmt.Fprintf(stderr, "gate: %s: %v\n", positional[0], err)
			return 2
		}
		if len(file.Cases) == 0 {
			_, _ = fmt.Fprintf(stderr, "gate: %s has no cases\n", positional[0])
			return 2
		}
		results, failed := make([]gateResult, 0, len(file.Cases)), 0
		for i, c := range file.Cases {
			if c.Name == "" {
				c.Name = fmt.Sprintf("case %d", i+1)
			}
			expect := strings.ToLower(strings.TrimSpace(c.Expect))
			if expect != "allow" && expect != "deny" && expect != "ask" {
				_, _ = fmt.Fprintf(stderr, "gate: %s: expect must be allow, deny or ask (got %q)\n", c.Name, c.Expect)
				return 2
			}
			r, err := decideGateCase(cfg.ToolGate, c)
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

// loadConfigFile reads and parses a config file for a subcommand that reports
// "<command>: read config: ..." or "<command>: parse config: ...".
func loadConfigFile(path string) (*config.Config, error) {
	// #nosec G304 -- the operator's own --config argument.
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg config.Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
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
	v, err := evaluateToolPolicy(gate, c.Tool, args, func(_, arg string) (*big.Rat, error) {
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
