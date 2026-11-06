package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/renezander030/draftcat/internal/config"
	"github.com/renezander030/draftcat/internal/httpaction"
	"github.com/renezander030/draftcat/internal/redact"
)

// The http_request action is the outbound side of an approval step. The
// approval step that precedes it shows the operator the request itself
// (method, URL, headers, body) instead of the model's draft, and its approval
// is bound to a digest of exactly that request. The action sends only a
// request whose digest was approved in this run.

const httpRequestAction = "http_request"

func isHTTPRequest(st config.StepConfig) bool {
	return st.Type == "deterministic" && st.Action == httpRequestAction
}

// releasedHTTPRequest returns the http_request step an approval step at index
// i releases: the first one after it, unless another approval step comes
// first. nil = the approval step releases no request.
func releasedHTTPRequest(steps []config.StepConfig, i int) *config.StepConfig {
	for j := i + 1; j < len(steps); j++ {
		if steps[j].Type == "approval" {
			return nil
		}
		if isHTTPRequest(steps[j]) {
			return &steps[j]
		}
	}
	return nil
}

// renderHTTPRequest builds the request an http_request step would send now.
func renderHTTPRequest(step config.StepConfig, runID string, data map[string]interface{}) (httpaction.Spec, []byte, string, error) {
	spec, ferrs := httpaction.Parse(step.Vars)
	if len(ferrs) > 0 {
		return spec, nil, "", fmt.Errorf("%s %q: %w", httpRequestAction, step.Name, ferrs[0])
	}
	body, err := httpaction.Body(spec, data)
	if err != nil {
		return spec, nil, "", fmt.Errorf("%s %q: %w", httpRequestAction, step.Name, err)
	}
	return spec, body, httpaction.IdempotencyKey(runID, step.Name), nil
}

// outboundDraft is what the operator reviews for a request, and the digest
// its approval binds to.
func outboundDraft(step config.StepConfig, runID string, data map[string]interface{}) (string, string, error) {
	spec, body, key, err := renderHTTPRequest(step, runID, data)
	if err != nil {
		return "", "", err
	}
	return httpaction.Describe(spec, body, key), httpaction.Digest(spec, body, key), nil
}

// revisedOutput is a rewrite approved in place of the model's first output:
// a JSON object where the first output was one, otherwise the text.
func revisedOutput(previous interface{}, text string) interface{} {
	if _, wasObject := previous.(map[string]interface{}); wasObject {
		var obj map[string]interface{}
		if json.Unmarshal([]byte(strings.TrimSpace(text)), &obj) == nil {
			return obj
		}
	}
	return text
}

// runHTTPRequest sends the step's request once and adds the response to the
// run's data. approved is the digest the releasing approval step bound; a
// request that does not match it is not sent.
func runHTTPRequest(ctx context.Context, runID, pipeline string, step config.StepConfig, data map[string]interface{}, approved string) error {
	spec, body, key, err := renderHTTPRequest(step, runID, data)
	if err != nil {
		return err
	}
	if approved == "" {
		return errors.New("http_request is sent only after an approval step showed it to the operator, and none did in this run; nothing was sent")
	}
	if httpaction.Digest(spec, body, key) != approved {
		return errors.New("http_request changed after it was approved; nothing was sent")
	}
	credential := ""
	if spec.AuthEnv != "" {
		credential = os.Getenv(spec.AuthEnv)
		if credential == "" {
			return fmt.Errorf("http_request: env var %s is empty; nothing was sent", spec.AuthEnv)
		}
		// Registered here as well as at start, so a pipeline added by a
		// reload has its credential redacted too.
		redact.Register(spec.AuthEnv, credential)
	}
	resp, err := httpaction.Do(ctx, spec, body, key, credential)
	if err != nil {
		return fmt.Errorf("http_request: %w", err)
	}
	data["http_response"] = resp.Data(key)
	data["http_status"] = strconv.Itoa(resp.Status)
	data["http_response_body"] = resp.Text
	note := ""
	if resp.Truncated {
		note = fmt.Sprintf(", response cut at %d bytes", spec.MaxResponseBytes)
	}
	log.Printf("[pipeline:%s][step:%s] http_request %s %s -> HTTP %d (%d bytes%s)",
		pipeline, step.Name, spec.Method, spec.URL.Redacted(), resp.Status, len(resp.Text), note)
	return nil
}
