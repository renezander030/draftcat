package state

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpgradeStoreBeforeRunIDColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(context.Background(), `CREATE TABLE pipeline_runs (id INTEGER PRIMARY KEY, pipeline TEXT, started_at INTEGER, ended_at INTEGER, status TEXT, error_text TEXT);
 CREATE TABLE action_approvals (id INTEGER PRIMARY KEY, pipeline TEXT, step TEXT, decided_at INTEGER, decision TEXT, operator_id INTEGER, payload_hash TEXT, quorum_n INTEGER, quorum_got INTEGER);
 INSERT INTO pipeline_runs VALUES (1,'p',100,101,'ok','');
 INSERT INTO action_approvals VALUES (1,'p','review',100,'approve',7,'payload',1,1);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	rows, err := st.AllApprovals(10)
	if err != nil || len(rows) != 1 || rows[0].Version != 1 || rows[0].PayloadHash != "payload" {
		t.Fatalf("legacy approvals=%+v err=%v", rows, err)
	}
	runs, err := st.AllRecentRuns(10)
	if err != nil || len(runs) != 1 || runs[0].RunID != "" {
		t.Fatalf("legacy runs=%+v err=%v", runs, err)
	}
	if err := st.RecordRunForID("new-run", "p", time.Now(), time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStateStore(path)
	if err != nil {
		t.Fatalf("repeat upgrade: %v", err)
	}
	_ = reopened.Close()
}

func TestAtomicReplayClaimAcrossConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first, err := OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st := first
			if i%2 == 1 {
				st = second
			}
			won, err := st.TryMarkSeen("hooks", "sig", "shared", time.Now())
			if err != nil {
				t.Error(err)
			}
			if won {
				winners.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("winners=%d", winners.Load())
	}
}

func TestDurableWebhookRetryIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	a := WebhookAdmission{ID: "wh_first", Pipeline: "p", BodyHash: "body-a", IdempotencyKey: "hash-key", CreatedAt: now, UpdatedAt: now}
	saved, created, err := st.AdmitWebhook(a)
	if err != nil || !created {
		t.Fatalf("first=%+v created=%v err=%v", saved, created, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	a.ID = "wh_retry"
	saved, created, err = st.AdmitWebhook(a)
	if err != nil || created || saved.ID != "wh_first" {
		t.Fatalf("retry=%+v created=%v err=%v", saved, created, err)
	}
	a.BodyHash = "body-b"
	if _, _, err := st.AdmitWebhook(a); err == nil {
		t.Fatal("changed retry payload accepted")
	}
	a.Pipeline = "other"
	if _, created, err := st.AdmitWebhook(a); err != nil || !created {
		t.Fatalf("pipeline scope created=%v err=%v", created, err)
	}
}

func TestAuditStoreReadOnlyAndMissingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	if st, err := OpenStateStoreReadOnly(path); err == nil {
		_ = st.Close()
		t.Fatal("missing store opened")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("missing file created: %v", err)
	}
	st, err := OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	ro, err := OpenStateStoreReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	if _, err := ro.AllRecentRuns(10); err != nil {
		t.Fatal(err)
	}
	if err := ro.RecordRun("p", time.Now(), time.Now(), nil); err == nil {
		t.Fatal("read-only audit connection wrote a row")
	}
}
