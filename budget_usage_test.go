package main

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"

	_ "modernc.org/sqlite"
)

// Settled calls are attributed to the provider model and the pipeline that
// made them, and budget status splits the day by both.
func TestBudgetUsageAttributedToModelAndPipeline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, smallPaidCompletion) }))
	defer srv.Close()
	cfg := llmCfg(srv.URL, "openrouter")
	cfg.Models["big"] = config.ModelConfig{Model: "vendor/big", MaxTokens: 10}
	cfg.Models["small"] = config.ModelConfig{Model: "vendor/small", MaxTokens: 10}
	cfg.Roles = map[string]string{"drafter": "big", "classifier": "small"}
	st := providerBudgetStore(t)
	b := &BudgetTracker{dayStart: time.Now()}
	if err := b.attachStore(st); err != nil {
		t.Fatal(err)
	}
	replies := withRun(context.Background(), "run-1", "replies")
	digest := withRun(context.Background(), "run-2", "digest")
	for _, call := range []struct {
		ctx  context.Context
		role string
	}{{replies, "drafter"}, {replies, "classifier"}, {digest, "classifier"}} {
		if _, err := callLLM(call.ctx, cfg, call.role, "p", b); err != nil {
			t.Fatal(err)
		}
	}
	byModel, err := st.BudgetUsageBy(context.Background(), time.Now(), "model")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]statestore.UsageShare{}
	for _, u := range byModel {
		got[u.Name] = u
	}
	if got["vendor/small"].Calls != 2 || got["vendor/big"].Calls != 1 || got["vendor/small"].Tokens != 4 {
		t.Fatalf("by model: %+v", byModel)
	}
	byPipeline, err := st.BudgetUsageBy(context.Background(), time.Now(), "pipeline")
	if err != nil {
		t.Fatal(err)
	}
	if len(byPipeline) != 2 || byPipeline[0].Name != "replies" || byPipeline[0].Calls != 2 || byPipeline[0].Cost < 1.19 {
		t.Fatalf("by pipeline: %+v", byPipeline)
	}
}

// A store written before attribution existed reports its calls as
// unattributed after the engine migrates it, and a read-only status on an
// unmigrated store shows no split rather than failing.
func TestBudgetUsageOnOlderStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().Format("2006-01-02")
	if _, err := db.ExecContext(context.Background(), `CREATE TABLE budget_calls (call_id TEXT PRIMARY KEY, day TEXT NOT NULL, status TEXT NOT NULL,
		tokens INTEGER NOT NULL DEFAULT 0, cost REAL NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, settled_at INTEGER NOT NULL DEFAULT 0);
		INSERT INTO budget_calls VALUES ('old-call', '`+day+`', 'settled', 5, 0.1, 1, 2);`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	ro, err := statestore.OpenStateStoreReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if shares, err := ro.BudgetUsageBy(context.Background(), time.Now(), "model"); err != nil || shares != nil {
		t.Fatalf("unmigrated store: %+v %v", shares, err)
	}
	_ = ro.Close()
	st, err := statestore.OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	shares, err := st.BudgetUsageBy(context.Background(), time.Now(), "model")
	if err != nil || len(shares) != 1 || shares[0].Name != "" || shares[0].Tokens != 5 {
		t.Fatalf("migrated store: %+v %v", shares, err)
	}
}
