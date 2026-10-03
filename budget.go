package main

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

// BudgetTracker shares the daily ledger and admission gate, while each run owns
// its pipeline totals. Cost caps remain stop thresholds checked between calls.
type BudgetTracker struct {
	mu                 sync.Mutex
	gateOnce           sync.Once
	gate               chan struct{}
	parent             *BudgetTracker
	store              *statestore.StateStore
	stateErr           error
	unsettled          int
	tokensUsedToday    int
	tokensUsedPipeline int
	callsToday         int
	callMinutesToday   int
	costToday          float64
	costPipeline       float64
	dayStart           time.Time
	dayCostLimit       float64
	pipelineCostLimit  float64
	pipelineTokenLimit int
}

func (b *BudgetTracker) root() *BudgetTracker {
	if b.parent != nil {
		return b.parent.root()
	}
	return b
}

func (b *BudgetTracker) newRun(tokenLimit int) *BudgetTracker {
	r := b.root()
	r.mu.Lock()
	defer r.mu.Unlock()
	return &BudgetTracker{parent: r, dayCostLimit: r.dayCostLimit, pipelineCostLimit: r.pipelineCostLimit, pipelineTokenLimit: tokenLimit}
}

func (b *BudgetTracker) attachStore(store *statestore.StateStore) error {
	r := b.root()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.store = store
	r.refreshLocked(time.Now())
	return r.stateErr
}

func (b *BudgetTracker) refreshLocked(now time.Time) {
	r := b.root()
	now = now.UTC()
	if r.dayStart.UTC().Format("2006-01-02") != now.Format("2006-01-02") {
		r.tokensUsedToday = 0
		r.costToday = 0
		r.callsToday = 0
		r.callMinutesToday = 0
		r.dayStart = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	}
	if r.store != nil {
		d, err := r.store.BudgetDay(context.Background(), now)
		if err != nil {
			r.stateErr = err
			return
		}
		r.tokensUsedToday = d.Tokens
		r.costToday = d.Cost
		r.callsToday = d.Calls
		r.callMinutesToday = d.CallMinutes
		r.unsettled = d.Unsettled
	}
}

func (b *BudgetTracker) resetIfNewDay() {
	r := b.root()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked(time.Now())
}

func (b *BudgetTracker) checkLocked(dayLimit, requested int, dayCost, pipelineCost float64) error {
	r := b.root()
	if r.stateErr != nil {
		return fmt.Errorf("BUDGET_BLOCKED: usage ledger unavailable: %w", r.stateErr)
	}
	if r.unsettled > 0 {
		return fmt.Errorf("BUDGET_BLOCKED: unresolved provider usage; reconcile pending calls before retrying")
	}
	if requested < 0 || r.tokensUsedToday < 0 || b.tokensUsedPipeline < 0 || !validCost(r.costToday) || !validCost(b.costPipeline) || !validCost(dayCost) || !validCost(pipelineCost) {
		return fmt.Errorf("BUDGET_BLOCKED: invalid usage or budget limits")
	}
	if dayLimit > 0 && requested > dayLimit-r.tokensUsedToday {
		return fmt.Errorf("BUDGET_BLOCKED: daily token limit %d would be exceeded (used: %d, requested: %d)", dayLimit, r.tokensUsedToday, requested)
	}
	if b.pipelineTokenLimit > 0 && requested > b.pipelineTokenLimit-b.tokensUsedPipeline {
		return fmt.Errorf("BUDGET_BLOCKED: per-pipeline token limit %d would be exceeded (used: %d, requested: %d)", b.pipelineTokenLimit, b.tokensUsedPipeline, requested)
	}
	if dayCost > 0 && r.costToday >= dayCost {
		return fmt.Errorf("BUDGET_BLOCKED: daily cost limit %.4f reached (spent: %.4f)", dayCost, r.costToday)
	}
	if pipelineCost > 0 && b.costPipeline >= pipelineCost {
		return fmt.Errorf("BUDGET_BLOCKED: per-pipeline cost limit %.4f reached (spent: %.4f)", pipelineCost, b.costPipeline)
	}
	return nil
}

func validCost(cost float64) bool { return cost >= 0 && !math.IsNaN(cost) && !math.IsInf(cost, 0) }

func (b *BudgetTracker) check(limit, requested int) error {
	r := b.root()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked(time.Now())
	pipelineCost := 0.0
	if b.parent != nil {
		pipelineCost = b.pipelineCostLimit
	}
	return b.checkLocked(limit, requested, r.dayCostLimit, pipelineCost)
}

func (b *BudgetTracker) CheckCost(dayLimit, pipelineLimit float64) error {
	r := b.root()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked(time.Now())
	return b.checkLocked(0, 0, dayLimit, pipelineLimit)
}

func (b *BudgetTracker) recordUsageLocked(tokens int, cost float64) {
	r := b.root()
	if tokens < 0 || !validCost(cost) || tokens > int(^uint(0)>>1)-r.tokensUsedToday || tokens > int(^uint(0)>>1)-b.tokensUsedPipeline || !validCost(r.costToday+cost) || !validCost(b.costPipeline+cost) {
		r.stateErr = fmt.Errorf("invalid provider usage")
		return
	}
	r.tokensUsedToday += tokens
	r.costToday += cost
	b.tokensUsedPipeline += tokens
	b.costPipeline += cost
}

func (b *BudgetTracker) record(tokens int) {
	r := b.root()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked(time.Now())
	b.recordUsageLocked(tokens, 0)
	if r.stateErr == nil && r.store != nil {
		r.stateErr = r.store.AddBudgetUsage(context.Background(), time.Now(), tokens, 0, 0, 0)
	}
}

func (b *BudgetTracker) RecordCost(cost float64) {
	r := b.root()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked(time.Now())
	b.recordUsageLocked(0, cost)
	if r.stateErr == nil && r.store != nil {
		r.stateErr = r.store.AddBudgetUsage(context.Background(), time.Now(), 0, cost, 0, 0)
	}
}

func (b *BudgetTracker) CheckCalls(limit int) error {
	r := b.root()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked(time.Now())
	if err := b.checkLocked(0, 0, 0, 0); err != nil {
		return err
	}
	if limit > 0 && r.callsToday >= limit {
		return fmt.Errorf("BUDGET_BLOCKED: daily call limit %d would be exceeded (used: %d)", limit, r.callsToday)
	}
	return nil
}

func (b *BudgetTracker) CheckCallMinutes(limit, requested int) error {
	r := b.root()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked(time.Now())
	if err := b.checkLocked(0, 0, 0, 0); err != nil {
		return err
	}
	if requested < 0 {
		return fmt.Errorf("BUDGET_BLOCKED: invalid call minutes")
	}
	if limit > 0 && requested > limit-r.callMinutesToday {
		return fmt.Errorf("BUDGET_BLOCKED: daily call-minute limit %d would be exceeded (used: %d, requested: %d)", limit, r.callMinutesToday, requested)
	}
	return nil
}

func (b *BudgetTracker) RecordCall(minutes int) {
	r := b.root()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked(time.Now())
	if minutes < 0 {
		r.stateErr = fmt.Errorf("invalid call duration")
		return
	}
	r.callsToday++
	r.callMinutesToday += minutes
	if r.store != nil {
		r.stateErr = r.store.AddBudgetUsage(context.Background(), time.Now(), 0, 0, 1, minutes)
	}
}

func (b *BudgetTracker) snapshot() *BudgetTracker { return b.snapshotAt(time.Now()) }

func (b *BudgetTracker) snapshotAt(now time.Time) *BudgetTracker {
	r := b.root()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked(now)
	return &BudgetTracker{tokensUsedToday: r.tokensUsedToday, tokensUsedPipeline: b.tokensUsedPipeline, costToday: r.costToday, costPipeline: b.costPipeline, callsToday: r.callsToday, callMinutesToday: r.callMinutesToday, dayStart: r.dayStart, dayCostLimit: r.dayCostLimit, pipelineCostLimit: b.pipelineCostLimit, stateErr: r.stateErr, unsettled: r.unsettled}
}

func (b *BudgetTracker) acquire(ctx context.Context) error {
	r := b.root()
	r.gateOnce.Do(func() { r.gate = make(chan struct{}, 1) })
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case r.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (b *BudgetTracker) release() { <-b.root().gate }

func (b *BudgetTracker) admit(ctx context.Context, cfg *config.Config, requested int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	r := b.root()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked(time.Now())
	r.dayCostLimit = cfg.Budgets.PerDayCost
	r.pipelineCostLimit = cfg.Budgets.PerPipelineCost
	pipelineCost := 0.0
	if b.parent != nil {
		b.pipelineCostLimit = cfg.Budgets.PerPipelineCost
		pipelineCost = b.pipelineCostLimit
	}
	if err := b.checkLocked(cfg.Budgets.PerDayTokens, requested, r.dayCostLimit, pipelineCost); err != nil {
		return "", err
	}
	id := newRunID(time.Now())
	if r.store != nil {
		if err := r.store.BeginBudgetCall(ctx, id, time.Now(), requested, cfg.Budgets.PerDayTokens, r.dayCostLimit); err != nil {
			return "", err
		}
	}
	r.unsettled++
	return id, nil
}

func (b *BudgetTracker) finish(id string, resp *CompletionResponse, callErr error, cfg *config.Config) error {
	r := b.root()
	r.mu.Lock()
	defer r.mu.Unlock()
	settleCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if resp != nil {
		tokens := resp.InputTokens + resp.OutputTokens
		if resp.InputTokens < 0 || resp.OutputTokens < 0 || tokens < 0 || !validCost(resp.CostUSD) {
			return fmt.Errorf("BUDGET_BLOCKED: invalid provider usage; reconciliation required")
		}
		if r.store != nil {
			if err := r.store.SettleBudgetCall(settleCtx, id, tokens, resp.CostUSD); err != nil {
				r.stateErr = err
				return fmt.Errorf("BUDGET_BLOCKED: could not settle provider usage: %w", err)
			}
		}
		b.recordUsageLocked(tokens, resp.CostUSD)
		r.unsettled--
		if r.stateErr != nil {
			return fmt.Errorf("BUDGET_BLOCKED: %w", r.stateErr)
		}
		if cfg.Budgets.PerDayTokens > 0 && r.tokensUsedToday > cfg.Budgets.PerDayTokens {
			return fmt.Errorf("BUDGET_BLOCKED: response exceeded daily token limit %d (used: %d)", cfg.Budgets.PerDayTokens, r.tokensUsedToday)
		}
		if b.pipelineTokenLimit > 0 && b.tokensUsedPipeline > b.pipelineTokenLimit {
			return fmt.Errorf("BUDGET_BLOCKED: response exceeded per-pipeline token limit %d (used: %d)", b.pipelineTokenLimit, b.tokensUsedPipeline)
		}
		return nil
	}
	if !uncertainProviderError(callErr) {
		if r.store != nil {
			if err := r.store.ReleaseBudgetCall(settleCtx, id); err != nil {
				r.stateErr = err
				return fmt.Errorf("BUDGET_BLOCKED: could not release rejected request: %w", err)
			}
		}
		r.unsettled--
	}
	return nil
}
