// Package obs emits structured, single-line JSON spans for pipeline and step
// execution. It is the lightweight, dependency-free observability seam: one
// span per pipeline run and one per step, carrying duration, status, and (for
// AI steps) token + cost attributes. A future OpenTelemetry/Prometheus exporter
// can wrap the same Emit surface without touching the engine.
//
// Disabled by default. main calls Enable(...) when config sets
// `observability.spans: true` or the DRAFTCAT_TRACE env var is set, so when off
// every call here is a cheap no-op and callers need no guards.
package obs

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

var (
	mu      sync.Mutex
	enabled bool      // JSON span emission (the original behavior)
	promOn  bool      // Prometheus metric recording
	otlpOn  bool      // OTLP/HTTP trace export
	out     io.Writer = os.Stderr
)

// Enable turns JSON span emission on and (optionally) overrides the output
// writer. A nil writer keeps the default (stderr). Safe to call once at startup.
func Enable(w io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	enabled = true
	if w != nil {
		out = w
	}
}

// Enabled reports whether JSON span emission is on.
func Enabled() bool {
	mu.Lock()
	defer mu.Unlock()
	return enabled
}

// EnablePrometheus turns on in-process metric recording. Independent of JSON
// span emission: counters/histograms are updated even when Spans is off.
func EnablePrometheus() {
	mu.Lock()
	defer mu.Unlock()
	promOn = true
}

// prometheusOn reports whether metric recording is on.
func prometheusOn() bool {
	mu.Lock()
	defer mu.Unlock()
	return promOn
}

// active reports whether ANY observability seam needs a live span: JSON spans,
// Prometheus metrics, or OTLP export. When all are off, span creation is a
// cheap no-op and the engine needs no guards.
func active() bool {
	mu.Lock()
	defer mu.Unlock()
	return enabled || promOn || otlpOn
}

// Span is an in-flight pipeline-level timing record. The zero value (returned
// when tracing is disabled) is safe to End.
type Span struct {
	name  string
	start time.Time
	live  bool
}

// Pipeline starts a pipeline-level span. End it (typically via defer) with the
// final status and any aggregate fields (steps_completed, tokens, error).
func Pipeline(name string) *Span {
	if !active() {
		return &Span{}
	}
	return &Span{name: name, start: time.Now(), live: true}
}

// End writes the pipeline span. status is "ok" | "error". fields carries extra
// structured attributes; nil values are dropped.
func (s *Span) End(status string, fields map[string]interface{}) {
	if s == nil || !s.live {
		return
	}
	durMs := time.Since(s.start).Milliseconds()
	if Enabled() {
		rec := map[string]interface{}{
			"span":        "pipeline",
			"pipeline":    s.name,
			"status":      status,
			"duration_ms": durMs,
		}
		merge(rec, fields)
		write(rec)
	}
	if prometheusOn() {
		RecordPipeline(s.name, status, durMs)
	}
	if otlpEnabled() {
		fireOTLP("pipeline", s.name, s.start, durMs, status, fields)
	}
}

// EmitStep writes a completed step span. The caller owns the start time so the
// engine can record a step's span from any exit path (normal, skip, error)
// without restructuring its control flow. status is "ok" | "skip" | "error".
func EmitStep(pipeline, name, stepType string, start time.Time, status string, fields map[string]interface{}) {
	if !active() {
		return
	}
	durMs := time.Since(start).Milliseconds()
	if Enabled() {
		rec := map[string]interface{}{
			"span":        "step",
			"pipeline":    pipeline,
			"name":        name,
			"step_type":   stepType,
			"status":      status,
			"duration_ms": durMs,
		}
		merge(rec, fields)
		write(rec)
	}
	if prometheusOn() {
		tokens, _ := fields["tokens"].(int)
		cost, _ := fields["cost_usd"].(float64)
		RecordStep(pipeline, name, stepType, status, durMs, tokens, cost)
	}
	if otlpEnabled() {
		fireOTLP("step", pipeline+"/"+name, start, durMs, status, fields)
	}
}

func merge(rec, fields map[string]interface{}) {
	for k, v := range fields {
		if v == nil {
			continue
		}
		rec[k] = v
	}
}

func write(rec map[string]interface{}) {
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	_, _ = out.Write(append(b, '\n'))
}
