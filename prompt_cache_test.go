package main

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/renezander030/draftcat/internal/config"
)

// cacheServer records each request's user message content and answers with
// OpenRouter's usage shape.
type cacheServer struct {
	mu       sync.Mutex
	contents []json.RawMessage
	usage    string
}

func (s *cacheServer) handler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &req)
	s.mu.Lock()
	if len(req.Messages) > 0 {
		s.contents = append(s.contents, req.Messages[0].Content)
	}
	s.mu.Unlock()
	_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"billing"}}],"usage":`+s.usage+`,"model":"m"}`)
}

type contentPart struct {
	Type         string            `json:"type"`
	Text         string            `json:"text"`
	CacheControl map[string]string `json:"cache_control"`
}

func cacheConfig(t *testing.T, url, provider, model string) (*config.Config, config.PipelineConfig) {
	t.Helper()
	var cfg config.Config
	body := `
provider: {type: ` + provider + `, base_url: "` + url + `"}
models:
  m: {model: ` + model + `, max_tokens: 16, prompt_cache: true}
roles: {classifier: m}
pipelines:
  - name: classify
    steps:
      - name: classify
        type: ai
        role: classifier
        vars: {business: a plumbing company}
        prompt: "Classify messages for {{business}}. Long rules follow.\nMessage: {{text}}"
`
	if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}
	return &cfg, cfg.Pipelines[0]
}

func runCacheStep(t *testing.T, cfg *config.Config, p config.PipelineConfig, text string) {
	t.Helper()
	previous := state
	state = nil
	t.Cleanup(func() { state = previous })
	if err := runPipeline(cfg, p, &BudgetTracker{dayStart: time.Now()}, nil, nil, map[string]interface{}{"text": text}); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// An Anthropic model on OpenRouter with prompt_cache on gets the step's
// instructions (step vars filled in, up to the first data placeholder) as a
// text part marked for prompt caching, and the message data as a second,
// unmarked part, so two runs share one cacheable prefix.
func TestPromptCacheMarksStepInstructionsForAnthropic(t *testing.T) {
	s := &cacheServer{usage: `{"prompt_tokens":10,"completion_tokens":1}`}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()
	cfg, p := cacheConfig(t, srv.URL, "openrouter", "anthropic/claude-haiku-5.5")
	runCacheStep(t, cfg, p, "My invoice is wrong")
	runCacheStep(t, cfg, p, "Can we move Tuesday")

	const stable = "Classify messages for a plumbing company. Long rules follow.\nMessage: "
	if len(s.contents) != 2 {
		t.Fatalf("provider saw %d calls, want 2", len(s.contents))
	}
	for i, want := range []string{"My invoice is wrong", "Can we move Tuesday"} {
		var parts []contentPart
		if err := json.Unmarshal(s.contents[i], &parts); err != nil {
			t.Fatalf("call %d: content is not a list of parts: %s", i, s.contents[i])
		}
		if len(parts) != 2 || parts[0].Type != "text" || parts[0].Text != stable ||
			parts[0].CacheControl["type"] != "ephemeral" ||
			parts[1].Type != "text" || parts[1].Text != want || parts[1].CacheControl != nil {
			t.Fatalf("call %d: content %s, want the instructions marked ephemeral then %q unmarked", i, s.contents[i], want)
		}
	}
}

// Every other model or provider keeps the plain string it was always sent.
func TestPromptCacheLeavesOtherModelsAndProvidersPlain(t *testing.T) {
	for _, tc := range []struct{ provider, model string }{
		{"openrouter", "openai/gpt-4o-mini"},
		{"openai", "anthropic/claude-haiku-5.5"},
	} {
		s := &cacheServer{usage: `{"prompt_tokens":10,"completion_tokens":1}`}
		srv := httptest.NewServer(http.HandlerFunc(s.handler))
		cfg, p := cacheConfig(t, srv.URL, tc.provider, tc.model)
		runCacheStep(t, cfg, p, "hello")
		srv.Close()
		var plain string
		if len(s.contents) != 1 || json.Unmarshal(s.contents[0], &plain) != nil || !strings.HasSuffix(plain, "Message: hello") {
			t.Fatalf("%s %s: content %s, want the plain prompt string", tc.provider, tc.model, s.contents)
		}
	}
}

// A prompt with no placeholder is cacheable as a whole: one marked part.
func TestPromptCacheMarksWholeStaticPrompt(t *testing.T) {
	s := &cacheServer{usage: `{"prompt_tokens":10,"completion_tokens":1}`}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()
	cfg, p := cacheConfig(t, srv.URL, "openrouter", "~anthropic/claude-sonnet-latest")
	p.Steps[0].Prompt = "Summarize today's queue."
	runCacheStep(t, cfg, p, "")
	var parts []contentPart
	if len(s.contents) != 1 || json.Unmarshal(s.contents[0], &parts) != nil || len(parts) != 1 ||
		parts[0].Text != "Summarize today's queue." || parts[0].CacheControl["type"] != "ephemeral" {
		t.Fatalf("content %s, want one marked part holding the whole prompt", s.contents)
	}
}

// Without a provider-reported charge, a cache write is counted at the write
// price (1.25x input) and every input token, cached or not, still counts
// toward the token caps once.
func TestRatesCountPromptCacheWritesAtWritePrice(t *testing.T) {
	s := &cacheServer{usage: `{"prompt_tokens":1000,"completion_tokens":1000,` +
		`"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":800}}`}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()
	b := &BudgetTracker{dayStart: time.Now()}
	resp, err := callLLM(withRun(context.Background(), "run-1", "classify"), llmCfg(srv.URL, "openai"), "drafter", "p", b)
	if err != nil {
		t.Fatal(err)
	}
	// 1000 input at 1/1k + 800 written at the 0.25/1k premium + 1000 output at 2/1k.
	if math.Abs(resp.CostUSD-3.2) > 1e-9 {
		t.Fatalf("cost = %v, want 3.2 (cache writes at 1.25x the input rate)", resp.CostUSD)
	}
	snap := b.snapshot()
	if snap.tokensUsedToday != 2000 || math.Abs(snap.costToday-3.2) > 1e-9 {
		t.Fatalf("budget recorded %d tokens, %v cost; want 2000 tokens, 3.2 cost", snap.tokensUsedToday, snap.costToday)
	}
}
