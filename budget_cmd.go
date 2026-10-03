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

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
	"gopkg.in/yaml.v3"
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
		return printBudgetStatus(st, jsonOut)
	}
	// This administrative write requires an existing regular database. It must
	// never turn a misspelled path into a fresh store with an empty spend ledger.
	statePath := strings.TrimSpace(os.Getenv("DRAFTCAT_STATE_PATH"))
	if statePath == "" {
		// #nosec G304 -- the local operator supplies their config path.
		if data, err := os.ReadFile(path); err == nil {
			var cfg config.Config
			if yaml.Unmarshal(data, &cfg) == nil {
				statePath = cfg.State.Path
			}
		}
	}
	if statePath == "" {
		statePath = "./state.db"
	}
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

func printBudgetStatus(st *statestore.StateStore, jsonOut bool) int {
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
	if jsonOut {
		report := struct {
			Day         string                         `json:"day"`
			Tokens      int                            `json:"tokens"`
			Cost        float64                        `json:"cost"`
			Calls       int                            `json:"calls"`
			CallMinutes int                            `json:"call_minutes"`
			Pending     []statestore.PendingBudgetCall `json:"pending_calls"`
		}{now.Format("2006-01-02"), day.Tokens, day.Cost, day.Calls, day.CallMinutes, pending}
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			fmt.Fprintf(os.Stderr, "budget: output: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Printf("%s UTC: %d tokens, %.6f cost, %d voice calls, %d call minutes\n", now.Format("2006-01-02"), day.Tokens, day.Cost, day.Calls, day.CallMinutes)
	for _, call := range pending {
		fmt.Printf("Unsettled provider call %s (day %s, started %s)\n", call.CallID, call.Day, call.CreatedAt.Format(time.RFC3339))
	}
	if len(pending) > 0 {
		fmt.Println("Stop the engine, confirm provider usage, then use budget reconcile before resuming.")
	}
	return 0
}
