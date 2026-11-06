package obs

import (
	"bytes"
	"strings"
	"testing"
)

func TestShadowCountersRender(t *testing.T) {
	resetMetrics()
	defer resetMetrics()

	var buf bytes.Buffer
	_ = WriteMetrics(&buf)
	if strings.Contains(buf.String(), "shadow") {
		t.Fatalf("no shadow series before anything is recorded:\n%s", buf.String())
	}

	RecordShadowDifference("send_email", "allow", "deny")
	RecordShadowDifference("send_email", "allow", "deny")
	RecordShadowError()
	buf.Reset()
	_ = WriteMetrics(&buf)
	out := buf.String()
	for _, want := range []string{
		"# TYPE draftcat_gate_shadow_differences_total counter",
		`draftcat_gate_shadow_differences_total{tool="send_email",live="allow",shadow="deny"} 2`,
		"draftcat_gate_shadow_errors_total 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition lacks %q:\n%s", want, out)
		}
	}
}
