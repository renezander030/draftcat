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
