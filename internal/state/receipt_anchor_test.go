package state

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// The scheduled anchor's row survives a reopen, and opening the store again
// migrates nothing twice.
func TestReceiptAnchorStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReceiptAnchorState(); err != nil || got != (ReceiptAnchorState{}) {
		t.Fatalf("a new store has no anchor state: %+v %v", got, err)
	}
	want := ReceiptAnchorState{SlotAt: time.Unix(1793516400, 0), Link: "sha256:aa", File: "/mnt/anchors.jsonl", FileLink: "sha256:aa", NotifyLink: "sha256:bb"}
	if err := s.SetReceiptAnchorState(want); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	got, err := s.ReceiptAnchorState()
	if err != nil || !got.SlotAt.Equal(want.SlotAt) || got.Link != want.Link || got.File != want.File || got.FileLink != want.FileLink || got.NotifyLink != want.NotifyLink {
		t.Fatalf("got %+v %v, want %+v", got, err, want)
	}
	var none *StateStore
	if _, err := none.ReceiptAnchorState(); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil store: %v", err)
	}
}
