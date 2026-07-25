package main

import (
	"testing"

	"github.com/renezander030/draftcat/internal/config"
)

// A step's `approvers:` list narrows who may decide that gate. The critical
// property is that it INTERSECTS with the channel's allowed_users and never
// widens it — a step must not be able to hand approval rights to someone the
// channel does not already trust.

func botWithAllowed(ids ...int64) *TGBot {
	return &TGBot{security: config.ChannelSecurity{AllowedUsers: ids}}
}

func TestIsApprover_EmptyListMeansAnyAllowedUser(t *testing.T) {
	b := botWithAllowed(111, 222)
	if !b.isApprover(222, nil) {
		t.Error("with no step scoping, any allowed user may approve")
	}
}

func TestIsApprover_NarrowsToListedOperator(t *testing.T) {
	b := botWithAllowed(111, 222, 333)
	if !b.isApprover(222, []int64{222}) {
		t.Error("listed approver 222 should be permitted")
	}
	if b.isApprover(333, []int64{222}) {
		t.Error("operator 333 is allowed on the channel but NOT on this step — must be rejected")
	}
}

// The security-critical case: a step cannot widen the channel's trust set.
// If it could, a typo'd or malicious id in a step would grant approval rights
// to a stranger.
func TestIsApprover_CannotWidenBeyondAllowedUsers(t *testing.T) {
	b := botWithAllowed(111)
	if b.isApprover(999, []int64{999}) {
		t.Fatal("step approvers must never grant rights to a non-allowed user — this is a privilege escalation")
	}
}

func TestIsApprover_RejectsUnknownUserEntirely(t *testing.T) {
	b := botWithAllowed(111, 222)
	if b.isApprover(555, nil) {
		t.Error("a user absent from allowed_users must always be rejected")
	}
}

// isAllowedUser is the pre-existing entry point (used by non-step paths such as
// the /reply command); it must keep behaving as an unscoped check.
func TestIsAllowedUser_UnchangedSemantics(t *testing.T) {
	b := botWithAllowed(111, 222)
	if !b.isAllowedUser(111) {
		t.Error("allowed user should pass the unscoped check")
	}
	if b.isAllowedUser(333) {
		t.Error("non-allowed user should fail the unscoped check")
	}
}
