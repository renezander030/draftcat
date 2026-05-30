//go:build !voice

package main

import statestore "github.com/renezander030/draftcat/internal/state"

func bootVoice(cfg *Config, st *statestore.StateStore) {}
func shutdownVoice()                                   {}
