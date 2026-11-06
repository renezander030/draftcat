package validate

import (
	"github.com/renezander030/draftcat/internal/config"
)

// voicePluginBuilt is set by the voice build, the only one that counts calls
// against budgets.per_day_calls and budgets.per_day_call_minutes.
var voicePluginBuilt bool

// checkVoiceCallCaps validates the daily voice call caps. A negative cap is
// an error; a cap no call is ever counted against is a warning.
func checkVoiceCallCaps(cfg *config.Config, rep *validateReport) {
	b := cfg.Budgets
	if b.PerDayCalls < 0 {
		rep.errf("budgets.per_day_calls", "must be 0 (no cap) or a positive call count, got %d", b.PerDayCalls)
	}
	if b.PerDayCallMinutes < 0 {
		rep.errf("budgets.per_day_call_minutes", "must be 0 (no cap) or a positive minute count, got %d", b.PerDayCallMinutes)
	}
	path := "budgets.per_day_calls"
	switch {
	case b.PerDayCalls > 0:
	case b.PerDayCallMinutes > 0:
		path = "budgets.per_day_call_minutes"
	default:
		return
	}
	if !voicePluginBuilt {
		rep.warnf(path, "this build has no voice plugin (build with -tags voice); no call is counted against the cap")
		return
	}
	var voice struct {
		Enabled bool `yaml:"enabled"`
	}
	if cfg.Voice.Kind != 0 {
		_ = cfg.Voice.Decode(&voice)
	}
	if !voice.Enabled {
		rep.warnf(path, "voice.enabled is false; no call is counted against the cap")
	}
}
