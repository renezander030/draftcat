package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

func runBudgetCmd(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Println("Usage: draftcat budget status [--json] [--config path]")
		fmt.Println("       draftcat budget reconcile <call-id> --tokens N --cost USD [--config path]")
		fmt.Println("Reconcile only after stopping the engine and confirming actual provider usage.")
		return 0
	}
	cmd := args[0]
	if cmd != "status" && cmd != "reconcile" {
		fmt.Fprintf(os.Stderr, "budget: unknown command %q\n", cmd)
		return 2
	}
	path, id := "config.yaml", ""
	tokens, cost := 0, 0.0
	hasTokens, hasCost, jsonOut := false, false, false
	for i := 1; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--json":
			jsonOut = true
		case "--config", "--tokens", "--cost":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "budget: %s requires a value\n", arg)
				return 2
			}
			i++
			value := args[i]
			switch arg {
			case "--config":
				path = value
			case "--tokens":
				n, err := strconv.Atoi(value)
				if err != nil || n < 0 {
					fmt.Fprintln(os.Stderr, "budget: tokens must be a nonnegative integer")
					return 2
				}
				tokens, hasTokens = n, true
			case "--cost":
				n, err := strconv.ParseFloat(value, 64)
				if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
					fmt.Fprintln(os.Stderr, "budget: cost must be a finite nonnegative amount")
					return 2
				}
				cost, hasCost = n, true
			}
		default:
			if cmd != "reconcile" || id != "" || strings.HasPrefix(arg, "-") || !validActionID(arg) {
				fmt.Fprintf(os.Stderr, "budget: unexpected argument %q\n", arg)
				return 2
			}
			id = arg
		}
	}
	if cmd == "status" && (hasTokens || hasCost) {
		fmt.Fprintln(os.Stderr, "budget: usage flags require reconcile")
		return 2
	}
	if cmd == "reconcile" && (id == "" || !hasTokens || !hasCost || jsonOut) {
		fmt.Fprintln(os.Stderr, "budget: reconcile requires one call-id, --tokens and --cost")
		return 2
	}
	if cmd == "status" {
		st, closeStore, code := openStateForCmd(path)
		if code != 0 {
			return code
		}
		defer closeStore()
		return printBudgetStatus(st, jsonOut, pipelineCapsFromConfigFile(path))
	}
	// This administrative write requires an existing regular database. It must
	// never turn a misspelled path into a fresh store with an empty spend ledger.
	statePath := statePathFromConfigFile(path)
	info, err := os.Stat(statePath)
	if err != nil || !info.Mode().IsRegular() {
		fmt.Fprintln(os.Stderr, "budget: existing state database required")
		return 1
	}
	st, err := statestore.OpenStateStore(statePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "budget: open state: %v\n", err)
		return 1
	}
	defer func() { _ = st.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := st.SettleBudgetCall(ctx, id, tokens, cost); err != nil {
		fmt.Fprintf(os.Stderr, "budget: reconcile: %v\n", err)
		return 1
	}
	fmt.Printf("Reconciled %s: %d tokens, %.6f cost\n", id, tokens, cost)
	return 0
}

// pipelineDayCap is one capped pipeline's settled usage today against its own
// daily caps (0 = that cap is off).
type pipelineDayCap struct {
	Name         string  `json:"name"`
	Calls        int     `json:"calls"`
	Tokens       int     `json:"tokens"`
	PerDayTokens int     `json:"per_day_tokens"`
	Cost         float64 `json:"cost"`
	PerDayCost   float64 `json:"per_day_cost"`
	Reached      bool    `json:"reached"`
}

// pipelineCapsFromConfigFile lists the pipelines with a daily budget in config
// order. An unreadable or unparsable config shows no pipeline caps.
func pipelineCapsFromConfigFile(configPath string) []config.PipelineCap {
	var cfg config.Config
	// #nosec G304 -- configPath is the operator's own --config argument.
	data, err := os.ReadFile(configPath)
	if err != nil || yaml.Unmarshal(data, &cfg) != nil {
		return nil
	}
	return cfg.CappedPipelines()
}

func pipelineDayCaps(caps []config.PipelineCap, byPipeline []statestore.UsageShare) []pipelineDayCap {
	used := map[string]statestore.UsageShare{}
	for _, u := range byPipeline {
		used[u.Name] = u
	}
	var out []pipelineDayCap
	for _, c := range caps {
		u := used[c.Name]
		out = append(out, pipelineDayCap{
			Name: c.Name, Calls: u.Calls, Tokens: u.Tokens, PerDayTokens: c.Budget.PerDayTokens,
			Cost: u.Cost, PerDayCost: c.Budget.PerDayCost,
			Reached: (c.Budget.PerDayTokens > 0 && u.Tokens >= c.Budget.PerDayTokens) || (c.Budget.PerDayCost > 0 && u.Cost >= c.Budget.PerDayCost),
		})
	}
	return out
}

func printBudgetStatus(st *statestore.StateStore, jsonOut bool, caps []config.PipelineCap) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := time.Now().UTC()
	day, err := st.BudgetDay(ctx, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "budget: read usage: %v\n", err)
		return 1
	}
	pending, err := st.PendingBudgetCalls(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "budget: read pending calls: %v\n", err)
		return 1
	}
	byModel, err := st.BudgetUsageBy(ctx, now, "model")
	if err != nil {
		fmt.Fprintf(os.Stderr, "budget: read usage by model: %v\n", err)
		return 1
	}
	byPipeline, err := st.BudgetUsageBy(ctx, now, "pipeline")
	if err != nil {
		fmt.Fprintf(os.Stderr, "budget: read usage by pipeline: %v\n", err)
		return 1
	}
	capped := pipelineDayCaps(caps, byPipeline)
	if jsonOut {
		report := struct {
			Day         string                         `json:"day"`
			Tokens      int                            `json:"tokens"`
			Cost        float64                        `json:"cost"`
			Calls       int                            `json:"calls"`
			CallMinutes int                            `json:"call_minutes"`
			Pending     []statestore.PendingBudgetCall `json:"pending_calls"`
			ByModel     []statestore.UsageShare        `json:"by_model"`
			ByPipeline  []statestore.UsageShare        `json:"by_pipeline"`
			PipelineCap []pipelineDayCap               `json:"pipeline_caps,omitempty"`
		}{statestore.UTCDay(now), day.Tokens, day.Cost, day.Calls, day.CallMinutes, pending, nonNilShares(byModel), nonNilShares(byPipeline), capped}
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			fmt.Fprintf(os.Stderr, "budget: output: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Printf("%s UTC: %d tokens, %.6f cost, %d voice calls, %d call minutes\n", statestore.UTCDay(now), day.Tokens, day.Cost, day.Calls, day.CallMinutes)
	printUsageShares("model", byModel)
	printUsageShares("pipeline", byPipeline)
	printPipelineCaps(capped)
	for _, call := range pending {
		fmt.Printf("Unsettled provider call %s (day %s, started %s)\n", call.CallID, call.Day, call.CreatedAt.Format(time.RFC3339))
	}
	if len(pending) > 0 {
		fmt.Println("Stop the engine, confirm provider usage, then use budget reconcile before resuming.")
	}
	return 0
}

func nonNilShares(s []statestore.UsageShare) []statestore.UsageShare {
	if s == nil {
		return []statestore.UsageShare{}
	}
	return s
}

// printUsageShares lists today's settled model calls split by model or
// pipeline. Calls settled before attribution was recorded show as unattributed.
func printUsageShares(by string, shares []statestore.UsageShare) {
	if len(shares) == 0 {
		return
	}
	fmt.Printf("By %s:\n", by)
	for _, u := range shares {
		name := u.Name
		if name == "" {
			name = "(unattributed)"
		}
		fmt.Printf("  %-40s %4d call(s) %9d tokens %12.6f cost\n", name, u.Calls, u.Tokens, u.Cost)
	}
}

// printPipelineCaps lists each capped pipeline's usage today against its caps.
func printPipelineCaps(caps []pipelineDayCap) {
	if len(caps) == 0 {
		return
	}
	fmt.Println("Pipeline daily caps:")
	for _, c := range caps {
		tokens, cost := "no cap", "no cap"
		if c.PerDayTokens > 0 {
			tokens = strconv.Itoa(c.PerDayTokens)
		}
		if c.PerDayCost > 0 {
			cost = strconv.FormatFloat(c.PerDayCost, 'f', 6, 64)
		}
		line := fmt.Sprintf("  %-40s %9d of %s tokens %12.6f of %s cost", c.Name, c.Tokens, tokens, c.Cost, cost)
		if c.Reached {
			line += "  REACHED"
		}
		fmt.Println(line)
	}
}
