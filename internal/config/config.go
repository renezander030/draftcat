// Package config holds the YAML-parsed configuration types shared by the engine
// (package main), the validator (internal/validate), and the test runner. The
// runtime-resolved secret fields (Telegram token, provider API key) are
// unexported and reached through accessors so callers in other packages can set
// and read them without widening the YAML surface.
package config

import (
	"gopkg.in/yaml.v3"

	ghlapi "github.com/renezander030/draftcat/internal/ghl"
	gmailapi "github.com/renezander030/draftcat/internal/gmail"
)

type Config struct {
	Telegram  TelegramConfig         `yaml:"telegram"`
	Gmail     gmailapi.GmailConfig   `yaml:"gmail"`
	GHL       ghlapi.GHLConfig       `yaml:"gohighlevel"`
	State     StateConfig            `yaml:"state"`
	Provider  ProviderConfig         `yaml:"provider"`
	Models    map[string]ModelConfig `yaml:"models"`
	Roles     map[string]string      `yaml:"roles"`
	Budgets   BudgetConfig           `yaml:"budgets"`
	Timeouts  TimeoutConfig          `yaml:"timeouts"`
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
	apiKey    string
}

// APIKey / SetAPIKey access the runtime-resolved provider key (never parsed from YAML).
func (p ProviderConfig) APIKey() string      { return p.apiKey }
func (p *ProviderConfig) SetAPIKey(s string) { p.apiKey = s }

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
	secret       string
}

// Secret / SetSecret access the runtime-resolved webhook token (never parsed from YAML).
func (w WebhookConfig) Secret() string      { return w.secret }
func (w *WebhookConfig) SetSecret(s string) { w.secret = s }

// ObservabilityConfig toggles structured span emission (see internal/obs).
// Off by default. The DRAFTCAT_TRACE env var also enables it.
type ObservabilityConfig struct {
	Spans bool `yaml:"spans"`
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
}
