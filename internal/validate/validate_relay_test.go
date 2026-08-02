package validate

import (
	"strings"
	"testing"

	"github.com/renezander030/draftcat/internal/config"
)

// findings collects the report's error paths for assertion.
func relayFindings(t *testing.T, cfg *config.Config) []string {
	t.Helper()
	rep := &validateReport{}
	checkRelay(cfg, rep)
	var out []string
	for _, f := range rep.Findings {
		if f.Level == "error" {
			out = append(out, f.Path+": "+f.Message)
		}
	}
	return out
}

func hasPath(findings []string, prefix string) bool {
	for _, f := range findings {
		if strings.HasPrefix(f, prefix) {
			return true
		}
	}
	return false
}

func goodRelay() *config.Config {
	return &config.Config{Relay: config.RelayConfig{
		URL:       "https://flow.example.com/trigger",
		SecretEnv: "DRAFTCAT_RELAY_SECRET",
		PublicURL: "https://gate.example.com",
		Operators: []config.RelayOperator{
			{ID: 111, Identity: "alice@example.com"},
			{ID: 222, Identity: "bob@example.com"},
		},
		Security: config.ChannelSecurity{AllowedUsers: []int64{111, 222}},
	}}
}

func TestCheckRelay_DisabledIsSilent(t *testing.T) {
	if f := relayFindings(t, &config.Config{}); len(f) != 0 {
		t.Fatalf("an unconfigured relay produced errors: %v", f)
	}
}

func TestCheckRelay_ValidConfigPasses(t *testing.T) {
	t.Setenv("DRAFTCAT_RELAY_SECRET", "s3cret")
	if f := relayFindings(t, goodRelay()); len(f) != 0 {
		t.Fatalf("a valid relay config produced errors: %v", f)
	}
}

func TestCheckRelay_MissingSecretIsAnError(t *testing.T) {
	t.Setenv("DRAFTCAT_RELAY_SECRET", "")
	if f := relayFindings(t, goodRelay()); !hasPath(f, "relay.secret_env") {
		t.Fatalf("an empty relay secret was accepted: %v", f)
	}
}

func TestCheckRelay_MissingPublicURLIsAnError(t *testing.T) {
	t.Setenv("DRAFTCAT_RELAY_SECRET", "s3cret")
	cfg := goodRelay()
	cfg.Relay.PublicURL = ""
	if f := relayFindings(t, cfg); !hasPath(f, "relay.public_url") {
		t.Fatalf("a relay with no public_url was accepted — decisions would have nowhere to land: %v", f)
	}
}

func TestCheckRelay_RelativeURLsRejected(t *testing.T) {
	t.Setenv("DRAFTCAT_RELAY_SECRET", "s3cret")
	cfg := goodRelay()
	cfg.Relay.URL = "flow.example.com/trigger"
	cfg.Relay.PublicURL = "gate.example.com"
	f := relayFindings(t, cfg)
	if !hasPath(f, "relay.url") || !hasPath(f, "relay.public_url") {
		t.Fatalf("scheme-less URLs were accepted: %v", f)
	}
}

// An allowed user with no wire identity can never be presented to, so a quorum
// that looks satisfiable would hang to timeout. That must fail at validate.
func TestCheckRelay_AllowedUserWithoutIdentityIsAnError(t *testing.T) {
	t.Setenv("DRAFTCAT_RELAY_SECRET", "s3cret")
	cfg := goodRelay()
	cfg.Relay.Security.AllowedUsers = []int64{111, 222, 333}
	if f := relayFindings(t, cfg); !hasPath(f, "relay.security.allowed_users") {
		t.Fatalf("an allowed user with no relay identity was accepted: %v", f)
	}
}

func TestCheckRelay_DuplicateOperatorsRejected(t *testing.T) {
	t.Setenv("DRAFTCAT_RELAY_SECRET", "s3cret")
	cfg := goodRelay()
	cfg.Relay.Operators = append(cfg.Relay.Operators, config.RelayOperator{ID: 111, Identity: "carol@example.com"})
	if f := relayFindings(t, cfg); !hasPath(f, "relay.operators[2].id") {
		t.Fatalf("a duplicate operator id was accepted: %v", f)
	}
	cfg2 := goodRelay()
	cfg2.Relay.Operators = append(cfg2.Relay.Operators, config.RelayOperator{ID: 333, Identity: "alice@example.com"})
	if f := relayFindings(t, cfg2); !hasPath(f, "relay.operators[2].identity") {
		t.Fatalf("a duplicate operator identity was accepted: %v", f)
	}
}

// Operator id 0 is the audit trail's marker for a system/timeout row, so it must
// not also name a human.
func TestCheckRelay_ZeroOperatorIDRejected(t *testing.T) {
	t.Setenv("DRAFTCAT_RELAY_SECRET", "s3cret")
	cfg := goodRelay()
	cfg.Relay.Operators = []config.RelayOperator{{ID: 0, Identity: "alice@example.com"}}
	cfg.Relay.Security.AllowedUsers = []int64{0}
	if f := relayFindings(t, cfg); !hasPath(f, "relay.operators[0].id") {
		t.Fatalf("operator id 0 was accepted: %v", f)
	}
}

func TestCheckRelay_NoOperatorsIsAnError(t *testing.T) {
	t.Setenv("DRAFTCAT_RELAY_SECRET", "s3cret")
	cfg := goodRelay()
	cfg.Relay.Operators = nil
	cfg.Relay.Security.AllowedUsers = nil
	f := relayFindings(t, cfg)
	if !hasPath(f, "relay.operators") || !hasPath(f, "relay.security.allowed_users") {
		t.Fatalf("a relay admitting no approver was accepted: %v", f)
	}
}
