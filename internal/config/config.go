// Package config holds the YAML-parsed configuration types shared by the engine
// (package main), the validator (internal/validate), and the test runner. The
// runtime-resolved secret fields (Telegram token, provider API key) are
// unexported and reached through accessors so callers in other packages can set
// and read them without widening the YAML surface.
package config

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	ghlapi "github.com/renezander030/draftcat/internal/ghl"
	gmailapi "github.com/renezander030/draftcat/internal/gmail"
)

type Config struct {
	Telegram  TelegramConfig         `yaml:"telegram"`
	Relay     RelayConfig            `yaml:"relay"`
	Gmail     gmailapi.GmailConfig   `yaml:"gmail"`
	GHL       ghlapi.GHLConfig       `yaml:"gohighlevel"`
	State     StateConfig            `yaml:"state"`
	Provider  ProviderConfig         `yaml:"provider"`
	Models    map[string]ModelConfig `yaml:"models"`
	Roles     map[string]string      `yaml:"roles"`
	Budgets   BudgetConfig           `yaml:"budgets"`
	Timeouts  TimeoutConfig          `yaml:"timeouts"`
	Policy    ApprovalPolicy         `yaml:"approval_policy"`
	ToolGate  ToolGateConfig         `yaml:"tool_gate"`
	Webhook   WebhookConfig          `yaml:"webhook"`
	Observ    ObservabilityConfig    `yaml:"observability"`
	Pipelines []PipelineConfig       `yaml:"pipelines"`
	// Voice is parsed unconditionally as raw YAML. Decoded into voice.Config
	// only when draftcat is built with -tags voice. Lean builds ignore it.
	Voice yaml.Node `yaml:"voice"`
}

// StateConfig points at the SQLite file used for cross-run state (dedup +
// run history). Empty Path defaults to "./state.db".
type StateConfig struct {
	Path string `yaml:"path"`
}

type TelegramConfig struct {
	TokenEnv string          `yaml:"token_env"`
	ChatID   int64           `yaml:"chat_id"`
	Security ChannelSecurity `yaml:"security"`
	token    string
}

// Token / SetToken access the runtime-resolved bot token (never parsed from YAML).
func (t TelegramConfig) Token() string      { return t.token }
func (t *TelegramConfig) SetToken(s string) { t.token = s }

// RelayConfig configures the `relay` operator channel: the hitl/v0 protocol
// endpoint that lets any external presenter (a Power Automate flow, a bot, n8n,
// a shell script) run the human round trip on draftcat's behalf.
//
// The relay exists so draftcat never owns a vendor's bot lifecycle. Teams is
// the motivating case — incoming webhooks were disabled in May 2026 and the
// only remaining in-binary path is an Azure app registration plus tenant admin
// consent, per vendor. See docs/hitl-protocol.md.
//
// The relay is untrusted: it can DENY (never answer, and the gate times out and
// the action does not fire) but it cannot AUTHORISE. Five checks enforce that —
// body-bound HMAC, clock-skew window, single-use nonce, payload-hash echo, and
// approver membership against Operators below.
type RelayConfig struct {
	// URL is the relay's dispatch endpoint. Empty = channel disabled.
	URL string `yaml:"url"`
	// SecretEnv names the env var holding the shared HMAC secret used in BOTH
	// directions. REQUIRED when URL is set.
	SecretEnv string `yaml:"secret_env"`
	// CallbackAddr is where the gate listens for decision envelopes, e.g.
	// "127.0.0.1:8089". Default "127.0.0.1:8089".
	CallbackAddr string `yaml:"callback_addr"`
	// PublicURL is the externally reachable base the relay posts decisions
	// back to; the callback path is appended. REQUIRED when URL is set,
	// because the relay cannot reach a loopback address.
	PublicURL string `yaml:"public_url"`
	// Operators maps each permitted human to the internal numeric ID the rest
	// of the engine already uses for quorum counting and approver scoping.
	// Identity is what travels on the wire and what the relay reports back.
	Operators []RelayOperator `yaml:"operators"`
	// Security carries the same allowed-user and input limits every operator
	// channel must declare. AllowedUsers holds the numeric IDs from Operators.
	Security ChannelSecurity `yaml:"security"`

	secret string
}

// RelayOperator binds a wire identity to the numeric operator ID used
// internally. Two representations exist because quorum, approver scoping and
// the audit trail were all built on int64 operator IDs, while a relay speaks in
// whatever identity its surface uses (an email, an SSO subject).
type RelayOperator struct {
	ID       int64  `yaml:"id"`
	Identity string `yaml:"identity"`
}

// Secret / SetSecret access the runtime-resolved relay secret (never parsed
// from YAML), matching how the Telegram bot token is handled.
func (r RelayConfig) Secret() string      { return r.secret }
func (r *RelayConfig) SetSecret(s string) { r.secret = s }

// Enabled reports whether the relay channel is configured at all.
func (r RelayConfig) Enabled() bool { return strings.TrimSpace(r.URL) != "" }

// IdentityFor returns the wire identity for a numeric operator ID.
func (r RelayConfig) IdentityFor(id int64) string {
	for _, op := range r.Operators {
		if op.ID == id {
			return op.Identity
		}
	}
	return ""
}

// OperatorFor returns the numeric operator ID for a wire identity, and whether
// it is known. An unknown identity is never admitted.
func (r RelayConfig) OperatorFor(identity string) (int64, bool) {
	for _, op := range r.Operators {
		if op.Identity == identity {
			return op.ID, true
		}
	}
	return 0, false
}

// ChannelSecurity is REQUIRED per operator channel. Engine refuses to start without it.
type ChannelSecurity struct {
	AllowedUsers   []int64 `yaml:"allowed_users"`    // TG user IDs / Slack user IDs that may interact
	MaxInputLength int     `yaml:"max_input_length"` // max chars per operator message (default 500)
	RateLimit      int     `yaml:"rate_limit"`       // max messages per minute per user (default 10)
	StripMarkdown  bool    `yaml:"strip_markdown"`   // strip formatting that could break prompt boundaries
}

type ProviderConfig struct {
	Type      string `yaml:"type"`
	APIKeyEnv string `yaml:"api_key_env"`
	BaseURL   string `yaml:"base_url"`
	// UsageAccounting asks the provider to report what each call actually cost
	// (OpenRouter: `usage.include`) and enforces the cost caps on that number,
	// falling back to the configured per-1k rates when the response carries no
	// cost. Rates alone undercount whenever a model bills reasoning tokens or
	// discounts cached ones. Omitted = on for OpenRouter, off for any other
	// OpenAI-compatible endpoint (those reject unknown request fields).
	UsageAccounting *bool `yaml:"usage_accounting"`
	// MaxRetries bounds how many times one call is retried on a transient
	// failure (429, 408, 5xx, network error). 0 = default (3).
	MaxRetries int `yaml:"max_retries"`
	apiKey     string
}

// APIKey / SetAPIKey access the runtime-resolved provider key (never parsed from YAML).
func (p ProviderConfig) APIKey() string      { return p.apiKey }
func (p *ProviderConfig) SetAPIKey(s string) { p.apiKey = s }

// IsOpenRouter reports whether the provider is OpenRouter. An empty type has
// always meant OpenRouter (the key falls back to OPENROUTER_API_KEY).
func (p ProviderConfig) IsOpenRouter() bool {
	t := strings.ToLower(strings.TrimSpace(p.Type))
	return t == "" || t == "openrouter"
}

// UsageAccountingOn resolves the tri-state: explicit setting wins, otherwise
// on exactly when the provider is OpenRouter.
func (p ProviderConfig) UsageAccountingOn() bool {
	if p.UsageAccounting != nil {
		return *p.UsageAccounting
	}
	return p.IsOpenRouter()
}

// RetriesOrDefault returns the retry budget for one LLM call.
func (p ProviderConfig) RetriesOrDefault() int {
	if p.MaxRetries > 0 {
		return p.MaxRetries
	}
	return 3
}

type ModelConfig struct {
	Model     string  `yaml:"model"`
	MaxTokens int     `yaml:"max_tokens"`
	CostIn    float64 `yaml:"cost_per_1k_input"`
	CostOut   float64 `yaml:"cost_per_1k_output"`
}

type BudgetConfig struct {
	PerStepTokens     int `yaml:"per_step_tokens"`
	PerPipelineTokens int `yaml:"per_pipeline_tokens"`
	PerDayTokens      int `yaml:"per_day_tokens"`
	PerDayCalls       int `yaml:"per_day_calls"`
	PerDayCallMinutes int `yaml:"per_day_call_minutes"`
	// PerDayCost / PerPipelineCost cap spend in MONEY rather than tokens, using
	// the same unit as models.<role>.cost_per_1k_input/output (draftcat does not
	// assume a currency — put your provider's rates in and the cap matches them).
	// Token caps answer "how much did it think"; these answer the question an
	// owner actually asks, "what will this cost me today". 0 = no cap, so
	// existing configs are unaffected.
	//
	// Enforcement is BETWEEN calls: a call is refused once spend has already
	// reached the cap, so a single in-flight call can overshoot by at most one
	// step. Pair with per_step_tokens to bound that overshoot.
	PerDayCost      float64 `yaml:"per_day_cost"`
	PerPipelineCost float64 `yaml:"per_pipeline_cost"`
}

type TimeoutConfig struct {
	AICall           string `yaml:"ai_call"`
	OperatorApproval string `yaml:"operator_approval"`
	PipelineTotal    string `yaml:"pipeline_total"`
}

// WebhookConfig enables the opt-in HTTP trigger server. A pipeline with
// `schedule: webhook` runs only when an authenticated POST hits
// /hooks/<pipeline>. Default (disabled) opens no port — lean behavior is
// unchanged. The webhook only TRIGGERS a pipeline; the pipeline still runs its
// own approval gates, so the deterministic boundary is preserved.
type WebhookConfig struct {
	Enabled bool `yaml:"enabled"`
	// Addr is the listen address, e.g. "127.0.0.1:8088". Default "127.0.0.1:8088".
	Addr string `yaml:"addr"`
	// SecretEnv names the env var holding the bearer token required on every
	// request (Authorization: Bearer <token>). REQUIRED when enabled.
	SecretEnv string `yaml:"secret_env"`
	// MaxBodyBytes caps the request body read into data["webhook_body"].
	// Default 65536.
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
	// RequireSignature demands a body-bound HMAC signature header on every
	// request, on top of the bearer token:
	//
	//     X-Draftcat-Signature: t=<unix>,v1=<hex hmac-sha256(t + "." + body)>
	//
	// A bearer token alone proves only that the caller once saw the token — a
	// captured header replays forever and the body is not bound to it, so an
	// intercepted request can be re-fired, or its body swapped, to start a
	// pipeline. The signature binds caller, body and time, which is the
	// Stripe/GitHub webhook norm.
	//
	// Default false keeps existing deployments working. Note that a signature
	// header, once present, is ALWAYS verified even when this is false — a
	// signer that breaks should fail loudly rather than be silently ignored.
	RequireSignature bool `yaml:"require_signature"`
	// MaxSkewSeconds bounds how far the signed timestamp may be from now, in
	// either direction. Default 300 (5 minutes).
	MaxSkewSeconds int64 `yaml:"max_skew_seconds"`
	secret         string
}

// Secret / SetSecret access the runtime-resolved webhook token (never parsed from YAML).
func (w WebhookConfig) Secret() string      { return w.secret }
func (w *WebhookConfig) SetSecret(s string) { w.secret = s }

// ObservabilityConfig toggles structured span emission (see internal/obs) and
// the opt-in exporters. Off by default. The DRAFTCAT_TRACE env var also enables
// span emission. The Prometheus/OTLP exporters are independent of Spans: either
// can be on while JSON span emission stays off.
type ObservabilityConfig struct {
	Spans      bool             `yaml:"spans"`
	Prometheus PrometheusConfig `yaml:"prometheus"`
	OTLP       OTLPConfig       `yaml:"otlp"`
}

// PrometheusConfig serves a /metrics endpoint when Enabled. Pull-based; opens
// one local port. Default disabled = no port opened (matches webhook ergonomics).
type PrometheusConfig struct {
	Enabled bool   `yaml:"enabled"`
	Addr    string `yaml:"addr"` // default "127.0.0.1:9090"
	Path    string `yaml:"path"` // default "/metrics"
}

// OTLPConfig pushes spans to an OTLP/HTTP collector. Default disabled.
type OTLPConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Endpoint string `yaml:"endpoint"` // e.g. "http://127.0.0.1:4318/v1/traces"
	// HeaderEnv names an env var holding "key=value,key2=value2" headers
	// (e.g. an auth token). Never put secrets in YAML.
	HeaderEnv string `yaml:"header_env"`
}

type PipelineConfig struct {
	Name     string       `yaml:"name"`
	Schedule string       `yaml:"schedule"`
	Steps    []StepConfig `yaml:"steps"`
}

type StepConfig struct {
	Name         string                 `yaml:"name"`
	Type         string                 `yaml:"type"`   // deterministic, ai, approval
	Action       string                 `yaml:"action"` // deterministic action name
	Role         string                 `yaml:"role"`
	Skill        string                 `yaml:"skill"` // reference to skills/<name>.yaml
	Prompt       string                 `yaml:"prompt"`
	Vars         map[string]string      `yaml:"vars"` // variables injected into skill prompt
	Mode         string                 `yaml:"mode"`
	Channel      string                 `yaml:"channel"`
	OutputSchema map[string]interface{} `yaml:"output_schema"`
	// Quorum is the number of distinct human operators that must approve this
	// approval step before the action is released. 0 or 1 = single approver
	// (default, unchanged behavior). Only the telegram channel implements N>=2.
	Quorum int `yaml:"quorum"`
	// Approvers narrows WHO may decide this step to a subset of the channel's
	// allowed_users. Empty = anyone on allowed_users, i.e. today's behavior.
	//
	// allowed_users is one flat list and quorum is only a count, so until now
	// every operator could approve every action: the assistant who triages
	// inbox drafts could also release an invoice. This is the missing axis —
	// quorum says how many, approvers says which ones.
	//
	// A quorum step needs at least Quorum entries here or it can never be
	// satisfied; the validator enforces that rather than letting it hang until
	// the approval timeout.
	Approvers []int64 `yaml:"approvers"`
	// Risk is the operator's own classification of what this step releases:
	// "low", "normal" (default) or "high". It is declared in config, never
	// inferred by a model. It travels on the hitl/v0 envelope so a relay can
	// style the prompt, and it is the axis approval_policy matches on.
	//
	// "high" can never be auto-approved, whatever the policy says.
	Risk string `yaml:"risk"`
	// EscalateAfter re-notifies the operator channel once this much of the
	// approval window has passed with no decision, e.g. "30m". Empty = no
	// reminder, today's behavior.
	EscalateAfter string `yaml:"escalate_after"`
	// EscalateTo names additional operators to notify at the reminder. They are
	// NOT added to the permitted approver set — escalation widens who is TOLD,
	// never who may decide, because widening authority on a timer would let a
	// slow operator silently promote someone the config never approved.
	EscalateTo []int64 `yaml:"escalate_to"`
}

// Risk levels. Declared by the operator in config, never inferred.
const (
	RiskLow    = "low"
	RiskNormal = "normal"
	RiskHigh   = "high"
)

// RiskOf returns the step's declared risk, defaulting to normal.
func (s StepConfig) RiskOf() string {
	switch strings.ToLower(strings.TrimSpace(s.Risk)) {
	case RiskLow:
		return RiskLow
	case RiskHigh:
		return RiskHigh
	default:
		return RiskNormal
	}
}

// ApprovalPolicy lets an operator decide IN ADVANCE that a declared class of
// action does not need a fresh tap every time.
//
// The reason this exists is that an approval gate people switch off protects
// nothing. Operators facing a prompt for every low-risk action reliably disable
// the gate wholesale, which trades a narrow, audited exemption for a total one.
// A policy tier is still an explicit operator decision — made once, in version
// control, reviewable — and every auto-approval is written to the audit trail
// with decision "policy_approve" and the rule that fired, so the trail always
// distinguishes what a human tapped from what a policy released.
//
// Empty policy = nothing is ever auto-approved, which is the default.
type ApprovalPolicy struct {
	AutoApprove []AutoApproveRule `yaml:"auto_approve"`
}

// AutoApproveRule matches an approval step. Every non-empty field must match;
// an empty field is a wildcard. A rule with no Risk is rejected by the
// validator — an unscoped auto-approve is how a policy tier turns into "no gate
// at all" by accident.
type AutoApproveRule struct {
	Risk     string `yaml:"risk"`
	Pipeline string `yaml:"pipeline"`
	Step     string `yaml:"step"`
	// MaxCost, when > 0, refuses the exemption once the pipeline has already
	// spent that much, so a cheap-per-action rule cannot quietly cover an
	// expensive run.
	MaxCost float64 `yaml:"max_cost"`
	// Reason is the operator's note for the audit trail, e.g. "internal drafts
	// only". Recorded on every row the rule releases.
	Reason string `yaml:"reason"`
}

// Match reports whether the rule covers this step, and is the single place the
// exemption is decided. High-risk steps are excluded here rather than at the
// call site so no future caller can route around it.
func (r AutoApproveRule) Match(pipeline string, st StepConfig, pipelineCost float64) bool {
	if st.RiskOf() == RiskHigh {
		return false
	}
	if r.Risk == "" || !strings.EqualFold(r.Risk, st.RiskOf()) {
		return false
	}
	if r.Pipeline != "" && r.Pipeline != pipeline {
		return false
	}
	if r.Step != "" && r.Step != st.Name {
		return false
	}
	if r.MaxCost > 0 && pipelineCost >= r.MaxCost {
		return false
	}
	return true
}

// AutoApproves returns the first matching rule, or nil when the step needs a
// human.
func (p ApprovalPolicy) AutoApproves(pipeline string, st StepConfig, pipelineCost float64) *AutoApproveRule {
	for i := range p.AutoApprove {
		if p.AutoApprove[i].Match(pipeline, st, pipelineCost) {
			return &p.AutoApprove[i]
		}
	}
	return nil
}

// ToolGateConfig exposes the approval gate to an agent's individual tool calls,
// not just to declared pipeline steps.
//
// draftcat's claim is to be the gate an agent cannot route around. For a
// pipeline that is structurally true. For a harness that also calls MCP or SDK
// tools mid-run it was true only by convention: those calls never reached the
// gate. This endpoint closes that by letting the harness ask permission for one
// tool call and get a decision back, with the same policy, audit and human
// escalation the pipeline steps use.
//
// Default is deny: a tool nobody listed is refused, so forgetting to configure
// a tool fails closed.
type ToolGateConfig struct {
	Enabled bool `yaml:"enabled"`
	// Tools is the allowlist. A name not listed here is denied.
	Tools []ToolRule `yaml:"tools"`
	// NotifyDenials tells the operator channel about calls the gate refused
	// WITHOUT asking a human — an unlisted tool, an argument that failed a
	// rule with on_mismatch: deny, the repeat guard. A human's own Skip is not
	// repeated back. Notices are deduplicated per agent+tool+reason inside
	// notify_window. Omitted = on.
	NotifyDenials *bool `yaml:"notify_denials"`
	// NotifyWindow is the dedup window for denial notices. Default 10m.
	NotifyWindow string `yaml:"notify_window"`
	// RepeatWindow is how long the gate remembers its decision on an identical
	// call (same agent, tool and argument hash). Inside it a call the operator
	// or a rule already denied is denied again without a new prompt, and a rule
	// with remember_approval reuses an approval. "0" disables. Default 10m.
	RepeatWindow string `yaml:"repeat_window"`
	// MaxRepeats caps how many times the identical call may reach the human
	// inside repeat_window before the gate stops asking and denies. 0 = no cap.
	MaxRepeats int `yaml:"max_repeats"`
}

// NotifyDenialsOn resolves the tri-state; omitted means on.
func (g ToolGateConfig) NotifyDenialsOn() bool {
	if g.NotifyDenials != nil {
		return *g.NotifyDenials
	}
	return true
}

// NotifyWindowOrDefault parses notify_window; unparseable or empty = 10m.
func (g ToolGateConfig) NotifyWindowOrDefault() time.Duration {
	return durationOr(g.NotifyWindow, 10*time.Minute)
}

// RepeatWindowOrDefault parses repeat_window; empty = 10m, "0" = off.
func (g ToolGateConfig) RepeatWindowOrDefault() time.Duration {
	return durationOr(g.RepeatWindow, 10*time.Minute)
}

func durationOr(s string, def time.Duration) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	if s == "0" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return def
	}
	return d
}

// ToolRule declares how one named tool call is handled.
type ToolRule struct {
	Name string `yaml:"name"`
	// Risk classifies the tool the same way a step is classified.
	Risk string `yaml:"risk"`
	// RequireApproval sends the call to the operator channel before allowing
	// it. Without this, a listed tool is allowed on the strength of being
	// listed — which is a real, auditable decision the operator made in config.
	RequireApproval bool `yaml:"require_approval"`
	// Args constrains the call's arguments. Each key names a top-level
	// argument; the call matches the rule only when every constraint holds.
	// A listed key that is absent from the call is a mismatch unless the
	// constraint says `optional: true`. What happens on a mismatch is
	// OnMismatch — the rule never widens on a mismatch, only tightens.
	Args map[string]ArgConstraint `yaml:"args"`
	// OnMismatch is what the gate does when the arguments fail the
	// constraints: "approve" (default) asks a human even if the rule would
	// otherwise allow silently; "deny" refuses without asking.
	OnMismatch string `yaml:"on_mismatch"`
	// RememberApproval lets an identical call (same agent, tool, argument hash)
	// inside tool_gate.repeat_window reuse an operator's approval instead of
	// asking again. Off by default: approving one send does not approve the
	// next, because the next one has the same side effect.
	RememberApproval bool `yaml:"remember_approval"`
}

// ArgConstraint is one condition on one argument. Every non-empty field must
// hold. Values are compared in their string form (numbers without exponent,
// booleans as true/false, anything structured as compact JSON) except min/max,
// which need a number.
type ArgConstraint struct {
	Equals   interface{} `yaml:"equals"`
	OneOf    []string    `yaml:"one_of"`
	Glob     string      `yaml:"glob"`  // path.Match syntax: * ? [...]
	Regex    string      `yaml:"regex"` // Go RE2, unanchored unless you write ^ $
	Max      *float64    `yaml:"max"`
	Min      *float64    `yaml:"min"`
	Optional bool        `yaml:"optional"`
}

// Empty reports a constraint with no condition at all — almost certainly a
// typo'd key, and worth refusing at validate time.
func (c ArgConstraint) Empty() bool {
	return c.Equals == nil && len(c.OneOf) == 0 && c.Glob == "" && c.Regex == "" && c.Max == nil && c.Min == nil
}

// Check returns whether v satisfies the constraint and, when it does not, a
// short reason naming the failing condition. present=false means the argument
// was not in the call at all.
func (c ArgConstraint) Check(v interface{}, present bool) (bool, string) {
	if !present {
		if c.Optional {
			return true, ""
		}
		return false, "missing"
	}
	s := ArgString(v)
	if c.Equals != nil && s != ArgString(c.Equals) {
		return false, fmt.Sprintf("%q is not %q", clip(s), clip(ArgString(c.Equals)))
	}
	if len(c.OneOf) > 0 {
		found := false
		for _, o := range c.OneOf {
			if s == o {
				found = true
				break
			}
		}
		if !found {
			return false, fmt.Sprintf("%q is not one of [%s]", clip(s), strings.Join(c.OneOf, ", "))
		}
	}
	if c.Glob != "" {
		ok, err := path.Match(c.Glob, s)
		if err != nil || !ok {
			return false, fmt.Sprintf("%q does not match glob %q", clip(s), c.Glob)
		}
	}
	if c.Regex != "" {
		re, err := regexp.Compile(c.Regex)
		if err != nil || !re.MatchString(s) {
			return false, fmt.Sprintf("%q does not match regex %q", clip(s), c.Regex)
		}
	}
	if c.Max != nil || c.Min != nil {
		f, ok := argNumber(v)
		if !ok {
			return false, fmt.Sprintf("%q is not a number", clip(s))
		}
		if c.Max != nil && f > *c.Max {
			return false, fmt.Sprintf("%s exceeds max %s", s, strconv.FormatFloat(*c.Max, 'f', -1, 64))
		}
		if c.Min != nil && f < *c.Min {
			return false, fmt.Sprintf("%s is below min %s", s, strconv.FormatFloat(*c.Min, 'f', -1, 64))
		}
	}
	return true, ""
}

// ArgString renders an argument value the way constraints compare it.
func ArgString(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case json.Number:
		return x.String()
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}

func argNumber(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func clip(s string) string {
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}

// MatchArgs checks the call's arguments against the rule. It returns true when
// the rule has no constraints or all of them hold; otherwise false and a
// one-line reason naming the first failing argument. Keys are checked in
// sorted order so the reason is stable.
func (t ToolRule) MatchArgs(args map[string]interface{}) (bool, string) {
	if len(t.Args) == 0 {
		return true, ""
	}
	keys := make([]string, 0, len(t.Args))
	for k := range t.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, present := args[k]
		if ok, why := t.Args[k].Check(v, present); !ok {
			return false, k + ": " + why
		}
	}
	return true, ""
}

// DeniesOnMismatch reports whether an argument mismatch is refused outright
// rather than escalated to a human.
func (t ToolRule) DeniesOnMismatch() bool {
	return strings.EqualFold(strings.TrimSpace(t.OnMismatch), "deny")
}

// RiskOf returns the rule's declared risk, defaulting to normal.
func (t ToolRule) RiskOf() string {
	switch strings.ToLower(strings.TrimSpace(t.Risk)) {
	case RiskLow:
		return RiskLow
	case RiskHigh:
		return RiskHigh
	default:
		return RiskNormal
	}
}

// Lookup returns the rule for a tool name, and whether one exists. An unknown
// tool has no rule and is therefore denied.
func (g ToolGateConfig) Lookup(name string) (ToolRule, bool) {
	for _, t := range g.Tools {
		if t.Name == name {
			return t, true
		}
	}
	return ToolRule{}, false
}
