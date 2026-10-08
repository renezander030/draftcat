package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

func alertCollector() (func(string), func(n int, within time.Duration) []string) {
	ch := make(chan string, 16)
	notify := func(m string) { ch <- m }
	collect := func(n int, within time.Duration) []string {
		var out []string
		deadline := time.After(within)
		for len(out) < n {
			select {
			case m := <-ch:
				out = append(out, m)
			case <-deadline:
				return out
			}
		}
		// Anything extra arriving shortly after is a duplicate.
		select {
		case m := <-ch:
			out = append(out, m)
		case <-time.After(50 * time.Millisecond):
		}
		return out
	}
	return notify, collect
}

func alertConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Budgets.PerDayCost = 10
	cfg.Budgets.PerDayTokens = 1000
	cfg.Budgets.AlertAt = []float64{0.8, 0.5}
	return cfg
}

func TestBudgetAlertFiresOncePerThreshold(t *testing.T) {
	b := &BudgetTracker{dayStart: time.Now()}
	notify, collect := alertCollector()
	b.configureAlerts(alertConfig(), notify)

	b.RecordCost(4) // 40%: nothing
	if got := collect(1, 100*time.Millisecond); len(got) != 0 {
		t.Fatalf("alert below threshold: %q", got)
	}
	b.RecordCost(1.5) // 55%
	got := collect(1, time.Second)
	if len(got) != 1 || !strings.Contains(got[0], "50% of today's cost cap") {
		t.Fatalf("at 55%%: %q", got)
	}
	b.RecordCost(0.1) // 56%: already sent
	if got := collect(1, 100*time.Millisecond); len(got) != 0 {
		t.Fatalf("repeated alert: %q", got)
	}
	b.RecordCost(3) // 86%
	got = collect(1, time.Second)
	if len(got) != 1 || !strings.Contains(got[0], "80% of today's cost cap") {
		t.Fatalf("at 86%%: %q", got)
	}
}

func TestBudgetAlertJumpReportsHighestThreshold(t *testing.T) {
	b := &BudgetTracker{dayStart: time.Now()}
	notify, collect := alertCollector()
	b.configureAlerts(alertConfig(), notify)
	b.record(900) // 0 -> 90% of the token cap in one step
	got := collect(1, time.Second)
	if len(got) != 1 || !strings.Contains(got[0], "80% of today's token cap used: 900 of 1000") {
		t.Fatalf("got %q", got)
	}
}

func TestBudgetAlertMarksSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := statestore.OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	b := &BudgetTracker{dayStart: time.Now()}
	if err := b.attachStore(st); err != nil {
		t.Fatal(err)
	}
	notify, collect := alertCollector()
	b.configureAlerts(alertConfig(), notify)
	b.RecordCost(6)
	if got := collect(1, time.Second); len(got) != 1 {
		t.Fatalf("first process: %q", got)
	}
	st.Close()

	st2, err := statestore.OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	b2 := &BudgetTracker{dayStart: time.Now()}
	if err := b2.attachStore(st2); err != nil {
		t.Fatal(err)
	}
	notify2, collect2 := alertCollector()
	b2.configureAlerts(alertConfig(), notify2)
	b2.RecordCost(0.5) // 65%: 50% was already sent before the restart
	if got := collect2(1, 150*time.Millisecond); len(got) != 0 {
		t.Fatalf("alert repeated after restart: %q", got)
	}
}

func TestBudgetAlertsOffWithoutCapOrThresholds(t *testing.T) {
	cfg := &config.Config{}
	cfg.Budgets.AlertAt = []float64{0.5}
	b := &BudgetTracker{dayStart: time.Now()}
	notify, collect := alertCollector()
	b.configureAlerts(cfg, notify)
	b.RecordCost(100)
	if got := collect(1, 100*time.Millisecond); len(got) != 0 {
		t.Fatalf("alert without a cap: %q", got)
	}
	if b.alerts != nil {
		t.Fatal("alerts configured without a daily cap")
	}
}
