package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	statestore "github.com/renezander030/draftcat/internal/state"
)

// seedAnchorStore records count signed v2 receipts rcpt_1..rcpt_n and points
// the read-only commands at the store.
func seedAnchorStore(t *testing.T, n int) (*statestore.StateStore, func(int)) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := statestore.OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	t.Setenv("DRAFTCAT_STATE_PATH", path)
	secret := []byte("anchor-secret")
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	next := 0
	add := func(k int) {
		for range k {
			next++
			at := base.Add(time.Duration(next) * time.Minute)
			e := statestore.ApprovalEnvelope{ReceiptID: fmt.Sprintf("rcpt_%d", next), Pipeline: "replies", Step: "send", DecidedAt: at,
				Decision: "approve", OperatorID: 7, PayloadHash: "sha256:" + strings.Repeat("b", 64),
				QuorumN: 1, QuorumGot: 1, ExpiresAt: at.Add(time.Hour), Lifecycle: "decided"}
			signApprovalEnvelope(secret, &e)
			if err := st.RecordApprovalV2(e); err != nil {
				t.Fatal(err)
			}
		}
	}
	add(n)
	return st, add
}

func anchorJSON(t *testing.T) (receiptAnchor, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := runReceiptsAnchor([]string{"--json"}, &out, &errOut); code != 0 {
		t.Fatalf("anchor exit %d: %s", code, errOut.String())
	}
	var a receiptAnchor
	if err := json.Unmarshal(out.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "anchor.json")
	if err := os.WriteFile(file, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return a, file
}

func verifyAnchor(t *testing.T, anchor string) (anchorResult, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runReceiptsVerifyAnchor([]string{anchor, "--json"}, strings.NewReader(""), &out, &errOut)
	var res anchorResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("exit %d, stdout %q, stderr %q: %v", code, out.String(), errOut.String(), err)
	}
	return res, code
}

func execAnchorSQL(t *testing.T, st *statestore.StateStore, q string) {
	t.Helper()
	if _, err := st.DB().ExecContext(context.Background(), q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func TestReceiptAnchorStableAndDescribesHistory(t *testing.T) {
	seedAnchorStore(t, 4)
	first, file := anchorJSON(t)
	second, _ := anchorJSON(t)
	if first.Link != second.Link || len(first.Trail) != 4 {
		t.Fatalf("anchor must be stable: %+v vs %+v", first, second)
	}
	if first.Count != 4 || first.FirstReceiptID != "rcpt_1" || first.LastReceiptID != "rcpt_4" ||
		first.LastDecidedAt != "2026-10-01T09:04:00Z" || !validLink(first.Link) {
		t.Fatalf("anchor summary: %+v", first)
	}
	for _, arg := range []string{file, first.Link} {
		res, code := verifyAnchor(t, arg)
		if code != 0 || res.Status != "match" || res.AnchorCount != 4 || res.Appended != 0 {
			t.Fatalf("%s: unchanged history must match: %+v (exit %d)", arg, res, code)
		}
	}
	var out, errOut bytes.Buffer
	if code := runReceiptsAnchor(nil, &out, &errOut); code != 0 || !strings.Contains(out.String(), "anchor        "+first.Link) {
		t.Fatalf("text anchor (exit %d):\n%s", code, out.String())
	}
}

func TestReceiptAnchorEmptyStore(t *testing.T) {
	_, add := seedAnchorStore(t, 0)
	a, file := anchorJSON(t)
	genesis := sha256.Sum256([]byte("draftcat-receipt-chain/v1"))
	if a.Count != 0 || a.Link != "sha256:"+hex.EncodeToString(genesis[:]) || a.FirstReceiptID != "" {
		t.Fatalf("empty anchor: %+v", a)
	}
	add(2)
	res, code := verifyAnchor(t, file)
	if code != 0 || res.Status != "match" || res.Appended != 2 {
		t.Fatalf("receipts after an empty anchor are appended: %+v", res)
	}
}

func TestReceiptAnchorAcceptsAppendedReceipts(t *testing.T) {
	_, add := seedAnchorStore(t, 3)
	a, file := anchorJSON(t)
	add(2)
	for _, arg := range []string{file, a.Link} {
		res, code := verifyAnchor(t, arg)
		if code != 0 || res.Status != "match" || res.AnchorCount != 3 || res.StoreCount != 5 || res.Appended != 2 {
			t.Fatalf("%s: appended receipts must still verify: %+v", arg, res)
		}
	}
}

func TestReceiptAnchorDetectsTampering(t *testing.T) {
	cases := []struct {
		name, kind, anchorID, storeID string
		position                      int
		tamper                        func(*testing.T, *statestore.StateStore)
	}{
		{"delete middle", "deleted", "rcpt_3", "rcpt_4", 3, func(t *testing.T, st *statestore.StateStore) {
			execAnchorSQL(t, st, `DELETE FROM action_approvals WHERE receipt_id='rcpt_3'`)
		}},
		{"delete last", "deleted", "rcpt_5", "", 5, func(t *testing.T, st *statestore.StateStore) {
			execAnchorSQL(t, st, `DELETE FROM action_approvals WHERE receipt_id='rcpt_5'`)
		}},
		{"swap two", "reordered", "rcpt_2", "rcpt_4", 2, func(t *testing.T, st *statestore.StateStore) {
			execAnchorSQL(t, st, `UPDATE action_approvals SET id=-1 WHERE receipt_id='rcpt_2'`)
			execAnchorSQL(t, st, `UPDATE action_approvals SET id=2 WHERE receipt_id='rcpt_4'`)
			execAnchorSQL(t, st, `UPDATE action_approvals SET id=4 WHERE receipt_id='rcpt_2'`)
		}},
		{"edit signed field", "changed", "rcpt_4", "rcpt_4", 4, func(t *testing.T, st *statestore.StateStore) {
			execAnchorSQL(t, st, `UPDATE action_approvals SET operator_id=999 WHERE receipt_id='rcpt_4'`)
		}},
		{"edit unsigned lifecycle", "changed", "rcpt_1", "rcpt_1", 1, func(t *testing.T, st *statestore.StateStore) {
			execAnchorSQL(t, st, `UPDATE action_approvals SET lifecycle='executed' WHERE receipt_id='rcpt_1'`)
		}},
		{"insert", "inserted", "rcpt_3", "rcpt_forged", 3, func(t *testing.T, st *statestore.StateStore) {
			// Shift rows 3..5 up to open a slot, then place a forged row in it.
			for _, id := range []int{5, 4, 3} {
				execAnchorSQL(t, st, fmt.Sprintf(`UPDATE action_approvals SET id=%d WHERE id=%d`, id+10, id))
			}
			execAnchorSQL(t, st, `INSERT INTO action_approvals (id, pipeline, step, decided_at, decision, operator_id, payload_hash, receipt_version, receipt_id)
				VALUES (3, 'replies', 'send', 1790850000, 'approve', 7, 'sha256:x', 2, 'rcpt_forged')`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := seedAnchorStore(t, 5)
			a, file := anchorJSON(t)
			tc.tamper(t, st)
			res, code := verifyAnchor(t, file)
			if code != 1 || res.Status != "mismatch" || res.Position != tc.position || res.Kind != tc.kind ||
				res.AnchorReceiptID != tc.anchorID || res.StoreReceiptID != tc.storeID {
				t.Fatalf("want %s at %d (%s/%s), got %+v (exit %d)", tc.kind, tc.position, tc.anchorID, tc.storeID, res, code)
			}
			if res, code := verifyAnchor(t, a.Link); code != 1 || res.Status != "mismatch" {
				t.Fatalf("a bare link must also fail: %+v", res)
			}
		})
	}
}

func TestReceiptAnchorRefusesMalformedAnchor(t *testing.T) {
	seedAnchorStore(t, 1)
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"format":"other","count":0,"link":"sha256:00","trail":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"sha256:abc", bad, filepath.Join(t.TempDir(), "missing.json")} {
		var out, errOut bytes.Buffer
		if code := runReceiptsVerifyAnchor([]string{arg}, strings.NewReader(""), &out, &errOut); code != 2 {
			t.Fatalf("%s: malformed anchor must exit 2, got %d", arg, code)
		}
	}
}

func TestReceiptAnchorDoesNotCreateStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")
	t.Setenv("DRAFTCAT_STATE_PATH", path)
	var out, errOut bytes.Buffer
	if code := runReceiptsAnchor(nil, &out, &errOut); code == 0 {
		t.Fatal("anchor of a missing store must fail")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("anchor must not create a store: %v", err)
	}
}

func TestSplitAnchorFlagKeepsPositionals(t *testing.T) {
	rest, anchor, present, ok := splitAnchorFlag([]string{"replies", "--anchor", "a.json", "c.yaml"})
	if !ok || !present || anchor != "a.json" || strings.Join(rest, ",") != "replies,c.yaml" {
		t.Fatalf("rest=%v anchor=%q present=%v ok=%v", rest, anchor, present, ok)
	}
	if rest, _, present, ok := splitAnchorFlag([]string{"replies"}); !ok || present || len(rest) != 1 {
		t.Fatal("without the flag the arguments pass through")
	}
	if _, _, _, ok := splitAnchorFlag([]string{"replies", "--anchor"}); ok {
		t.Fatal("--anchor without a value is refused")
	}
}

func TestAuditVerifyAnchorFlag(t *testing.T) {
	st, _ := seedAnchorStore(t, 3)
	t.Setenv("DRAFTCAT_APPROVAL_SECRET", "anchor-secret")
	_, file := anchorJSON(t)
	if code := runAuditVerify([]string{"replies", "--anchor", file}); code != 0 {
		t.Fatalf("matching anchor: exit %d", code)
	}
	execAnchorSQL(t, st, `DELETE FROM action_approvals WHERE receipt_id='rcpt_2'`)
	if code := runAuditVerify([]string{"replies"}); code != 0 {
		t.Fatalf("each remaining receipt still verifies on its own: exit %d", code)
	}
	if code := runAuditVerify([]string{"replies", "--anchor=" + file}); code != 1 {
		t.Fatalf("a deleted receipt must fail the anchor: exit %d", code)
	}
}
