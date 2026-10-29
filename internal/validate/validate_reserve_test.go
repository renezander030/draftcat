package validate

import (
	"testing"

	"github.com/renezander030/draftcat/internal/config"
)

func TestCheckReserveMaxCostWarnsForUnpricedCappedRole(t *testing.T) {
	cfg := &config.Config{
		Models: map[string]config.ModelConfig{
			"priced": {Model: "a", MaxTokens: 100, CostIn: 1, CostOut: 2},
			"free":   {Model: "b", MaxTokens: 100},
		},
		Roles: map[string]string{"drafter": "priced", "classifier": "free"},
		Pipelines: []config.PipelineConfig{
			{Name: "p1", Budget: config.PipelineBudgetConfig{PerDayCost: 1, ReserveMaxCost: true},
				Steps: []config.StepConfig{{Name: "s", Type: "ai", Role: "drafter"}}},
			{Name: "p2", Budget: config.PipelineBudgetConfig{PerDayCost: 1, ReserveMaxCost: true},
				Steps: []config.StepConfig{{Name: "s", Type: "ai", Role: "classifier"}}},
		},
	}
	cfg.Budgets.PerDayCost = 5
	cfg.Budgets.ReserveMaxCost = true
	rep := &validateReport{}
	checkReserveMaxCost(cfg, rep)
	got := map[string]string{}
	for _, f := range rep.Findings {
		got[f.Path] = f.Level
	}
	want := map[string]string{
		"budgets.reserve_max_cost":                "warn", // classifier has no prices
		"pipelines[1:p2].budget.reserve_max_cost": "warn",
	}
	if len(got) != len(want) {
		t.Fatalf("findings %+v, want %v", rep.Findings, want)
	}
	for p, l := range want {
		if got[p] != l {
			t.Fatalf("findings %+v, want %v", rep.Findings, want)
		}
	}

	cfg.Models["free"] = config.ModelConfig{Model: "b", MaxTokens: 100, CostOut: 1}
	rep = &validateReport{}
	checkReserveMaxCost(cfg, rep)
	if len(rep.Findings) != 0 {
		t.Fatalf("priced models still warn: %+v", rep.Findings)
	}

	// Without the key nothing is reported, and without a cap the key warns.
	off := &config.Config{Models: cfg.Models, Roles: map[string]string{"x": "free"}}
	off.Models["free"] = config.ModelConfig{Model: "b"}
	rep = &validateReport{}
	checkReserveMaxCost(off, rep)
	if len(rep.Findings) != 0 {
		t.Fatalf("no key, findings: %+v", rep.Findings)
	}
	off.Budgets.ReserveMaxCost = true
	rep = &validateReport{}
	checkReserveMaxCost(off, rep)
	if len(rep.Findings) != 1 || rep.Findings[0].Path != "budgets.reserve_max_cost" {
		t.Fatalf("key without per_day_cost: %+v", rep.Findings)
	}
}
