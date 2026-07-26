package validate

import (
	"strings"
	"testing"

	"github.com/renezander030/draftcat/internal/config"
)

// A pipeline with no steps schedules, runs, and does nothing — which looks like
// success in the logs. Almost always a half-finished edit or a YAML indent slip.
func TestValidateEmptyPipelineIsError(t *testing.T) {
	cfg := &config.Config{Pipelines: []config.PipelineConfig{{Name: "empty"}}}
	if errs := findingsAt(runCheckPipelines(cfg), "error", ".steps"); len(errs) == 0 {
		t.Fatalf("a pipeline with no steps must error; got %+v", runCheckPipelines(cfg).Findings)
	}
}

func TestValidateNonEmptyPipelineHasNoStepsError(t *testing.T) {
	cfg := &config.Config{Pipelines: []config.PipelineConfig{{
		Name:  "p",
		Steps: []config.StepConfig{{Name: "fetch", Type: "deterministic"}},
	}}}
	if errs := findingsAt(runCheckPipelines(cfg), "error", ".steps"); len(errs) != 0 {
		t.Fatalf("a pipeline with steps must not trip the empty check; got %+v", errs)
	}
}

// The issue asked for this by name: "unknown type 'ia' — did you mean 'ai'?".
func TestValidateSuggestsStepTypeTypo(t *testing.T) {
	cfg := &config.Config{Pipelines: []config.PipelineConfig{{
		Name:  "p",
		Steps: []config.StepConfig{{Name: "s", Type: "ia"}},
	}}}
	errs := findingsAt(runCheckPipelines(cfg), "error", ".type")
	if len(errs) == 0 {
		t.Fatal("an invalid step type must error")
	}
	if !strings.Contains(errs[0].Message, `did you mean "ai"`) {
		t.Errorf("message %q should suggest \"ai\"", errs[0].Message)
	}
}

func TestValidateSuggestsActionTypo(t *testing.T) {
	cfg := &config.Config{Pipelines: []config.PipelineConfig{{
		Name:  "p",
		Steps: []config.StepConfig{{Name: "s", Type: "deterministic", Action: "gmail_unred"}},
	}}}
	errs := findingsAt(runCheckPipelines(cfg), "error", ".action")
	if len(errs) == 0 {
		t.Fatal("an unknown action must error")
	}
	if !strings.Contains(errs[0].Message, `did you mean "gmail_unread"`) {
		t.Errorf("message %q should suggest \"gmail_unread\"", errs[0].Message)
	}
}

// A confidently wrong suggestion is worse than none, so a value that is not a
// near miss must not get one.
func TestValidateNoSuggestionWhenNothingIsClose(t *testing.T) {
	cfg := &config.Config{Pipelines: []config.PipelineConfig{{
		Name:  "p",
		Steps: []config.StepConfig{{Name: "s", Type: "quantum"}},
	}}}
	errs := findingsAt(runCheckPipelines(cfg), "error", ".type")
	if len(errs) == 0 {
		t.Fatal("an invalid step type must error")
	}
	if strings.Contains(errs[0].Message, "did you mean") {
		t.Errorf("message %q should not guess for a value nothing is close to", errs[0].Message)
	}
}

func TestDidYouMean(t *testing.T) {
	cands := []string{"deterministic", "ai", "approval"}
	for _, tc := range []struct {
		got  string
		want string // substring, "" = no suggestion
	}{
		{"ia", `"ai"`},
		{"aproval", `"approval"`},
		{"determinstic", `"deterministic"`},
		{"", ""},           // nothing typed
		{"quantum", ""},    // not close to anything
		{"xyzzyplugh", ""}, // far from everything
	} {
		got := didYouMean(tc.got, cands)
		if tc.want == "" {
			if got != "" {
				t.Errorf("didYouMean(%q) = %q, want no suggestion", tc.got, got)
			}
			continue
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("didYouMean(%q) = %q, want it to contain %s", tc.got, got, tc.want)
		}
	}
}

func TestEditDistance(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"ai", "ai", 0},
		{"ia", "ai", 1},   // adjacent transposition is ONE edit (Damerau)
		{"ab", "ba", 1},   // ditto
		{"abc", "cba", 2}, // non-adjacent swap is not a single transposition
		{"", "abc", 3},
		{"abc", "", 3},
		{"kitten", "sitting", 3},
	} {
		if got := editDistance(tc.a, tc.b); got != tc.want {
			t.Errorf("editDistance(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
