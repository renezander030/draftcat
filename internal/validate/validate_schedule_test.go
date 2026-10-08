package validate

import (
	"testing"

	"github.com/renezander030/draftcat/internal/config"
)

func schedulePipeline(p config.PipelineConfig) *config.Config {
	p.Name = "p"
	p.Steps = []config.StepConfig{{Name: "fetch", Type: "deterministic"}}
	return &config.Config{Pipelines: []config.PipelineConfig{p}}
}

func TestValidateAcceptsCalendarSchedules(t *testing.T) {
	for _, spec := range []string{"30m", "0 8 * * 1-5", "@daily", "*/15 9-17 * * mon-fri"} {
		rep := runCheckPipelines(schedulePipeline(config.PipelineConfig{Schedule: spec, Timezone: "Europe/Berlin"}))
		if errs := findingsAt(rep, "error", ".schedule"); len(errs) != 0 {
			t.Errorf("%q rejected: %+v", spec, errs)
		}
	}
}

func TestValidateRejectsBadScheduleAndZone(t *testing.T) {
	if errs := findingsAt(runCheckPipelines(schedulePipeline(config.PipelineConfig{Schedule: "0 25 * * *"})), "error", ".schedule"); len(errs) == 0 {
		t.Error("hour 25 accepted")
	}
	if errs := findingsAt(runCheckPipelines(schedulePipeline(config.PipelineConfig{Schedule: "0 8 * * *", Timezone: "Europe/Atlantis"})), "error", ".schedule"); len(errs) == 0 {
		t.Error("unknown time zone accepted")
	}
}

func TestValidateWarnsOnIneffectiveScheduleOptions(t *testing.T) {
	rep := runCheckPipelines(schedulePipeline(config.PipelineConfig{Schedule: "1h", Timezone: "UTC", CatchUp: true}))
	if len(findingsAt(rep, "warn", ".timezone")) == 0 || len(findingsAt(rep, "warn", ".catch_up")) == 0 {
		t.Errorf("expected timezone and catch_up warnings for an interval, got %+v", rep.Findings)
	}
	rep = runCheckPipelines(schedulePipeline(config.PipelineConfig{Schedule: "manual", PauseAfterFailures: 3}))
	if len(findingsAt(rep, "warn", ".pause_after_failures")) == 0 {
		t.Errorf("expected pause_after_failures warning on a manual pipeline, got %+v", rep.Findings)
	}
	rep = runCheckPipelines(schedulePipeline(config.PipelineConfig{Schedule: "5m", PauseAfterFailures: -1}))
	if len(findingsAt(rep, "error", ".pause_after_failures")) == 0 {
		t.Errorf("negative pause_after_failures accepted: %+v", rep.Findings)
	}
}

func TestValidateBudgetAlertsAndShutdownGrace(t *testing.T) {
	cfg := &config.Config{}
	cfg.Budgets.PerDayCost = 20
	cfg.Budgets.AlertAt = []float64{0.5, 0.8, 1.2, 0, 0.8}
	cfg.Timeouts.ShutdownGrace = "-5s"
	rep := &validateReport{}
	checkBudgetAlerts(cfg, rep)
	checkTimeouts(cfg, rep)
	if n := len(findingsAt(rep, "error", "budgets.alert_at")); n != 2 {
		t.Errorf("want 2 alert_at errors (1.2 and 0), got %d: %+v", n, rep.Findings)
	}
	if len(findingsAt(rep, "warn", "budgets.alert_at")) != 1 {
		t.Errorf("want a duplicate warning, got %+v", rep.Findings)
	}
	if len(findingsAt(rep, "error", "timeouts.shutdown_grace")) != 1 {
		t.Errorf("negative shutdown_grace accepted: %+v", rep.Findings)
	}

	noCap := &config.Config{}
	noCap.Budgets.AlertAt = []float64{0.8}
	rep = &validateReport{}
	checkBudgetAlerts(noCap, rep)
	if len(findingsAt(rep, "warn", "budgets.alert_at")) != 1 {
		t.Errorf("alerts without a daily cap should warn, got %+v", rep.Findings)
	}
}
