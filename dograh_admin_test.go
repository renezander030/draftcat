//go:build voice

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/renezander030/draftcat/voice"
)

func newTempGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func TestGitCommitWorkflowWritesAndCommits(t *testing.T) {
	dir := newTempGitRepo(t)
	target := filepath.Join(dir, "workflows", "dach.json")
	_ = os.MkdirAll(filepath.Dir(target), 0o755)

	data := map[string]interface{}{
		"proposed_definition": map[string]any{"nodes": []any{}, "edges": []any{}},
		"commit_message":      "voice: tighten dach screening prompt",
	}
	vars := map[string]string{
		"path":        target,
		"content_var": "proposed_definition",
		"message_var": "commit_message",
		"repo_dir":    dir,
	}
	sha, err := gitCommitWorkflow(vars, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(sha) != 40 {
		t.Fatalf("want 40-char sha, got %q", sha)
	}
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), `"nodes"`) {
		t.Fatalf("workflow file missing nodes: %s", contents)
	}
	logCmd := exec.Command("git", "log", "-1", "--pretty=%s")
	logCmd.Dir = dir
	out, _ := logCmd.CombinedOutput()
	if !strings.Contains(string(out), "tighten dach screening prompt") {
		t.Fatalf("commit message missing: %s", out)
	}
}

func TestGitCommitWorkflowRequiresPath(t *testing.T) {
	if _, err := gitCommitWorkflow(map[string]string{}, map[string]interface{}{}); err == nil {
		t.Fatal("want error when vars.path missing")
	}
}

func TestDograhTriggerRunPostsToCorrectURL(t *testing.T) {
	var capturedPath, capturedAuth string
	var capturedBody map[string]any
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &capturedBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"workflow_run_id": 4242}`))
	}))
	t.Cleanup(mock.Close)

	t.Setenv("TEST_DOGRAH_KEY", "dograh-secret")
	prev := voiceCfg
	t.Cleanup(func() { voiceCfg = prev })
	voiceCfg = voice.Config{Dograh: voice.DograhConfig{
		StagingURL: mock.URL,
		BaseURL:    mock.URL,
		APIKeyEnv:  "TEST_DOGRAH_KEY",
	}}

	data := map[string]interface{}{
		"workflow_uuid": "wf-uuid-abc",
		"caller_ctx":    map[string]any{"first_name": "Anna"},
	}
	vars := map[string]string{
		"workflow_uuid_var":   "workflow_uuid",
		"initial_context_var": "caller_ctx",
	}
	runID, err := dograhTriggerRun(setEnv(vars, "staging"), data)
	if err != nil {
		t.Fatal(err)
	}
	if runID != "4242" {
		t.Fatalf("want run_id=4242, got %q", runID)
	}
	if capturedPath != "/api/v1/public/agent/workflow/wf-uuid-abc" {
		t.Fatalf("wrong URL path: %s", capturedPath)
	}
	if capturedAuth != "Bearer dograh-secret" {
		t.Fatalf("wrong auth header: %q", capturedAuth)
	}
	if got, _ := capturedBody["initial_context"].(map[string]any); got["first_name"] != "Anna" {
		t.Fatalf("initial_context not forwarded: %v", capturedBody)
	}
}

func TestDograhTriggerRunMissingBaseURL(t *testing.T) {
	prev := voiceCfg
	t.Cleanup(func() { voiceCfg = prev })
	voiceCfg = voice.Config{}
	if _, err := dograhTriggerRun(setEnv(map[string]string{}, "staging"), map[string]interface{}{}); err == nil {
		t.Fatal("want error when staging_url not configured")
	}
}

func TestDograhUpdateWorkflowPutsDefinition(t *testing.T) {
	var capturedPath, capturedMethod, capturedAuth string
	var capturedBody map[string]any
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedMethod = r.Method
		capturedAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &capturedBody)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id": 42, "version": 7}`))
	}))
	t.Cleanup(mock.Close)

	t.Setenv("TEST_DOGRAH_KEY", "prod-secret")
	prev := voiceCfg
	t.Cleanup(func() { voiceCfg = prev })
	voiceCfg = voice.Config{Dograh: voice.DograhConfig{
		BaseURL:   mock.URL,
		APIKeyEnv: "TEST_DOGRAH_KEY",
	}}

	def := filepath.Join(t.TempDir(), "dach.json")
	_ = os.WriteFile(def, []byte(`{"nodes":[{"id":"n1"}],"edges":[]}`), 0o644)

	data := map[string]interface{}{"workflow_id": 42}
	vars := map[string]string{"definition_path": def}
	if err := dograhUpdateWorkflow(vars, data); err != nil {
		t.Fatal(err)
	}
	if capturedMethod != "PUT" || capturedPath != "/api/v1/workflow/42" {
		t.Fatalf("wrong call: %s %s", capturedMethod, capturedPath)
	}
	if capturedAuth != "Bearer prod-secret" {
		t.Fatalf("wrong auth: %q", capturedAuth)
	}
	innerDef, ok := capturedBody["workflow_definition"].(map[string]any)
	if !ok {
		t.Fatalf("body missing workflow_definition: %v", capturedBody)
	}
	nodes, ok := innerDef["nodes"].([]any)
	if !ok || len(nodes) != 1 {
		t.Fatalf("workflow_definition.nodes not forwarded: %v", innerDef)
	}
}

func TestDograhUpdateWorkflowSurfacesHTTPError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"workflow not found"}`, http.StatusNotFound)
	}))
	t.Cleanup(mock.Close)

	prev := voiceCfg
	t.Cleanup(func() { voiceCfg = prev })
	voiceCfg = voice.Config{Dograh: voice.DograhConfig{BaseURL: mock.URL}}

	def := filepath.Join(t.TempDir(), "x.json")
	_ = os.WriteFile(def, []byte(`{}`), 0o644)

	err := dograhUpdateWorkflow(map[string]string{"definition_path": def}, map[string]interface{}{"workflow_id": 99})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("want 404 error, got %v", err)
	}
}
