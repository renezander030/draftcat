//go:build voice

package voice

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	t.Setenv("TEST_VOICE_TOKEN", "secret")

	cfg := Config{
		Enabled:    true,
		ListenAddr: "127.0.0.1:0",
		Auth:       AuthConfig{Method: "bearer", TokenEnv: "TEST_VOICE_TOKEN"},
		PreCall:    PreCallConfig{TimeoutMS: 300},
	}
	s, err := NewServer(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func post(t *testing.T, s *Server, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(w, req)
	return w
}

func TestAuthRequired(t *testing.T) {
	s := newTestServer(t)
	w := post(t, s, "/voice/session_start", map[string]any{"id": "s1"}, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestSessionStartHappy(t *testing.T) {
	s := newTestServer(t)
	w := post(t, s, "/voice/session_start", map[string]any{
		"id":           "sess_1",
		"caller_phone": "+491234567890",
		"workflow":     "dach-standard",
	}, "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestSessionStartRequiresID(t *testing.T) {
	s := newTestServer(t)
	w := post(t, s, "/voice/session_start", map[string]any{}, "secret")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestEventRequiresKind(t *testing.T) {
	s := newTestServer(t)
	w := post(t, s, "/voice/event", map[string]any{"session_id": "sess_1"}, "secret")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestOutcomePersistsAndCompletes(t *testing.T) {
	s := newTestServer(t)
	post(t, s, "/voice/session_start", map[string]any{"id": "sess_2", "workflow": "x"}, "secret")
	w := post(t, s, "/voice/session_end", map[string]any{
		"session_id":    "sess_2",
		"outcome":       map[string]string{"intent": "qualified"},
		"recording_url": "https://example/r.wav",
		"cost_cents":    1234,
	}, "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}

	row := s.store.db.QueryRow(`SELECT status, ended_at FROM voice_sessions WHERE id = 'sess_2'`)
	var status string
	var endedAt int64
	if err := row.Scan(&status, &endedAt); err != nil {
		t.Fatal(err)
	}
	if status != "completed" {
		t.Fatalf("status want completed, got %s", status)
	}
	if endedAt == 0 {
		t.Fatal("ended_at should be set")
	}
}

func TestHandoffReturnsPendingTarget(t *testing.T) {
	s := newTestServer(t)
	post(t, s, "/voice/session_start", map[string]any{"id": "sess_h", "workflow": "x"}, "secret")
	w := post(t, s, "/voice/handoff", map[string]any{
		"session_id": "sess_h",
		"reason":     "complex VPC question",
	}, "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var got map[string]any
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["target"] != "pending_review" {
		t.Fatalf("want target=pending_review, got %v", got["target"])
	}
}

func TestLearningRequiresDescription(t *testing.T) {
	s := newTestServer(t)
	w := post(t, s, "/voice/learning", map[string]any{"session_id": "sess_l"}, "secret")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestPreCallReturnsDefaultWorkflow(t *testing.T) {
	s := newTestServer(t)
	s.cfg.PreCall.RoutingRules = []RoutingRule{{Default: "standard-de"}}
	s.lookups = NewLookupRunner(s.cfg.PreCall)

	w := post(t, s, "/voice/pre_call_context", map[string]any{
		"caller_phone": "+491234567890",
	}, "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var res PreCallResult
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res.Workflow != "standard-de" {
		t.Fatalf("want workflow=standard-de, got %q", res.Workflow)
	}
}

func TestBearerTokenValidation(t *testing.T) {
	s := newTestServer(t)
	w := post(t, s, "/voice/session_start", map[string]any{"id": "s1"}, "wrong-token")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 with wrong token, got %d", w.Code)
	}
}

func TestCustomHTTPLookupMergesContext(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer crm-secret" {
			t.Errorf("want bearer header, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"support_tier":"enterprise","product":"DACH-VPC"}`))
	}))
	t.Cleanup(mock.Close)
	t.Setenv("CRM_TOKEN", "crm-secret")

	s := newTestServer(t)
	s.cfg.PreCall.Lookups = []LookupSpec{{
		Source: "custom_http",
		URL:    mock.URL,
		Header: "Authorization=Bearer {{env.CRM_TOKEN}}",
	}}
	s.cfg.PreCall.RoutingRules = []RoutingRule{
		{If: "support_tier == 'enterprise'", Workflow: "dach-enterprise"},
		{Default: "standard-de"},
	}
	s.lookups = NewLookupRunner(s.cfg.PreCall)

	w := post(t, s, "/voice/pre_call_context", map[string]any{
		"caller_phone": "+491111111111",
	}, "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	var res PreCallResult
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res.Workflow != "dach-enterprise" {
		t.Fatalf("want workflow=dach-enterprise, got %q", res.Workflow)
	}
	if !strings.Contains(string(res.ContextVars), "enterprise") {
		t.Fatalf("expected context_vars to include enterprise, got %s", res.ContextVars)
	}
}

func TestRegisteredBackendIsCalled(t *testing.T) {
	s := newTestServer(t)
	called := 0
	s.cfg.PreCall.Lookups = []LookupSpec{{Source: "ghl"}}
	s.cfg.PreCall.RoutingRules = []RoutingRule{
		{If: "support_tier == 'priority'", Workflow: "priority-de"},
		{Default: "standard-de"},
	}
	s.lookups = NewLookupRunner(s.cfg.PreCall)
	s.lookups.Register("ghl", func(_ context.Context, phone string) (map[string]any, error) {
		called++
		if phone != "+491111111111" {
			t.Errorf("unexpected phone %q", phone)
		}
		return map[string]any{"support_tier": "priority", "account_name": "Acme"}, nil
	})

	w := post(t, s, "/voice/pre_call_context", map[string]any{
		"caller_phone": "+491111111111",
	}, "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if called != 1 {
		t.Fatalf("backend called %d times, want 1", called)
	}
	var res PreCallResult
	_ = json.NewDecoder(w.Body).Decode(&res)
	if res.Workflow != "priority-de" {
		t.Fatalf("want priority-de, got %q", res.Workflow)
	}
}

func TestLookupCacheServesRepeats(t *testing.T) {
	calls := 0
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"support_tier":"basic"}`))
	}))
	t.Cleanup(mock.Close)

	s := newTestServer(t)
	s.cfg.PreCall.Lookups = []LookupSpec{{Source: "custom_http", URL: mock.URL}}
	s.cfg.PreCall.RoutingRules = []RoutingRule{{Default: "standard-de"}}
	s.lookups = NewLookupRunner(s.cfg.PreCall)

	for i := 0; i < 3; i++ {
		w := post(t, s, "/voice/pre_call_context", map[string]any{"caller_phone": "+49000"}, "secret")
		if w.Code != http.StatusOK {
			t.Fatalf("iter %d: want 200, got %d", i, w.Code)
		}
	}
	if calls != 1 {
		t.Fatalf("backend called %d times, want 1 (cache should serve repeats)", calls)
	}
}

func TestMissingBackendEmitsWarning(t *testing.T) {
	s := newTestServer(t)
	s.cfg.PreCall.Lookups = []LookupSpec{{Source: "salesforce"}}
	s.lookups = NewLookupRunner(s.cfg.PreCall)
	w := post(t, s, "/voice/pre_call_context", map[string]any{"caller_phone": "+49222"}, "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var res PreCallResult
	_ = json.NewDecoder(w.Body).Decode(&res)
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "salesforce") {
		t.Fatalf("expected warning about missing salesforce backend, got %v", res.Warnings)
	}
}

func TestResolveHandoffPersists(t *testing.T) {
	s := newTestServer(t)
	post(t, s, "/voice/session_start", map[string]any{"id": "sess_rh", "workflow": "x"}, "secret")
	w := post(t, s, "/voice/handoff", map[string]any{"session_id": "sess_rh", "reason": "VPC question"}, "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var hresp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&hresp)
	id, _ := hresp["handoff_id"].(float64)

	if err := s.store.ResolveHandoff(int64(id), "sales_engineering", 12345); err != nil {
		t.Fatal(err)
	}
	row := s.store.db.QueryRow(`SELECT target_resolved, resolved_at FROM voice_handoffs WHERE id = ?`, int64(id))
	var target string
	var resolvedAt int64
	if err := row.Scan(&target, &resolvedAt); err != nil {
		t.Fatal(err)
	}
	if target != "sales_engineering" || resolvedAt != 12345 {
		t.Fatalf("want sales_engineering/12345, got %s/%d", target, resolvedAt)
	}

	// Subsequent FetchPendingHandoffs should not return the resolved one.
	pending, err := s.store.FetchPendingHandoffs(10)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if p.ID == int64(id) {
			t.Fatalf("resolved handoff %d still listed as pending", p.ID)
		}
	}
}
