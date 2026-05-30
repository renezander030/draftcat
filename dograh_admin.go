//go:build voice

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// gitCommitWorkflow stages and commits a workflow definition file. Used by the
// 7-step guardrail flow between AI-proposed changes and the staging smoke run.
//
// vars:
//
//	path            (required) destination path for the workflow JSON
//	content_var     (optional) data key whose value is written to path before
//	                git add (used when the AI step emits the proposed
//	                definition as a structured value in data[content_var])
//	message_var     (optional) data key for the commit message
//	message         (optional) literal commit message (fallback)
//	repo_dir        (optional) repository directory; defaults to cwd
func gitCommitWorkflow(vars map[string]string, data map[string]interface{}) (string, error) {
	path := vars["path"]
	if path == "" {
		return "", fmt.Errorf("git_commit_workflow_update requires vars.path")
	}

	if cv := vars["content_var"]; cv != "" {
		raw, ok := data[cv]
		if !ok {
			return "", fmt.Errorf("git_commit_workflow_update: data[%q] not set by an upstream step", cv)
		}
		buf, err := marshalForFile(raw)
		if err != nil {
			return "", fmt.Errorf("git_commit_workflow_update: serialize data[%q]: %w", cv, err)
		}
		if err := os.WriteFile(path, buf, 0o644); err != nil {
			return "", fmt.Errorf("git_commit_workflow_update: write %s: %w", path, err)
		}
	}

	repoDir := vars["repo_dir"]
	if repoDir == "" {
		repoDir = "."
	}

	msg := vars["message"]
	if mv := vars["message_var"]; mv != "" {
		if v, ok := data[mv].(string); ok && v != "" {
			msg = v
		}
	}
	if msg == "" {
		msg = "voice workflow update via draftcat guardrail"
	}

	if out, err := runGit(repoDir, "add", path); err != nil {
		return "", fmt.Errorf("git add %s: %v: %s", path, err, out)
	}
	if out, err := runGit(repoDir, "commit", "-m", msg); err != nil {
		return "", fmt.Errorf("git commit: %v: %s", err, out)
	}
	sha, err := runGit(repoDir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %v: %s", err, sha)
	}
	return strings.TrimSpace(sha), nil
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func marshalForFile(v any) ([]byte, error) {
	switch x := v.(type) {
	case []byte:
		return x, nil
	case string:
		return []byte(x), nil
	default:
		return json.MarshalIndent(v, "", "  ")
	}
}

// dograhTriggerRun fires an outbound agent run on the staging or prod Dograh
// instance via POST /api/v1/public/agent/workflow/{workflow_uuid}.
//
// vars:
//
//	workflow_uuid_var   (optional) data key holding the workflow UUID;
//	                    defaults to "workflow_uuid"
//	initial_context_var (optional) data key holding initial_context map; sent
//	                    in the request body when present
//	env                 (optional) "staging" or "prod"; defaults to "staging"
func dograhTriggerRun(vars map[string]string, data map[string]interface{}) (string, error) {
	target := vars["env"]
	if target == "" {
		target = "staging"
	}
	baseURL := voiceCfg.Dograh.StagingURL
	if target == "prod" {
		baseURL = voiceCfg.Dograh.BaseURL
	}
	if baseURL == "" {
		return "", fmt.Errorf("dograh trigger: voice.dograh.%s_url not configured", strings.TrimSuffix(target, "_url"))
	}

	uuidKey := vars["workflow_uuid_var"]
	if uuidKey == "" {
		uuidKey = "workflow_uuid"
	}
	workflowUUID, ok := data[uuidKey].(string)
	if !ok || workflowUUID == "" {
		return "", fmt.Errorf("dograh trigger: data[%q] not a non-empty string", uuidKey)
	}

	body := map[string]any{}
	if ctxKey := vars["initial_context_var"]; ctxKey != "" {
		if v, ok := data[ctxKey]; ok {
			body["initial_context"] = v
		}
	}

	url := fmt.Sprintf("%s/api/v1/public/agent/workflow/%s", strings.TrimRight(baseURL, "/"), workflowUUID)
	respBody, err := dograhRequest(http.MethodPost, url, body)
	if err != nil {
		return "", err
	}

	var resp struct {
		WorkflowRunID any `json:"workflow_run_id"`
	}
	_ = json.Unmarshal(respBody, &resp)
	if resp.WorkflowRunID == nil {
		return "", nil
	}
	return fmt.Sprint(resp.WorkflowRunID), nil
}

// dograhUpdateWorkflow PUTs an updated workflow_definition to prod via
// PUT /api/v1/workflow/{workflow_id}. Dograh auto-versions the definition,
// preserving the previous version's history.
//
// vars:
//
//	workflow_id_var  (optional) data key for the workflow_id (int);
//	                 defaults to "workflow_id"
//	definition_path  (required) path to the workflow_definition JSON file
//	                 on disk (typically the same file git_commit_workflow_update
//	                 committed earlier in the pipeline)
func dograhUpdateWorkflow(vars map[string]string, data map[string]interface{}) error {
	if voiceCfg.Dograh.BaseURL == "" {
		return fmt.Errorf("dograh publish: voice.dograh.base_url not configured")
	}

	idKey := vars["workflow_id_var"]
	if idKey == "" {
		idKey = "workflow_id"
	}
	idRaw, ok := data[idKey]
	if !ok {
		return fmt.Errorf("dograh publish: data[%q] not set", idKey)
	}
	workflowID := fmt.Sprint(idRaw)

	defPath := vars["definition_path"]
	if defPath == "" {
		return fmt.Errorf("dograh publish: vars.definition_path required")
	}
	defBytes, err := os.ReadFile(defPath)
	if err != nil {
		return fmt.Errorf("dograh publish: read %s: %w", defPath, err)
	}

	var defObj any
	if err := json.Unmarshal(defBytes, &defObj); err != nil {
		return fmt.Errorf("dograh publish: %s is not valid JSON: %w", defPath, err)
	}

	url := fmt.Sprintf("%s/api/v1/workflow/%s", strings.TrimRight(voiceCfg.Dograh.BaseURL, "/"), workflowID)
	body := map[string]any{"workflow_definition": defObj}
	if _, err := dograhRequest(http.MethodPut, url, body); err != nil {
		return err
	}
	return nil
}

func dograhRequest(method, url string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(buf)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if env := voiceCfg.Dograh.APIKeyEnv; env != "" {
		if token := os.Getenv(env); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dograh %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		snippet := string(respBody)
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		return nil, fmt.Errorf("dograh %s %s: HTTP %d: %s", method, url, resp.StatusCode, snippet)
	}
	return respBody, nil
}
