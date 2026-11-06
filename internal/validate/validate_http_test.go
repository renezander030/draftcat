package validate

import (
	"strings"
	"testing"

	"github.com/renezander030/draftcat/internal/config"
)

func httpCfg(vars map[string]string, before ...config.StepConfig) *config.Config {
	send := config.StepConfig{Name: "send", Type: "deterministic", Action: "http_request", Vars: vars}
	return &config.Config{Pipelines: []config.PipelineConfig{{Name: "p", Steps: append(before, send)}}}
}

var reviewStep = config.StepConfig{Name: "review", Type: "approval", Channel: "telegram"}

func TestValidateHTTPRequestAccepted(t *testing.T) {
	t.Setenv("ORDERS_TOKEN", "set")
	rep := runCheckPipelines(httpCfg(map[string]string{"url": "https://example.com/orders", "auth_env": "ORDERS_TOKEN"}, reviewStep))
	if errs := findingsAt(rep, "error", ".steps[1:send]"); len(errs) != 0 {
		t.Fatalf("unexpected errors: %+v", errs)
	}
	if warns := findingsAt(rep, "warn", ".steps[1:send]"); len(warns) != 0 {
		t.Fatalf("unexpected warnings: %+v", warns)
	}
}

func TestValidateHTTPRequestNeedsApprovalBefore(t *testing.T) {
	vars := map[string]string{"url": "https://example.com"}
	if errs := findingsAt(runCheckPipelines(httpCfg(vars)), "error", "send"); len(errs) == 0 || !strings.Contains(errs[0].Message, "approval step") {
		t.Fatalf("an http_request with no approval before it must error: %+v", errs)
	}
	// Approval after the request does not release it.
	cfg := httpCfg(vars)
	cfg.Pipelines[0].Steps = append(cfg.Pipelines[0].Steps, reviewStep)
	if errs := findingsAt(runCheckPipelines(cfg), "error", "send"); len(errs) == 0 {
		t.Fatal("an approval after the request must not count")
	}
	// One approval releases one request.
	cfg = httpCfg(vars, reviewStep, config.StepConfig{Name: "first", Type: "deterministic", Action: "http_request", Vars: vars})
	errs := findingsAt(runCheckPipelines(cfg), "error", ".steps[2:send]")
	if len(errs) == 0 {
		t.Fatal("a second request after the same approval must error")
	}
	if errs := findingsAt(runCheckPipelines(cfg), "error", ".steps[1:first]"); len(errs) != 0 {
		t.Fatalf("the first request is released: %+v", errs)
	}
}

func TestValidateHTTPRequestVarErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		vars map[string]string
		path string
	}{
		"missing url":     {map[string]string{}, ".vars.url"},
		"non-http url":    {map[string]string{"url": "file:///etc/passwd"}, ".vars.url"},
		"bad method":      {map[string]string{"url": "https://example.com", "method": "CONNECT"}, ".vars.method"},
		"no auth header":  {map[string]string{"url": "https://example.com", "auth_env": "T", "auth_header": "a b"}, ".vars.auth_header"},
		"no auth scheme":  {map[string]string{"url": "https://example.com", "auth_env": "T", "auth_scheme": "a b"}, ".vars.auth_scheme"},
		"static secret":   {map[string]string{"url": "https://example.com", "headers": "Authorization: Basic abc"}, ".vars.headers"},
		"bad timeout":     {map[string]string{"url": "https://example.com", "timeout": "-1s"}, ".vars.timeout"},
		"bad expect":      {map[string]string{"url": "https://example.com", "expect_status": "7xx"}, ".vars.expect_status"},
		"bad bound":       {map[string]string{"url": "https://example.com", "max_response_bytes": "lots"}, ".vars.max_response_bytes"},
		"body and key":    {map[string]string{"url": "https://example.com", "body": "{}", "body_key": "x"}, ".vars.body"},
		"get with a body": {map[string]string{"url": "https://example.com", "method": "GET", "body_key": "x"}, ".vars.body"},
	} {
		if errs := findingsAt(runCheckPipelines(httpCfg(tc.vars, reviewStep)), "error", tc.path); len(errs) == 0 {
			t.Errorf("%s: expected an error at %s", name, tc.path)
		}
	}
}

func TestValidateHTTPRequestWarnings(t *testing.T) {
	t.Setenv("EMPTY_TOKEN", "")
	for name, tc := range map[string]struct {
		vars map[string]string
		path string
	}{
		"empty credential env": {map[string]string{"url": "https://example.com", "auth_env": "EMPTY_TOKEN"}, ".vars.auth_env"},
		"plain remote http":    {map[string]string{"url": "http://example.com/hook"}, ".vars.url"},
		"scheme without env":   {map[string]string{"url": "https://example.com", "auth_scheme": "Token"}, ".vars.auth_scheme"},
		"unknown var":          {map[string]string{"url": "https://example.com", "auth_evn": "X"}, ".vars.auth_evn"},
	} {
		if warns := findingsAt(runCheckPipelines(httpCfg(tc.vars, reviewStep)), "warn", tc.path); len(warns) == 0 {
			t.Errorf("%s: expected a warning at %s", name, tc.path)
		}
	}
	if warns := findingsAt(runCheckPipelines(httpCfg(map[string]string{"url": "http://127.0.0.1:8080/hook"}, reviewStep)), "warn", ".vars.url"); len(warns) != 0 {
		t.Fatalf("plain http to loopback is fine: %+v", warns)
	}
	if warns := findingsAt(runCheckPipelines(httpCfg(map[string]string{"url": "https://example.com", "auth_evn": "X"}, reviewStep)), "warn", ".vars.auth_evn"); !strings.Contains(warns[0].Message, `did you mean "auth_env"`) {
		t.Fatalf("typo hint: %+v", warns)
	}
}
