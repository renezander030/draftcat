package main

import (
	"strings"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

func TestFormatPendingLines_Empty(t *testing.T) {
	got := formatPendingLines(nil, time.Now())
	if len(got) != 1 || !strings.Contains(got[0], "No gates open") {
		t.Fatalf("got %v, want the no-gates line", got)
	}
}

func TestFormatPendingLines_WaitingAndExpired(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	rows := []statestore.PendingApproval{
		{Pipeline: "invoices", Step: "release-payment", QuorumN: 2, OpenedAt: now.Add(-12 * time.Minute), ExpiresAt: now.Add(3*time.Hour + 48*time.Minute)},
		{Pipeline: "tool-gate", Step: "send_email", QuorumN: 1, OpenedAt: now.Add(-5 * time.Hour), ExpiresAt: now.Add(-time.Hour)},
	}
	got := strings.Join(formatPendingLines(rows, now), "\n")
	for _, want := range []string{"2 gate(s) waiting", "invoices / release-payment", "opened 12m ago", "3h48m left", "quorum 2",
		"tool-gate / send_email", "expired 1h ago"} {
		if !strings.Contains(got, want) {
			t.Fatalf("pending report missing %q:\n%s", want, got)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second: "30s", 5 * time.Minute: "5m", 2 * time.Hour: "2h", 3*time.Hour + 48*time.Minute: "3h48m",
	} {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestStatusReport_ShowsSpendAgainstCapsAndOpenGates(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	budget := &BudgetTracker{tokensUsedToday: 1234, costToday: 4.12, dayCostLimit: 20, pipelineCostLimit: 2, dayStart: now.Add(-3 * time.Hour)}
	cfg := &config.Config{Budgets: config.BudgetConfig{PerDayTokens: 100000}}
	open := []statestore.PendingApproval{{Pipeline: "invoices", Step: "release", OpenedAt: now.Add(-9 * time.Minute)}}

	got := statusReport(2, 1, budget, cfg, open, nil, now)
	for _, want := range []string{
		"Pipelines: 2 active, 1 paused",
		"Tokens today: 1234 / 100000 (99% left)",
		"Spend today: 4.1200 / 20.0000 (79% left)",
		"Per-pipeline cap: 2.0000",
		"Gates open: 1 (oldest 9m ago — /pending)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status missing %q:\n%s", want, got)
		}
	}
}

func TestStatusReport_NoCapsNoGates(t *testing.T) {
	got := statusReport(0, 0, &BudgetTracker{dayStart: time.Now()}, &config.Config{}, nil, nil, time.Now())
	if !strings.Contains(got, "Tokens today: 0 (no cap)") || !strings.Contains(got, "Spend today: 0.0000 (no cap)") || !strings.Contains(got, "Gates open: 0") {
		t.Fatalf("status without caps rendered as:\n%s", got)
	}
}
