package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestQuorumDefaultsToOne: a StepConfig with no `quorum` key decodes to 0, which
// the runner treats as a single approver (<=1). No magic default of 1 is needed.
func TestQuorumDefaultsToOne(t *testing.T) {
	var sc StepConfig
	if err := yaml.Unmarshal([]byte("name: gate\ntype: approval\nchannel: telegram\n"), &sc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sc.Quorum != 0 {
		t.Errorf("Quorum default = %d, want 0 (single approver)", sc.Quorum)
	}
}

// TestQuorumParsed: an explicit quorum value round-trips.
func TestQuorumParsed(t *testing.T) {
	var sc StepConfig
	if err := yaml.Unmarshal([]byte("name: gate\ntype: approval\nchannel: telegram\nquorum: 2\n"), &sc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sc.Quorum != 2 {
		t.Errorf("Quorum = %d, want 2", sc.Quorum)
	}
}
