package main

import (
	"fmt"
	"maps"
	"slices"

	"github.com/renezander030/draftcat/internal/config"
)

// A rule's definition_file holds the approved tool definition. It is read at
// start, by the offline `draftcat gate` checks, and again on every reload, so
// approving a tool's new definition is replacing the file and reloading. A
// file that cannot be read is refused like an invalid config: the gate never
// decides on a pin it does not have.

// loadToolDefinitions reads every rule's definition_file and pins the rule to
// it. The rules are copied first, so the config as parsed, which a reload
// compares against, is left as it was.
func loadToolDefinitions(gate *config.ToolGateConfig) error {
	pins, err := readToolDefinitions(gate.Tools)
	if err != nil || len(pins) == 0 {
		return err
	}
	tools := slices.Clone(gate.Tools)
	for i, p := range pins {
		tools[i].SetPinned(p)
	}
	gate.Tools = tools
	return nil
}

// readToolDefinitions reads the definition_file of each rule that has one, by
// the rule's index, without touching the rules.
func readToolDefinitions(tools []config.ToolRule) (map[int]*config.PinnedDefinition, error) {
	pins := map[int]*config.PinnedDefinition{}
	for i, t := range tools {
		if t.DefinitionFile == "" {
			continue
		}
		at := fmt.Sprintf("tool_gate.tools[%d] (%s)", i, t.Name)
		if t.DefinitionSHA256 != "" {
			return nil, fmt.Errorf("%s: set definition_sha256 or definition_file, not both", at)
		}
		def, err := config.ReadToolDefinition(t.DefinitionFile)
		if err != nil {
			return nil, fmt.Errorf("%s.definition_file: %w", at, err)
		}
		pins[i] = &config.PinnedDefinition{Definition: def, SHA256: hashCanonical(def)}
	}
	return pins, nil
}

// applyToolDefinitions swaps freshly read definitions into running rules and
// returns the tools whose pin changed. Copies of the rules share the pin, so
// the running gate decides on the new one from its next call.
func applyToolDefinitions(tools []config.ToolRule, pins map[int]*config.PinnedDefinition) []string {
	var changed []string
	for _, i := range slices.Sorted(maps.Keys(pins)) {
		if old := tools[i].Pinned(); old == nil || old.SHA256 != pins[i].SHA256 {
			changed = append(changed, tools[i].Name)
		}
		tools[i].SetPinned(pins[i])
	}
	return changed
}
