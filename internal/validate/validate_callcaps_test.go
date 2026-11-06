package validate

import (
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/renezander030/draftcat/internal/config"
)

func callCapsReport(t *testing.T, calls, minutes int, voiceYAML string) *validateReport {
	t.Helper()
	cfg := &config.Config{Budgets: config.BudgetConfig{PerDayCalls: calls, PerDayCallMinutes: minutes}}
	if voiceYAML != "" {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(voiceYAML), &doc); err != nil {
			t.Fatal(err)
		}
		cfg.Voice = *doc.Content[0]
	}
	rep := &validateReport{}
	checkVoiceCallCaps(cfg, rep)
	return rep
}

func TestValidateVoiceCallCaps(t *testing.T) {
	if rep := callCapsReport(t, 0, 0, ""); len(rep.Findings) != 0 {
		t.Fatalf("no caps, no findings: %+v", rep.Findings)
	}
	rep := callCapsReport(t, -1, -5, "")
	if len(findingsAt(rep, "error", "budgets.per_day_calls")) != 1 || len(findingsAt(rep, "error", "budgets.per_day_call_minutes")) != 1 {
		t.Fatalf("negative caps must error: %+v", rep.Findings)
	}
	if warns := findingsAt(callCapsReport(t, 10, 0, "enabled: false"), "warn", "budgets.per_day_calls"); len(warns) != 1 {
		t.Fatalf("a cap nothing is counted against must warn: %+v", warns)
	}
	if warns := findingsAt(callCapsReport(t, 0, 60, ""), "warn", "budgets.per_day_call_minutes"); len(warns) != 1 {
		t.Fatalf("minute cap without voice must warn: %+v", warns)
	}
}
