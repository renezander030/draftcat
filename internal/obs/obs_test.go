package obs

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// reset restores the package to its disabled default. Tests manipulate the
// unexported globals directly (white-box) since Enable has no inverse.
func reset() {
	mu.Lock()
	defer mu.Unlock()
	enabled = false
	out = os.Stderr
}

func TestDisabledIsNoop(t *testing.T) {
	reset()
	if Enabled() {
		t.Fatal("tracing should be disabled by default")
	}
	// Point out at a buffer but leave disabled — nothing must be written.
	var buf bytes.Buffer
	mu.Lock()
	out = &buf
	mu.Unlock()
	defer reset()

	EmitStep("p", "s", "ai", time.Now(), "ok", map[string]interface{}{"tokens": 1})
	Pipeline("p").End("ok", nil)

	if buf.Len() != 0 {
		t.Errorf("expected no output while disabled, got: %q", buf.String())
	}
}

func TestEmitStepAndPipeline(t *testing.T) {
	var buf bytes.Buffer
	Enable(&buf)
	defer reset()

	EmitStep("pipe1", "classify", "ai", time.Now().Add(-5*time.Millisecond), "ok",
		map[string]interface{}{"tokens": 42, "cost_usd": 0.001})
	Pipeline("pipe1").End("ok", map[string]interface{}{"steps_completed": 1})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 span lines, got %d: %q", len(lines), buf.String())
	}

	var step map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &step); err != nil {
		t.Fatalf("step span not valid JSON: %v", err)
	}
	if step["span"] != "step" || step["pipeline"] != "pipe1" || step["name"] != "classify" {
		t.Errorf("unexpected step span: %v", step)
	}
	if step["status"] != "ok" || step["step_type"] != "ai" {
		t.Errorf("unexpected step status/type: %v", step)
	}
	if _, ok := step["tokens"]; !ok {
		t.Error("expected tokens field on step span")
	}
	if _, ok := step["duration_ms"]; !ok {
		t.Error("expected duration_ms on step span")
	}

	var pipe map[string]interface{}
	if err := json.Unmarshal([]byte(lines[1]), &pipe); err != nil {
		t.Fatalf("pipeline span not valid JSON: %v", err)
	}
	if pipe["span"] != "pipeline" || pipe["pipeline"] != "pipe1" || pipe["status"] != "ok" {
		t.Errorf("unexpected pipeline span: %v", pipe)
	}
}

func TestNilFieldsDropped(t *testing.T) {
	var buf bytes.Buffer
	Enable(&buf)
	defer reset()

	EmitStep("p", "s", "deterministic", time.Now(), "skip",
		map[string]interface{}{"error": nil, "action": "notify"})

	var rec map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &rec); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if _, ok := rec["error"]; ok {
		t.Error("nil field 'error' should have been dropped")
	}
	if rec["action"] != "notify" {
		t.Errorf("expected action=notify, got %v", rec["action"])
	}
}
