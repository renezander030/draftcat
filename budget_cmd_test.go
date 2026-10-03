package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	statestore "github.com/renezander030/draftcat/internal/state"
)

func TestBudgetCommandRecoveryChargesOriginalDayOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := statestore.OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 9, 30, 23, 59, 0, 0, time.UTC)
	if err := st.BeginBudgetCall(context.Background(), "call-uncertain", day, 100, 1000, 1); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	t.Setenv("DRAFTCAT_STATE_PATH", path)
	if code := runBudgetCmd([]string{"status", "--json"}); code != 0 {
		t.Fatalf("status exit %d", code)
	}
	if code := runBudgetCmd([]string{"reconcile", "call-uncertain", "--tokens", "120", "--cost", "0.12"}); code != 0 {
		t.Fatalf("reconcile exit %d", code)
	}
	if code := runBudgetCmd([]string{"reconcile", "call-uncertain", "--tokens", "120", "--cost", "0.12"}); code == 0 {
		t.Fatal("duplicate reconciliation succeeded")
	}
	st, err = statestore.OpenStateStoreReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	got, err := st.BudgetDay(context.Background(), day)
	if err != nil || got.Tokens != 120 || got.Cost != 0.12 || got.Unsettled != 0 {
		t.Fatalf("usage %+v error %v", got, err)
	}
	calls, err := st.PendingBudgetCalls(context.Background())
	if err != nil || len(calls) != 0 {
		t.Fatalf("pending %+v error %v", calls, err)
	}
}

func TestBudgetCommandsDoNotCreateMissingState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	t.Setenv("DRAFTCAT_STATE_PATH", path)
	for _, args := range [][]string{{"status"}, {"reconcile", "call", "--tokens", "0", "--cost", "0"}} {
		if code := runBudgetCmd(args); code == 0 {
			t.Fatalf("missing database accepted: %v", args)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("command created database: %v", err)
		}
	}
}

func TestBudgetReconcileRejectsInvalidUsage(t *testing.T) {
	for _, args := range [][]string{{"reconcile", "call", "--tokens", "-1", "--cost", "0"}, {"reconcile", "call", "--tokens", "1", "--cost", "NaN"}, {"reconcile", "call", "--tokens", "1"}, {"status", "--tokens", "1"}} {
		if code := runBudgetCmd(args); code != 2 {
			t.Fatalf("invalid arguments %v exit %d", args, code)
		}
	}
}
