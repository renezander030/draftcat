package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/renezander030/draftcat/internal/config"
)

func TestInlineOutputSchemaErrorsBeforeRun(t *testing.T) {
	cfg := &config.Config{Pipelines: []config.PipelineConfig{{Name: "p", Steps: []config.StepConfig{{
		Name: "classify", Type: "ai", Prompt: "classify", OutputSchema: map[string]interface{}{
			"score": map[string]interface{}{"type": "int", "min": 5, "max": 1},
		},
	}}}}}
	errs := findingsAt(runCheckPipelines(cfg), "error", "output_schema.score")
	if len(errs) != 1 || !strings.Contains(errs[0].Message, "min exceeds max") {
		t.Fatalf("bad inline schema didn't fail startup: %+v", errs)
	}
}

func TestSkillOutputSchemaErrorsBeforeRun(t *testing.T) {
	dir := t.TempDir()
	content := []byte("name: classify\nprompt: classify\noutput_schema:\n  score: {type: unsupported}\n  choice: {enum: wrong}\n")
	if err := os.WriteFile(filepath.Join(dir, "classify.yaml"), content, 0600); err != nil {
		t.Fatal(err)
	}
	rep := &validateReport{}
	loadSkillsForValidate(dir, rep)
	errs := findingsAt(rep, "error", "output_schema")
	if len(errs) != 2 || !strings.HasSuffix(errs[0].Path, "choice") || !strings.HasSuffix(errs[1].Path, "score") {
		t.Fatalf("bad skill schema didn't return ordered startup errors: %+v", rep.Findings)
	}
}

func TestInputSchemaErrorsBeforeRun(t *testing.T) {
	cfg := &config.Config{Pipelines: []config.PipelineConfig{{
		Name: "p", Schedule: "webhook", Steps: []config.StepConfig{{Name: "fetch", Type: "deterministic"}},
		InputSchema: map[string]interface{}{"amount": map[string]interface{}{"type": "string", "min": 1}},
	}}}
	errs := findingsAt(runCheckPipelines(cfg), "error", "input_schema.amount")
	if len(errs) != 1 || !strings.Contains(errs[0].Message, "min/max require") {
		t.Fatalf("bad input_schema didn't fail validation: %+v", errs)
	}
}

func TestInputSchemaWarnsOutsideWebhookPipelines(t *testing.T) {
	schema := map[string]interface{}{"amount": map[string]interface{}{"type": "int"}}
	for _, sched := range []string{"manual", "1h"} {
		cfg := &config.Config{Pipelines: []config.PipelineConfig{{
			Name: "p", Schedule: sched, InputSchema: schema,
			Steps: []config.StepConfig{{Name: "fetch", Type: "deterministic"}},
		}}}
		if len(findingsAt(runCheckPipelines(cfg), "warn", ".input_schema")) != 1 {
			t.Errorf("%s: input_schema on a non-webhook pipeline did not warn", sched)
		}
	}
	cfg := &config.Config{Pipelines: []config.PipelineConfig{{
		Name: "p", Schedule: "webhook", InputSchema: schema,
		Steps: []config.StepConfig{{Name: "fetch", Type: "deterministic"}},
	}}}
	rep := runCheckPipelines(cfg)
	if f := append(findingsAt(rep, "error", ".input_schema"), findingsAt(rep, "warn", ".input_schema")...); len(f) != 0 {
		t.Errorf("valid webhook input_schema reported findings: %+v", f)
	}
}
