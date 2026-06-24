package obs

// Hand-rolled Prometheus exposition. We deliberately do NOT import
// prometheus/client_golang: the text exposition format is stable and trivial,
// and the lean single-binary property is a selling point. All series carry a
// LOW-CARDINALITY label set (pipeline, step, status, decision, step_type) and
// never user IDs or payload content — that is a data-minimisation requirement,
// not a style choice.

import (
	"context"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// durationBuckets are the cumulative histogram upper bounds (milliseconds),
// shared by the pipeline and step duration histograms.
var durationBuckets = []float64{50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 300000}

type counterVec struct {
	name string
	help string
	vals map[string]float64 // key = inner label block, e.g. `pipeline="p",status="ok"`
}

func newCounter(name, help string) *counterVec {
	return &counterVec{name: name, help: help, vals: map[string]float64{}}
}

func (c *counterVec) add(labels string, v float64) { c.vals[labels] += v }

func (c *counterVec) render(sb *strings.Builder) {
	if len(c.vals) == 0 {
		return
	}
	sb.WriteString("# HELP " + c.name + " " + c.help + "\n")
	sb.WriteString("# TYPE " + c.name + " counter\n")
	for _, k := range sortedKeys(c.vals) {
		sb.WriteString(c.name + "{" + k + "} " + formatNum(c.vals[k]) + "\n")
	}
}

type histData struct {
	counts []uint64 // cumulative count of observations <= durationBuckets[i]
	sum    float64
	count  uint64
}

type histVec struct {
	name   string
	help   string
	series map[string]*histData
}

func newHist(name, help string) *histVec {
	return &histVec{name: name, help: help, series: map[string]*histData{}}
}

func (h *histVec) observe(labels string, v float64) {
	d := h.series[labels]
	if d == nil {
		d = &histData{counts: make([]uint64, len(durationBuckets))}
		h.series[labels] = d
	}
	for i, b := range durationBuckets {
		if v <= b {
			d.counts[i]++
		}
	}
	d.sum += v
	d.count++
}

func (h *histVec) render(sb *strings.Builder) {
	if len(h.series) == 0 {
		return
	}
	sb.WriteString("# HELP " + h.name + " " + h.help + "\n")
	sb.WriteString("# TYPE " + h.name + " histogram\n")
	for _, k := range sortedKeysH(h.series) {
		d := h.series[k]
		for i, b := range durationBuckets {
			sb.WriteString(h.name + "_bucket{" + k + ",le=\"" + formatNum(b) + "\"} " + strconv.FormatUint(d.counts[i], 10) + "\n")
		}
		sb.WriteString(h.name + "_bucket{" + k + ",le=\"+Inf\"} " + strconv.FormatUint(d.count, 10) + "\n")
		sb.WriteString(h.name + "_sum{" + k + "} " + formatNum(d.sum) + "\n")
		sb.WriteString(h.name + "_count{" + k + "} " + strconv.FormatUint(d.count, 10) + "\n")
	}
}

// registry holds every series. Guarded by its own mutex (independent of the
// JSON-span mutex) so metric recording never contends with span writes.
type registry struct {
	mu sync.Mutex

	pipelineRuns *counterVec
	pipelineDur  *histVec
	stepRuns     *counterVec
	stepDur      *histVec
	aiTokens     *counterVec
	aiCost       *counterVec
	approvals    *counterVec
}

func newRegistry() *registry {
	return &registry{
		pipelineRuns: newCounter("draftcat_pipeline_runs_total", "Total pipeline runs by status."),
		pipelineDur:  newHist("draftcat_pipeline_duration_ms", "Pipeline run duration in milliseconds."),
		stepRuns:     newCounter("draftcat_step_runs_total", "Total step runs by type and status."),
		stepDur:      newHist("draftcat_step_duration_ms", "Step duration in milliseconds."),
		aiTokens:     newCounter("draftcat_ai_tokens_total", "Total AI tokens consumed."),
		aiCost:       newCounter("draftcat_ai_cost_usd_total", "Total AI cost in USD."),
		approvals:    newCounter("draftcat_approvals_total", "Total approval decisions by outcome."),
	}
}

var reg = newRegistry()

// resetMetrics clears the registry and disables metric recording. Test-only.
// It reassigns the metric vecs (which hold no locks) without replacing reg.mu.
func resetMetrics() {
	mu.Lock()
	promOn = false
	otlpOn = false
	mu.Unlock()
	fresh := newRegistry()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.pipelineRuns = fresh.pipelineRuns
	reg.pipelineDur = fresh.pipelineDur
	reg.stepRuns = fresh.stepRuns
	reg.stepDur = fresh.stepDur
	reg.aiTokens = fresh.aiTokens
	reg.aiCost = fresh.aiCost
	reg.approvals = fresh.approvals
}

// RecordPipeline records a completed pipeline run. Pure registry update — always
// records regardless of which exporter flag is set; the call sites gate on
// prometheusOn(). status is "ok" | "error".
func RecordPipeline(pipeline, status string, durationMs int64) {
	lbl := lbls("pipeline", pipeline, "status", status)
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.pipelineRuns.add(lbl, 1)
	reg.pipelineDur.observe(lbl, float64(durationMs))
}

// RecordStep records a completed step. tokens/cost series are only created when
// non-zero (keeps the surface to AI steps that actually spent budget).
func RecordStep(pipeline, step, stepType, status string, durationMs int64, tokens int, costUSD float64) {
	runLbl := lbls("pipeline", pipeline, "step", step, "step_type", stepType, "status", status)
	durLbl := lbls("pipeline", pipeline, "step", step, "status", status)
	aiLbl := lbls("pipeline", pipeline, "step", step)
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.stepRuns.add(runLbl, 1)
	reg.stepDur.observe(durLbl, float64(durationMs))
	if tokens > 0 {
		reg.aiTokens.add(aiLbl, float64(tokens))
	}
	if costUSD > 0 {
		reg.aiCost.add(aiLbl, costUSD)
	}
}

// RecordApproval increments the approval-decision counter. decision ∈
// approve|skip|adjust|timeout|quorum_fail. No operator ID is ever recorded as a
// label (data minimisation) — operator identity lives only in the durable
// action_approvals audit table.
func RecordApproval(pipeline, step, decision string) {
	lbl := lbls("pipeline", pipeline, "step", step, "decision", decision)
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.approvals.add(lbl, 1)
}

// WriteMetrics renders the full text exposition to w.
func WriteMetrics(w io.Writer) error {
	reg.mu.Lock()
	var sb strings.Builder
	reg.pipelineRuns.render(&sb)
	reg.pipelineDur.render(&sb)
	reg.stepRuns.render(&sb)
	reg.stepDur.render(&sb)
	reg.aiTokens.render(&sb)
	reg.aiCost.render(&sb)
	reg.approvals.render(&sb)
	reg.mu.Unlock()
	_, err := io.WriteString(w, sb.String())
	return err
}

// ServePrometheus starts an HTTP server exposing the metrics at path. Binds the
// given addr (localhost by default at the call site). Returns an io.Closer that
// gracefully shuts the server down. A nil/empty addr or path falls back to the
// documented defaults.
func ServePrometheus(addr, path string) (io.Closer, error) {
	if addr == "" {
		addr = "127.0.0.1:9090"
	}
	if path == "" {
		path = "/metrics"
	}
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = WriteMetrics(w)
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		_ = srv.ListenAndServe() // returns ErrServerClosed on graceful shutdown
	}()
	return prometheusCloser{srv}, nil
}

type prometheusCloser struct{ srv *http.Server }

func (c prometheusCloser) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return c.srv.Shutdown(ctx)
}

// --- label helpers ---

// lbls builds a Prometheus inner label block (no braces) from alternating
// key,value pairs, preserving order. Values are escaped per the exposition
// format. Odd trailing args are ignored.
func lbls(pairs ...string) string {
	var sb strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(pairs[i])
		sb.WriteString(`="`)
		sb.WriteString(escapeLabel(pairs[i+1]))
		sb.WriteByte('"')
	}
	return sb.String()
}

func escapeLabel(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	r := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n")
	return r.Replace(s)
}

// formatNum renders whole numbers without a decimal point (so counters read
// `800`, not `800.000000`) and fractional values compactly (`0.004`).
func formatNum(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysH(m map[string]*histData) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
