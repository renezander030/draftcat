package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	statestore "github.com/renezander030/draftcat/internal/state"
)

func seedReportStore(t *testing.T, secret []byte, now time.Time) (string, *statestore.StateStore) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := statestore.OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	add := func(id, pipeline, step, decision string, op int64, at time.Time) {
		e := statestore.ApprovalEnvelope{ReceiptID: id, Pipeline: pipeline, Step: step, DecidedAt: at,
			Decision: decision, OperatorID: op, PayloadHash: "sha256:" + strings.Repeat("a", 64),
			QuorumN: 1, QuorumGot: 1, ExpiresAt: at.Add(time.Hour), Lifecycle: "decided"}
		signApprovalEnvelope(secret, &e)
		if err := st.RecordApprovalV2(e); err != nil {
			t.Fatal(err)
		}
	}
	add("rcpt_old", "replies", "review", "approve", 7, now.AddDate(0, 0, -40))
	add("rcpt_a", "replies", "review", "approve", 7, now.Add(-72*time.Hour))
	add("rcpt_b", "replies", "review", "skip", 8, now.Add(-48*time.Hour))
	add("rcpt_c", "replies", "review", "timeout", 0, now.Add(-24*time.Hour))
	add("rcpt_d", "tool-gate", "create_invoice", "policy_approve", 0, now.Add(-time.Hour))
	if err := st.RecordApprovalNote(statestore.ApprovalNote{ReceiptID: "rcpt_b", OperatorID: 8, Reason: "wrong customer", NotedAt: now.Add(-47 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return path, st
}

func TestReportSummarizesOversight(t *testing.T) {
	secret := []byte("report-secret")
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	path, _ := seedReportStore(t, secret, now)
	t.Setenv("DRAFTCAT_STATE_PATH", path)
	t.Setenv("DRAFTCAT_APPROVAL_SECRET", string(secret))
	var out, errOut bytes.Buffer
	if code := reportCmd([]string{"--json"}, &out, &errOut, now); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var rep oversightReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Receipts != 4 || rep.ByPerson != 2 || rep.Decisions["approve"] != 1 || rep.Decisions["timeout"] != 1 {
		t.Fatalf("window should hold the four recent receipts: %+v", rep)
	}
	if rep.Skips != 1 || rep.Reasons != 1 || rep.Operators["7"] != 1 || rep.Operators["8"] != 1 {
		t.Fatalf("operators and skip reasons: %+v", rep)
	}
	if rep.Integrity["ok"] != 4 || rep.First != "rcpt_a" || rep.Last != "rcpt_d" || !strings.HasPrefix(rep.Digest, "sha256:") {
		t.Fatalf("integrity and completeness: %+v", rep)
	}

	out.Reset()
	if code := reportCmd([]string{"--since", "2026-09-01", "--until", "2026-10-13", "--pipeline", "replies"}, &out, &errOut, now); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	text := out.String()
	for _, want := range []string{"# Oversight report", "3 decision(s); 3 made by a person", "| replies | review | 2 | 1 |", "1 of 1 skip(s) carry a recorded reason"} {
		if !strings.Contains(text, want) {
			t.Fatalf("markdown report lacks %q:\n%s", want, text)
		}
	}
}

func TestReportFailsOnTamperedReceipt(t *testing.T) {
	secret := []byte("report-secret")
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	path, st := seedReportStore(t, secret, now)
	if _, err := st.DB().ExecContext(context.Background(), `UPDATE action_approvals SET decision='approve' WHERE receipt_id='rcpt_c'`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DRAFTCAT_STATE_PATH", path)
	t.Setenv("DRAFTCAT_APPROVAL_SECRET", string(secret))
	var out, errOut bytes.Buffer
	if code := reportCmd(nil, &out, &errOut, now); code != 1 {
		t.Fatalf("a tampered receipt must exit 1, got %d", code)
	}
	if !strings.Contains(out.String(), "tampered 1") {
		t.Fatalf("report should name the tampered receipt count:\n%s", out.String())
	}
}

func TestReportWindowParsing(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	from, to, err := reportWindow("7d", "", now)
	if err != nil || !to.Equal(now) || !from.Equal(now.AddDate(0, 0, -7)) {
		t.Fatalf("7d: %v %v %v", from, to, err)
	}
	for _, bad := range [][2]string{{"0d", ""}, {"yesterday", ""}, {"2026-10-20", "2026-10-01"}} {
		if _, _, err := reportWindow(bad[0], bad[1], now); err == nil {
			t.Fatalf("%v should be refused", bad)
		}
	}
}
