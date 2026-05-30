//go:build voice

package main

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/renezander030/draftcat/voice"
)

var (
	voiceServer *voice.Server
	voiceCfg    voice.Config
)

func bootVoice(cfg *Config, st *StateStore) {
	if st == nil {
		return
	}
	var vcfg voice.Config
	if err := cfg.Voice.Decode(&vcfg); err != nil {
		log.Printf("[voice] config decode failed (skipping voice): %v", err)
		return
	}
	if !vcfg.Enabled {
		return
	}
	voiceCfg = vcfg
	srv, err := voice.NewServer(vcfg, st.DB())
	if err != nil {
		log.Printf("[voice] init failed: %v", err)
		return
	}
	if srv == nil {
		return
	}
	voiceServer = srv
	registerVoiceLookups(srv)
	go func() {
		if err := srv.Start(); err != nil && err.Error() != "http: Server closed" {
			log.Printf("[voice] server stopped: %v", err)
		}
	}()
}

// registerVoiceLookups wires draftcat's existing connectors into the voice
// plugin's pre-call lookup runner. Each backend is opt-in: when the underlying
// connector isn't configured, the source is simply not registered and the
// lookup runner emits a warning if a pipeline tries to use it.
func registerVoiceLookups(srv *voice.Server) {
	if ghl == nil {
		return
	}
	srv.Lookups().Register("ghl", func(ctx context.Context, phone string) (map[string]any, error) {
		c, err := ghl.FetchContactByPhone(phone)
		if err != nil {
			return nil, err
		}
		if c == nil {
			return nil, nil
		}
		out := map[string]any{
			"account_name": strings.TrimSpace(c.FirstName + " " + c.LastName),
			"email":        c.Email,
			"contact_id":   c.ID,
			"source":       c.Source,
		}
		for _, cf := range c.CustomField {
			if cf.ID != "" && cf.Value != "" {
				out["cf_"+cf.ID] = cf.Value
			}
		}
		return out, nil
	})
}

func shutdownVoice() {
	if voiceServer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := voiceServer.Shutdown(ctx); err != nil {
		log.Printf("[voice] shutdown error: %v", err)
	}
}
