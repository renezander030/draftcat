package main

import (
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/renezander030/draftcat/internal/config"
)

const defaultStatePath = "./state.db"

// resolveStatePath is where the engine and every subcommand find the state store:
// DRAFTCAT_STATE_PATH first (so a container can point state at a mounted disk
// without shadowing the baked config), then the config's state.path, then
// ./state.db.
func resolveStatePath(cfg *config.Config) string {
	if v := strings.TrimSpace(os.Getenv("DRAFTCAT_STATE_PATH")); v != "" {
		return v
	}
	if cfg != nil && cfg.State.Path != "" {
		return cfg.State.Path
	}
	return defaultStatePath
}

// statePathFromConfigFile applies resolveStatePath for a subcommand that only has a
// --config argument. The config is read only when the env override is unset;
// an unreadable or unparsable file falls through to the default.
func statePathFromConfigFile(configPath string) string {
	if v := strings.TrimSpace(os.Getenv("DRAFTCAT_STATE_PATH")); v != "" {
		return v
	}
	var cfg config.Config
	// #nosec G304 -- configPath is the operator's own --config argument on
	// their own machine; reading the file they named is the feature.
	if data, err := os.ReadFile(configPath); err == nil && yaml.Unmarshal(data, &cfg) == nil {
		return resolveStatePath(&cfg)
	}
	return defaultStatePath
}
