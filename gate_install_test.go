package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func runInstall(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := gateInstallCmd(args, &out, &errb)
	return code, out.String(), errb.String()
}

func readJSONFile(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

// draftcatEntries returns the event's entries whose command (at the top or in
// a matcher group) runs `draftcat gate hook`.
func draftcatEntries(t *testing.T, file map[string]interface{}, event string) []map[string]interface{} {
	t.Helper()
	hooks, _ := file["hooks"].(map[string]interface{})
	list, _ := hooks[event].([]interface{})
	var out []map[string]interface{}
	for _, item := range list {
		e, _ := item.(map[string]interface{})
		if cmd, _ := e["command"].(string); strings.Contains(cmd, hookCommandMarker) {
			out = append(out, e)
			continue
		}
		inner, _ := e["hooks"].([]interface{})
		for _, h := range inner {
			if cmd, _ := h.(map[string]interface{})["command"].(string); strings.Contains(cmd, hookCommandMarker) {
				out = append(out, e)
			}
		}
	}
	return out
}

// existingSettings is a settings file the operator already has: a key before
// hooks, another hook on the same event and one on another event.
var existingSettings = map[string]string{
	"claude-code": `{
  "permissions": {"allow": ["Bash(git status)"]},
  "hooks": {
    "PreToolUse": [{"matcher": "Write", "hooks": [{"type": "command", "command": "./lint.sh"}]}],
    "PostToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": "./log.sh"}]}]
  },
  "model": "m"
}`,
	"codex": `{
  "description": "team hooks",
  "hooks": {
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "./lint.sh"}]}],
    "Stop": [{"hooks": [{"type": "command", "command": "./log.sh"}]}]
  }
}`,
	"copilot-cli": `{
  "version": 1,
  "hooks": {
    "preToolUse": [{"type": "command", "bash": "./audit.sh", "timeoutSec": 10}],
    "PreToolUse": [{"type": "command", "matcher": "Bash", "command": "./lint.sh"}]
  }
}`,
	"gemini-cli": `{
  "general": {"vimMode": true},
  "hooks": {
    "BeforeTool": [{"matcher": "write_file", "hooks": [{"name": "lint", "type": "command", "command": "./lint.sh"}]}],
    "AfterTool": [{"matcher": "*", "hooks": [{"name": "log", "type": "command", "command": "./log.sh"}]}]
  }
}`,
}

func TestGateInstall_FreshFilePerHarness(t *testing.T) {
	for _, tg := range hookTargets {
		dir := t.TempDir()
		code, out, errOut := runInstall(t, tg.name, "--dir", dir)
		if code != 0 {
			t.Fatalf("%s: exit %d %s", tg.name, code, errOut)
		}
		path := filepath.Join(dir, tg.project)
		file := readJSONFile(t, path)
		entries := draftcatEntries(t, file, tg.event)
		if len(entries) != 1 {
			t.Fatalf("%s: %d draftcat entries in %v", tg.name, len(entries), file)
		}
		if entries[0]["matcher"] != tg.matcher {
			t.Errorf("%s: matcher %v", tg.name, entries[0]["matcher"])
		}
		if !strings.Contains(out, "added") || !strings.Contains(out, "tool names the hook sends to the gate:") {
			t.Errorf("%s: notice %q", tg.name, out)
		}
		raw, _ := os.ReadFile(path)
		if strings.Contains(string(raw), "TOKEN=") || strings.Contains(string(raw), "http://") {
			t.Errorf("%s: a token or URL was written: %s", tg.name, raw)
		}
		if tg.name == "copilot-cli" && file["version"] != float64(1) {
			t.Errorf("copilot file needs version 1: %v", file)
		}
	}
}

func TestGateInstall_KeepsOtherKeysAndHooks_IdempotentAndRemovable(t *testing.T) {
	for _, tg := range hookTargets {
		dir := t.TempDir()
		path := filepath.Join(dir, tg.project)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(existingSettings[tg.name]), 0o600); err != nil {
			t.Fatal(err)
		}
		var original map[string]interface{}
		_ = json.Unmarshal([]byte(existingSettings[tg.name]), &original)

		if code, _, errOut := runInstall(t, tg.name, "--dir", dir); code != 0 {
			t.Fatalf("%s: install exit %d %s", tg.name, code, errOut)
		}
		installed, _ := os.ReadFile(path)
		file := readJSONFile(t, path)
		if len(draftcatEntries(t, file, tg.event)) != 1 {
			t.Fatalf("%s: want one draftcat entry: %s", tg.name, installed)
		}
		for k, v := range original {
			if k == "hooks" {
				continue
			}
			if !reflect.DeepEqual(file[k], v) {
				t.Errorf("%s: key %s changed: %v", tg.name, k, file[k])
			}
		}
		origHooks := original["hooks"].(map[string]interface{})
		gotHooks := file["hooks"].(map[string]interface{})
		for ev, list := range origHooks {
			got := gotHooks[ev].([]interface{})
			for i, item := range list.([]interface{}) {
				if !reflect.DeepEqual(got[i], item) {
					t.Errorf("%s: %s[%d] changed: %v", tg.name, ev, i, got[i])
				}
			}
		}
		// The keys stay where they were.
		firstKey := map[string]string{"claude-code": "permissions", "codex": "description", "copilot-cli": "version", "gemini-cli": "general"}[tg.name]
		if !bytes.HasPrefix(bytes.TrimSpace(bytes.TrimPrefix(installed, []byte("{"))), []byte(`"`+firstKey+`"`)) {
			t.Errorf("%s: first key moved, want %q first: %s", tg.name, firstKey, installed)
		}
		if tg.name == "claude-code" && bytes.Index(installed, []byte(`"model"`)) < bytes.Index(installed, []byte(`"hooks"`)) {
			t.Errorf("claude-code: model moved before hooks: %s", installed)
		}
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v, want the file's own 0600", tg.name, info.Mode().Perm())
		}

		// Again: nothing changes.
		code, out, _ := runInstall(t, tg.name, "--dir", dir)
		again, _ := os.ReadFile(path)
		if code != 0 || !bytes.Equal(installed, again) || !strings.Contains(out, "already installed") {
			t.Errorf("%s: re-run changed the file or said %q", tg.name, out)
		}

		// A new matcher replaces the entry in place.
		if code, out, _ := runInstall(t, tg.name, "--dir", dir, "--matcher", "Bash"); code != 0 || !strings.Contains(out, "updated") {
			t.Errorf("%s: matcher change: %d %q", tg.name, code, out)
		}
		entries := draftcatEntries(t, readJSONFile(t, path), tg.event)
		if len(entries) != 1 || entries[0]["matcher"] != "Bash" {
			t.Errorf("%s: entries after matcher change: %v", tg.name, entries)
		}

		// Remove: only draftcat's entry goes.
		if code, out, _ := runInstall(t, tg.name, "--dir", dir, "--remove"); code != 0 || !strings.Contains(out, "removed") {
			t.Errorf("%s: remove: %d %q", tg.name, code, out)
		}
		var after map[string]interface{}
		_ = json.Unmarshal(mustRead(t, path), &after)
		if tg.name == "copilot-cli" {
			delete(original, "version")
			delete(after, "version")
		}
		if !reflect.DeepEqual(after, original) {
			t.Errorf("%s: after remove\n got %v\nwant %v", tg.name, after, original)
		}
		if code, out, _ := runInstall(t, tg.name, "--dir", dir, "--remove"); code != 0 || !strings.Contains(out, "no draftcat hook") {
			t.Errorf("%s: second remove: %d %q", tg.name, code, out)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGateInstall_RemoveKeepsAGroupWithOtherHooks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	shared := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[` +
		`{"type":"command","command":"./lint.sh"},` +
		`{"type":"command","command":"draftcat gate hook --format claude"}]}]}}`
	_ = os.WriteFile(path, []byte(shared), 0o644)
	if code, _, errOut := runInstall(t, "claude-code", "--dir", dir, "--remove"); code != 0 {
		t.Fatal(errOut)
	}
	var got map[string]interface{}
	_ = json.Unmarshal(mustRead(t, path), &got)
	want := map[string]interface{}{"hooks": map[string]interface{}{"PreToolUse": []interface{}{
		map[string]interface{}{"matcher": "Bash", "hooks": []interface{}{map[string]interface{}{"type": "command", "command": "./lint.sh"}}},
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestGateInstall_UserScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("COPILOT_HOME", filepath.Join(home, "copilot-home"))
	want := map[string]string{
		"claude-code": filepath.Join(home, ".claude", "settings.json"),
		"codex":       filepath.Join(home, ".codex", "hooks.json"),
		"copilot-cli": filepath.Join(home, "copilot-home", "hooks", "draftcat.json"),
		"gemini-cli":  filepath.Join(home, ".gemini", "settings.json"),
	}
	for name, path := range want {
		if code, _, errOut := runInstall(t, name, "--user"); code != 0 {
			t.Fatalf("%s: %s", name, errOut)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if code, _, _ := runInstall(t, "codex", "--user", "--dir", home); code != 2 {
		t.Error("--user with --dir must be refused")
	}
}

func TestGateInstall_DryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	code, out, notes := runInstall(t, "gemini-cli", "--dir", dir, "--dry-run")
	if code != 0 {
		t.Fatal(notes)
	}
	var file map[string]interface{}
	if err := json.Unmarshal([]byte(out), &file); err != nil {
		t.Fatalf("dry run prints the file: %v %q", err, out)
	}
	if len(draftcatEntries(t, file, "BeforeTool")) != 1 || !strings.Contains(notes, "would write") {
		t.Fatalf("dry run: %s / %s", out, notes)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gemini")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dry run wrote to disk")
	}
}

func TestGateInstall_RefusesAFileItCannotRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gemini", "settings.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	jsonc := "{\n  // comment\n  \"hooks\": {}\n}\n"
	_ = os.WriteFile(path, []byte(jsonc), 0o644)
	if code, _, errOut := runInstall(t, "gemini-cli", "--dir", dir); code != 1 || !strings.Contains(errOut, "cannot update") {
		t.Fatalf("exit %d %q", code, errOut)
	}
	if string(mustRead(t, path)) != jsonc {
		t.Fatal("an unreadable file was changed")
	}
	for _, bad := range []string{`{"hooks": []}`, `{"hooks": {"BeforeTool": {}}}`, `[1]`} {
		_ = os.WriteFile(path, []byte(bad), 0o644)
		if code, _, _ := runInstall(t, "gemini-cli", "--dir", dir); code != 1 {
			t.Errorf("%s: exit %d", bad, code)
		}
	}
}

func TestGateInstall_FollowsASymlinkedSettingsFile(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "settings.json")
	_ = os.MkdirAll(filepath.Dir(real), 0o755)
	_ = os.WriteFile(real, []byte(`{"model":"m"}`), 0o644)
	link := filepath.Join(dir, "proj", ".claude", "settings.json")
	_ = os.MkdirAll(filepath.Dir(link), 0o755)
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if code, _, errOut := runInstall(t, "claude-code", "--dir", filepath.Join(dir, "proj")); code != 0 {
		t.Fatal(errOut)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a file")
	}
	if len(draftcatEntries(t, readJSONFile(t, real), "PreToolUse")) != 1 {
		t.Fatal("the link target was not updated")
	}
}

func TestGateInstall_CommandCarriesOptionsAndFailsClosed(t *testing.T) {
	claude, _ := lookupHookTarget("claude-code")
	copilot, _ := lookupHookTarget("copilot-cli")
	o := installOptions{bin: "/opt/my tools/draftcat", agent: "agent-1", tokenEnv: "GATE_TOKEN_AGENT1", wait: 2 * hookDefaultWait}
	cmd := hookCommand(claude, o)
	for _, want := range []string{"'/opt/my tools/draftcat' gate hook --format claude", "--agent agent-1", "--token-env GATE_TOKEN_AGENT1", "--wait 10m0s", "exit 2"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command %q lacks %q", cmd, want)
		}
	}
	if c := hookCommand(copilot, installOptions{bin: "draftcat", wait: hookDefaultWait}); c != "draftcat gate hook --format copilot" {
		t.Errorf("copilot command %q", c)
	}
	if shellQuote("it's") != `'it'\''s'` {
		t.Errorf("quote: %s", shellQuote("it's"))
	}

	// A command whose executable is missing still blocks with exit 2.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	run := exec.CommandContext(context.Background(), "sh", "-c", hookCommand(claude, installOptions{bin: filepath.Join(t.TempDir(), "missing"), wait: hookDefaultWait}))
	run.Stdin = strings.NewReader("{}")
	var stderr bytes.Buffer
	run.Stderr = &stderr
	err := run.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(stderr.String(), "blocked") {
		t.Fatalf("missing executable: %v %q", err, stderr.String())
	}
}

func TestGateInstall_MatcherNames(t *testing.T) {
	check := func(name, matcher string, want []string) {
		t.Helper()
		tg, _ := lookupHookTarget(name)
		got, err := sentToolNames(tg, matcher)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s %q: %v %v, want %v", name, matcher, got, err, want)
		}
	}
	check("claude-code", "^(Bash|Write|Edit|MultiEdit|NotebookEdit|mcp__.*)$", []string{"Bash", "Write", "Edit", "MultiEdit", "NotebookEdit", "mcp__<server>__<tool>"})
	check("claude-code", "Edit", []string{"Edit"})
	check("claude-code", "Edit|Write", []string{"Write", "Edit"})
	check("codex", "^(Bash|apply_patch|mcp__.*)$", []string{"Bash", "apply_patch", "mcp__<server>__<tool>"})
	check("copilot-cli", "Bash|Write|Edit", []string{"Bash", "Write", "Edit"})
	check("copilot-cli", "Web.*", []string{"WebFetch", "WebSearch"})
	check("gemini-cli", "^(run_shell_command|write_file|replace|mcp_.*)$", []string{"run_shell_command", "write_file", "replace", "mcp_<server>_<tool>"})
	check("gemini-cli", "write_file", []string{"write_file"})
	tg, _ := lookupHookTarget("claude-code")
	if all, _ := sentToolNames(tg, "*"); len(all) != len(tg.tools)+1 {
		t.Errorf("* sends every tool: %v", all)
	}
}

func TestGateInstall_UsageErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"cursor"}, {"claude-code", "codex"}, {"claude-code", "--wait", "0s"}, {"claude-code", "--nope"}} {
		if code, _, _ := runInstall(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	dir := t.TempDir()
	if code, _, errOut := runInstall(t, "--dir", dir, "claude-code", "--dry-run"); code != 0 {
		t.Errorf("flags before the harness: %s", errOut)
	}
}
