package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

func providerBudgetConfig(url string) *config.Config {
	cfg := llmCfg(url, "openrouter")
	cfg.Models["m"] = config.ModelConfig{Model: "x", MaxTokens: 1}
	cfg.Roles["classifier"] = "m"
	return cfg
}

func providerBudgetStore(t *testing.T) *statestore.StateStore {
	t.Helper()
	st, err := statestore.OpenStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

const smallPaidCompletion = `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"cost":0.6}}`

func TestBudgetConcurrentAdmissionStopsAfterSettledCap(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, smallPaidCompletion)
	}))
	defer srv.Close()
	cfg := providerBudgetConfig(srv.URL)
	cfg.Budgets.PerDayCost = .5
	b := &BudgetTracker{dayStart: time.Now()}
	if err := b.attachStore(providerBudgetStore(t)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var succeeded atomic.Int32
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := callLLM(context.Background(), cfg, "drafter", "p", b.newRun(0)); err == nil {
				succeeded.Add(1)
			}
		}()
	}
	wg.Wait()
	if hits.Load() != 1 || succeeded.Load() != 1 {
		t.Fatalf("provider hits=%d successes=%d, want exactly one paid call", hits.Load(), succeeded.Load())
	}
	snapshot := b.snapshot()
	if snapshot.costToday != .6 || snapshot.tokensUsedToday != 2 || snapshot.unsettled != 0 {
		t.Fatalf("settled snapshot: %+v", snapshot)
	}
}

func TestBudgetRunCountersStayIndependent(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, smallPaidCompletion)
	}))
	defer srv.Close()
	cfg := providerBudgetConfig(srv.URL)
	cfg.Budgets.PerPipelineCost = .5
	b := &BudgetTracker{dayStart: time.Now()}
	a, c := b.newRun(10), b.newRun(10)
	for _, run := range []*BudgetTracker{a, c} {
		if _, err := callLLM(context.Background(), cfg, "drafter", "p", run); err != nil {
			t.Fatal(err)
		}
		if _, err := callLLM(context.Background(), cfg, "drafter", "p", run); err == nil {
			t.Fatal("run exceeded its own cost threshold")
		}
	}
	if hits.Load() != 2 || a.snapshot().tokensUsedPipeline != 2 || c.snapshot().tokensUsedPipeline != 2 || b.snapshot().costToday != 1.2 {
		t.Fatalf("hits=%d a=%+v c=%+v day=%+v", hits.Load(), a.snapshot(), c.snapshot(), b.snapshot())
	}
}

func TestBudgetPipelineTokenOverrunChargesAndHaltsOutput(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":1,"cost":0.6}}`)
	}))
	defer srv.Close()
	cfg := providerBudgetConfig(srv.URL)
	b := &BudgetTracker{dayStart: time.Now()}
	run := b.newRun(5)
	response, err := callLLM(context.Background(), cfg, "drafter", "p", run)
	if response != nil || err == nil || !strings.Contains(err.Error(), "per-pipeline token limit") {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	if _, err := callLLM(context.Background(), cfg, "drafter", "p", run); err == nil {
		t.Fatal("overrun was admitted again")
	}
	if hits.Load() != 1 || b.snapshot().tokensUsedToday != 6 || run.snapshot().tokensUsedPipeline != 6 {
		t.Fatalf("lost charged usage: %+v", b.snapshot())
	}
}

func TestBudgetDailyTokenOverrunChargesAndHaltsOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, smallPaidCompletion) }))
	defer srv.Close()
	cfg := providerBudgetConfig(srv.URL)
	cfg.Budgets.PerDayTokens = 1
	b := &BudgetTracker{dayStart: time.Now()}
	response, err := callLLM(context.Background(), cfg, "classifier", "p", b)
	if response != nil || err == nil || !strings.Contains(err.Error(), "daily token limit") || b.snapshot().tokensUsedToday != 2 {
		t.Fatalf("response=%+v err=%v usage=%+v", response, err, b.snapshot())
	}
}

func TestBudgetDeniedOutputStillChargesPaidUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, smallPaidCompletion) }))
	defer srv.Close()
	cfg := providerBudgetConfig(srv.URL)
	cfg.Budgets.PerDayCost = .5
	cfg.ModelPolicy.Rules = []config.ModelPolicyRule{{ID: "output-denied", Phase: "output", Pattern: "hi", Action: "deny", Reason: "blocked claim"}}
	b := &BudgetTracker{dayStart: time.Now()}
	st := providerBudgetStore(t)
	if err := b.attachStore(st); err != nil {
		t.Fatal(err)
	}
	if response, err := callLLM(context.Background(), cfg, "drafter", "p", b); response != nil || err == nil || !strings.Contains(err.Error(), "blocked by policy") {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	day, err := st.BudgetDay(context.Background(), time.Now())
	if err != nil || day.Cost != .6 || day.Tokens != 2 || day.Unsettled != 0 {
		t.Fatalf("paid denied usage=%+v err=%v", day, err)
	}
	restarted := &BudgetTracker{dayStart: time.Now()}
	if err := restarted.attachStore(st); err != nil {
		t.Fatal(err)
	}
	if _, err := callLLM(context.Background(), cfg, "classifier", "intent", restarted); err == nil || !strings.Contains(err.Error(), "cost limit") {
		t.Fatalf("restart allowed paid intent classification: %v", err)
	}
}

type budgetReviewChannel struct {
	stubApprovalChannel
	entered chan struct{}
	done    chan struct{}
}

func (s *budgetReviewChannel) SendForApproval(ctx context.Context, _ string, _ []int64) (OperatorDecision, error) {
	close(s.entered)
	select {
	case <-s.done:
		return OperatorDecision{Action: "approve", ApproverID: 7}, nil
	case <-ctx.Done():
		return OperatorDecision{}, ctx.Err()
	}
}

func TestBudgetPaidReviewReleasesAdmissionBeforeHumanDecision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		text := "hi"
		if request.Messages[0].Content == "review" {
			text = "guarantee"
		}
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"cost":0.1}}`, text)
	}))
	defer srv.Close()
	cfg := providerBudgetConfig(srv.URL)
	cfg.Timeouts.OperatorApproval = "5s"
	cfg.ModelPolicy.Rules = []config.ModelPolicyRule{{ID: "review", Phase: "output", Pattern: "guarantee", Action: "review"}}
	previousState, previousChannel := state, opChan
	state = nil
	review := &budgetReviewChannel{entered: make(chan struct{}), done: make(chan struct{})}
	opChan = review
	t.Cleanup(func() { state, opChan = previousState, previousChannel })
	b := &BudgetTracker{dayStart: time.Now()}
	first := make(chan error, 1)
	go func() { _, err := callLLM(context.Background(), cfg, "drafter", "review", b); first <- err }()
	select {
	case <-review.entered:
	case <-time.After(time.Second):
		t.Fatal("output review did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := callLLM(ctx, cfg, "classifier", "another run", b); err != nil {
		close(review.done)
		t.Fatalf("human review held model admission: %v", err)
	}
	if b.snapshot().costToday != .2 {
		t.Fatalf("usage not settled before review: %+v", b.snapshot())
	}
	close(review.done)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

func TestBudgetInvalidUsageRequiresReconciliationAcrossRestart(t *testing.T) {
	for name, usage := range map[string]string{"negative": `{"prompt_tokens":-10,"completion_tokens":1,"cost":0.1}`, "missing": `{"completion_tokens":1,"cost":0.1}`, "negative-cost": `{"prompt_tokens":1,"completion_tokens":1,"cost":-0.1}`} {
		t.Run(name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":"hi"}}],"usage":%s}`, usage)
			}))
			defer srv.Close()
			st := providerBudgetStore(t)
			cfg := providerBudgetConfig(srv.URL)
			b := &BudgetTracker{dayStart: time.Now()}
			if err := b.attachStore(st); err != nil {
				t.Fatal(err)
			}
			if _, err := callLLM(context.Background(), cfg, "drafter", "p", b); err == nil {
				t.Fatal("invalid usage accepted")
			}
			restarted := &BudgetTracker{dayStart: time.Now()}
			if err := restarted.attachStore(st); err != nil {
				t.Fatal(err)
			}
			if _, err := callLLM(context.Background(), cfg, "classifier", "p", restarted); err == nil || !strings.Contains(err.Error(), "unresolved") {
				t.Fatalf("unresolved call reopened admission: %v", err)
			}
			if hits.Load() != 1 {
				t.Fatalf("invalid response retried %d times", hits.Load())
			}
		})
	}
}

func TestBudgetAdmissionWaitHonorsContext(t *testing.T) {
	b := &BudgetTracker{}
	if err := b.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer b.release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := b.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting admission ignored context: %v", err)
	}
}

func TestBudgetInvalidLegacyUsageCannotReopenCap(t *testing.T) {
	for _, cost := range []float64{-1, math.NaN(), math.Inf(1)} {
		b := &BudgetTracker{dayStart: time.Now(), costToday: 1}
		b.RecordCost(cost)
		if err := b.CheckCost(2, 0); err == nil {
			t.Fatalf("invalid cost %v reopened cap", cost)
		}
	}
	b := &BudgetTracker{dayStart: time.Now(), tokensUsedToday: 10}
	b.record(-10)
	if err := b.check(100, 1); err == nil {
		t.Fatal("negative tokens reopened token budget")
	}
}

func TestBudgetDayResetUsesFullUTCDate(t *testing.T) {
	b := &BudgetTracker{dayStart: time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC), tokensUsedToday: 9, costToday: 1}
	b.snapshotAt(time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
	if b.tokensUsedToday != 0 || b.costToday != 0 {
		t.Fatal("same day-of-month in a new month kept old usage")
	}
}

func TestCallLLMMaxTokensRespectsPerStepCap(t *testing.T) {
	var requested int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		requested = req.MaxTokens
		_, _ = io.WriteString(w, smallPaidCompletion)
	}))
	defer srv.Close()
	cfg := providerBudgetConfig(srv.URL)
	cfg.Models["m"] = config.ModelConfig{Model: "x", MaxTokens: 50}
	cfg.Budgets.PerStepTokens = 7
	if _, err := callLLM(context.Background(), cfg, "drafter", "p", &BudgetTracker{dayStart: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if requested != 7 {
		t.Fatalf("max_tokens=%d, want per-step cap 7", requested)
	}
}

func TestBudgetPipelineUsesModelAllowanceWithoutSharedRunReset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, smallPaidCompletion) }))
	defer srv.Close()
	cfg := providerBudgetConfig(srv.URL)
	cfg.Budgets.PerStepTokens = 10
	cfg.Budgets.PerPipelineTokens = 3
	b := &BudgetTracker{dayStart: time.Now()}
	pipeline := config.PipelineConfig{Name: "scoped", Steps: []config.StepConfig{{Name: "draft", Type: "ai", Role: "drafter", Prompt: "p"}}}
	previous := state
	state = nil
	t.Cleanup(func() { state = previous })
	for i := 0; i < 2; i++ {
		if err := runPipeline(cfg, pipeline, b, nil, nil, nil); err != nil {
			t.Fatalf("run %d rejected actual model allowance: %v", i, err)
		}
	}
	if b.snapshot().tokensUsedToday != 4 {
		t.Fatalf("run restart lost shared daily usage: %+v", b.snapshot())
	}
}

func TestBudgetInputPolicyRejectionDoesNotAdmitProvider(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, smallPaidCompletion)
	}))
	defer srv.Close()
	cfg := providerBudgetConfig(srv.URL)
	cfg.ModelPolicy.Rules = []config.ModelPolicyRule{{ID: "deny-input", Phase: "input", Pattern: "private", Action: "deny"}}
	b := &BudgetTracker{dayStart: time.Now()}
	st := providerBudgetStore(t)
	if err := b.attachStore(st); err != nil {
		t.Fatal(err)
	}
	if _, err := callLLM(context.Background(), cfg, "drafter", "private", b); err == nil {
		t.Fatal("input policy did not deny")
	}
	day, err := st.BudgetDay(context.Background(), time.Now())
	if err != nil || hits.Load() != 0 || day.Tokens != 0 || day.Cost != 0 || day.Unsettled != 0 {
		t.Fatalf("rejected input spent/admitted usage: hits=%d usage=%+v err=%v", hits.Load(), day, err)
	}
}
