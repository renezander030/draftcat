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
