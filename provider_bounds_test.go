package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type interruptedProviderReader struct{}

func (interruptedProviderReader) Read(p []byte) (int, error) {
	copy(p, "partial provider data")
	return 21, io.ErrUnexpectedEOF
}

func TestProviderResponseReadBoundsAndErrors(t *testing.T) {
	if _, err := readProviderResponse(context.Background(), strings.NewReader(strings.Repeat("x", int(maxProviderResponseBytes+1)))); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized response accepted: %v", err)
	}
	if _, err := readProviderResponse(context.Background(), interruptedProviderReader{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("body read error ignored: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readProviderResponse(ctx, strings.NewReader("data")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled body read accepted: %v", err)
	}
}

func TestBudgetOversizedResponseRemainsUnsettledAndIsNotRetried(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, strings.Repeat("secret-provider-data", int(maxProviderResponseBytes)/20+100))
	}))
	defer srv.Close()
	cfg := providerBudgetConfig(srv.URL)
	st := providerBudgetStore(t)
	b := &BudgetTracker{dayStart: time.Now()}
	if err := b.attachStore(st); err != nil {
		t.Fatal(err)
	}
	_, err := callLLM(context.Background(), cfg, "drafter", "p", b)
	if err == nil || !strings.Contains(err.Error(), "exceeds") || strings.Contains(err.Error(), "secret-provider-data") {
		t.Fatalf("unsafe oversized response error: %v", err)
	}
	if _, err := callLLM(context.Background(), cfg, "drafter", "p", b); err == nil || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("uncertain bill reopened budget: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("oversized paid response retried %d times", hits.Load())
	}
}

func TestCallLLMServerFailureIsNotRetriedOrDumped(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `{"error":"secret-provider-data"}`)
	}))
	defer srv.Close()
	cfg := providerBudgetConfig(srv.URL)
	b := &BudgetTracker{dayStart: time.Now()}
	_, err := callLLM(context.Background(), cfg, "drafter", "p", b)
	if err == nil || hits.Load() != 1 || strings.Contains(err.Error(), "secret-provider-data") {
		t.Fatalf("hits=%d unsafe error=%v", hits.Load(), err)
	}
	if b.snapshot().unsettled != 1 {
		t.Fatal("uncertain server failure was treated as free")
	}
}

func TestBudgetTruncatedPaidResponseIsNotRetried(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Length", "1000")
		_, _ = io.WriteString(w, `{"choices":[`)
	}))
	defer srv.Close()
	cfg := providerBudgetConfig(srv.URL)
	b := &BudgetTracker{dayStart: time.Now()}
	_, err := callLLM(context.Background(), cfg, "drafter", "p", b)
	if !errors.Is(err, io.ErrUnexpectedEOF) || hits.Load() != 1 || b.snapshot().unsettled != 1 {
		t.Fatalf("truncated response: hits=%d error=%v snapshot=%+v", hits.Load(), err, b.snapshot())
	}
}
