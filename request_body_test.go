package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The gate endpoints that take a JSON body answer the same way to a body they
// cannot use: too large is 413, malformed or ambiguous JSON is 400.
func TestGateEndpoints_RejectUnusableBodies(t *testing.T) {
	g := newToolGate(gateCfg(), nil)
	endpoints := []struct {
		name  string
		path  string
		limit int
		serve http.HandlerFunc
	}{
		{"tool-call", toolGatePath, 1 << 20, g.HandleCall},
		{"consume", toolGatePath + "/a1/consume", 4096, g.HandleStatus},
		{"revoke", toolGatePath + "/a1/revoke", 4096, g.HandleStatus},
		{"complete", toolGatePath + "/a1/complete", 4096, g.HandleStatus},
	}
	bodies := []struct {
		name string
		body func(limit int) string
		code int
		text string
	}{
		{"malformed", func(int) string { return `{"binding_hash":` }, http.StatusBadRequest, "malformed json"},
		{"duplicate key", func(int) string { return `{"binding_hash":"a","binding_hash":"b"}` }, http.StatusBadRequest, "malformed json"},
		{"unknown field", func(int) string { return `{"nope":1}` }, http.StatusBadRequest, "malformed json"},
		{"trailing content", func(int) string { return `{} {}` }, http.StatusBadRequest, "malformed json"},
		{"too large", func(limit int) string { return `{"x":"` + strings.Repeat("a", limit) + `"}` }, http.StatusRequestEntityTooLarge, "request body too large"},
	}
	for _, e := range endpoints {
		for _, b := range bodies {
			t.Run(e.name+"/"+b.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				e.serve(rec, httptest.NewRequest(http.MethodPost, e.path, strings.NewReader(b.body(e.limit))))
				if rec.Code != b.code || strings.TrimSpace(rec.Body.String()) != b.text {
					t.Fatalf("got %d %q, want %d %q", rec.Code, rec.Body.String(), b.code, b.text)
				}
			})
		}
	}
}
