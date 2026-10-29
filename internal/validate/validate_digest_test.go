package validate

import (
	"testing"

	"github.com/renezander030/draftcat/internal/config"
)

func TestTelegramDigestValidation(t *testing.T) {
	cases := []struct {
		name    string
		digest  *config.DigestConfig
		wantErr bool
	}{
		{"absent", nil, false},
		{"valid", &config.DigestConfig{At: "08:00", Timezone: "Europe/Berlin"}, false},
		{"valid local zone", &config.DigestConfig{At: "21:30"}, false},
		{"missing time", &config.DigestConfig{Timezone: "UTC"}, true},
		{"bad time", &config.DigestConfig{At: "25:00"}, true},
		{"bad zone", &config.DigestConfig{At: "08:00", Timezone: "Mars/Base"}, true},
	}
	for _, c := range cases {
		cfg := &config.Config{Telegram: config.TelegramConfig{Digest: c.digest}}
		rep := &validateReport{}
		checkConfigSecurity(cfg, rep)
		got := false
		for _, f := range rep.Findings {
			if f.Path == "telegram.digest" {
				got = true
			}
		}
		if got != c.wantErr {
			t.Errorf("%s: telegram.digest error = %v, want %v (%+v)", c.name, got, c.wantErr, rep.Findings)
		}
	}
}
