package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
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

// A pipe in a step or tool name would open an extra column and a line break
// would split the row, so the Markdown tables escape both; --json keeps the
// names as recorded.
func TestReportKeepsOddNamesInsideTheirCells(t *testing.T) {
	secret := []byte("report-secret")
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	path, st := seedReportStore(t, secret, now)
	at := now.Add(-2 * time.Hour)
	for i, step := range []string{"draft|send", "check\r\nforged | row"} {
		e := statestore.ApprovalEnvelope{ReceiptID: "rcpt_odd" + strconv.Itoa(i), Pipeline: "replies", Step: step, DecidedAt: at,
			Decision: "approve", OperatorID: 7, PayloadHash: "sha256:" + strings.Repeat("a", 64),
			QuorumN: 1, QuorumGot: 1, ExpiresAt: at.Add(time.Hour), Lifecycle: "decided"}
		signApprovalEnvelope(secret, &e)
		if err := st.RecordApprovalV2(e); err != nil {
			t.Fatal(err)
		}
	}
	a := statestore.ToolAction{ActionID: "act_odd", Tool: "fs|write", ArgsHash: "args", PolicyHash: "policy", BindingHash: "binding",
		CreatedAt: at, UpdatedAt: at, ExpiresAt: at.Add(time.Hour)}
	if _, _, err := st.ReserveToolAction(a); err != nil {
		t.Fatal(err)
	}
	if err := st.DecideToolAction(a.ActionID, "allowed", "allow", "approved", "operator", at); err != nil {
		t.Fatal(err)
	}
	consume := statestore.ApprovalEnvelope{ReceiptID: "rcpt_odd_consume", ActionID: a.ActionID, Pipeline: "tool-gate", Step: a.Tool,
		PayloadHash: a.ArgsHash, PolicyHash: a.PolicyHash, BindingHash: a.BindingHash, ExpiresAt: a.ExpiresAt, DecidedAt: at,
		Decision: "consume", Lifecycle: "consume"}
	signApprovalEnvelope(secret, &consume)
	if _, ok, err := st.ConsumeToolActionWithReceipt(a.ActionID, a.BindingHash, at, consume); err != nil || !ok {
		t.Fatalf("consume=%v %v", ok, err)
	}
	if _, ok, err := st.CompleteToolAction(statestore.ToolOutcome{ActionID: a.ActionID, BindingHash: a.BindingHash, Status: "succeeded", ResultHash: "sha256:r", CompletedAt: at}); err != nil || !ok {
		t.Fatalf("complete=%v %v", ok, err)
	}
	t.Setenv("DRAFTCAT_STATE_PATH", path)
	t.Setenv("DRAFTCAT_APPROVAL_SECRET", string(secret))

	var out, errOut bytes.Buffer
	if code := reportCmd(nil, &out, &errOut, now); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	text := out.String()
	// Every row of a table has its header's cells; a pipe after a backslash
	// is part of a cell, as in GitHub Markdown.
	cells := func(row string) int { return strings.Count(row, "|") - strings.Count(row, `\|`) }
	header := -1
	for _, line := range strings.Split(text, "\n") {
		switch {
		case line == "":
			header = -1
		case header < 0 && strings.HasPrefix(line, "| "):
			header = cells(line)
		case header >= 0 && (!strings.HasPrefix(line, "|") || cells(line) != header):
			t.Fatalf("table row %q does not match its header's %d delimiters:\n%s", line, header, text)
		}
	}
	for _, want := range []string{`| replies | draft\|send | 1 | 0 | 0 | 0 | 0 |`, `| replies | check forged \| row | 1 | 0 | 0 | 0 | 0 |`, `| fs\|write | 1 | 0 |`} {
		if !strings.Contains(text, want) {
			t.Fatalf("markdown report lacks %q:\n%s", want, text)
		}
	}

	out.Reset()
	if code := reportCmd([]string{"--json"}, &out, &errOut, now); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var rep oversightReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	steps := map[string]bool{}
	for _, s := range rep.Steps {
		steps[s.Step] = true
	}
	if !steps["draft|send"] || !steps["check\r\nforged | row"] || rep.Outcomes["fs|write"]["succeeded"] != 1 {
		t.Fatalf("--json must keep the names as recorded: %+v", rep)
	}
}

// The Markdown report word for word, with a pipe or a line break in a cell of
// every table: a decision (in its own table and as a column header), a
// pipeline, a step and a tool name.
func TestPrintOversightReportWordForWord(t *testing.T) {
	r := oversightReport{
		From: "2026-10-01T00:00:00Z", To: "2026-10-15T12:00:00Z", Receipts: 6, ByPerson: 4,
		Decisions: map[string]int{"approve": 3, "odd|\nchoice": 1, "skip": 1, "timeout": 1},
		Steps: []reportRow{
			{Pipeline: "replies", Step: "draft|send", Decisions: map[string]int{"approve": 2, "skip": 1}},
			{Pipeline: "re\r\nplies", Step: "check\nforged | row", Decisions: map[string]int{"approve": 1, "odd|\nchoice": 1, "timeout": 1}},
		},
		Operators: map[string]int{"7": 3, "8": 1},
		Skips:     1, Reasons: 1,
		Outcomes:  map[string]map[string]int{"fs|write": {"succeeded": 1}, "mail\rsend": {"succeeded": 2, "failed": 1}},
		Integrity: map[string]int{"ok": 5, "unsigned": 1},
		First:     "rcpt_a", Last: "rcpt_f", Digest: "sha256:0f",
	}
	want := `# Oversight report

2026-10-01T00:00:00Z to 2026-10-15T12:00:00Z (UTC), all pipelines

6 decision(s); 4 made by a person, the rest by configured policy or the clock.

## Decisions

| Decision | Count |
|---|---|
| approve | 3 |
| odd\| choice | 1 |
| skip | 1 |
| timeout | 1 |

## By pipeline and step

| Pipeline | Step | approve | odd\| choice | skip | timeout |
|---|---|---|---|---|---|
| replies | draft\|send | 2 | 0 | 1 | 0 |
| re plies | check forged \| row | 1 | 1 | 0 | 1 |

## By operator

| Operator | Decisions |
|---|---|
| 7 | 3 |
| 8 | 1 |

1 of 1 skip(s) carry a recorded reason.

## Reported tool executions

| Tool | Succeeded | Failed |
|---|---|---|
| fs\|write | 1 | 0 |
| mail send | 2 | 1 |

## Receipts

Signatures: ok 5, unsigned 1.
First rcpt_a, last rcpt_f; receipt ids sha256 sha256:0f.
`
	var out bytes.Buffer
	printOversightReport(&out, r)
	if out.String() != want {
		t.Fatalf("markdown report:\n%s\nwant:\n%s", out.String(), want)
	}
}
