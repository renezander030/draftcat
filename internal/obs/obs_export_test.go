package obs

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestPrometheusExposition records a pipeline and an AI step, then asserts the
// rendered exposition contains the expected series with whole-number formatting
// and no duplicate HELP/TYPE lines.
func TestPrometheusExposition(t *testing.T) {
	resetMetrics()
	defer resetMetrics()

	RecordPipeline("p", "ok", 120)
	RecordStep("p", "classify", "ai", "ok", 100, 800, 0.004)

	var buf bytes.Buffer
	if err := WriteMetrics(&buf); err != nil {
		t.Fatalf("WriteMetrics: %v", err)
	}
	out := buf.String()

	wantContains := []string{
		`draftcat_pipeline_runs_total{pipeline="p",status="ok"} 1`,
		`draftcat_ai_tokens_total{pipeline="p",step="classify"} 800`,
		`draftcat_ai_cost_usd_total{pipeline="p",step="classify"} 0.004`,
		`draftcat_step_runs_total{pipeline="p",step="classify",step_type="ai",status="ok"} 1`,
		`draftcat_pipeline_duration_ms_bucket{pipeline="p",status="ok",le="250"} 1`,
		`draftcat_pipeline_duration_ms_count{pipeline="p",status="ok"} 1`,
	}
	for _, w := range wantContains {
		if !strings.Contains(out, w) {
			t.Errorf("exposition missing %q\n---\n%s", w, out)
		}
	}

	// No duplicate HELP/TYPE lines per metric family.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "# HELP ") || strings.HasPrefix(line, "# TYPE ") {
			if n := strings.Count(out, line); n != 1 {
				t.Errorf("metadata line appears %d times (want 1): %q", n, line)
			}
		}
	}
}

// TestMetricsRecordedWhenSpansOff proves the gating refactor: with Prometheus on
// but JSON spans off, counters are still recorded and NO JSON line is written to
// the span writer.
func TestMetricsRecordedWhenSpansOff(t *testing.T) {
	resetMetrics()
	reset()
	defer func() { resetMetrics(); reset() }()

	// Spans OFF (enabled=false) but point the span writer at a buffer to detect leaks.
	var spanBuf bytes.Buffer
	mu.Lock()
	out = &spanBuf
	enabled = false
	mu.Unlock()

	EnablePrometheus()

	EmitStep("p", "classify", "ai", time.Now().Add(-3*time.Millisecond), "ok",
		map[string]interface{}{"tokens": 800, "cost_usd": 0.004})
	Pipeline("p").End("ok", nil)

	if spanBuf.Len() != 0 {
		t.Errorf("expected NO JSON span output while spans off, got: %q", spanBuf.String())
	}

	var mbuf bytes.Buffer
	_ = WriteMetrics(&mbuf)
	mout := mbuf.String()
	if !strings.Contains(mout, `draftcat_step_runs_total{pipeline="p",step="classify",step_type="ai",status="ok"} 1`) {
		t.Errorf("step counter not recorded with spans off:\n%s", mout)
	}
	if !strings.Contains(mout, `draftcat_pipeline_runs_total{pipeline="p",status="ok"} 1`) {
		t.Errorf("pipeline counter not recorded with spans off:\n%s", mout)
	}
}

// TestLowCardinalityLabels feeds a step name carrying a user id / email and
// asserts it appears ONLY as the `step` label, and that no label key named
// operator / user / payload is ever emitted (data-minimisation guard).
func TestLowCardinalityLabels(t *testing.T) {
	resetMetrics()
	defer resetMetrics()

	RecordStep("p", "user-12345@example.com", "ai", "ok", 10, 5, 0)
	RecordApproval("p", "gate", "approve")

	var buf bytes.Buffer
	_ = WriteMetrics(&buf)
	out := buf.String()

	if !strings.Contains(out, `step="user-12345@example.com"`) {
		t.Errorf("step name not used verbatim as step label:\n%s", out)
	}
	for _, forbidden := range []string{`operator="`, `user="`, `payload="`} {
		if strings.Contains(out, forbidden) {
			t.Errorf("forbidden high-cardinality label key %q present:\n%s", forbidden, out)
		}
	}
}
