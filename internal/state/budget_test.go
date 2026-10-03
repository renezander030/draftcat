package state

import (
	"context"
	"math"
	"path/filepath"
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
	if err := st.BeginBudgetCall(ctx, "paid", at, 10, 100, .5); err != nil {
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
	if err := st.BeginBudgetCall(ctx, "same-day", at, 1, 100, .5); err == nil {
		t.Fatal("restart reopened settled cost cap")
	}
	next := at.Add(2 * time.Minute)
	if err := st.BeginBudgetCall(ctx, "next-day", next, 1, 100, .5); err != nil {
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
	if err := st.BeginBudgetCall(ctx, "unknown", at, 1, 0, 0); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	st, err = OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.BeginBudgetCall(ctx, "next", at.AddDate(0, 0, 1), 1, 0, 0); err == nil {
		t.Fatal("pending usage was forgotten on restart/new day")
	}
	if err := st.SettleBudgetCall(ctx, "unknown", 12, .2); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginBudgetCall(ctx, "next", at, 1, 100, .5); err != nil {
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
			if st.BeginBudgetCall(ctx, id, time.Now(), 1, 100, 1) == nil {
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
	if err := st.BeginBudgetCall(ctx, "pending", at, 1, 0, 0); err != nil {
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
