package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newTempStore(t *testing.T) *StateStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := OpenStateStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStateStore_DedupRoundtrip(t *testing.T) {
	s := newTempStore(t)

	// First batch: nothing seen → all returned.
	ids := []string{"m1", "m2", "m3"}
	unseen, err := s.FilterUnseen("p", "gmail", ids)
	if err != nil {
		t.Fatalf("FilterUnseen first: %v", err)
	}
	if len(unseen) != 3 {
		t.Fatalf("first pass got %d, want 3", len(unseen))
	}
	if err := s.MarkSeen("p", "gmail", ids); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}

	// Same batch: all already seen → empty.
	unseen, err = s.FilterUnseen("p", "gmail", ids)
	if err != nil {
		t.Fatalf("FilterUnseen second: %v", err)
	}
	if len(unseen) != 0 {
		t.Errorf("second pass got %v, want []", unseen)
	}

	// Mixed batch: only m4 is new.
	unseen, err = s.FilterUnseen("p", "gmail", []string{"m2", "m3", "m4"})
	if err != nil {
		t.Fatalf("FilterUnseen mixed: %v", err)
	}
	if len(unseen) != 1 || unseen[0] != "m4" {
		t.Errorf("mixed pass got %v, want [m4]", unseen)
	}

	// Different scope: dedup is scoped.
	unseen, err = s.FilterUnseen("p", "ghl_contacts", ids)
	if err != nil {
		t.Fatalf("FilterUnseen scope: %v", err)
	}
	if len(unseen) != 3 {
		t.Errorf("scope isolation broken: got %v", unseen)
	}

	// Different pipeline: dedup is per-pipeline.
	unseen, err = s.FilterUnseen("other", "gmail", ids)
	if err != nil {
		t.Fatalf("FilterUnseen other pipeline: %v", err)
	}
	if len(unseen) != 3 {
		t.Errorf("pipeline isolation broken: got %v", unseen)
	}
}

func TestStateStore_RecordAndListRuns(t *testing.T) {
	s := newTempStore(t)
	t0 := time.Now()
	if err := s.RecordRun("p", t0, t0.Add(2*time.Second), nil); err != nil {
		t.Fatalf("RecordRun ok: %v", err)
	}
	if err := s.RecordRun("p", t0.Add(time.Minute), t0.Add(time.Minute+time.Second), errors.New("boom")); err != nil {
		t.Fatalf("RecordRun err: %v", err)
	}
	runs, err := s.RecentRuns("p", 10)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}
	if runs[0].Status != "error" || runs[0].Error != "boom" {
		t.Errorf("newest run wrong: %+v", runs[0])
	}
	if runs[1].Status != "ok" {
		t.Errorf("oldest run wrong: %+v", runs[1])
	}
}

func TestStateStore_EmptyInputs(t *testing.T) {
	s := newTempStore(t)
	if unseen, err := s.FilterUnseen("p", "x", nil); err != nil || unseen != nil {
		t.Errorf("nil input not handled: %v %v", unseen, err)
	}
	if err := s.MarkSeen("p", "x", nil); err != nil {
		t.Errorf("MarkSeen nil: %v", err)
	}
}
