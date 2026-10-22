package main

import (
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

// budgetAlerts notifies the operator when today's usage crosses a configured
// fraction of a daily cap (budgets.alert_at), globally or for a pipeline with
// its own budget. Each threshold fires once per UTC day and cap; the mark is
// kept in the state store so a restart does not repeat it.
type budgetAlerts struct {
	thresholds []float64 // ascending, each in (0, 1)
	tokenCap   int
	costCap    float64
	pipelines  map[string]config.PipelineBudgetConfig // capped pipelines only
	notify     func(string)
	day        string
	fired      map[string]bool
}

// configureAlerts enables threshold alerts on the shared daily ledger.
// notify is called outside the budget lock.
func (b *BudgetTracker) configureAlerts(cfg *config.Config, notify func(string)) {
	var th []float64
	for _, f := range cfg.Budgets.AlertAt {
		if f > 0 && f < 1 {
			th = append(th, f)
		}
	}
	pipelines := map[string]config.PipelineBudgetConfig{}
	for _, p := range cfg.CappedPipelines() {
		pipelines[p.Name] = p.Budget
	}
	r := b.root()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(th) == 0 || notify == nil || (cfg.Budgets.PerDayTokens <= 0 && cfg.Budgets.PerDayCost <= 0 && len(pipelines) == 0) {
		r.alerts = nil
		return
	}
	sort.Float64s(th)
	r.alerts = &budgetAlerts{
		thresholds: th,
		tokenCap:   cfg.Budgets.PerDayTokens,
		costCap:    cfg.Budgets.PerDayCost,
		pipelines:  pipelines,
		notify:     notify,
		fired:      map[string]bool{},
	}
}

func (a *budgetAlerts) dayLocked(r *BudgetTracker) string {
	day := statestore.UTCDay(r.dayStart)
	if day != a.day {
		a.day = day
		a.fired = map[string]bool{}
	}
	return day
}

// observeLocked runs with the root budget lock held, after usage changed.
func (a *budgetAlerts) observeLocked(r *BudgetTracker) {
	day := a.dayLocked(r)
	if a.tokenCap > 0 {
		used := float64(r.tokensUsedToday) / float64(a.tokenCap)
		if f, ok := a.crossLocked(r, day, "tokens", used); ok {
			a.send(fmt.Sprintf("[budget] %d%% of today's token cap used: %d of %d (UTC day %s).", pct(f), r.tokensUsedToday, a.tokenCap, day))
		}
	}
	if a.costCap > 0 {
		used := r.costToday / a.costCap
		if f, ok := a.crossLocked(r, day, "cost", used); ok {
			a.send(fmt.Sprintf("[budget] %d%% of today's cost cap used: %.4f of %.4f (UTC day %s).", pct(f), r.costToday, a.costCap, day))
		}
	}
}

// observePipelineLocked runs with the root budget lock held, after a call
// settled for a pipeline; used is that pipeline's total for the day.
func (a *budgetAlerts) observePipelineLocked(r *BudgetTracker, used statestore.UsageShare) {
	caps, ok := a.pipelines[used.Name]
	if !ok {
		return
	}
	day := a.dayLocked(r)
	if caps.PerDayTokens > 0 {
		if f, ok := a.crossLocked(r, day, "pipeline:"+used.Name+":tokens", float64(used.Tokens)/float64(caps.PerDayTokens)); ok {
			a.send(fmt.Sprintf("[budget] %d%% of the daily token cap of pipeline %s used: %d of %d (UTC day %s).", pct(f), used.Name, used.Tokens, caps.PerDayTokens, day))
		}
	}
	if caps.PerDayCost > 0 {
		if f, ok := a.crossLocked(r, day, "pipeline:"+used.Name+":cost", used.Cost/caps.PerDayCost); ok {
			a.send(fmt.Sprintf("[budget] %d%% of the daily cost cap of pipeline %s used: %.4f of %.4f (UTC day %s).", pct(f), used.Name, used.Cost, caps.PerDayCost, day))
		}
	}
}

// crossLocked marks every threshold at or below used that has not fired today
// and returns the highest one newly crossed.
func (a *budgetAlerts) crossLocked(r *BudgetTracker, day, kind string, used float64) (float64, bool) {
	var top float64
	crossed := false
	for _, f := range a.thresholds {
		if used < f {
			break
		}
		key := fmt.Sprintf("%s:%s:%g", day, kind, f)
		if a.fired[key] {
			continue
		}
		a.fired[key] = true
		if r.store != nil {
			first, err := r.store.TryMarkSeen("_budget", "alert", key, time.Now())
			if err != nil {
				log.Printf("[budget] alert mark %s: %v", key, err)
			} else if !first {
				continue // already sent before a restart
			}
		}
		top, crossed = f, true
	}
	return top, crossed
}

func (a *budgetAlerts) send(msg string) {
	notify := a.notify
	go notify(msg)
}

func pct(f float64) int { return int(f*100 + 0.5) }
