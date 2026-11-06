package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestAgentFreezeSetClearAndList(t *testing.T) {
	s := newTempStore(t)
	at := time.Unix(1_800_000_000, 0)
	if changed, err := s.SetAgentFreeze("agent-2", "refused 5 times within 10m", "trip", at.Add(time.Minute)); err != nil || !changed {
		t.Fatalf("freeze agent-2: %v %v", changed, err)
	}
	if changed, err := s.SetAgentFreeze("agent-1", "refused 3 times within 1m", "trip", at); err != nil || !changed {
		t.Fatalf("freeze agent-1: %v %v", changed, err)
	}
	if changed, _ := s.SetAgentFreeze("agent-1", "later", "cli", at.Add(time.Hour)); changed {
		t.Fatal("a second freeze keeps the first")
	}
	f, ok, err := s.AgentFreeze("agent-1")
	if err != nil || !ok || f.Reason != "refused 3 times within 1m" || f.By != "trip" || !f.At.Equal(at) {
		t.Fatalf("agent-1 = %+v %v %v", f, ok, err)
	}
	all, err := s.AgentFreezes()
	if err != nil || len(all) != 2 || all[0].Agent != "agent-1" || all[1].Agent != "agent-2" {
		t.Fatalf("list, longest frozen first = %+v %v", all, err)
	}
	if changed, err := s.ClearAgentFreeze("agent-1", "cli", at.Add(2*time.Hour)); err != nil || !changed {
		t.Fatalf("clear: %v %v", changed, err)
	}
	if changed, _ := s.ClearAgentFreeze("agent-1", "cli", at); changed {
		t.Fatal("clearing an agent that is not frozen changes nothing")
	}
	if _, ok, _ := s.AgentFreeze("agent-1"); ok {
		t.Fatal("agent-1 still frozen")
	}
	// Frozen again after a lift: the new reason and time are kept.
	if changed, _ := s.SetAgentFreeze("agent-1", "again", "trip", at.Add(3*time.Hour)); !changed {
		t.Fatal("refreeze after a lift")
	}
	if f, _, _ := s.AgentFreeze("agent-1"); f.Reason != "again" {
		t.Fatalf("refrozen = %+v", f)
	}
}

func TestAgentFreezeOnAnUnmigratedStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `CREATE TABLE seen_items (pipeline TEXT)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	ro, err := OpenStateStoreReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	if _, ok, err := ro.AgentFreeze("a"); ok || err != nil {
		t.Fatalf("unmigrated store: %v %v", ok, err)
	}
	if all, err := ro.AgentFreezes(); len(all) != 0 || err != nil {
		t.Fatalf("unmigrated store list: %v %v", all, err)
	}
	var nilStore *StateStore
	if _, _, err := nilStore.AgentFreeze("a"); err == nil {
		t.Fatal("a nil store cannot answer")
	}
}
