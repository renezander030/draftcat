package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
)

// reserveConfig prices one output token so that, for a one-token completion
// limit, the worst case of a call is worstCase (the 1-byte prompt adds 0).
func reserveConfig(url string, worstCase, dayCap float64) *config.Config {
	cfg := providerBudgetConfig(url)
	cfg.Models["m"] = config.ModelConfig{Model: "x", MaxTokens: 1, CostOut: worstCase * 1000}
	cfg.Budgets.PerDayCost = dayCap
	return cfg
}

func TestReserveMaxCostRefusesCallThatCouldCrossCap(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, smallPaidCompletion)
	}))
	defer srv.Close()
	for _, reserve := range []bool{true, false} {
		cfg := reserveConfig(srv.URL, 0.10, 1.0)
		cfg.Budgets.ReserveMaxCost = reserve
		b := &BudgetTracker{dayStart: time.Now()}
		if err := b.attachStore(providerBudgetStore(t)); err != nil {
			t.Fatal(err)
		}
		b.RecordCost(0.95)
		before := hits.Load()
		_, err := callLLM(context.Background(), cfg, "drafter", "p", b.newRun(0))
		if reserve {
			if err == nil || !strings.Contains(err.Error(), "BUDGET_BLOCKED") || !strings.Contains(err.Error(), "maximum cost") {
				t.Fatalf("reserve: call admitted or unnamed refusal: %v", err)
			}
			if hits.Load() != before {
				t.Fatal("refused call reached the provider")
			}
		} else if err != nil {
			t.Fatalf("without the key the call is admitted as before: %v", err)
		}
	}
}

func TestReserveMaxCostConcurrentCallsAdmitOnlyWhatFits(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"cost":0.3}}`)
	}))
	defer srv.Close()
	run := func(reserve bool) int32 {
		cfg := reserveConfig(srv.URL, 0.3, 1.0)
		cfg.Budgets.ReserveMaxCost = reserve
		b := &BudgetTracker{dayStart: time.Now()}
		if err := b.attachStore(providerBudgetStore(t)); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var ok atomic.Int32
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := callLLM(context.Background(), cfg, "drafter", "p", b.newRun(0)); err == nil {
					ok.Add(1)
				}
			}()
		}
		wg.Wait()
		return ok.Load()
	}
	if got := run(true); got != 3 {
		t.Fatalf("with reservation %d calls admitted, want 3 (3 x 0.3 settled, a 4th could reach 1.2)", got)
	}
	if got := run(false); got != 4 {
		t.Fatalf("without reservation %d calls admitted, want 4 (stop on settled spend)", got)
	}
}

func TestReserveMaxCostSettlementReplacesReservation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"cost":0.01}}`)
	}))
	defer srv.Close()
	cfg := reserveConfig(srv.URL, 0.4, 1.0)
	cfg.Budgets.ReserveMaxCost = true
	b := &BudgetTracker{dayStart: time.Now()}
	if err := b.attachStore(providerBudgetStore(t)); err != nil {
		t.Fatal(err)
	}
	// Each call settles at 0.01 although it reserved 0.4; the reservation does
	// not linger, so calls keep being admitted until 0.4 of headroom is left.
	for i := 0; i < 5; i++ {
		if _, err := callLLM(context.Background(), cfg, "drafter", "p", b.newRun(0)); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if s := b.snapshot(); s.costToday < 0.0499 || s.costToday > 0.0501 || s.unsettled != 0 {
		t.Fatalf("settled snapshot: %+v", s)
	}
	b.RecordCost(0.56)
	if _, err := callLLM(context.Background(), cfg, "drafter", "p", b.newRun(0)); err == nil {
		t.Fatal("0.61 settled + 0.4 worst case exceeds 1.0 but was admitted")
	}
}

func TestReserveMaxCostPipelineCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, smallPaidCompletion)
	}))
	defer srv.Close()
	cfg := cappedPipelineConfig(srv.URL, config.PipelineBudgetConfig{PerDayCost: 1, ReserveMaxCost: true})
	cfg.Models["m"] = config.ModelConfig{Model: "x", MaxTokens: 1, CostOut: 500}
	b := &BudgetTracker{dayStart: time.Now()}
	if err := b.attachStore(providerBudgetStore(t)); err != nil {
		t.Fatal(err)
	}
	replies := withRun(context.Background(), "run-1", "replies")
	digest := withRun(context.Background(), "run-2", "digest")
	if _, err := callLLM(replies, cfg, "drafter", "p", b.newRun(0)); err != nil {
		t.Fatalf("first call (0.5 worst case, empty day): %v", err)
	}
	// 0.6 settled + 0.5 worst case > 1.0 for the capped pipeline only.
	if _, err := callLLM(replies, cfg, "drafter", "p", b.newRun(0)); err == nil || !strings.Contains(err.Error(), `pipeline "replies" daily cost limit`) || !strings.Contains(err.Error(), "maximum cost") {
		t.Fatalf("capped pipeline admitted: %v", err)
	}
	if _, err := callLLM(digest, cfg, "drafter", "p", b.newRun(0)); err != nil {
		t.Fatalf("other pipeline refused: %v", err)
	}
}
