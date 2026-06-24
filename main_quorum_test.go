package main

import (
	"context"
	"reflect"
	"testing"
)

// fakeChannel is a no-op OperatorChannel used only to assert at compile time
// that the interface (including SendForQuorumApproval) stays implementable by
// non-Telegram channels. The quorum decision logic itself is tested via the
// pure quorumReducer below, no poll loop required.
type fakeChannel struct{}

func (fakeChannel) Send(string) error { return nil }
func (fakeChannel) SendForApproval(context.Context, string) (OperatorDecision, error) {
	return OperatorDecision{Action: "skip"}, nil
}
func (fakeChannel) SendForQuorumApproval(context.Context, string, int) (QuorumDecision, error) {
	return QuorumDecision{Action: "skip"}, nil
}

var _ OperatorChannel = fakeChannel{}

func allowedSet(ids ...int64) map[int64]bool {
	m := map[int64]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func TestQuorumReachedDistinctUsers(t *testing.T) {
	got := quorumReducer(
		[]approvalEvent{{111, "approve"}, {222, "approve"}},
		2, allowedSet(111, 222, 333),
	)
	if got.Action != "approve" {
		t.Fatalf("action = %q, want approve", got.Action)
	}
	if !reflect.DeepEqual(got.Approvers, []int64{111, 222}) {
		t.Errorf("approvers = %v, want [111 222]", got.Approvers)
	}
}

func TestQuorumSameUserTwiceCountsOnce(t *testing.T) {
	got := quorumReducer(
		[]approvalEvent{{111, "approve"}, {111, "approve"}},
		2, allowedSet(111, 222),
	)
	if got.Action == "approve" {
		t.Errorf("same user twice must not reach a quorum of 2, got %q", got.Action)
	}
}

func TestQuorumSingleSkipVetoes(t *testing.T) {
	got := quorumReducer(
		[]approvalEvent{{111, "approve"}, {222, "skip"}},
		3, allowedSet(111, 222, 333),
	)
	if got.Action != "skip" {
		t.Errorf("a single skip must veto immediately, got %q", got.Action)
	}
}

func TestQuorumRejectsDisallowedUser(t *testing.T) {
	got := quorumReducer(
		[]approvalEvent{{999, "approve"}},
		1, allowedSet(111),
	)
	if got.Action == "approve" {
		t.Errorf("disallowed user must be ignored, got %q", got.Action)
	}
}

func TestQuorumAdjustResetsTally(t *testing.T) {
	got := quorumReducer(
		[]approvalEvent{{111, "approve"}, {222, "adjust"}},
		2, allowedSet(111, 222),
	)
	if got.Action != "adjust" {
		t.Errorf("adjust must take the floor, got %q", got.Action)
	}
}

func TestQuorumBackCompat(t *testing.T) {
	got := quorumReducer(
		[]approvalEvent{{111, "approve"}},
		1, allowedSet(111),
	)
	if got.Action != "approve" {
		t.Errorf("need<=1 should behave like single-approver, got %q", got.Action)
	}
}
