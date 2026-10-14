package config

import "testing"

func TestCredentialEnvNames(t *testing.T) {
	var c Config
	if got := c.Telegram.TokenEnvName(); got != "DRAFTCAT_TG_TOKEN" {
		t.Fatalf("telegram default = %q", got)
	}
	if got := c.Relay.SecretEnvName(); got != "DRAFTCAT_RELAY_SECRET" {
		t.Fatalf("relay default = %q", got)
	}
	if got := c.Provider.APIKeyEnvName(); got != "OPENROUTER_API_KEY" {
		t.Fatalf("provider default = %q", got)
	}
	c.Telegram.TokenEnv, c.Relay.SecretEnv, c.Provider.APIKeyEnv = "MY_TG", "  ", "MY_KEY"
	if got := c.Telegram.TokenEnvName(); got != "MY_TG" {
		t.Fatalf("telegram configured = %q", got)
	}
	if got := c.Relay.SecretEnvName(); got != "DRAFTCAT_RELAY_SECRET" {
		t.Fatalf("blank relay name must fall back, got %q", got)
	}
	if got := c.Provider.APIKeyEnvName(); got != "MY_KEY" {
		t.Fatalf("provider configured = %q", got)
	}
}
