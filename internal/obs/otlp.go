package obs

// Minimal OTLP/HTTP JSON trace exporter. We deliberately do NOT import the
// OpenTelemetry Go SDK (~40 transitive deps) for two span shapes — we hand-build
// the ResourceSpans JSON document. Export is best-effort and fire-and-forget: a
// failed export logs a warning and is dropped; it NEVER fails the pipeline.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// otlpSpan is the engine-neutral span record handed to the exporter.
type otlpSpan struct {
	name      string
	startNano int64
	endNano   int64
	status    string // "ok" | "error" | other
	attrs     map[string]string
}

// otlpExporter posts spans to an OTLP/HTTP collector. The http.Client is
// injectable so tests can supply a stub RoundTripper (no real socket).
type otlpExporter struct {
	endpoint string
	headers  map[string]string
	client   *http.Client
}

var otlpEx *otlpExporter

// EnableOTLP configures and turns on the OTLP/HTTP exporter. headers may be nil.
func EnableOTLP(endpoint string, headers map[string]string) {
	mu.Lock()
	defer mu.Unlock()
	otlpOn = true
	otlpEx = &otlpExporter{
		endpoint: endpoint,
		headers:  headers,
		client:   &http.Client{Timeout: 5 * time.Second},
	}
}

func otlpEnabled() bool {
	mu.Lock()
	defer mu.Unlock()
	return otlpOn
}

// fireOTLP builds one span and exports it asynchronously. Never blocks the
// caller; a failed export is logged and dropped.
func fireOTLP(spanType, name string, start time.Time, durMs int64, status string, fields map[string]interface{}) {
	mu.Lock()
	ex := otlpEx
	mu.Unlock()
	if ex == nil {
		return
	}
	if durMs <= 0 {
		durMs = 1 // guarantee start < end even for instantaneous spans
	}
	attrs := map[string]string{"draftcat.span": spanType}
	for k, v := range fields {
		if v == nil {
			continue
		}
		attrs[k] = fmt.Sprintf("%v", v)
	}
	sp := otlpSpan{
		name:      name,
		startNano: start.UnixNano(),
		endNano:   start.Add(time.Duration(durMs) * time.Millisecond).UnixNano(),
		status:    status,
		attrs:     attrs,
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := ex.export(ctx, []otlpSpan{sp}); err != nil {
			log.Printf("[obs] OTLP export dropped: %v", err)
		}
	}()
}

// export builds and POSTs the OTLP/HTTP JSON document. Returns an error on
// failure; callers treat success as best-effort.
func (e *otlpExporter) export(ctx context.Context, spans []otlpSpan) error {
	body, err := buildOTLPDocument(spans)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.headers {
		req.Header.Set(k, v)
	}
	client := e.client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("otlp endpoint returned %d", resp.StatusCode)
	}
	return nil
}

// buildOTLPDocument constructs the OTLP/HTTP JSON ResourceSpans document by
// hand. trace_id (16 bytes) and span_id (8 bytes) are hex-encoded per the
// OTLP/JSON mapping; 64-bit unix-nano timestamps are encoded as strings per the
// proto3 JSON rules.
func buildOTLPDocument(spans []otlpSpan) ([]byte, error) {
	type kv struct {
		Key   string `json:"key"`
		Value struct {
			StringValue string `json:"stringValue"`
		} `json:"value"`
	}
	mkAttrs := func(m map[string]string) []kv {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]kv, 0, len(keys))
		for _, k := range keys {
			var a kv
			a.Key = k
			a.Value.StringValue = m[k]
			out = append(out, a)
		}
		return out
	}

	type jsonSpan struct {
		TraceID           string `json:"traceId"`
		SpanID            string `json:"spanId"`
		Name              string `json:"name"`
		Kind              int    `json:"kind"`
		StartTimeUnixNano string `json:"startTimeUnixNano"`
		EndTimeUnixNano   string `json:"endTimeUnixNano"`
		Attributes        []kv   `json:"attributes"`
		Status            struct {
			Code int `json:"code"`
		} `json:"status"`
	}

	jspans := make([]jsonSpan, 0, len(spans))
	for _, s := range spans {
		traceID, err := randHex(16)
		if err != nil {
			return nil, err
		}
		spanID, err := randHex(8)
		if err != nil {
			return nil, err
		}
		var js jsonSpan
		js.TraceID = traceID
		js.SpanID = spanID
		js.Name = s.name
		js.Kind = 1 // SPAN_KIND_INTERNAL
		js.StartTimeUnixNano = strconv.FormatInt(s.startNano, 10)
		js.EndTimeUnixNano = strconv.FormatInt(s.endNano, 10)
		js.Attributes = mkAttrs(s.attrs)
		js.Status.Code = statusCode(s.status)
		jspans = append(jspans, js)
	}

	doc := map[string]interface{}{
		"resourceSpans": []map[string]interface{}{
			{
				"resource": map[string]interface{}{
					"attributes": []kv{mkServiceName()},
				},
				"scopeSpans": []map[string]interface{}{
					{
						"scope": map[string]interface{}{"name": "draftcat"},
						"spans": jspans,
					},
				},
			},
		},
	}
	return json.Marshal(doc)
}

func mkServiceName() (a struct {
	Key   string `json:"key"`
	Value struct {
		StringValue string `json:"stringValue"`
	} `json:"value"`
}) {
	a.Key = "service.name"
	a.Value.StringValue = "draftcat"
	return a
}

// statusCode maps a draftcat status to an OTLP StatusCode
// (0=UNSET, 1=OK, 2=ERROR).
func statusCode(status string) int {
	switch status {
	case "ok":
		return 1
	case "error":
		return 2
	default:
		return 0
	}
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ParseHeaderEnv parses a "key=value,key2=value2" string (from the configured
// OTLP header env var) into a header map. Empty input yields nil.
func ParseHeaderEnv(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if i := strings.IndexByte(part, '='); i >= 0 {
			k := strings.TrimSpace(part[:i])
			v := strings.TrimSpace(part[i+1:])
			if k != "" {
				out[k] = v
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
