package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

func cappedPipelineConfig(url string, caps config.PipelineBudgetConfig) *config.Config {
	cfg := providerBudgetConfig(url)
	cfg.Pipelines = []config.PipelineConfig{
		{Name: "replies", Budget: caps, Steps: []config.StepConfig{{Name: "draft", Type: "ai", Role: "drafter", Prompt: "p"}}},
		{Name: "digest", Steps: []config.StepConfig{{Name: "sum", Type: "ai", Role: "drafter", Prompt: "p"}}},
	}
	return cfg
}

// A pipeline that has spent its own daily cost cap is refused at admission,
// another pipeline keeps running, and a restart on the same state keeps the
// day's pipeline total.
func TestBudgetPipelineDailyCostCapRefusesOnlyThatPipelineAcrossRestart(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, smallPaidCompletion)
	}))
	defer srv.Close()
	cfg := cappedPipelineConfig(srv.URL, config.PipelineBudgetConfig{PerDayCost: 1})
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := statestore.OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	b := &BudgetTracker{dayStart: time.Now()}
	if err := b.attachStore(st); err != nil {
		t.Fatal(err)
	}
	replies := withRun(context.Background(), "run-1", "replies")
	digest := withRun(context.Background(), "run-2", "digest")
	for i := 0; i < 2; i++ { // 0.6, then 1.2: the second call starts below the cap
		if _, err := callLLM(replies, cfg, "drafter", "p", b.newRun(0)); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, err := callLLM(replies, cfg, "drafter", "p", b.newRun(0)); err == nil || !strings.Contains(err.Error(), `BUDGET_BLOCKED: pipeline "replies" daily cost limit`) {
		t.Fatalf("capped pipeline admitted: %v", err)
	}
	if _, err := callLLM(digest, cfg, "drafter", "p", b.newRun(0)); err != nil {
		t.Fatalf("uncapped pipeline refused: %v", err)
	}
	if hits.Load() != 3 {
		t.Fatalf("provider hits=%d, want 3", hits.Load())
	}
	_ = st.Close()

	st, err = statestore.OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	restarted := &BudgetTracker{dayStart: time.Now()}
	if err := restarted.attachStore(st); err != nil {
		t.Fatal(err)
	}
	if _, err := callLLM(replies, cfg, "drafter", "p", restarted.newRun(0)); err == nil || !strings.Contains(err.Error(), "daily cost limit") {
		t.Fatalf("restart reopened the pipeline cap: %v", err)
	}
	if _, err := callLLM(digest, cfg, "drafter", "p", restarted.newRun(0)); err != nil {
		t.Fatalf("uncapped pipeline refused after restart: %v", err)
	}
	if hits.Load() != 4 {
		t.Fatalf("provider hits=%d, want 4", hits.Load())
	}
	got, err := st.PipelineBudgetDay(context.Background(), time.Now(), "replies")
	if err != nil || got.Calls != 2 || got.Tokens != 4 {
		t.Fatalf("replies usage %+v %v", got, err)
	}
}

// The token cap counts the requested completion like budgets.per_day_tokens:
// a call that would cross it is refused, an overrun response is charged and
// halts, and the global daily ledger still sees every call.
func TestBudgetPipelineDailyTokenCapMatchesGlobalSemantics(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, smallPaidCompletion)
	}))
	defer srv.Close()
	cfg := cappedPipelineConfig(srv.URL, config.PipelineBudgetConfig{PerDayTokens: 3})
	b := &BudgetTracker{dayStart: time.Now()}
	replies := withRun(context.Background(), "run-1", "replies")
	if _, err := callLLM(replies, cfg, "drafter", "p", b); err != nil {
		t.Fatal(err)
	}
	response, err := callLLM(replies, cfg, "drafter", "p", b)
	if response != nil || err == nil || !strings.Contains(err.Error(), `response exceeded pipeline "replies" daily token limit 3`) {
		t.Fatalf("overrun response=%+v err=%v", response, err)
	}
	if _, err := callLLM(replies, cfg, "drafter", "p", b); err == nil || !strings.Contains(err.Error(), "would be exceeded (used: 4, requested: 1)") {
		t.Fatalf("over-cap pipeline admitted: %v", err)
	}
	if _, err := callLLM(context.Background(), cfg, "drafter", "p", b); err != nil {
		t.Fatalf("call outside a pipeline refused: %v", err)
	}
	if hits.Load() != 3 || b.snapshot().tokensUsedToday != 6 {
		t.Fatalf("hits=%d day=%+v", hits.Load(), b.snapshot())
	}
}

// Global caps keep applying to a pipeline that has its own, larger cap.
func TestBudgetPipelineCapDoesNotLiftGlobalCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, smallPaidCompletion) }))
	defer srv.Close()
	cfg := cappedPipelineConfig(srv.URL, config.PipelineBudgetConfig{PerDayCost: 100})
	cfg.Budgets.PerDayCost = .5
	b := &BudgetTracker{dayStart: time.Now()}
	if err := b.attachStore(providerBudgetStore(t)); err != nil {
		t.Fatal(err)
	}
	replies := withRun(context.Background(), "run-1", "replies")
	if _, err := callLLM(replies, cfg, "drafter", "p", b); err != nil {
		t.Fatal(err)
	}
	if _, err := callLLM(replies, cfg, "drafter", "p", b); err == nil || !strings.Contains(err.Error(), "BUDGET_BLOCKED: daily cost limit") {
		t.Fatalf("global cap lifted: %v", err)
	}
}

// Concurrent runs of a capped pipeline and an uncapped one: the capped
// pipeline never exceeds one call past its settled cap, the other is unaffected.
func TestBudgetPipelineCapConcurrentRuns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, smallPaidCompletion) }))
	defer srv.Close()
	cfg := cappedPipelineConfig(srv.URL, config.PipelineBudgetConfig{PerDayCost: .5})
	st := providerBudgetStore(t)
	b := &BudgetTracker{dayStart: time.Now()}
	if err := b.attachStore(st); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var replies, digest atomic.Int32
	for i := 0; i < 8; i++ {
		for _, name := range []string{"replies", "digest"} {
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				ctx := withRun(context.Background(), newRunID(time.Now()), name)
				if _, err := callLLM(ctx, cfg, "drafter", "p", b.newRun(0)); err == nil {
					if name == "replies" {
						replies.Add(1)
					} else {
						digest.Add(1)
					}
				}
			}(name)
		}
	}
	wg.Wait()
	if replies.Load() != 1 || digest.Load() != 8 {
		t.Fatalf("replies=%d digest=%d, want 1 and 8", replies.Load(), digest.Load())
	}
}

func TestBudgetPipelineAlertFiresOncePerThreshold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, smallPaidCompletion) }))
	defer srv.Close()
	cfg := cappedPipelineConfig(srv.URL, config.PipelineBudgetConfig{PerDayCost: 2})
	cfg.Budgets.AlertAt = []float64{0.5}
	b := &BudgetTracker{dayStart: time.Now()}
	if err := b.attachStore(providerBudgetStore(t)); err != nil {
		t.Fatal(err)
	}
	notify, collect := alertCollector()
	b.configureAlerts(cfg, notify)
	replies := withRun(context.Background(), "run-1", "replies")
	digest := withRun(context.Background(), "run-2", "digest")
	for _, ctx := range []context.Context{digest, digest, replies, replies, replies} { // replies: 30%, 60%, 90%
		if _, err := callLLM(ctx, cfg, "drafter", "p", b); err != nil {
			t.Fatal(err)
		}
	}
	got := collect(1, time.Second)
	if len(got) != 1 || !strings.Contains(got[0], "50% of the daily cost cap of pipeline replies used: 1.2000 of 2.0000") {
		t.Fatalf("alerts=%q", got)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		data, _ := io.ReadAll(r)
		done <- string(data)
	}()
	fn()
	os.Stdout = orig
	_ = w.Close()
	return <-done
}

func TestBudgetStatusShowsPipelineCaps(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")
	st, err := statestore.OpenStateStore(statePath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.BeginBudgetCall(ctx, "c1", time.Now(), 1, 0, 0, statestore.UsageLabel{Pipeline: "replies"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SettleBudgetCall(ctx, "c1", 40, .25); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	t.Setenv("DRAFTCAT_STATE_PATH", statePath)
	plain := filepath.Join(dir, "plain.yaml")
	capped := filepath.Join(dir, "capped.yaml")
	if err := os.WriteFile(plain, []byte("pipelines:\n  - name: replies\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(capped, []byte("pipelines:\n  - name: replies\n    budget: {per_day_tokens: 40, per_day_cost: 1}\n  - name: digest\n    budget: {per_day_cost: 2}\n  - name: other\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { runBudgetCmd([]string{"status", "--json", "--config", plain}) })
	if strings.Contains(out, "pipeline_caps") {
		t.Fatalf("uncapped config gained a field: %s", out)
	}
	out = captureStdout(t, func() { runBudgetCmd([]string{"status", "--json", "--config", capped}) })
	var report struct {
		PipelineCaps []pipelineDayCap `json:"pipeline_caps"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	want := []pipelineDayCap{
		{Name: "replies", Calls: 1, Tokens: 40, PerDayTokens: 40, Cost: .25, PerDayCost: 1, Reached: true},
		{Name: "digest", PerDayCost: 2},
	}
	if len(report.PipelineCaps) != 2 || report.PipelineCaps[0] != want[0] || report.PipelineCaps[1] != want[1] {
		t.Fatalf("pipeline caps %+v", report.PipelineCaps)
	}
	out = captureStdout(t, func() { runBudgetCmd([]string{"status", "--config", capped}) })
	if !strings.Contains(out, "Pipeline daily caps:") || !strings.Contains(out, "40 of 40 tokens") || !strings.Contains(out, "REACHED") || !strings.Contains(out, "0 of no cap tokens") {
		t.Fatalf("text status: %s", out)
	}
}
