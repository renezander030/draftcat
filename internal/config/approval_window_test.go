package config

import (
	"testing"
	"time"
)

func TestOperatorApprovalWindow(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", DefaultOperatorApproval},
		{"0", DefaultOperatorApproval},
		{"-1h", DefaultOperatorApproval},
		{"soon", DefaultOperatorApproval},
		{"\t1h", DefaultOperatorApproval}, // not trimmed, as before
		{"90m", 90 * time.Minute},
	}
	for _, tc := range cases {
		if got := (TimeoutConfig{OperatorApproval: tc.in}).OperatorApprovalWindow(); got != tc.want {
			t.Errorf("OperatorApprovalWindow(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	if DefaultOperatorApproval != 4*time.Hour {
		t.Fatalf("default approval window changed: %v", DefaultOperatorApproval)
	}
}

func TestToolRuleApprovalWindow(t *testing.T) {
	const global = 4 * time.Hour
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", global},
		{"0", global},
		{"-1m", global},
		{"soon", global},
		{"2s", 2 * time.Second},
		{"8h", 8 * time.Hour},
	}
	for _, tc := range cases {
		if got := (ToolRule{ApprovalWindow: tc.in}).ApprovalWindowOr(global); got != tc.want {
			t.Errorf("ApprovalWindowOr(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
