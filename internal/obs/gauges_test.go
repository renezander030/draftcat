package obs

import (
	"strings"
	"testing"
)

func TestGaugesRenderedAtScrape(t *testing.T) {
	resetMetrics()
	defer resetMetrics()
	calls := 0
	SetGaugeSource(func() []Gauge {
		calls++
		return []Gauge{
			{Name: "draftcat_pipeline_paused", Help: "1 when paused.", Labels: []string{"pipeline", "b"}, Value: 1},
			{Name: "draftcat_approvals_open", Help: "Open gates.", Value: 2},
			{Name: "draftcat_pipeline_paused", Labels: []string{"pipeline", "a\"x"}, Value: 0},
		}
	})
	var sb strings.Builder
	if err := WriteMetrics(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"# TYPE draftcat_approvals_open gauge\ndraftcat_approvals_open 2\n",
		"# HELP draftcat_pipeline_paused 1 when paused.\n# TYPE draftcat_pipeline_paused gauge\n",
		`draftcat_pipeline_paused{pipeline="b"} 1`,
		`draftcat_pipeline_paused{pipeline="a\"x"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Count(out, "# TYPE draftcat_pipeline_paused gauge") != 1 {
		t.Errorf("gauge family rendered twice:\n%s", out)
	}
	if calls != 1 {
		t.Errorf("source called %d times", calls)
	}
}

func TestNoGaugesWithoutSource(t *testing.T) {
	resetMetrics()
	var sb strings.Builder
	_ = WriteMetrics(&sb)
	if strings.Contains(sb.String(), " gauge\n") {
		t.Errorf("gauge rendered without a source:\n%s", sb.String())
	}
}
