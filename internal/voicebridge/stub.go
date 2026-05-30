//go:build !voice

package voicebridge

import (
	"gopkg.in/yaml.v3"

	ghlapi "github.com/renezander030/draftcat/internal/ghl"
	statestore "github.com/renezander030/draftcat/internal/state"
)

// Lean build (no -tags voice): the voice plugin is compiled out. This stub
// gives package main the same Boot/Bridge API to call unconditionally, without
// importing the voice server or Dograh client — so they stay out of the binary.
// A nil *Bridge is the normal result here; the methods are no-ops.

type Bridge struct{}

func Boot(_ yaml.Node, _ *statestore.StateStore, _ *ghlapi.GHLConnector) *Bridge {
	return nil
}

func (b *Bridge) Shutdown() {}

func (b *Bridge) TryAction(action, pipelineName string, vars map[string]string, data map[string]interface{}) (bool, bool, error) {
	return false, false, nil
}
