package obs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// TestOTLPDocumentShape builds the OTLP/HTTP JSON body for one span and asserts
// the document structure without POSTing anywhere.
func TestOTLPDocumentShape(t *testing.T) {
	body, err := buildOTLPDocument([]otlpSpan{{
		name:      "pipe1",
		startNano: 1000,
		endNano:   2000,
		status:    "ok",
		attrs:     map[string]string{"draftcat.span": "pipeline", "steps_completed": "3"},
	}})
	if err != nil {
		t.Fatalf("buildOTLPDocument: %v", err)
	}

	var doc struct {
		ResourceSpans []struct {
			Resource struct {
				Attributes []struct {
					Key   string `json:"key"`
					Value struct {
						StringValue string `json:"stringValue"`
					} `json:"value"`
				} `json:"attributes"`
			} `json:"resource"`
			ScopeSpans []struct {
				Spans []struct {
					TraceID           string `json:"traceId"`
					SpanID            string `json:"spanId"`
					Name              string `json:"name"`
					StartTimeUnixNano string `json:"startTimeUnixNano"`
					EndTimeUnixNano   string `json:"endTimeUnixNano"`
					Status            struct {
						Code int `json:"code"`
					} `json:"status"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("document not valid JSON: %v\n%s", err, body)
	}

	if len(doc.ResourceSpans) != 1 {
		t.Fatalf("want 1 resourceSpans, got %d", len(doc.ResourceSpans))
	}
	rs := doc.ResourceSpans[0]

	var svc string
	for _, a := range rs.Resource.Attributes {
		if a.Key == "service.name" {
			svc = a.Value.StringValue
		}
	}
	if svc != "draftcat" {
		t.Errorf("resource service.name = %q, want draftcat", svc)
	}

	if len(rs.ScopeSpans) != 1 || len(rs.ScopeSpans[0].Spans) != 1 {
		t.Fatalf("want exactly one span")
	}
	sp := rs.ScopeSpans[0].Spans[0]
	if len(sp.TraceID) != 32 {
		t.Errorf("traceId = %q, want 32 hex chars", sp.TraceID)
	}
	if len(sp.SpanID) != 16 {
		t.Errorf("spanId = %q, want 16 hex chars", sp.SpanID)
	}
	start, _ := strconv.ParseInt(sp.StartTimeUnixNano, 10, 64)
	end, _ := strconv.ParseInt(sp.EndTimeUnixNano, 10, 64)
	if start >= end {
		t.Errorf("start (%d) should be < end (%d)", start, end)
	}
	if sp.Status.Code != 1 {
		t.Errorf("status code = %d, want 1 (OK)", sp.Status.Code)
	}
}

// errRoundTripper always fails, simulating an unroutable endpoint without a real
// socket.
type errRoundTripper struct{}

func (errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("simulated dial failure")
}

// TestOTLPExportNeverBlocksOnBadEndpoint asserts export returns promptly with an
// error (best-effort) when the transport fails — it must not hang.
func TestOTLPExportNeverBlocksOnBadEndpoint(t *testing.T) {
	ex := &otlpExporter{
		endpoint: "http://192.0.2.1:4318/v1/traces", // TEST-NET-1, unroutable
		client:   &http.Client{Transport: errRoundTripper{}, Timeout: 50 * time.Millisecond},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- ex.export(ctx, []otlpSpan{{name: "s", startNano: 1, endNano: 2, status: "ok"}})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected an error from a failing transport (best-effort export)")
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Errorf("export took %v, expected to return quickly", elapsed)
		}
	case <-time.After(800 * time.Millisecond):
		t.Fatal("export blocked — did not return within budget")
	}
}
