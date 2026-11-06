package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStatePathFromConfigFile(t *testing.T) {
	dir := t.TempDir()
	withPath := filepath.Join(dir, "with.yaml")
	if err := os.WriteFile(withPath, []byte("state:\n  path: /data/state.db\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blank := filepath.Join(dir, "blank.yaml")
	if err := os.WriteFile(blank, []byte("state:\n  path: \"  \"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(broken, []byte("state: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, env, config, want, wantZK string
	}{
		{"env wins over config", " /env/state.db ", withPath, "/env/state.db", "/env/state.db"},
		{"config path", "", withPath, "/data/state.db", "/data/state.db"},
		{"missing config defaults", "", filepath.Join(dir, "absent.yaml"), "./state.db", "./state.db"},
		{"unparsable config defaults", "", broken, "./state.db", "./state.db"},
		{"whitespace path kept except by zk", "", blank, "  ", "./state.db"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DRAFTCAT_STATE_PATH", tc.env)
			if got := statePathFromConfigFile(tc.config); got != tc.want {
				t.Fatalf("statePathFromConfigFile = %q, want %q", got, tc.want)
			}
			if got := resolveZKStatePath(tc.config); got != tc.wantZK {
				t.Fatalf("resolveZKStatePath = %q, want %q", got, tc.wantZK)
			}
		})
	}
}

// budget status reads the pipelines' caps from the same --config; a file it
// cannot read or parse shows no caps rather than failing the command.
func TestPipelineCapsFromConfigFile(t *testing.T) {
	dir := t.TempDir()
	capped := filepath.Join(dir, "capped.yaml")
	if err := os.WriteFile(capped, []byte("pipelines:\n  - name: leads\n    budget:\n      per_day_tokens: 500\n  - name: digest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(broken, []byte("pipelines: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := pipelineCapsFromConfigFile(capped); len(got) != 1 || got[0].Name != "leads" || got[0].Budget.PerDayTokens != 500 {
		t.Fatalf("caps = %+v", got)
	}
	for _, p := range []string{broken, filepath.Join(dir, "absent.yaml")} {
		if got := pipelineCapsFromConfigFile(p); got != nil {
			t.Fatalf("caps from %s = %+v, want none", p, got)
		}
	}
}
