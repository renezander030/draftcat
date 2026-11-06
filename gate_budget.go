package main

// GET /gate/budget: how much of today's budget is left, for a harness about to
// spend it. Read-only, behind the same tokens as the tool gate, and scoped to
// the caller's tools.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

const toolGateBudgetPath = "/gate/budget"

// gateBudget is the body of GET /gate/budget. Field names follow
// `draftcat budget status --json` where the two overlap.
type gateBudget struct {
	Day          string  `json:"day"`
	Tokens       int     `json:"tokens"`
	PerDayTokens int     `json:"per_day_tokens"` // 0 = no cap
	Cost         float64 `json:"cost"`
	PerDayCost   float64 `json:"per_day_cost"` // 0 = no cap
	// ReservedCost is what pending calls hold against per_day_cost; present
	// only with budgets.reserve_max_cost.
	ReservedCost *float64         `json:"reserved_cost,omitempty"`
	PipelineCaps []pipelineDayCap `json:"pipeline_caps,omitempty"`
	Frozen       bool             `json:"frozen"`
	Tools        []gateToolBudget `json:"tools"`
}

// gateToolBudget is one capped tool's running totals today. They are the
// tool's own totals, the ones its caps are checked against.
type gateToolBudget struct {
	Tool           string          `json:"tool"`
	Calls          *int            `json:"calls,omitempty"`
	MaxCallsPerDay *int            `json:"max_calls_per_day,omitempty"`
	Args           []gateArgBudget `json:"args,omitempty"`
}

type gateArgBudget struct {
	Arg       string      `json:"arg"`
	Total     json.Number `json:"total"` // exact decimal
	MaxPerDay float64     `json:"max_per_day"`
}

// HandleBudget serves GET /gate/budget. A store that cannot be read answers
// 503 rather than numbers that may be wrong.
func (g *toolGate) HandleBudget(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if state == nil {
		http.Error(w, "budget unavailable", http.StatusServiceUnavailable)
		return
	}
	body, err := g.budget(r.Context(), gateCallerFrom(r.Context()))
	if err != nil {
		log.Printf("[tool-gate] budget read failed: %v", err)
		http.Error(w, "budget unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// budget reads today's usage against the live budgets and, for each capped
// tool the caller may call (every tool for the shared token), its totals.
func (g *toolGate) budget(parent context.Context, caller *config.GateCaller) (gateBudget, error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	now := g.now().UTC()
	run := currentConfig(g.cfg)
	b := gateBudget{
		Day: statestore.UTCDay(now), PerDayTokens: run.Budgets.PerDayTokens, PerDayCost: run.Budgets.PerDayCost,
		Tools: []gateToolBudget{},
	}
	day, err := state.BudgetDay(ctx, now)
	if err != nil {
		return b, fmt.Errorf("usage: %w", err)
	}
	b.Tokens, b.Cost = day.Tokens, day.Cost
	byPipeline, err := state.BudgetUsageBy(ctx, now, "pipeline")
	if err != nil {
		return b, fmt.Errorf("usage by pipeline: %w", err)
	}
	b.PipelineCaps = pipelineDayCaps(run.CappedPipelines(), byPipeline)
	if run.Budgets.ReserveMaxCost {
		reserved, err := state.ReservedBudgetCost(ctx)
		if err != nil {
			return b, fmt.Errorf("reserved cost: %w", err)
		}
		b.ReservedCost = &reserved
	}
	f, err := state.GateFreeze()
	if err != nil {
		return b, fmt.Errorf("freeze: %w", err)
	}
	b.Frozen = f.Frozen
	for _, rule := range g.cfg.ToolGate.Tools {
		if !rule.Capped() || (caller != nil && !caller.Allows(rule.Name)) {
			continue
		}
		t := gateToolBudget{Tool: rule.Name, MaxCallsPerDay: rule.MaxCallsPerDay}
		if rule.MaxCallsPerDay != nil {
			n, err := state.ToolArgTotal(rule.Name, config.CallCountKey, b.Day)
			if err != nil {
				return b, fmt.Errorf("calls of %s: %w", rule.Name, err)
			}
			calls := int(n.Num().Int64())
			t.Calls = &calls
		}
		for _, arg := range rule.DailyCaps() {
			total, err := state.ToolArgTotal(rule.Name, arg, b.Day)
			if err != nil {
				return b, fmt.Errorf("total of %s.%s: %w", rule.Name, arg, err)
			}
			t.Args = append(t.Args, gateArgBudget{Arg: arg, Total: json.Number(config.DecimalString(total)), MaxPerDay: *rule.Args[arg].MaxPerDay})
		}
		b.Tools = append(b.Tools, t)
	}
	return b, nil
}
