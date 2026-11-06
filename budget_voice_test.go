package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

// Voice calls recorded by the voice plugin show in /status and in
// `draftcat budget status`, read back from the shared ledger.
func TestStatusAndBudgetStatusReadRecordedVoiceCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := statestore.OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now()
	caps := statestore.VoiceCallCaps{Calls: 2, Minutes: 100}
	for _, id := range []string{"c1", "c2"} {
		if _, err := st.AdmitVoiceCall(ctx, id, now, caps); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.EndVoiceCall(ctx, "c1", now, 7); err != nil {
		t.Fatal(err)
	}

	b := &BudgetTracker{dayStart: now}
	if err := b.attachStore(st); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Budgets: config.BudgetConfig{PerDayCalls: 2, PerDayCallMinutes: 100}}
	got := statusReport(1, 0, b, cfg, nil, nil, now)
	for _, want := range []string{"Calls today: 2 / 2 (reached; new calls are refused)", "Call minutes today: 7 / 100\n"} {
		if !strings.Contains(got+"\n", want) {
			t.Fatalf("status missing %q:\n%s", want, got)
		}
	}
	_ = st.Close()

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("budgets:\n  per_day_calls: 2\n  per_day_call_minutes: 100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DRAFTCAT_STATE_PATH", path)
	out := captureStdout(t, func() {
		if code := runBudgetCmd([]string{"status", "--json", "--config", cfgPath}); code != 0 {
			t.Errorf("status exit %d", code)
		}
	})
	var report struct {
		Calls       int `json:"calls"`
		CallMinutes int `json:"call_minutes"`
		CallCaps    *struct {
			PerDayCalls int  `json:"per_day_calls"`
			Reached     bool `json:"reached"`
		} `json:"call_caps"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if report.Calls != 2 || report.CallMinutes != 7 || report.CallCaps == nil || report.CallCaps.PerDayCalls != 2 || !report.CallCaps.Reached {
		t.Fatalf("budget status = %s", out)
	}
	text := captureStdout(t, func() { runBudgetCmd([]string{"status", "--config", cfgPath}) })
	if !strings.Contains(text, "Voice call caps: 2 of 2 calls, 7 of 100 call minutes  REACHED") {
		t.Fatalf("budget status text:\n%s", text)
	}
	// Without caps nothing about them is shown.
	if err := os.WriteFile(cfgPath, []byte("budgets: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if text := captureStdout(t, func() { runBudgetCmd([]string{"status", "--config", cfgPath}) }); strings.Contains(text, "Voice call caps") {
		t.Fatalf("uncapped status shows caps:\n%s", text)
	}
}
