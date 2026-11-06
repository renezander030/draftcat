package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

// anchorJobFixture seeds n receipts, makes the store the engine's, and
// returns a job writing to a fresh anchor file with its messages collected.
func anchorJobFixture(t *testing.T, n int, a config.ReceiptAnchorConfig) (*statestore.StateStore, func(int), string, *[]string, func() *receiptAnchorJob) {
	t.Helper()
	st, add := seedAnchorStore(t, n)
	prev := state
	state = st
	t.Cleanup(func() { state = prev })
	if a.File == "" {
		a.File = filepath.Join(t.TempDir(), "anchors.jsonl")
	}
	sent := &[]string{}
	build := func() *receiptAnchorJob {
		ac := a
		j, err := newReceiptAnchorJob(&config.Config{Receipts: config.ReceiptsConfig{Anchor: &ac}}, func(m string) error {
			*sent = append(*sent, m)
			return nil
		})
		if err != nil || j == nil {
			t.Fatalf("job: %v %v", j, err)
		}
		return j
	}
	return st, add, a.File, sent, build
}

func anchorLines(t *testing.T, path string) []receiptAnchor {
	t.Helper()
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []receiptAnchor
	for i, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		a, err := parseAnchorDocument([]byte(line))
		if err != nil {
			t.Fatalf("line %d: %v: %s", i+1, err, line)
		}
		out = append(out, a)
	}
	return out
}

func TestReceiptAnchorJobWritesOnlyWhenTheHistoryChanged(t *testing.T) {
	_, add, file, sent, build := anchorJobFixture(t, 3, config.ReceiptAnchorConfig{Every: "1h", Notify: true})
	j := build()
	t0 := time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC)

	j.tick(t0) // never anchored: the first check takes one
	lines := anchorLines(t, file)
	if len(lines) != 1 || lines[0].Count != 3 || len(lines[0].Trail) != 3 || lines[0].AnchoredAt != "2026-11-01T07:00:00Z" {
		t.Fatalf("first anchor: %+v", lines)
	}
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("anchor file mode: %v %v", info, err)
	}
	if len(*sent) != 1 || !strings.Contains((*sent)[0], "[anchor] 3 receipt(s)") || !strings.Contains((*sent)[0], lines[0].Link) {
		t.Fatalf("message: %q", *sent)
	}

	j.tick(t0.Add(30 * time.Minute)) // not due
	j.tick(t0.Add(time.Hour))        // due, nothing changed
	if got := anchorLines(t, file); len(got) != 1 || len(*sent) != 1 {
		t.Fatalf("an unchanged history writes nothing: %d line(s), %d message(s)", len(got), len(*sent))
	}

	add(2)
	j.tick(t0.Add(90 * time.Minute)) // not due yet: the last slot was at 08:00
	j.tick(t0.Add(2 * time.Hour))
	lines = anchorLines(t, file)
	if len(lines) != 2 || lines[1].Count != 5 || len(*sent) != 2 || strings.Contains((*sent)[1], "WARNING") {
		t.Fatalf("appended receipts: %+v, %q", lines, *sent)
	}

	// A restart on the same store: the slot and both destinations are known.
	j2 := build()
	j2.tick(t0.Add(2*time.Hour + 10*time.Minute))
	j2.tick(t0.Add(3 * time.Hour))
	if got := anchorLines(t, file); len(got) != 2 || len(*sent) != 2 {
		t.Fatalf("a restart must not repeat: %d line(s), %d message(s)", len(got), len(*sent))
	}
}

func TestReceiptAnchorJobRunsAMissedSlotOnce(t *testing.T) {
	st, add, file, sent, build := anchorJobFixture(t, 2, config.ReceiptAnchorConfig{At: "07:00", Timezone: "UTC"})
	day1 := time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC)
	build().tick(day1)
	if len(anchorLines(t, file)) != 1 || len(*sent) != 0 {
		t.Fatalf("file only: %d message(s)", len(*sent))
	}
	if s, err := st.ReceiptAnchorState(); err != nil || !s.SlotAt.Equal(day1) || s.FileLink == "" || s.NotifyLink != "" {
		t.Fatalf("state: %+v %v", s, err)
	}
	add(1)
	// The engine was down for three days and starts after noon.
	j := build()
	j.tick(day1.Add(3*24*time.Hour + 5*time.Hour))
	j.tick(day1.Add(3*24*time.Hour + 6*time.Hour))
	if lines := anchorLines(t, file); len(lines) != 2 || lines[1].Count != 3 {
		t.Fatalf("the missed slots run once: %+v", lines)
	}
	j.tick(day1.Add(4 * 24 * time.Hour)) // the next 07:00, nothing new
	if lines := anchorLines(t, file); len(lines) != 2 {
		t.Fatalf("unchanged: %d line(s)", len(lines))
	}
}

func TestReceiptAnchorJobRetriesOnlyTheFailedDestination(t *testing.T) {
	_, _, file, _, _ := anchorJobFixture(t, 2, config.ReceiptAnchorConfig{Every: "24h", Notify: true})
	sc, err := (config.ReceiptAnchorConfig{Every: "24h"}).Schedule()
	if err != nil {
		t.Fatal(err)
	}
	fail, sent := true, []string{}
	j := &receiptAnchorJob{sched: sc, file: file, notify: func(m string) error {
		if fail {
			return fmt.Errorf("channel down")
		}
		sent = append(sent, m)
		return nil
	}}
	now := time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC)
	j.tick(now)
	fail = false
	j.tick(now.Add(10 * time.Second)) // inside the back-off
	if len(sent) != 0 {
		t.Fatal("the retry waits")
	}
	j.tick(now.Add(2 * time.Minute))
	if len(sent) != 1 || len(anchorLines(t, file)) != 1 {
		t.Fatalf("message retried, line not repeated: %q, %d line(s)", sent, len(anchorLines(t, file)))
	}
}

func TestReceiptAnchorJobWithoutReceiptsWritesNothing(t *testing.T) {
	_, add, file, sent, build := anchorJobFixture(t, 0, config.ReceiptAnchorConfig{Every: "1h", Notify: true})
	j := build()
	now := time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC)
	j.tick(now)
	if _, err := os.Stat(file); !os.IsNotExist(err) || len(*sent) != 0 {
		t.Fatalf("an empty history has nothing to anchor: %v, %q", err, *sent)
	}
	add(1)
	j.tick(now.Add(time.Hour))
	if len(anchorLines(t, file)) != 1 || len(*sent) != 1 {
		t.Fatalf("the first receipt is anchored: %q", *sent)
	}
}

// The round trip: anchors written by the engine, a receipt changed afterwards,
// and verify-anchor names it from the file, from its newest line taken as it
// is, and says mismatch from the message.
func TestReceiptAnchorJobRoundTripNamesTheChangedReceipt(t *testing.T) {
	st, add, file, sent, build := anchorJobFixture(t, 3, config.ReceiptAnchorConfig{Every: "1h", Notify: true})
	j := build()
	t0 := time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC)
	j.tick(t0)
	add(2)
	j.tick(t0.Add(time.Hour))
	if len(anchorLines(t, file)) != 2 {
		t.Fatal("setup: two anchors")
	}
	res, code := verifyAnchorArgs(t, "--log", file)
	if code != 0 || res.Status != "match" || res.Anchors != 2 || res.Line != 2 || res.AnchoredAt != "2026-11-01T08:00:00Z" {
		t.Fatalf("untouched history: %+v (exit %d)", res, code)
	}

	execAnchorSQL(t, st, `UPDATE action_approvals SET decision='skip' WHERE receipt_id='rcpt_2'`)
	res, code = verifyAnchorArgs(t, "--log", file)
	if code != 1 || res.Status != "mismatch" || res.Line != 1 || res.Position != 2 || res.Kind != "changed" || res.StoreReceiptID != "rcpt_2" {
		t.Fatalf("the log names the changed receipt: %+v (exit %d)", res, code)
	}
	body, _ := os.ReadFile(file)
	newest := strings.Split(strings.TrimSpace(string(body)), "\n")[1]
	if res, code := verifyAnchorArgs(t, newest); code != 1 || res.Position != 2 || res.Kind != "changed" {
		t.Fatalf("a line taken as it is: %+v (exit %d)", res, code)
	}
	msg := (*sent)[1]
	notice := msg[strings.LastIndex(msg, "\n")+1:]
	if res, code := verifyAnchorArgs(t, notice); code != 1 || res.Status != "mismatch" || res.AnchorCount != 5 {
		t.Fatalf("the message line taken as it is: %+v (exit %d)", res, code)
	}

	// The next slot anchors the changed history and says it no longer holds
	// the previous anchor; the log still names the change.
	j.tick(t0.Add(2 * time.Hour))
	if len(*sent) != 3 || !strings.Contains((*sent)[2], "WARNING: the stored history no longer reproduces the previous anchor") {
		t.Fatalf("warning: %q", *sent)
	}
	if res, code := verifyAnchorArgs(t, "--log", file); code != 1 || res.Line != 1 || res.Position != 2 || res.Anchors != 3 {
		t.Fatalf("an anchor taken after the change does not hide it: %+v (exit %d)", res, code)
	}
	var out, errOut bytes.Buffer
	if code := runReceiptsVerifyAnchor([]string{"--log", file}, strings.NewReader(""), &out, &errOut); code != 1 ||
		!strings.Contains(out.String(), "anchor log MISMATCH: line 1, anchored 2026-11-01T07:00:00Z is the first of 3 anchor(s)") ||
		!strings.Contains(out.String(), "position 2 (changed)") {
		t.Fatalf("text (exit %d):\n%s", code, out.String())
	}
}

func verifyAnchorArgs(t *testing.T, args ...string) (anchorResult, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runReceiptsVerifyAnchor(append(args, "--json"), strings.NewReader(""), &out, &errOut)
	var res anchorResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("exit %d, stdout %q, stderr %q: %v", code, out.String(), errOut.String(), err)
	}
	return res, code
}

func TestVerifyAnchorLogRefusesBadInput(t *testing.T) {
	seedAnchorStore(t, 1)
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.jsonl")
	torn := filepath.Join(dir, "torn.jsonl")
	if err := os.WriteFile(empty, []byte("\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(torn, []byte(`{"format":"draftcat-receipt-anchor/v1","count":1`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--log", empty}, {"--log", torn}, {"--log", filepath.Join(dir, "missing")}, {"--log"}, {"--log", empty, "sha256:" + strings.Repeat("a", 64)}, {}} {
		var out, errOut bytes.Buffer
		if code := runReceiptsVerifyAnchor(args, strings.NewReader(""), &out, &errOut); code != 2 {
			t.Errorf("%v: exit %d, want 2 (%s)", args, code, errOut.String())
		}
	}
}

func TestAppendAnchorLineStartsAfterACutOffLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anchors.jsonl")
	if err := os.WriteFile(path, []byte(`{"cut off`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendAnchorLine(path, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := appendAnchorLine(path, []byte(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	if string(body) != "{\"cut off\n{\"a\":1}\n{\"a\":2}\n" {
		t.Fatalf("file: %q", body)
	}
}

func TestNoReceiptAnchorJobWithoutConfig(t *testing.T) {
	j, err := newReceiptAnchorJob(&config.Config{}, func(string) error { return nil })
	if j != nil || err != nil {
		t.Fatalf("absent anchor = no job: %v %v", j, err)
	}
	j.tick(time.Now()) // nil job is a no-op
	if _, err := newReceiptAnchorJob(&config.Config{Receipts: config.ReceiptsConfig{Anchor: &config.ReceiptAnchorConfig{Every: "24h"}}}, nil); err == nil {
		t.Fatal("an anchor kept nowhere is refused")
	}
}
