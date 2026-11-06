package validate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/renezander030/draftcat/internal/config"
)

func TestReceiptAnchorValidation(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state", "state.db")
	t.Setenv("DRAFTCAT_STATE_PATH", statePath)
	offDisk := filepath.Join(dir, "mounted", "anchors.jsonl")
	cases := []struct {
		name   string
		anchor *config.ReceiptAnchorConfig
		want   map[string]string // path -> level
	}{
		{"absent", nil, nil},
		{"every with file", &config.ReceiptAnchorConfig{Every: "24h", File: offDisk}, nil},
		{"at with zone and notify", &config.ReceiptAnchorConfig{At: "07:00", Timezone: "Europe/Berlin", Notify: true}, nil},
		{"neither every nor at", &config.ReceiptAnchorConfig{Notify: true}, map[string]string{"receipts.anchor": "error"}},
		{"both every and at", &config.ReceiptAnchorConfig{Every: "1h", At: "07:00", Notify: true}, map[string]string{"receipts.anchor": "error"}},
		{"every below the minimum", &config.ReceiptAnchorConfig{Every: "30s", Notify: true}, map[string]string{"receipts.anchor": "error"}},
		{"every not a duration", &config.ReceiptAnchorConfig{Every: "daily", Notify: true}, map[string]string{"receipts.anchor": "error"}},
		{"bad time", &config.ReceiptAnchorConfig{At: "7am", Notify: true}, map[string]string{"receipts.anchor": "error"}},
		{"bad zone", &config.ReceiptAnchorConfig{At: "07:00", Timezone: "Mars/Base", Notify: true}, map[string]string{"receipts.anchor": "error"}},
		{"kept nowhere", &config.ReceiptAnchorConfig{Every: "24h"}, map[string]string{"receipts.anchor": "error"}},
		{"zone with every", &config.ReceiptAnchorConfig{Every: "24h", Timezone: "UTC", Notify: true}, map[string]string{"receipts.anchor.timezone": "warn"}},
		{"file in the state directory", &config.ReceiptAnchorConfig{Every: "24h", File: filepath.Join(dir, "state", "anchors.jsonl")}, map[string]string{"receipts.anchor.file": "warn"}},
		{"file below the state directory", &config.ReceiptAnchorConfig{Every: "24h", File: filepath.Join(dir, "state", "a", "anchors.jsonl"), Notify: true}, map[string]string{"receipts.anchor.file": "warn"}},
		{"file is the store", &config.ReceiptAnchorConfig{Every: "24h", File: statePath}, map[string]string{"receipts.anchor.file": "error"}},
		{"file is the store's WAL", &config.ReceiptAnchorConfig{Every: "24h", File: statePath + "-wal"}, map[string]string{"receipts.anchor.file": "error"}},
		{"file is a directory", &config.ReceiptAnchorConfig{Every: "24h", File: dir}, map[string]string{"receipts.anchor.file": "error"}},
	}
	if err := os.MkdirAll(filepath.Dir(offDisk), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		rep := &validateReport{}
		checkReceiptAnchor(&config.Config{Receipts: config.ReceiptsConfig{Anchor: c.anchor}}, rep)
		got := map[string]string{}
		for _, f := range rep.Findings {
			got[f.Path] = f.Level
		}
		if len(got) != len(c.want) || len(rep.Findings) != len(c.want) {
			t.Errorf("%s: findings %+v, want %v", c.name, rep.Findings, c.want)
			continue
		}
		for p, l := range c.want {
			if got[p] != l {
				t.Errorf("%s: findings %+v, want %v", c.name, rep.Findings, c.want)
			}
		}
	}
}
