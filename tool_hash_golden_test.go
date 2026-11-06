package main

import (
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
)

// The permit binding and the gate's policy hash are stored with every action
// and receipt; these values pin their exact bytes.
func TestToolGateHashesAreStable(t *testing.T) {
	exp := time.Unix(1_800_000_000, 0)
	req := ToolCallRequest{ActionID: "send-1", Agent: "a", Tool: "send_email", RunID: "r1"}
	if got := hashToolBinding(req, "sha256:aa", "sha256:bb", exp); got != "sha256:8ff6832f317b0082b1e1b008eb5180367fb0e103b305cc9339a4379cadacd10a" {
		t.Fatalf("v2 binding: %s", got)
	}
	req.Precondition = `etag:"v7"`
	if got := hashToolBinding(req, "sha256:aa", "sha256:bb", exp); got != "sha256:9abd73a9e9c16d66f84c6bacd631254cff80784ffb074cedcdbd8d95038caf69" {
		t.Fatalf("v3 binding: %s", got)
	}
	g := newToolGate(&config.Config{ToolGate: config.ToolGateConfig{Enabled: true}, Timeouts: config.TimeoutConfig{OperatorApproval: "2s"}}, nil)
	if got := g.policyHash("send_email", config.ToolRule{Name: "send_email", RequireApproval: true}, true); got != "sha256:f32b80070c17fef0359bb67abde97a34fe8c9196f596d2550b7d36dd9215a63c" {
		t.Fatalf("policy hash: %s", got)
	}
	// A rule pinned by definition_sha256, and the same pin moved into a
	// definition_file (keys in another order): one policy hash.
	const pinnedPolicy = "sha256:f46dc5621d1f691036bf88e6e20cc8bfe7a6de57b146076096a2666200377c82"
	byHash := config.ToolRule{Name: "send_email", RequireApproval: true, DefinitionSHA256: pinOf(t, pinDefinition)}
	if got := g.policyHash("send_email", byHash, true); got != pinnedPolicy {
		t.Fatalf("definition_sha256 policy hash: %s", got)
	}
	byFile := pinToFile(t, config.ToolRule{Name: "send_email", RequireApproval: true}, pinDefinitionReordered)
	if got := g.policyHash("send_email", byFile, true); got != pinnedPolicy {
		t.Fatalf("definition_file policy hash: %s", got)
	}
}
