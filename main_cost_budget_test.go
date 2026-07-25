package main

import (
	"strings"
	"testing"
	"time"
)

// Cost caps answer the question a business owner actually asks — "what is this
// costing me today" — which token caps never did. The per-call cost was already
// being computed and discarded; these tests pin that it now accumulates and
// blocks.

func TestCheckCost_UnderCapAllows(t *testing.T) {
	b := &BudgetTracker{dayStart: time.Now(), costToday: 0.5}
	if err := b.CheckCost(2.0, 0); err != nil {
		t.Fatalf("spend 0.5 under cap 2.0 should pass, got %v", err)
	}
}

func TestCheckCost_AtCapBlocks(t *testing.T) {
	// Reaching the cap blocks the NEXT call — enforcement is between calls.
	b := &BudgetTracker{dayStart: time.Now(), costToday: 2.0}
	err := b.CheckCost(2.0, 0)
	if err == nil {
		t.Fatal("spend equal to the cap must block the next call")
	}
	if !strings.Contains(err.Error(), "BUDGET_BLOCKED") {
		t.Errorf("error %q should carry the BUDGET_BLOCKED marker used by the other caps", err)
	}
}

func TestCheckCost_PipelineCapIsIndependentOfDailyCap(t *testing.T) {
	b := &BudgetTracker{dayStart: time.Now(), costToday: 0.1, costPipeline: 5.0}
	if err := b.CheckCost(100.0, 5.0); err == nil {
		t.Fatal("per-pipeline cap should block even when the daily cap has room")
	}
}

func TestCheckCost_ZeroLimitMeansNoCap(t *testing.T) {
	// Existing configs have no cost keys at all; they must be unaffected.
	b := &BudgetTracker{dayStart: time.Now(), costToday: 9999, costPipeline: 9999}
	if err := b.CheckCost(0, 0); err != nil {
		t.Fatalf("zero limits mean no cap, got %v", err)
	}
}

func TestRecordCost_Accumulates(t *testing.T) {
	b := &BudgetTracker{dayStart: time.Now()}
	b.RecordCost(0.25)
	b.RecordCost(0.75)
	if b.costToday != 1.0 {
		t.Errorf("costToday = %v, want 1.0", b.costToday)
	}
	if b.costPipeline != 1.0 {
		t.Errorf("costPipeline = %v, want 1.0", b.costPipeline)
	}
}

// The token gate is called before every LLM call; the money gate rides it so a
// call site added later cannot spend uncapped by forgetting to check.
func TestCheck_EnforcesCostCapAlongsideTokens(t *testing.T) {
	b := &BudgetTracker{
		dayStart:     time.Now(),
		costToday:    3.0,
		dayCostLimit: 1.0,
	}
	// Plenty of token headroom, but the money cap is blown.
	err := b.check(1_000_000, 10)
	if err == nil {
		t.Fatal("check() must enforce the cost cap, not just tokens")
	}
	if !strings.Contains(err.Error(), "cost limit") {
		t.Errorf("error %q should name the cost limit", err)
	}
}

func TestCheck_TokenCapStillEnforcedWithNoCostCap(t *testing.T) {
	b := &BudgetTracker{dayStart: time.Now(), tokensUsedToday: 900}
	if err := b.check(1000, 200); err == nil {
		t.Fatal("token cap must still block when no cost cap is configured")
	}
}

// A new day resets spend, matching how token and call counters already behave.
func TestResetIfNewDay_ClearsCostToday(t *testing.T) {
	b := &BudgetTracker{
		dayStart:  time.Now().AddDate(0, 0, -1),
		costToday: 42.0,
	}
	b.resetIfNewDay()
	if b.costToday != 0 {
		t.Errorf("costToday = %v after day rollover, want 0", b.costToday)
	}
}
