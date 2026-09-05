package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
)

func llmCfg(base, typ string) *config.Config {
	return &config.Config{
		Provider: config.ProviderConfig{Type: typ, BaseURL: base},
		Models:   map[string]config.ModelConfig{"m": {Model: "x", MaxTokens: 10, CostIn: 1, CostOut: 2}},
		Roles:    map[string]string{"drafter": "m"},
	}
}

// llmServer scripts a sequence of responses and records every request body.
type llmServer struct {
	mu     sync.Mutex
	steps  []func(w http.ResponseWriter)
	bodies []string
	hits   int
}

func (s *llmServer) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.bodies = append(s.bodies, string(b))
	i := s.hits
	s.hits++
	s.mu.Unlock()
	if i >= len(s.steps) {
		i = len(s.steps) - 1
	}
	s.steps[i](w)
}

func (s *llmServer) hitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

func status(code int, retryAfter string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
	}
}

const okWithCost = `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":1000,"completion_tokens":1000,"cost":0.0123,` +
	`"prompt_tokens_details":{"cached_tokens":500},"completion_tokens_details":{"reasoning_tokens":200}},"model":"x"}`
const okNoCost = `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":1000,"completion_tokens":1000},"model":"x"}`

func ok(body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) { _, _ = w.Write([]byte(body)) }
}

func TestCallLLM_RetriesRateLimitAndUsesProviderCost(t *testing.T) {
	s := &llmServer{steps: []func(http.ResponseWriter){status(429, "0"), ok(okWithCost)}}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()

	resp, err := callLLM(context.Background(), llmCfg(srv.URL, "openrouter"), "drafter", "p")
	if err != nil {
		t.Fatalf("callLLM: %v", err)
	}
	if resp.Attempts != 2 || s.hitCount() != 2 {
		t.Fatalf("attempts = %d (server hits %d), want 2 — a 429 must be retried", resp.Attempts, s.hitCount())
	}
	if resp.CostUSD != 0.0123 || resp.CostSource != "provider" {
		t.Fatalf("cost = %v from %q, want the provider's 0.0123", resp.CostUSD, resp.CostSource)
	}
	if resp.CachedTokens != 500 || resp.ReasoningTokens != 200 {
		t.Fatalf("token breakdown = cached %d reasoning %d, want 500/200", resp.CachedTokens, resp.ReasoningTokens)
	}
	var req map[string]interface{}
	if err := json.Unmarshal([]byte(s.bodies[0]), &req); err != nil {
		t.Fatalf("request body: %v", err)
	}
	if u, _ := req["usage"].(map[string]interface{}); u == nil || u["include"] != true {
		t.Fatalf("request to OpenRouter lacks usage.include: %s", s.bodies[0])
	}
}

func TestCallLLM_OtherProvidersGetNoUsageFieldAndRates(t *testing.T) {
	s := &llmServer{steps: []func(http.ResponseWriter){ok(okNoCost)}}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()

	resp, err := callLLM(context.Background(), llmCfg(srv.URL, "openai"), "drafter", "p")
	if err != nil {
		t.Fatalf("callLLM: %v", err)
	}
	if resp.CostSource != "rates" || resp.CostUSD != 3 {
		t.Fatalf("cost = %v from %q, want 3 from rates (1000/1k*1 + 1000/1k*2)", resp.CostUSD, resp.CostSource)
	}
	if strings.Contains(s.bodies[0], `"usage"`) {
		t.Fatalf("request to a non-OpenRouter endpoint must not carry usage.include: %s", s.bodies[0])
	}
}

func TestCallLLM_ProviderCostIgnoredWhenAccountingOff(t *testing.T) {
	s := &llmServer{steps: []func(http.ResponseWriter){ok(okWithCost)}}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()
	cfg := llmCfg(srv.URL, "openrouter")
	off := false
	cfg.Provider.UsageAccounting = &off

	resp, err := callLLM(context.Background(), cfg, "drafter", "p")
	if err != nil {
		t.Fatalf("callLLM: %v", err)
	}
	if resp.CostSource != "rates" || resp.CostUSD != 3 {
		t.Fatalf("cost = %v from %q, want rates when usage_accounting is off", resp.CostUSD, resp.CostSource)
	}
}

func TestCallLLM_ClientErrorIsNotRetried(t *testing.T) {
	s := &llmServer{steps: []func(http.ResponseWriter){status(400, "0")}}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()

	if _, err := callLLM(context.Background(), llmCfg(srv.URL, "openrouter"), "drafter", "p"); err == nil {
		t.Fatal("a 400 must fail the call")
	}
	if s.hitCount() != 1 {
		t.Fatalf("server hit %d time(s), want 1 — a 400 is not transient", s.hitCount())
	}
}

func TestCallLLM_GivesUpAfterTheRetryBudget(t *testing.T) {
	s := &llmServer{steps: []func(http.ResponseWriter){status(503, "0")}}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()
	cfg := llmCfg(srv.URL, "openrouter")
	cfg.Provider.MaxRetries = 2

	_, err := callLLM(context.Background(), cfg, "drafter", "p")
	if err == nil || !strings.Contains(err.Error(), "gave up after 3 attempt(s)") {
		t.Fatalf("err = %v, want a give-up after 3 attempts", err)
	}
	if s.hitCount() != 3 {
		t.Fatalf("server hit %d time(s), want 3", s.hitCount())
	}
}

func TestCallLLM_CancelledDuringBackoffReturnsPromptly(t *testing.T) {
	s := &llmServer{steps: []func(http.ResponseWriter){status(429, "30")}}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := callLLM(ctx, llmCfg(srv.URL, "openrouter"), "drafter", "p")
	if err == nil || !strings.Contains(err.Error(), "abandoned") {
		t.Fatalf("err = %v, want the call abandoned on ctx expiry", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %s to notice the cancelled context", time.Since(start))
	}
}

func TestRetryDelay_HonorsRetryAfterAndCaps(t *testing.T) {
	now := time.Now()
	if d := retryDelay(1, "5", now); d != 5*time.Second {
		t.Fatalf("Retry-After: 5 → %s, want 5s", d)
	}
	if d := retryDelay(1, "3600", now); d != 30*time.Second {
		t.Fatalf("Retry-After: 3600 → %s, want the 30s cap", d)
	}
	at := now.Add(10 * time.Second).UTC().Format(http.TimeFormat)
	if d := retryDelay(1, at, now); d < 9*time.Second || d > 10*time.Second {
		t.Fatalf("Retry-After: <http-date> → %s, want ~10s", d)
	}
	for i := 0; i < 20; i++ {
		if d := retryDelay(1, "", now); d < 750*time.Millisecond || d > 1250*time.Millisecond {
			t.Fatalf("retry 1 backoff %s outside 1s ±25%%", d)
		}
		if d := retryDelay(2, "", now); d < 1500*time.Millisecond || d > 2500*time.Millisecond {
			t.Fatalf("retry 2 backoff %s outside 2s ±25%%", d)
		}
		if d := retryDelay(10, "", now); d > 12500*time.Millisecond {
			t.Fatalf("retry 10 backoff %s exceeds the 10s cap (+jitter)", d)
		}
	}
}

func TestLLMRetryable(t *testing.T) {
	for code, want := range map[int]bool{429: true, 408: true, 500: true, 503: true, 400: false, 401: false, 404: false} {
		if got := llmRetryable(code); got != want {
			t.Errorf("llmRetryable(%d) = %v, want %v", code, got, want)
		}
	}
}
