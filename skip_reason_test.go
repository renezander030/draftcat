package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	"github.com/renezander030/draftcat/internal/relay"
	statestore "github.com/renezander030/draftcat/internal/state"
)

func TestTelegramSkipWithReason(t *testing.T) {
	bot, s := newScriptedBot()
	bot.startPump(2 * time.Millisecond)
	defer bot.stopPump()

	res := make(chan OperatorDecision, 1)
	go func() { d, _ := bot.SendForApproval(context.Background(), "draft", nil); res <- d }()
	waitFor(t, "prompt", func() bool { return s.sentCount() == 1 })

	s.push(cbUpdate(1, 1, 7, "skip_reason"))
	time.Sleep(20 * time.Millisecond)
	// Another operator's text does not count as the reason.
	s.push(textUpdate(2, 8, 100, "not mine"), textUpdate(3, 7, 100, "Customer already paid"))

	select {
	case d := <-res:
		if d.Action != "skip" || d.Text != "Customer already paid" || d.DeciderID != 7 {
			t.Fatalf("decision = %+v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("skip with reason never resolved")
	}
}

func TestTelegramSkipStandsWithoutReason(t *testing.T) {
	orig := skipReasonWait
	skipReasonWait = 30 * time.Millisecond
	defer func() { skipReasonWait = orig }()
	bot, s := newScriptedBot()
	bot.startPump(2 * time.Millisecond)
	defer bot.stopPump()

	res := make(chan OperatorDecision, 1)
	go func() { d, _ := bot.SendForApproval(context.Background(), "draft", nil); res <- d }()
	waitFor(t, "prompt", func() bool { return s.sentCount() == 1 })
	s.push(cbUpdate(1, 1, 7, "skip_reason"))

	select {
	case d := <-res:
		if d.Action != "skip" || d.Text != "" || d.DeciderID != 7 {
			t.Fatalf("decision = %+v, want a skip without reason", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("skip without reason never resolved")
	}
}

func TestTelegramPlainSkipNamesTheOperator(t *testing.T) {
	bot, s := newScriptedBot()
	bot.startPump(2 * time.Millisecond)
	defer bot.stopPump()
	res := make(chan OperatorDecision, 1)
	go func() { d, _ := bot.SendForApproval(context.Background(), "draft", nil); res <- d }()
	waitFor(t, "prompt", func() bool { return s.sentCount() == 1 })
	s.push(cbUpdate(1, 1, 8, "skip"))
	select {
	case d := <-res:
		if d.Action != "skip" || d.DeciderID != 8 {
			t.Fatalf("decision = %+v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("skip never resolved")
	}
}

func TestQuorumSkipWithReasonVetoesBeforeTheReason(t *testing.T) {
	bot, s := newScriptedBot()
	bot.startPump(2 * time.Millisecond)
	defer bot.stopPump()

	res := make(chan QuorumDecision, 1)
	go func() { d, _ := bot.SendForQuorumApproval(context.Background(), "draft", 2, nil); res <- d }()
	waitFor(t, "prompt", func() bool { return s.sentCount() == 1 })

	s.push(cbUpdate(1, 1, 7, "approve"))
	time.Sleep(20 * time.Millisecond)
	s.push(cbUpdate(2, 1, 8, "skip_reason"))
	time.Sleep(20 * time.Millisecond)
	// An approval after the veto must not complete the quorum.
	s.push(cbUpdate(3, 1, 7, "approve"), textUpdate(4, 8, 100, "Wrong invoice amount"))

	select {
	case d := <-res:
		if d.Action != "skip" || d.Text != "Wrong invoice amount" || d.DeciderID != 8 {
			t.Fatalf("decision = %+v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("veto never resolved")
	}
}

func TestRelaySkipCarriesReasonAndOperator(t *testing.T) {
	f := newFakeRelay(t)
	rc, callback := newTestRelayChannel(t, f, twoOps)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out := make(chan OperatorDecision, 1)
	go func() { d, _ := rc.SendForApproval(ctx, "draft", nil); out <- d }()
	req := f.dispatch(t)
	resp := postDecision(t, callback, relay.Decision{
		Protocol: relay.Version, ApprovalID: req.ApprovalID, Nonce: req.Callback.Nonce,
		Decision: relay.ActionSkip, PayloadHash: req.PayloadHash,
		Approver: relay.Approver{ID: "bob@example.com"}, Reason: "  duplicate of yesterday's offer ",
	}, testRelaySecret, time.Now())
	_ = resp.Body.Close()
	select {
	case d := <-out:
		if d.Action != "skip" || d.Text != "duplicate of yesterday's offer" || d.DeciderID != 222 {
			t.Fatalf("got %+v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("skip never resolved")
	}
}

func TestRecordSkipWritesSignedNote(t *testing.T) {
	st, err := statestore.OpenStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	orig := state
	state = st
	defer func() { state = orig }()
	t.Setenv("DRAFTCAT_APPROVAL_SECRET", "skip-secret")
	secret := []byte("skip-secret")
	step := config.StepConfig{Name: "send", Type: "approval"}

	if err := recordSkip(secret, "run1", "invoices", step, "draft", time.Hour, 1, 7, "  Customer already paid "); err != nil {
		t.Fatal(err)
	}
	if err := recordSkip(secret, "run1", "invoices", step, "draft2", time.Hour, 1, 8, ""); err != nil {
		t.Fatal(err)
	}
	recs, err := st.ApprovalsForRun("run1")
	if err != nil || len(recs) != 2 {
		t.Fatalf("recs=%d err=%v", len(recs), err)
	}
	ids := []string{recs[0].ReceiptID, recs[1].ReceiptID}
	notes, err := st.ApprovalNotes(ids)
	if err != nil || len(notes) != 1 {
		t.Fatalf("notes=%v err=%v", notes, err)
	}
	for _, r := range recs {
		if r.Decision != "skip" || (r.OperatorID != 7 && r.OperatorID != 8) {
			t.Errorf("receipt = %+v", r)
		}
		if v := statestore.VerifyApprovalRecord(secret, r); v != "ok" {
			t.Errorf("receipt verification = %s", v)
		}
		if n, ok := notes[r.ReceiptID]; ok {
			nv := noteView(n, secret)
			if nv.Reason != "Customer already paid" || nv.OperatorID != 7 || nv.Verification != "ok" {
				t.Errorf("note = %+v", nv)
			}
			n.Reason = "edited later"
			if noteView(n, secret).Verification != "tampered" {
				t.Error("edited note still verifies")
			}
			if noteView(n, nil).Verification != "unverified" {
				t.Error("note without key should be unverified")
			}
		}
	}
	runs := approvalsDuring(st, statestore.RunRecord{RunID: "run1"})
	found := false
	for _, a := range runs {
		if a.Reason == "Customer already paid" && a.ReasonOK == "ok" {
			found = true
		}
	}
	if !found {
		t.Fatalf("runs view lacks the reason: %+v", runs)
	}
}

func TestApprovalNotesOnStoreWithoutTable(t *testing.T) {
	st, err := statestore.OpenStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.DB().ExecContext(t.Context(), `DROP TABLE approval_notes`); err != nil {
		t.Fatal(err)
	}
	notes, err := st.ApprovalNotes([]string{"rcpt_x"})
	if err != nil || len(notes) != 0 {
		t.Fatalf("notes=%v err=%v", notes, err)
	}
}
