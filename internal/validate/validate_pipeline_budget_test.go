package validate

import (
	"math"
	"strings"
	"testing"

	"github.com/renezander030/draftcat/internal/config"
)

func TestValidatePipelineBudget(t *testing.T) {
	ai := []config.StepConfig{{Name: "draft", Type: "ai", Role: "drafter", Prompt: "p"}}
	cfg := &config.Config{
		Roles:  map[string]string{"drafter": "m"},
		Models: map[string]config.ModelConfig{"m": {Model: "x"}},
		Pipelines: []config.PipelineConfig{
			{Name: "ok", Schedule: "manual", Budget: config.PipelineBudgetConfig{PerDayTokens: 100, PerDayCost: 1}, Steps: ai},
			{Name: "neg", Schedule: "manual", Budget: config.PipelineBudgetConfig{PerDayTokens: -1, PerDayCost: -0.5}, Steps: ai},
			{Name: "nan", Schedule: "manual", Budget: config.PipelineBudgetConfig{PerDayCost: math.NaN()}, Steps: ai},
			{Name: "noai", Schedule: "manual", Budget: config.PipelineBudgetConfig{PerDayCost: 1}, Steps: []config.StepConfig{{Name: "n", Type: "deterministic", Action: "notify"}}},
		},
	}
	rep := runCheckPipelines(cfg)
	got := map[string]string{}
	for _, f := range rep.Findings {
		if strings.Contains(f.Path, ".budget") {
			got[f.Path] = f.Level
		}
	}
	want := map[string]string{
		"pipelines[1:neg].budget.per_day_tokens": "error",
		"pipelines[1:neg].budget.per_day_cost":   "error",
		"pipelines[2:nan].budget.per_day_cost":   "error",
		"pipelines[3:noai].budget":               "warn",
	}
	if len(got) != len(want) {
		t.Fatalf("findings %v, want %v", got, want)
	}
	for p, level := range want {
		if got[p] != level {
			t.Fatalf("findings %v, want %v", got, want)
		}
	}

	alerts := &config.Config{Pipelines: cfg.Pipelines[:1]}
	alerts.Budgets.AlertAt = []float64{0.8}
	rep = &validateReport{}
	checkBudgetAlerts(alerts, rep)
	if len(rep.Findings) != 0 {
		t.Fatalf("alert_at with a pipeline cap: %+v", rep.Findings)
	}
}
