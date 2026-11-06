package state

import (
	"testing"
	"time"
)

func TestShadowDiffsPruneGroupAndList(t *testing.T) {
	s := newTempStore(t)
	now := time.Unix(1_800_000_000, 0)
	add := func(at time.Time, tool, live, shadow string) {
		t.Helper()
		if err := s.RecordShadowDiff(ShadowDiff{At: at, Agent: "agent-1", Tool: tool, ArgsHash: "sha256:aa",
			LiveDecision: live, ShadowDecision: shadow, CandidateHash: "sha256:cc"}, at.Add(-30*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	add(now.Add(-40*24*time.Hour), "old", "allow", "deny")
	add(now.Add(-2*time.Hour), "send_email", "allow", "deny")
	add(now.Add(-time.Hour), "send_email", "allow", "deny")
	add(now, "create_invoice", "ask", "allow") // prunes the 40-day-old row

	all, err := s.ShadowDiffs(time.Time{}, 100)
	if err != nil || len(all) != 3 || all[0].Tool != "create_invoice" || all[2].At.Unix() != now.Add(-2*time.Hour).Unix() {
		t.Fatalf("rows, newest first, old one pruned: %+v %v", all, err)
	}
	if two, _ := s.ShadowDiffs(time.Time{}, 2); len(two) != 2 {
		t.Fatalf("limit: %+v", two)
	}
	groups, err := s.ShadowDiffGroups(now.Add(-90 * time.Minute))
	if err != nil || len(groups) != 2 {
		t.Fatalf("groups since 90m ago: %+v %v", groups, err)
	}
	for _, g := range groups {
		if g.Count != 1 {
			t.Fatalf("one row each inside the window: %+v", groups)
		}
	}
	groups, _ = s.ShadowDiffGroups(time.Time{})
	if groups[0].Tool != "send_email" || groups[0].Count != 2 || !groups[0].Latest.Equal(now.Add(-time.Hour)) {
		t.Fatalf("largest group first: %+v", groups)
	}
}

func TestToolCallsAllowedOnAndArgTotalExcept(t *testing.T) {
	s := newTempStore(t)
	day := time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)
	for i, id := range []string{"a1", "a2", "a3"} {
		at := day.Add(time.Duration(i) * time.Minute)
		if _, _, err := s.ReserveToolAction(ToolAction{ActionID: id, Tool: "pay", ArgsHash: "h", PolicyHash: "p", BindingHash: "b" + id,
			CreatedAt: at, UpdatedAt: at, ExpiresAt: at.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordToolActionAmounts(id, UTCDay(at), map[string]string{"amount": "100"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"a1", "a2"} {
		if err := s.DecideToolAction(id, "allowed", "allow", "ok", "allowlist", day); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.ToolCallsAllowedOn("pay", "2026-03-04", ""); err != nil || n != 2 {
		t.Fatalf("allowed today = %d %v", n, err)
	}
	if n, _ := s.ToolCallsAllowedOn("pay", "2026-03-04", "a2"); n != 1 {
		t.Fatalf("without a2 = %d", n)
	}
	if n, _ := s.ToolCallsAllowedOn("pay", "2026-03-05", ""); n != 0 {
		t.Fatalf("next day = %d", n)
	}
	if _, err := s.ToolCallsAllowedOn("pay", "yesterday", ""); err == nil {
		t.Fatal("a malformed day is an error")
	}
	if total, err := s.ToolArgTotalExcept("pay", "amount", "2026-03-04", "a1"); err != nil || total.RatString() != "100" {
		t.Fatalf("total without a1 = %v %v", total, err)
	}
	if total, _ := s.ToolArgTotal("pay", "amount", "2026-03-04"); total.RatString() != "200" {
		t.Fatalf("total = %v", total)
	}
}
