package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/renezander030/draftcat/internal/config"
)

func TestFailedRunNamesTheStepsThatDidNotRun(t *testing.T) {
	st := useReliabilityStore(t)
	cfg := &config.Config{}
	p := config.PipelineConfig{Name: "replies", Steps: []config.StepConfig{
		{Name: "classify", Type: "ai", Skill: "missing"},
		{Name: "review", Type: "approval"},
		{Name: "send", Type: "deterministic"},
	}}
	err := runPipeline(cfg, p, &BudgetTracker{}, nil, nil, nil)
	if err == nil {
		t.Fatal("expected the ai step to fail")
	}
	if got := haltNote(err); got != "\nhalted at: classify\nnot run: review, send" {
		t.Fatalf("halt note: %q", got)
	}
	runs, rerr := st.RecentRuns("replies", 1)
	if rerr != nil || len(runs) != 1 || strings.Contains(runs[0].Error, "not run") || runs[0].Error != err.Error() {
		t.Fatalf("the stored run error stays the step's own error: %+v %v", runs, rerr)
	}
	var inner *haltedRunError
	if !errors.As(err, &inner) || errors.Unwrap(err) == nil {
		t.Fatalf("the halt wraps, it does not replace, the error: %T", err)
	}
}

func TestHaltNoteOnLastStepAndPlainErrors(t *testing.T) {
	steps := []config.StepConfig{{Name: "a"}, {Name: "b"}}
	if got := haltNote(haltedRun(errors.New("x"), steps, 1)); got != "\nhalted at: b (last step)" {
		t.Fatalf("last step: %q", got)
	}
	if got := haltNote(errors.New("plain")); got != "" {
		t.Fatalf("plain error: %q", got)
	}
}
