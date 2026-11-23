package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	"github.com/renezander030/draftcat/internal/outputschema"
)

// completionWith is a paid provider reply carrying content as the model text.
func completionWith(content string) func(http.ResponseWriter) {
	body, _ := json.Marshal(map[string]interface{}{
		"choices": []map[string]interface{}{{"message": map[string]string{"content": content}}},
		"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5},
		"model":   "x",
	})
	return ok(string(body))
}

// scorePipeline is one ai step whose reply must be {"score": 1..5}.
func scorePipeline(url string) (*config.Config, config.PipelineConfig) {
	cfg := llmCfg(url, "openai")
	p := config.PipelineConfig{Name: "screen", Steps: []config.StepConfig{{
		Name: "classify", Type: "ai", Role: "drafter", Prompt: "Score this job.",
		OutputSchema: outputschema.Schema{"score": map[string]interface{}{"type": "int", "min": 1, "max": 5}},
	}}}
	return cfg, p
}

// promptOf returns the user prompt of one recorded provider request.
func promptOf(t *testing.T, body string) string {
	t.Helper()
	var req struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil || len(req.Messages) != 1 {
		t.Fatalf("provider request %q: %v", body, err)
	}
	return req.Messages[0].Content
}

// A reply that fails output_schema is asked for once more with the validation
// error, as its own budgeted call, and the run succeeds on a valid second reply.
func TestAIStepRetriesInvalidOutputOnceWithTheValidationError(t *testing.T) {
	st := useReliabilityStore(t)
	s := &llmServer{steps: []func(http.ResponseWriter){
		completionWith(`{"score": "five"}`),
		completionWith(`{"score": 4}`),
	}}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()
	cfg, p := scorePipeline(srv.URL)
	b := &BudgetTracker{dayStart: time.Now()}
	if err := b.attachStore(st); err != nil {
		t.Fatal(err)
	}

	if err := runPipeline(cfg, p, b, nil, nil, nil); err != nil {
		t.Fatalf("run failed although the second reply is valid: %v", err)
	}
	if s.hitCount() != 2 {
		t.Fatalf("provider requests = %d, want 2 (first call plus one retry)", s.hitCount())
	}
	retry := promptOf(t, s.bodies[1])
	if !strings.HasPrefix(retry, "Score this job.") || !strings.Contains(retry, "field score: expected int") {
		t.Fatalf("retry prompt does not carry the original prompt and the validation error: %q", retry)
	}
	if strings.Contains(retry, "five") {
		t.Fatalf("retry prompt quotes the rejected reply: %q", retry)
	}
	day, err := st.BudgetDay(context.Background(), time.Now())
	if err != nil || day.Tokens != 30 || day.Unsettled != 0 {
		t.Fatalf("budget ledger = %+v %v, want both calls settled (30 tokens)", day, err)
	}
	runs, err := st.RecentRuns("screen", 1)
	if err != nil || len(runs) != 1 || runs[0].Status != "ok" {
		t.Fatalf("recorded run = %+v %v", runs, err)
	}
}

// Retries stop at output_retries (1 when omitted, clamped to the maximum when a
// config skipped validation), and a retry the budget refuses is never sent.
func TestAIStepOutputRetriesAreBounded(t *testing.T) {
	n := func(v int) *int { return &v }
	cases := []struct {
		name      string
		retries   *int
		tokenCap  int
		wantHits  int
		wantError string
	}{
		{"default", nil, 0, 2, "output validation failed after 2 attempts: field score: expected int"},
		{"off", n(0), 0, 1, "[step:classify] output validation failed: field score: expected int"},
		{"clamped", n(99), 0, 1 + config.MaxOutputRetries, "after 4 attempts"},
		{"budget", n(3), 20, 1, "LLM call failed on output retry 1/3: BUDGET_BLOCKED: per-pipeline token limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := useReliabilityStore(t)
			s := &llmServer{steps: []func(http.ResponseWriter){completionWith(`{"score": "five"}`)}}
			srv := httptest.NewServer(http.HandlerFunc(s.handler))
			defer srv.Close()
			cfg, p := scorePipeline(srv.URL)
			p.Steps[0].OutputRetries = tc.retries
			cfg.Budgets.PerPipelineTokens = tc.tokenCap
			b := &BudgetTracker{dayStart: time.Now()}
			if err := b.attachStore(st); err != nil {
				t.Fatal(err)
			}
			err := runPipeline(cfg, p, b, nil, nil, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want %q", err, tc.wantError)
			}
			if s.hitCount() != tc.wantHits {
				t.Fatalf("provider requests = %d, want %d", s.hitCount(), tc.wantHits)
			}
			day, derr := st.BudgetDay(context.Background(), time.Now())
			if derr != nil || day.Tokens != 15*tc.wantHits || day.Unsettled != 0 {
				t.Fatalf("budget ledger = %+v %v, want %d settled call(s) of 15 tokens", day, derr, tc.wantHits)
			}
		})
	}
}

// A schema the operator got wrong is not the model's to fix: no retry is spent.
func TestOutputRetriesSkipABrokenSchema(t *testing.T) {
	step := config.StepConfig{Name: "classify", Type: "ai"}
	broken := map[string]interface{}{"score": map[string]interface{}{"type": "int", "min": 5, "max": 1}}
	if got := outputRetries(step, broken); got != 0 {
		t.Fatalf("broken schema retries = %d, want 0", got)
	}
	if got := outputRetries(step, nil); got != 0 {
		t.Fatalf("no schema retries = %d, want 0", got)
	}
	good := map[string]interface{}{"score": map[string]interface{}{"type": "int"}}
	if got := outputRetries(step, good); got != config.DefaultOutputRetries {
		t.Fatalf("default retries = %d, want %d", got, config.DefaultOutputRetries)
	}
}
