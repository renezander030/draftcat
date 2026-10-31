package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	"github.com/renezander030/draftcat/internal/schedule"
	statestore "github.com/renezander030/draftcat/internal/state"
)

func TestBuildDigestSilentWhenNothingNeedsAPerson(t *testing.T) {
	now := time.Date(2026, 1, 5, 8, 0, 0, 0, time.UTC)
	snap := digestSnapshot{Now: now, Open: []statestore.PendingApproval{
		{Pipeline: "p", Step: "s", OpenedAt: now.Add(-30 * time.Minute)},
	}}
	if text, ok := buildDigest(snap); ok || text != "" {
		t.Fatalf("a gate younger than 1h needs no digest: %q", text)
	}
}

func TestBuildDigestListsWhatNeedsAPerson(t *testing.T) {
	now := time.Date(2026, 1, 5, 8, 0, 0, 0, time.UTC)
	var open []statestore.PendingApproval
	for i := 0; i < 7; i++ {
		open = append(open, statestore.PendingApproval{Pipeline: "tool-gate", Step: fmt.Sprintf("tool_%d", i), OpenedAt: now.Add(-time.Duration(2+i) * time.Hour)})
	}
	open = append(open, statestore.PendingApproval{Pipeline: "fresh", Step: "x", OpenedAt: now.Add(-5 * time.Minute)})
	text, ok := buildDigest(digestSnapshot{
		Now: now, Open: open,
		Freeze:         statestore.GateFreeze{Frozen: true, Reason: "maintenance", By: "cli", At: now.Add(-time.Hour)},
		FailedOutcomes: 3,
	})
	if !ok {
		t.Fatal("digest expected")
	}
	for _, want := range []string{"7 approval(s) waiting for over 1h", "tool-gate / tool_6 — opened 8h ago", "… and 2 more", "Tool gate: FROZEN", "maintenance", "3 tool outcome(s) reported failed in the last 24h"} {
		if !strings.Contains(text, want) {
			t.Errorf("digest lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "fresh / x") || strings.Contains(text, "tool_0 ") && strings.Index(text, "tool_6") > strings.Index(text, "tool_0") {
		t.Errorf("only old gates, oldest first:\n%s", text)
	}
	if strings.Count(text, "\n  tool-gate") != 5 {
		t.Errorf("at most 5 listed:\n%s", text)
	}
}

func TestBuildDigestFreezeAloneIsEnough(t *testing.T) {
	text, ok := buildDigest(digestSnapshot{Now: time.Now(), Freeze: statestore.GateFreeze{Frozen: true, At: time.Now()}})
	if !ok || !strings.Contains(text, "FROZEN") || strings.Contains(text, "approval(s)") {
		t.Fatalf("freeze only: %v %q", ok, text)
	}
}

func digestJobFor(t *testing.T, sent *[]string) *digestJob {
	t.Helper()
	cfg := &config.Config{Telegram: config.TelegramConfig{Digest: &config.DigestConfig{At: "08:00", Timezone: "UTC"}}}
	j, err := newDigestJob(cfg, func(m string) error { *sent = append(*sent, m); return nil })
	if err != nil || j == nil {
		t.Fatalf("job: %v %v", j, err)
	}
	return j
}

func TestDigestJobSendsOncePerDayAcrossRestart(t *testing.T) {
	_, st := lifecycleHTTPStore(t)
	day := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	if _, err := st.BeginApproval("tool-gate", "send_email", "h", 1, day.Add(-3*time.Hour), day.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var sent []string
	j := digestJobFor(t, &sent)

	j.tick(day.Add(7*time.Hour + 59*time.Minute))
	if len(sent) != 0 {
		t.Fatalf("before the time: %q", sent)
	}
	j.tick(day.Add(8 * time.Hour))
	j.tick(day.Add(8*time.Hour + 2*time.Second))
	j.tick(day.Add(15 * time.Hour))
	if len(sent) != 1 || !strings.Contains(sent[0], "tool-gate / send_email") {
		t.Fatalf("once a day: %q", sent)
	}

	// A restart the same day builds a new job on the same store.
	j2 := digestJobFor(t, &sent)
	j2.tick(day.Add(16 * time.Hour))
	if len(sent) != 1 {
		t.Fatalf("a restart must not resend: %q", sent)
	}
	// The next day it sends again.
	j2.tick(day.Add(24*time.Hour + 8*time.Hour))
	if len(sent) != 2 {
		t.Fatalf("next day: %q", sent)
	}
}

func TestDigestJobSilentDayCountsAsHandled(t *testing.T) {
	_, st := lifecycleHTTPStore(t)
	day := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	var sent []string
	j := digestJobFor(t, &sent)
	j.tick(day.Add(8 * time.Hour))
	if _, err := st.BeginApproval("p", "s", "h", 1, day.Add(2*time.Hour), day.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	j.tick(day.Add(12 * time.Hour))
	if len(sent) != 0 {
		t.Fatalf("nothing needed a person at the time: %q", sent)
	}
	if last, _ := st.DigestLastDay(); last != "2026-01-05" {
		t.Fatalf("last day = %q", last)
	}
}

func TestDigestJobRetriesAfterSendFailureAndCountsFailedOutcomes(t *testing.T) {
	_, st := lifecycleHTTPStore(t)
	g := newToolGate(gateCfg(config.ToolRule{Name: "send"}), nil)
	_, allowed := postGate(t, g, `{"action_id":"f1","tool":"send"}`)
	consumeGate(t, g, "f1", allowed.BindingHash)
	hash := "sha256:" + strings.Repeat("a", 64)
	if code, _ := lifecyclePost(t, g, "f1", "complete", completeBody(allowed.BindingHash, "failed", hash)); code != 200 {
		t.Fatalf("complete: %d", code)
	}
	now := time.Now().UTC()
	n, err := st.FailedToolOutcomesSince(now.Add(-24 * time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("failed outcomes: %d %v", n, err)
	}

	sc, err := schedule.ParseDaily("00:00", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	fail, sent := true, []string{}
	j := &digestJob{sched: sc, send: func(m string) error {
		if fail {
			return fmt.Errorf("channel down")
		}
		sent = append(sent, m)
		return nil
	}}
	j.tick(now)
	if len(sent) != 0 {
		t.Fatal("send failed")
	}
	fail = false
	j.tick(now.Add(10 * time.Second)) // inside the retry back-off
	if len(sent) != 0 {
		t.Fatal("retry waits")
	}
	j.tick(now.Add(2 * time.Minute))
	if len(sent) != 1 || !strings.Contains(sent[0], "1 tool outcome(s) reported failed") {
		t.Fatalf("retried send: %q", sent)
	}
}

func TestNoDigestJobWithoutConfig(t *testing.T) {
	j, err := newDigestJob(&config.Config{}, func(string) error { return nil })
	if j != nil || err != nil {
		t.Fatalf("absent digest = no job: %v %v", j, err)
	}
	j.tick(time.Now()) // nil job is a no-op
}

func TestParseDaily(t *testing.T) {
	for _, bad := range []string{"", "8:00", "24:00", "08:60", "0800", "08:00:00", "ab:cd"} {
		if _, err := schedule.ParseDaily(bad, "UTC"); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	if _, err := schedule.ParseDaily("08:00", "Not/AZone"); err == nil {
		t.Error("unknown zone should be refused")
	}
	sc, err := schedule.ParseDaily("08:30", "Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	next := sc.Next(time.Date(2026, 1, 5, 0, 0, 0, 0, sc.Location()))
	if next.Hour() != 8 || next.Minute() != 30 {
		t.Fatalf("next = %v", next)
	}
}
