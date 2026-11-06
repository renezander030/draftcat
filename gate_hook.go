package main

// `draftcat gate hook`: the tool gate as an agent harness's pre-tool hook.
//
// Claude Code, Codex, GitHub Copilot CLI and Gemini CLI can run a command
// before every tool call and block the call on its answer. This command is
// that hook: it reads the harness's payload on stdin, asks the gate about the
// one call, consumes the permit an allowed decision carries, and answers in
// the harness's own format. Only a consume that returns `permit: execute`
// lets the call run; a gate that cannot be reached, refuses the token, does
// not decide in time, denies, or refuses the consume blocks it.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	hookDefaultURL       = "http://127.0.0.1:8088"
	hookURLEnv           = "DRAFTCAT_GATE_URL"
	hookDefaultTokenEnv  = "DRAFTCAT_GATE_TOKEN"     // #nosec G101 -- env var name
	hookFallbackTokenEnv = "DRAFTCAT_WEBHOOK_SECRET" // #nosec G101 -- env var name
	// hookDefaultWait bounds how long the hook waits for a decision. The
	// installer gives the harness a timeout a minute longer, because a hook a
	// harness times out lets the call through in most of them.
	hookDefaultWait = 5 * time.Minute
	// hookStdinLimit caps the payload read: a Write call carries the whole file.
	hookStdinLimit = 8 << 20
	// hookHoldCap bounds one held request, the same cap the gate puts on a
	// long-poll, so a proxy in between never gives up first.
	hookHoldCap = toolGateStatusWaitCap
	// hookRequestGrace is the client's margin over a held request.
	hookRequestGrace = 10 * time.Second
	// hookShortTimeout bounds a consume or a revoke.
	hookShortTimeout = 15 * time.Second
	// hookPollPause is the least time between two polls.
	hookPollPause = 250 * time.Millisecond
)

// hookFormat is one harness's hook protocol.
type hookFormat struct {
	agent  string   // default agent name sent to the gate
	events []string // hook_event_name values the payload may carry
}

var hookFormats = map[string]hookFormat{
	"claude":  {agent: "claude-code", events: []string{"PreToolUse"}},
	"codex":   {agent: "codex", events: []string{"PreToolUse"}},
	"copilot": {agent: "copilot-cli", events: []string{"PreToolUse", "preToolUse"}},
	"gemini":  {agent: "gemini-cli", events: []string{"BeforeTool"}},
}

const gateHookUsage = `Usage:
  draftcat gate hook --format claude|codex|copilot|gemini [--url URL] [--token-env VAR] [--agent NAME] [--wait 5m]

Run as an agent harness's pre-tool hook: reads the hook payload on stdin, asks
the tool gate about the call, consumes the permit of an allowed decision and
answers allow or deny in the harness's own format. Anything short of a
consumed permit blocks the call.

--url        the gate (default $DRAFTCAT_GATE_URL, else http://127.0.0.1:8088)
--token-env  variable holding the bearer token (default DRAFTCAT_GATE_TOKEN,
             falling back to DRAFTCAT_WEBHOOK_SECRET); a tool_gate.callers token works
--agent      agent name sent to the gate (default claude-code, codex, copilot-cli
             or gemini-cli); with a caller token, the caller's name
--wait       how long to wait for a human decision before blocking (default 5m)`

// hookCall is one tool call as the harness reported it.
type hookCall struct {
	Tool      string
	Args      map[string]interface{}
	SessionID string
	ToolUseID string
	Cwd       string
	Event     string
	Timestamp string
}

// hookVerdict is what the hook answers the harness.
type hookVerdict struct {
	Allow  bool
	Reason string
}

func gateHookCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gate hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	format := fs.String("format", "", "")
	gateURL := fs.String("url", "", "")
	tokenEnv := fs.String("token-env", "", "")
	agent := fs.String("agent", "", "")
	wait := fs.Duration("wait", hookDefaultWait, "")
	// A usage error exits 2 with the reason on stderr, which every supported
	// harness treats as a block.
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprintln(stdout, gateHookUsage)
			return 0
		}
		_, _ = fmt.Fprintf(stderr, "draftcat gate hook: %v; blocking the call\n%s\n", err, gateHookUsage)
		return 2
	}
	f, ok := hookFormats[*format]
	if !ok || fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "draftcat gate hook: --format must be claude, codex, copilot or gemini; blocking the call\n%s\n", gateHookUsage)
		return 2
	}
	if *wait <= 0 {
		_, _ = fmt.Fprintln(stderr, "draftcat gate hook: --wait must be a positive duration; blocking the call")
		return 2
	}
	if *agent == "" {
		*agent = f.agent
	}

	call, err := readHookCall(*format, stdin)
	var v hookVerdict
	if err != nil {
		v = hookVerdict{Reason: "the hook payload could not be read: " + err.Error()}
	} else {
		v = runGateHook(context.Background(), hookOptions{
			url: resolveHookURL(*gateURL), tokenEnv: *tokenEnv, agent: *agent, wait: *wait,
			client: &http.Client{},
		}, *format, call)
	}
	tool := call.Tool
	if tool == "" {
		tool = "the tool call"
	}
	if !v.Allow {
		_, _ = fmt.Fprintf(stderr, "draftcat gate hook: blocked %s: %s\n", tool, v.Reason)
	}
	if err := writeHookAnswer(stdout, *format, v, tool); err != nil {
		_, _ = fmt.Fprintf(stderr, "draftcat gate hook: cannot answer the harness (%v); blocking the call\n", err)
		return 2
	}
	return 0
}

func resolveHookURL(flagURL string) string {
	if u := strings.TrimSpace(flagURL); u != "" {
		return u
	}
	if u := strings.TrimSpace(os.Getenv(hookURLEnv)); u != "" {
		return u
	}
	return hookDefaultURL
}

func readHookCall(format string, r io.Reader) (hookCall, error) {
	raw, err := io.ReadAll(io.LimitReader(r, hookStdinLimit+1))
	if err != nil {
		return hookCall{}, err
	}
	if len(raw) > hookStdinLimit {
		return hookCall{}, fmt.Errorf("payload is larger than %d bytes", hookStdinLimit)
	}
	return parseHookPayload(format, raw)
}

// parseHookPayload reads one harness's pre-tool payload. Numbers keep their
// spelling, so the gate hashes exactly the arguments the harness proposed.
func parseHookPayload(format string, raw []byte) (hookCall, error) {
	f, ok := hookFormats[format]
	if !ok {
		return hookCall{}, fmt.Errorf("unknown format %q", format)
	}
	var p map[string]interface{}
	if err := decodeStrictJSON(bytes.TrimSpace(raw), &p); err != nil || p == nil {
		if err == nil {
			err = errors.New("null")
		}
		return hookCall{}, fmt.Errorf("not a JSON object: %w", err)
	}
	var c hookCall
	var input interface{}
	if _, camel := p["toolName"]; format == "copilot" && camel {
		// Copilot CLI's camelCase preToolUse payload: toolArgs is usually the
		// arguments as a JSON string.
		c.Tool, c.SessionID, c.Cwd = payloadString(p, "toolName"), payloadString(p, "sessionId"), payloadString(p, "cwd")
		c.Timestamp = payloadString(p, "timestamp")
		input = p["toolArgs"]
	} else {
		c.Tool, c.SessionID, c.Cwd = payloadString(p, "tool_name"), payloadString(p, "session_id"), payloadString(p, "cwd")
		c.ToolUseID, c.Timestamp = payloadString(p, "tool_use_id"), payloadString(p, "timestamp")
		c.Event = payloadString(p, "hook_event_name")
		input = p["tool_input"]
	}
	if c.Event != "" && !containsString(f.events, c.Event) {
		return hookCall{}, fmt.Errorf("the hook fired for %s, not %s", c.Event, strings.Join(f.events, " or "))
	}
	if strings.TrimSpace(c.Tool) == "" {
		return hookCall{}, errors.New("no tool name in the payload")
	}
	if s, isString := input.(string); isString && format == "copilot" {
		var parsed map[string]interface{}
		if err := decodeStrictJSON([]byte(s), &parsed); err != nil {
			return hookCall{}, fmt.Errorf("tool arguments are not a JSON object: %w", err)
		}
		input = parsed
	}
	switch in := input.(type) {
	case map[string]interface{}:
		c.Args = in
	case nil:
		// No arguments: the gate hashes them as null.
	default:
		return hookCall{}, fmt.Errorf("tool input is %T, not a JSON object", input)
	}
	return c, nil
}

// payloadString reads a string field, or a number in its own spelling (Copilot
// CLI's camelCase timestamp is one).
func payloadString(p map[string]interface{}, key string) string {
	switch v := p[key].(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	}
	return ""
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// hookActionID is the stable action_id of one harness call, so a retried hook
// joins the decision already made for it. It names the session and the
// harness's tool-use id; a harness that sends none is identified by the
// payload's own fields (session, timestamp, directory, tool, arguments).
func hookActionID(format string, c hookCall) string {
	h := sha256.New()
	field := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	field("draftcat-hook/v1")
	field(format)
	field(c.SessionID)
	if c.ToolUseID != "" {
		field("tool_use_id")
		field(c.ToolUseID)
	} else {
		field("payload")
		field(c.Timestamp)
		field(c.Cwd)
		field(c.Tool)
		field(hashToolArgs(c.Args))
	}
	return "hook-" + hex.EncodeToString(h.Sum(nil))
}

type hookOptions struct {
	url      string
	tokenEnv string
	agent    string
	wait     time.Duration
	client   *http.Client
}

// hookToken reads the bearer token: the named variable, or by default
// DRAFTCAT_GATE_TOKEN and then DRAFTCAT_WEBHOOK_SECRET. It returns the
// variable it read, for the reason when a token is missing or refused.
func hookToken(tokenEnv string) (string, string) {
	if tokenEnv != "" {
		return os.Getenv(tokenEnv), tokenEnv
	}
	if t := os.Getenv(hookDefaultTokenEnv); t != "" {
		return t, hookDefaultTokenEnv
	}
	return os.Getenv(hookFallbackTokenEnv), hookDefaultTokenEnv + " or " + hookFallbackTokenEnv
}

// runGateHook asks the gate about one call and consumes the permit of an
// allowed decision. Every path that does not end in `permit: execute` denies.
func runGateHook(ctx context.Context, o hookOptions, format string, call hookCall) hookVerdict {
	token, from := hookToken(o.tokenEnv)
	if token == "" {
		return hookVerdict{Reason: "no gate token: " + from + " is not set"}
	}
	base, err := url.Parse(strings.TrimRight(o.url, "/"))
	if err != nil {
		return hookVerdict{Reason: "the gate URL cannot be read: it is not an http(s) URL"}
	}
	if (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return hookVerdict{Reason: fmt.Sprintf("gate URL %s is not an http(s) URL", base.Redacted())}
	}
	gc := &hookGateClient{base: base.String(), shown: base.Redacted(), token: []byte(token), tokenFrom: from, client: o.client}
	deadline := time.Now().Add(o.wait)
	id := hookActionID(format, call)

	hold := minDuration(time.Until(deadline), hookHoldCap)
	ask, err := json.Marshal(struct {
		ActionID string                 `json:"action_id"`
		Tool     string                 `json:"tool"`
		Args     map[string]interface{} `json:"args"`
		Agent    string                 `json:"agent,omitempty"`
		RunID    string                 `json:"run_id,omitempty"`
		Wait     string                 `json:"wait,omitempty"`
	}{id, call.Tool, call.Args, o.agent, call.SessionID, hold.String()})
	if err != nil {
		return hookVerdict{Reason: "the tool arguments cannot be encoded: " + err.Error()}
	}
	resp, refusal := gc.call(ctx, http.MethodPost, toolGatePath, ask, hold+hookRequestGrace)
	if refusal != "" {
		return hookVerdict{Reason: refusal}
	}
	if resp.ActionID != "" {
		id = resp.ActionID
	}
	for hookPending(resp) {
		left := time.Until(deadline)
		if left <= 0 {
			// The hook stops waiting, so the ask is withdrawn like a sync caller
			// that hangs up: the prompt closes and nothing is left to approve.
			body, _ := json.Marshal(map[string]string{"binding_hash": resp.BindingHash})
			_, _ = gc.call(ctx, http.MethodPost, toolGatePath+"/"+id+"/revoke", body, hookShortTimeout)
			return hookVerdict{Reason: fmt.Sprintf("no decision within %s; the ask was withdrawn (action %s)", o.wait, id)}
		}
		hold = minDuration(left, hookHoldCap)
		started := time.Now()
		resp, refusal = gc.call(ctx, http.MethodGet, toolGatePath+"/"+id+"?wait="+url.QueryEscape(hold.String()), nil, hold+hookRequestGrace)
		if refusal != "" {
			return hookVerdict{Reason: refusal}
		}
		// A gate (or a proxy) that answers a long-poll at once is not polled
		// in a tight loop.
		if hookPending(resp) && time.Since(started) < hookPollPause {
			time.Sleep(minDuration(hookPollPause, time.Until(deadline)))
		}
	}
	if resp.Decision != "allow" || resp.State != "allowed" {
		return hookVerdict{Reason: hookDenialReason(resp)}
	}

	body, _ := json.Marshal(map[string]string{"binding_hash": resp.BindingHash})
	consumed, refusal := gc.call(ctx, http.MethodPost, toolGatePath+"/"+id+"/consume", body, hookShortTimeout)
	if refusal != "" {
		return hookVerdict{Reason: "the permit was not released: " + refusal}
	}
	if consumed.Permit != "execute" {
		return hookVerdict{Reason: "the permit was not released: " + hookDenialReason(consumed)}
	}
	reason := "draftcat tool gate allowed " + call.Tool
	if resp.DecidedBy != "" {
		reason += " (" + resp.DecidedBy + ")"
	}
	return hookVerdict{Allow: true, Reason: reason + ", action " + id}
}

// hookDenialReason is the gate's own reason for a response that is not an
// allowed decision, with the deciding rule when the gate named one.
func hookDenialReason(r ToolCallResponse) string {
	reason := strings.TrimSpace(r.Reason)
	switch {
	case r.State == "consumed":
		reason = "the permit for this call was already used"
	case reason == "":
		reason = "the gate did not allow the call"
	}
	if r.Rule != "" {
		reason += " (rule: " + r.Rule + ")"
	}
	if r.ActionID != "" {
		reason += " [action " + r.ActionID + "]"
	}
	return reason
}

func hookPending(r ToolCallResponse) bool { return r.Decision == "pending" || r.State == "pending" }

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// hookGateClient makes the hook's requests: the bearer token on every one, and
// every POST body signed with the same key the gate verifies it with.
type hookGateClient struct {
	base      string
	shown     string // base without a password, for reasons
	token     []byte
	tokenFrom string
	client    *http.Client
}

// call makes one request and returns the gate's answer, or a refusal reason
// when there is no usable answer.
func (c *hookGateClient) call(ctx context.Context, method, path string, body []byte, timeout time.Duration) (ToolCallResponse, string) {
	resp, status, refusal := c.once(ctx, method, path, body, timeout)
	if status == http.StatusUnauthorized && body != nil {
		// The gate refuses a signature it has already seen, and two identical
		// bodies signed in the same second carry the same one. A second try in
		// the next second tells a retried hook from a refused token.
		time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
		resp, _, refusal = c.once(ctx, method, path, body, timeout)
	}
	return resp, refusal
}

func (c *hookGateClient) once(ctx context.Context, method, path string, body []byte, timeout time.Duration) (ToolCallResponse, int, string) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return ToolCallResponse{}, 0, "cannot build the gate request: " + err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+string(c.token))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(webhookSigHeader, signHookBody(c.token, time.Now().Unix(), body))
	}
	res, err := c.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return ToolCallResponse{}, 0, "the gate at " + c.shown + " did not answer in time"
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return ToolCallResponse{}, 0, "the gate at " + c.shown + " is unreachable: " + err.Error()
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return ToolCallResponse{}, res.StatusCode, "the gate's answer could not be read: " + err.Error()
	}
	var resp ToolCallResponse
	decoded := json.Unmarshal(raw, &resp) == nil && resp.Decision != ""
	text := strings.TrimSpace(string(raw))
	if len(text) > 200 {
		text = text[:200]
	}
	switch {
	case res.StatusCode == http.StatusUnauthorized:
		return resp, res.StatusCode, "the gate refused the token from " + c.tokenFrom + " (or its request signature)"
	case res.StatusCode == http.StatusForbidden:
		return resp, res.StatusCode, "the gate refused the request (" + text + "); with a caller token, --agent must be the caller's name"
	case res.StatusCode == http.StatusRequestEntityTooLarge:
		return resp, res.StatusCode, "the tool input is larger than the gate accepts (webhook.max_body_bytes)"
	case decoded:
		// 200, 202, 404, 409 and 503 carry a decision; the caller reads it.
		return resp, res.StatusCode, ""
	case res.StatusCode == http.StatusNotFound:
		return resp, res.StatusCode, "no tool gate at " + c.shown + toolGatePath + " (is tool_gate.enabled on?)"
	default:
		return resp, res.StatusCode, fmt.Sprintf("the gate answered %d: %s", res.StatusCode, text)
	}
}

// signHookBody is the X-Draftcat-Signature value the gate verifies:
// t=<unix>,v1=<hex hmac-sha256(t + "." + body)>, keyed with the bearer token.
func signHookBody(key []byte, ts int64, body []byte) string {
	t := strconv.FormatInt(ts, 10)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(t))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return "t=" + t + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// writeHookAnswer answers in the harness's hook format. A deny always carries
// its reason, which the harness hands to the agent.
func writeHookAnswer(w io.Writer, format string, v hookVerdict, tool string) error {
	decision, reason := "allow", v.Reason
	if !v.Allow {
		decision = "deny"
		reason = "draftcat tool gate blocked " + tool + ": " + v.Reason
	}
	var out interface{}
	switch format {
	case "claude":
		out = map[string]interface{}{"hookSpecificOutput": map[string]string{
			"hookEventName": "PreToolUse", "permissionDecision": decision, "permissionDecisionReason": reason}}
	case "codex":
		// Codex refuses permissionDecision:allow without updatedInput, so an
		// allowed call answers nothing and runs.
		if v.Allow {
			return nil
		}
		out = map[string]interface{}{"hookSpecificOutput": map[string]string{
			"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": reason}}
	case "copilot":
		// Both payload formats: camelCase reads the top-level fields, the
		// Claude-compatible PreToolUse format the nested ones.
		out = map[string]interface{}{
			"permissionDecision": decision, "permissionDecisionReason": reason,
			"hookSpecificOutput": map[string]string{
				"hookEventName": "PreToolUse", "permissionDecision": decision, "permissionDecisionReason": reason},
		}
	case "gemini":
		if v.Allow {
			out = map[string]string{"decision": "allow"}
		} else {
			out = map[string]string{"decision": "deny", "reason": reason}
		}
	default:
		return fmt.Errorf("unknown format %q", format)
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(out)
}
