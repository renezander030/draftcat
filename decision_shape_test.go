package main

import (
	"reflect"
	"testing"
)

func TestDecisionShapes(t *testing.T) {
	cases := []struct {
		single OperatorDecision
		quorum QuorumDecision
	}{
		{OperatorDecision{Action: "approve", ApproverID: 7}, QuorumDecision{Action: "approve", Approvers: []int64{7}}},
		{OperatorDecision{Action: "approve"}, QuorumDecision{Action: "approve"}},
		{OperatorDecision{Action: "skip", Text: "no", DeciderID: 3}, QuorumDecision{Action: "skip", Text: "no", DeciderID: 3}},
		{OperatorDecision{Action: "adjust", Text: "shorter", DeciderID: 4}, QuorumDecision{Action: "adjust", Text: "shorter", DeciderID: 4}},
	}
	for _, tc := range cases {
		if got := tc.single.asQuorum(); !reflect.DeepEqual(got, tc.quorum) {
			t.Fatalf("%+v.asQuorum() = %+v, want %+v", tc.single, got, tc.quorum)
		}
		if got := tc.quorum.asSingle(); got != tc.single {
			t.Fatalf("%+v.asSingle() = %+v, want %+v", tc.quorum, got, tc.single)
		}
	}
	// The first approver stands for the gate; a non-approve never names one.
	if got := (QuorumDecision{Action: "approve", Approvers: []int64{9, 2}}).asSingle(); got.ApproverID != 9 {
		t.Fatalf("first approver = %d, want 9", got.ApproverID)
	}
	if got := (OperatorDecision{Action: "skip", ApproverID: 5}).asQuorum(); got.Approvers != nil {
		t.Fatalf("skip carried approvers %v", got.Approvers)
	}
}
