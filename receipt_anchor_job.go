package main

// Scheduled receipt anchor: on each slot the engine takes the anchor
// `receipts anchor` prints and keeps it where the host cannot rewrite it, as a
// line appended to a file and as a message in the operator channel's history.
// An unchanged history writes nothing.

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	"github.com/renezander030/draftcat/internal/schedule"
	statestore "github.com/renezander030/draftcat/internal/state"
)

const anchorRetryAfter = time.Minute

// anchorNotice is an anchor without its trail: short enough for a chat
// message, and accepted as-is by `receipts verify-anchor`.
type anchorNotice struct {
	Format         string `json:"format"`
	AnchoredAt     string `json:"anchored_at"`
	Count          int    `json:"count"`
	FirstReceiptID string `json:"first_receipt_id,omitempty"`
	LastReceiptID  string `json:"last_receipt_id,omitempty"`
	LastDecidedAt  string `json:"last_decided_at,omitempty"`
	Link           string `json:"link"`
}

// receiptAnchorJob takes the anchor on its schedule. What each destination
// last received is kept in the state store, so a restart does not repeat a
// line or a message, and a slot missed while the engine was down runs once at
// its first check.
type receiptAnchorJob struct {
	sched  schedule.Schedule
	file   string
	notify func(string) error // nil = no message

	mu      sync.Mutex
	loaded  bool
	last    statestore.ReceiptAnchorState
	retryAt time.Time
}

// newReceiptAnchorJob returns nil when receipts.anchor is absent.
func newReceiptAnchorJob(cfg *config.Config, send func(string) error) (*receiptAnchorJob, error) {
	a := cfg.Receipts.Anchor
	if a == nil {
		return nil, nil
	}
	sc, err := a.Schedule()
	if err != nil {
		return nil, fmt.Errorf("receipts.anchor: %w", err)
	}
	if a.File == "" && !a.Notify {
		return nil, fmt.Errorf("receipts.anchor: neither file nor notify is set")
	}
	j := &receiptAnchorJob{sched: sc, file: a.File}
	if a.Notify {
		j.notify = send
	}
	return j, nil
}

// tick is called from the engine loop; it does its work once per slot.
func (j *receiptAnchorJob) tick(now time.Time) {
	if j == nil || state == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if now.Before(j.retryAt) {
		return
	}
	if !j.loaded {
		last, err := state.ReceiptAnchorState()
		if err != nil {
			log.Printf("[anchor] state unreadable: %v", err)
			j.retryAt = now.Add(anchorRetryAfter)
			return
		}
		j.loaded, j.last = true, last
	}
	if !j.last.SlotAt.IsZero() && j.sched.Next(j.last.SlotAt).After(now) {
		return
	}
	rows, err := state.ApprovalsInStoredOrder()
	if err != nil {
		log.Printf("[anchor] receipts unreadable: %v", err)
		j.retryAt = now.Add(anchorRetryAfter)
		return
	}
	links := receiptChainLinks(rows)
	a := anchorFromLinks(rows, links)
	a.AnchoredAt = now.UTC().Format(time.RFC3339)
	extends := j.last.Link == "" || chainReaches(links, j.last.Link)
	if !extends {
		log.Printf("[anchor] WARNING: the receipt history no longer reproduces the previous anchor %s", j.last.Link)
	}
	// unchanged: this destination already holds this anchor, or the history
	// is still empty and it never got one.
	unchanged := func(had string) bool { return had == a.Link || (had == "" && a.Count == 0) }
	fileHad := j.last.FileLink
	if j.last.File != j.file {
		fileHad = "" // another file than the last line went to
	}

	if j.file != "" && !unchanged(fileHad) {
		line, err := json.Marshal(a)
		if err == nil {
			err = appendAnchorLine(j.file, line)
		}
		if err != nil {
			log.Printf("[anchor] could not write %s, retrying: %v", j.file, err)
			j.retryAt = now.Add(anchorRetryAfter)
			return
		}
		j.last.File, j.last.FileLink = j.file, a.Link
		j.save()
		log.Printf("[anchor] %d receipt(s) anchored to %s: %s", a.Count, j.file, a.Link)
	}
	if j.notify != nil && !unchanged(j.last.NotifyLink) {
		if err := j.notify(anchorMessage(a, extends, j.last.Link)); err != nil {
			log.Printf("[anchor] send failed, retrying: %v", err)
			j.retryAt = now.Add(anchorRetryAfter)
			return
		}
		j.last.NotifyLink = a.Link
		j.save()
		log.Printf("[anchor] %d receipt(s) anchored to the operator channel: %s", a.Count, a.Link)
	}
	j.last.SlotAt, j.last.Link = now.Truncate(time.Second), a.Link
	j.save()
}

func (j *receiptAnchorJob) save() {
	if err := state.SetReceiptAnchorState(j.last); err != nil {
		log.Printf("[anchor] could not record the anchor state: %v", err)
	}
}

// chainReaches reports whether some prefix of the chain ends in link: the
// stored history still holds the anchored one, with receipts appended or not.
func chainReaches(links [][32]byte, link string) bool {
	for _, l := range links {
		if formatLink(l) == link {
			return true
		}
	}
	return false
}

// anchorMessage is the operator message: a summary line and the anchor
// without its trail, which `receipts verify-anchor` takes as it is.
func anchorMessage(a receiptAnchor, extends bool, previous string) string {
	notice, _ := json.Marshal(anchorNotice{
		Format: a.Format, AnchoredAt: a.AnchoredAt, Count: a.Count,
		FirstReceiptID: a.FirstReceiptID, LastReceiptID: a.LastReceiptID, LastDecidedAt: a.LastDecidedAt, Link: a.Link,
	})
	head := fmt.Sprintf("[anchor] %d receipt(s) in the history", a.Count)
	if a.Count > 0 {
		head += fmt.Sprintf(", the last %s decided %s", a.LastReceiptID, a.LastDecidedAt)
	}
	if !extends {
		head += fmt.Sprintf("\nWARNING: the stored history no longer reproduces the previous anchor %s; a receipt was changed, removed or inserted since. Check it with: draftcat receipts verify-anchor <the earlier anchor>", previous)
	}
	return head + "\n" + string(notice)
}

// appendAnchorLine appends one line to the anchor file and syncs it to disk.
// The file is created 0600. A file that does not end in a newline (a write cut
// off earlier) gets one first, so the new line stands on its own.
func appendAnchorLine(path string, line []byte) error {
	_, statErr := os.Stat(path)
	created := os.IsNotExist(statErr)
	// #nosec G304 -- the operator names the anchor file in config.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	out := make([]byte, 0, len(line)+2)
	if info, err := f.Stat(); err == nil && info.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, info.Size()-1); err != nil && err != io.EOF {
			_ = f.Close()
			return err
		}
		if last[0] != '\n' {
			out = append(out, '\n')
		}
	}
	out = append(append(out, line...), '\n')
	if _, err := f.Write(out); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if created {
		// Make the new directory entry durable too.
		if dir, err := os.Open(filepath.Dir(path)); err == nil { // #nosec G304 -- the anchor file's own directory
			_ = dir.Sync()
			_ = dir.Close()
		}
	}
	return nil
}
