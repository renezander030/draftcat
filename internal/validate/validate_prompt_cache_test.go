package validate

import (
	"testing"

	"github.com/renezander030/draftcat/internal/config"
)

func TestCheckPromptCacheWarnsWhereNoMarkerIsSent(t *testing.T) {
	cfg := &config.Config{
		Provider: config.ProviderConfig{Type: "openrouter"},
		Models: map[string]config.ModelConfig{
			"haiku": {Model: "anthropic/claude-haiku-5.5", PromptCache: true},
			"gpt":   {Model: "openai/gpt-4o-mini", PromptCache: true},
			"off":   {Model: "openai/gpt-4o-mini"},
		},
	}
	rep := &validateReport{}
	checkPromptCache(cfg, rep)
	if len(rep.Findings) != 1 || rep.Findings[0].Path != "models.gpt.prompt_cache" || rep.Findings[0].Level != "warn" {
		t.Fatalf("findings %+v, want one warning on models.gpt.prompt_cache", rep.Findings)
	}

	cfg.Provider.Type = "openai"
	rep = &validateReport{}
	checkPromptCache(cfg, rep)
	if len(rep.Findings) != 2 {
		t.Fatalf("findings %+v, want both prompt_cache models warned off OpenRouter", rep.Findings)
	}
}
