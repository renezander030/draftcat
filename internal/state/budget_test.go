package state

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBudgetDurableUsageAndUTCFullDates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.db")
	ctx := context.Background()
	st, err := OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 30, 23, 59, 0, 0, time.UTC)
	if err := st.BeginBudgetCall(ctx, "paid", at, 10, 100, .5, UsageLabel{}); err != nil {
		t.Fatal(err)
	}
	if err := st.SettleBudgetCall(ctx, "paid", 20, .6); err != nil {
		t.Fatal(err)
	}
	if err := st.SettleBudgetCall(ctx, "paid", 20, .6); err == nil {
		t.Fatal("same call charged twice")
	}
	_ = st.Close()
	st, err = OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	day, err := st.BudgetDay(ctx, at)
	if err != nil || day.Tokens != 20 || day.Cost != .6 || day.Unsettled != 0 {
		t.Fatalf("durable usage=%+v err=%v", day, err)
	}
	if err := st.BeginBudgetCall(ctx, "same-day", at, 1, 100, .5, UsageLabel{}); err == nil {
		t.Fatal("restart reopened settled cost cap")
	}
	next := at.Add(2 * time.Minute)
	if err := st.BeginBudgetCall(ctx, "next-day", next, 1, 100, .5, UsageLabel{}); err != nil {
		t.Fatal(err)
	}
	if err := st.SettleBudgetCall(ctx, "next-day", 2, .1); err != nil {
		t.Fatal(err)
	}
	for date, want := range map[time.Time]int{at: 20, next: 2, at.AddDate(1, 0, 0): 0, at.AddDate(0, 1, 0): 0} {
		d, err := st.BudgetDay(ctx, date)
		if err != nil || d.Tokens != want {
			t.Fatalf("date=%s usage=%+v err=%v", date, d, err)
		}
	}
	local := time.Date(2026, 10, 1, 1, 59, 0, 0, time.FixedZone("plus-two", 2*3600))
	d, err := st.BudgetDay(ctx, local)
	if err != nil || d.Tokens != 20 {
		t.Fatalf("local time was not grouped by UTC date: %+v %v", d, err)
	}
}

func TestBudgetPendingCallSurvivesRestartAndBlocksUntilReconciled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.db")
	ctx := context.Background()
	at := time.Now()
	st, err := OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.BeginBudgetCall(ctx, "unknown", at, 1, 0, 0, UsageLabel{}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	st, err = OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.BeginBudgetCall(ctx, "next", at.AddDate(0, 0, 1), 1, 0, 0, UsageLabel{}); err == nil {
		t.Fatal("pending usage was forgotten on restart/new day")
	}
	if err := st.SettleBudgetCall(ctx, "unknown", 12, .2); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginBudgetCall(ctx, "next", at, 1, 100, .5, UsageLabel{}); err != nil {
		t.Fatal(err)
	}
}

func TestBudgetAdmissionIsAtomicAcrossStoreHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.db")
	ctx := context.Background()
	a, err := OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var wg sync.WaitGroup
	var admitted atomic.Int32
	for id, st := range map[string]*StateStore{"a": a, "b": b} {
		wg.Add(1)
		go func(id string, st *StateStore) {
			defer wg.Done()
			if st.BeginBudgetCall(ctx, id, time.Now(), 1, 100, 1, UsageLabel{}) == nil {
				admitted.Add(1)
			}
		}(id, st)
	}
	wg.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("admitted=%d, want exactly one outstanding provider call", admitted.Load())
	}
}

func TestBudgetRejectsInvalidAndOverflowUsageWithoutReleasingPending(t *testing.T) {
	st := newTempStore(t)
	ctx := context.Background()
	at := time.Now()
	if err := st.BeginBudgetCall(ctx, "pending", at, 1, 0, 0, UsageLabel{}); err != nil {
		t.Fatal(err)
	}
	for _, cost := range []float64{-1, math.NaN(), math.Inf(1)} {
		if err := st.SettleBudgetCall(ctx, "pending", 1, cost); err == nil {
			t.Fatalf("invalid cost %v settled", cost)
		}
	}
	if err := st.SettleBudgetCall(ctx, "pending", -1, 0); err == nil {
		t.Fatal("negative tokens settled")
	}
	if err := st.AddBudgetUsage(ctx, at, int(^uint(0)>>1), 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.SettleBudgetCall(ctx, "pending", 1, 0); err == nil {
		t.Fatal("token arithmetic overflow accepted")
	}
	day, err := st.BudgetDay(ctx, at)
	if err != nil || day.Unsettled != 1 || day.Tokens != int(^uint(0)>>1) {
		t.Fatalf("invalid accounting changed ledger: %+v %v", day, err)
	}
}

// A pipeline's daily caps are checked in the admission insert against the
// calls settled for that pipeline on the same UTC date, across store handles.
func TestBudgetPipelineDailyCapAdmission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.db")
	ctx := context.Background()
	a, err := OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	capped := UsageLabel{Pipeline: "replies", PipelineDayTokens: 50, PipelineDayCost: 1}
	if err := a.BeginBudgetCall(ctx, "r1", at, 10, 0, 0, capped); err != nil {
		t.Fatal(err)
	}
	if err := a.SettleBudgetCall(ctx, "r1", 45, .4); err != nil {
		t.Fatal(err)
	}
	if err := b.BeginBudgetCall(ctx, "r2", at, 10, 0, 0, capped); err == nil {
		t.Fatal("token cap: 45 settled + 10 requested admitted under 50")
	}
	if err := b.BeginBudgetCall(ctx, "r3", at, 5, 0, 0, capped); err != nil {
		t.Fatalf("call within the token cap refused: %v", err)
	}
	if err := b.SettleBudgetCall(ctx, "r3", 0, .6); err != nil {
		t.Fatal(err)
	}
	if err := a.BeginBudgetCall(ctx, "r4", at, 0, 0, 0, capped); err == nil {
		t.Fatal("cost cap: settled 1.0 admitted at cap 1")
	}
	if err := a.BeginBudgetCall(ctx, "r5", at.AddDate(0, 0, 1), 10, 0, 0, capped); err != nil {
		t.Fatalf("next UTC day still capped: %v", err)
	}
	if err := a.ReleaseBudgetCall(ctx, "r5"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var admitted sync.Map
	for id, call := range map[string]struct {
		st    *StateStore
		label UsageLabel
	}{"r6": {a, capped}, "d1": {b, UsageLabel{Pipeline: "digest"}}} {
		wg.Add(1)
		go func(id string, st *StateStore, label UsageLabel) {
			defer wg.Done()
			if st.BeginBudgetCall(ctx, id, at, 1, 0, 0, label) == nil {
				admitted.Store(id, true)
			}
		}(id, call.st, call.label)
	}
	wg.Wait()
	if _, ok := admitted.Load("r6"); ok {
		t.Fatal("capped pipeline admitted concurrently")
	}
	if _, ok := admitted.Load("d1"); !ok {
		t.Fatal("uncapped pipeline refused")
	}
	u, err := a.PipelineBudgetDay(ctx, at, "replies")
	if err != nil || u.Calls != 2 || u.Tokens != 45 || u.Cost != 1 {
		t.Fatalf("pipeline day %+v %v", u, err)
	}
}

func TestUTCDayIsTheUTCCalendarDate(t *testing.T) {
	berlin := time.FixedZone("CEST", 2*60*60)
	if got := UTCDay(time.Date(2026, 10, 22, 1, 30, 0, 0, berlin)); got != "2026-10-21" {
		t.Fatalf("01:30 CEST is still the 21st in UTC, got %s", got)
	}
}

// A pending reservation lives in the store: it survives a reopen, still
// blocks the next call, and is replaced by the actual cost on settlement.
func TestBudgetReservationSurvivesReopenAndSettlementReplacesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.db")
	ctx := context.Background()
	at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	st, err := OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddBudgetUsage(ctx, at, 0, .95, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginBudgetCall(ctx, "r1", at, 1, 0, 2, UsageLabel{ReserveCost: .5}); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginBudgetCall(ctx, "r2", at, 1, 0, 1, UsageLabel{ReserveCost: .05}); err == nil || !strings.Contains(err.Error(), "BUDGET_BLOCKED") {
		t.Fatalf("second call during a pending reservation: %v", err)
	}
	_ = st.Close()

	st, err = OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var reserved float64
	if err := st.db.QueryRowContext(ctx, `SELECT reserved_cost FROM budget_calls WHERE call_id='r1' AND status='pending'`).Scan(&reserved); err != nil || reserved != .5 {
		t.Fatalf("pending reservation after reopen: %v %v", reserved, err)
	}
	if err := st.BeginBudgetCall(ctx, "r3", at, 1, 0, 2, UsageLabel{ReserveCost: .1}); err == nil {
		t.Fatal("pending call forgotten after reopen")
	}
	if err := st.SettleBudgetCall(ctx, "r1", 2, .01); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRowContext(ctx, `SELECT reserved_cost FROM budget_calls WHERE call_id='r1'`).Scan(&reserved); err != nil || reserved != 0 {
		t.Fatalf("reservation after settlement: %v %v", reserved, err)
	}
	d, err := st.BudgetDay(ctx, at)
	if err != nil || math.Abs(d.Cost-.96) > 1e-9 {
		t.Fatalf("day after settlement: %+v %v", d, err)
	}
	// Settled .96: a worst case of .05 does not fit under 1.0, .04 does.
	if err := st.BeginBudgetCall(ctx, "r4", at, 1, 0, 1, UsageLabel{ReserveCost: .05}); err == nil {
		t.Fatal("0.96 settled + 0.05 admitted under 1.0")
	}
	if err := st.BeginBudgetCall(ctx, "r5", at, 1, 0, 1, UsageLabel{ReserveCost: .04}); err != nil {
		t.Fatalf("0.96 settled + 0.04 refused under 1.0: %v", err)
	}
}
