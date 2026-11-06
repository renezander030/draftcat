package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ReceiptAnchorState is the scheduled receipt anchor's one row: when a slot
// was last handled, the anchor taken then, and what each destination last
// received, so a restart or a retry neither repeats a line nor a message.
type ReceiptAnchorState struct {
	SlotAt     time.Time // zero = never handled
	Link       string
	File       string
	FileLink   string
	NotifyLink string
}

// ReceiptAnchorState reads the scheduled anchor's state. A store without one
// reports the zero state.
func (s *StateStore) ReceiptAnchorState() (ReceiptAnchorState, error) {
	if s == nil || s.db == nil {
		return ReceiptAnchorState{}, ErrNoStore
	}
	var a ReceiptAnchorState
	var slot int64
	err := s.db.QueryRowContext(context.Background(),
		`SELECT slot_at, link, file, file_link, notify_link FROM receipt_anchor_state WHERE id=1`).
		Scan(&slot, &a.Link, &a.File, &a.FileLink, &a.NotifyLink)
	if errors.Is(err, sql.ErrNoRows) {
		return ReceiptAnchorState{}, nil
	}
	if err != nil {
		return ReceiptAnchorState{}, err
	}
	if slot != 0 {
		a.SlotAt = time.Unix(slot, 0)
	}
	return a, nil
}

// SetReceiptAnchorState records the scheduled anchor's state.
func (s *StateStore) SetReceiptAnchorState(a ReceiptAnchorState) error {
	if s == nil || s.db == nil {
		return ErrNoStore
	}
	slot := int64(0)
	if !a.SlotAt.IsZero() {
		slot = a.SlotAt.Unix()
	}
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO receipt_anchor_state (id, slot_at, link, file, file_link, notify_link) VALUES (1, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET slot_at=excluded.slot_at, link=excluded.link, file=excluded.file,
		 file_link=excluded.file_link, notify_link=excluded.notify_link`,
		slot, a.Link, a.File, a.FileLink, a.NotifyLink)
	return err
}
