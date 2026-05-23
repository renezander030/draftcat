package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// --- Config ---

type Config struct {
	Telegram  TelegramConfig         `yaml:"telegram"`
	Gmail     GmailConfig            `yaml:"gmail"`
	GHL       GHLConfig              `yaml:"gohighlevel"`
	State     StateConfig            `yaml:"state"`
	Provider  ProviderConfig         `yaml:"provider"`
	Models    map[string]ModelConfig `yaml:"models"`
	Roles     map[string]string      `yaml:"roles"`
	Budgets   BudgetConfig           `yaml:"budgets"`
	Timeouts  TimeoutConfig          `yaml:"timeouts"`
	Pipelines []PipelineConfig       `yaml:"pipelines"`
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
}

type TimeoutConfig struct {
	AICall           string `yaml:"ai_call"`
	OperatorApproval string `yaml:"operator_approval"`
	PipelineTotal    string `yaml:"pipeline_total"`
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

// --- Skills ---

type SkillDef struct {
	Name         string                 `yaml:"name"`
	Description  string                 `yaml:"description"`
	Role         string                 `yaml:"role"`
	Prompt       string                 `yaml:"prompt"`
	OutputSchema map[string]interface{} `yaml:"output_schema"`
}

// SkillRegistry loads and holds all skills from the skills/ directory
type SkillRegistry struct {
	skills map[string]*SkillDef
}

func loadSkills(dir string) (*SkillRegistry, error) {
	reg := &SkillRegistry{skills: make(map[string]*SkillDef)}
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return reg, nil // no skills dir is fine
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			log.Printf("[skills] failed to read %s: %v", f, err)
			continue
		}
		var skill SkillDef
		if err := yaml.Unmarshal(data, &skill); err != nil {
			log.Printf("[skills] failed to parse %s: %v", f, err)
			continue
		}
		reg.skills[skill.Name] = &skill
		log.Printf("[skills] loaded: %s (%s)", skill.Name, skill.Description)
	}
	return reg, nil
}

func (r *SkillRegistry) Get(name string) (*SkillDef, bool) {
	s, ok := r.skills[name]
	return s, ok
}

func (r *SkillRegistry) List() []*SkillDef {
	var out []*SkillDef
	for _, s := range r.skills {
		out = append(out, s)
	}
	return out
}

// --- Scheduler ---

type PipelineState struct {
	Name     string
	Schedule string
	Paused   bool
	Running  bool
	LastRun  time.Time
	NextRun  time.Time
}

type Scheduler struct {
	mu        sync.Mutex
	pipelines map[string]*PipelineState
}

func newScheduler(pipelines []PipelineConfig) *Scheduler {
	s := &Scheduler{pipelines: make(map[string]*PipelineState)}
	for _, p := range pipelines {
		ps := &PipelineState{
			Name:     p.Name,
			Schedule: p.Schedule,
		}
		if p.Schedule != "manual" && p.Schedule != "" {
			ps.NextRun = calcNextRun(p.Schedule)
		}
		s.pipelines[p.Name] = ps
	}
	return s
}

// calcNextRun parses simple interval schedules like "5m", "1h", "30s"
func calcNextRun(schedule string) time.Time {
	d, err := time.ParseDuration(schedule)
	if err != nil {
		return time.Time{} // invalid schedule, won't auto-run
	}
	return time.Now().Add(d)
}

func (s *Scheduler) GetAll() []*PipelineState {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*PipelineState
	for _, ps := range s.pipelines {
		out = append(out, ps)
	}
	return out
}

func (s *Scheduler) Pause(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ps, ok := s.pipelines[name]; ok {
		ps.Paused = true
		return true
	}
	return false
}

func (s *Scheduler) Resume(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ps, ok := s.pipelines[name]; ok {
		ps.Paused = false
		if ps.Schedule != "manual" && ps.Schedule != "" {
			ps.NextRun = calcNextRun(ps.Schedule)
		}
		return true
	}
	return false
}

func (s *Scheduler) Reschedule(name string, schedule string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ps, ok := s.pipelines[name]; ok {
		ps.Schedule = schedule
		ps.NextRun = calcNextRun(schedule)
		return true
	}
	return false
}

func (s *Scheduler) MarkRun(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ps, ok := s.pipelines[name]; ok {
		ps.LastRun = time.Now()
		if ps.Schedule != "manual" && ps.Schedule != "" {
			ps.NextRun = calcNextRun(ps.Schedule)
		}
	}
}

func (s *Scheduler) SetRunning(name string, running bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ps, ok := s.pipelines[name]; ok {
		ps.Running = running
	}
}

func (s *Scheduler) GetDue() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []string
	now := time.Now()
	for name, ps := range s.pipelines {
		if ps.Paused || ps.Running || ps.Schedule == "manual" || ps.Schedule == "" {
			continue
		}
		if !ps.NextRun.IsZero() && now.After(ps.NextRun) {
			due = append(due, name)
		}
	}
	return due
}

// --- Guardrails ---

type BudgetTracker struct {
	tokensUsedToday    int
	tokensUsedPipeline int
	dayStart           time.Time
}

func (b *BudgetTracker) check(limit int, requested int) error {
	if b.dayStart.Day() != time.Now().Day() {
		b.tokensUsedToday = 0
		b.dayStart = time.Now()
	}
	if b.tokensUsedToday+requested > limit {
		return fmt.Errorf("BUDGET_BLOCKED: daily token limit %d would be exceeded (used: %d, requested: %d)", limit, b.tokensUsedToday, requested)
	}
	return nil
}

func (b *BudgetTracker) record(tokens int) {
	b.tokensUsedToday += tokens
	b.tokensUsedPipeline += tokens
}

// --- Input Security ---
// Applied to ALL operator input before it reaches any AI step.
// This is not optional — the engine validates channel security config at startup.

// Prompt injection patterns — operator input is short commands/adjustments,
// never system prompts. Flag anything that looks like it's trying to rewrite
// the AI's instructions.
var injectionPatterns = []string{
	"ignore previous",
	"ignore above",
	"ignore all",
	"disregard",
	"forget your instructions",
	"you are now",
	"new instructions",
	"system prompt",
	"act as",
	"pretend to be",
	"jailbreak",
	"do anything now",
	"developer mode",
	"ignore safety",
	"bypass",
	"<|im_start|>",
	"<|im_end|>",
	"[INST]",
	"[/INST]",
	"<<SYS>>",
	"</s>",
	"\\n\\nHuman:",
	"\\n\\nAssistant:",
}

type InputValidationResult struct {
	Clean  bool
	Text   string
	Reason string
}

func validateOperatorInput(text string, sec ChannelSecurity) InputValidationResult {
	// 1. Length check
	maxLen := sec.MaxInputLength
	if maxLen == 0 {
		maxLen = 500 // hard default
	}
	if len(text) > maxLen {
		return InputValidationResult{
			Clean:  false,
			Reason: fmt.Sprintf("INPUT_REJECTED: message too long (%d chars, max %d)", len(text), maxLen),
		}
	}

	// 2. Empty check
	text = strings.TrimSpace(text)
	if text == "" {
		return InputValidationResult{Clean: false, Reason: "INPUT_REJECTED: empty message"}
	}

	// 3. Prompt injection scan
	lower := strings.ToLower(text)
	for _, pattern := range injectionPatterns {
		if strings.Contains(lower, pattern) {
			return InputValidationResult{
				Clean:  false,
				Reason: fmt.Sprintf("INPUT_REJECTED: potential prompt injection detected (pattern: %q)", pattern),
			}
		}
	}

	// 4. Strip markdown/special chars that could break prompt boundaries
	if sec.StripMarkdown {
		text = strings.NewReplacer(
			"```", "",
			"~~~", "",
			"---", "",
			"===", "",
		).Replace(text)
	}

	// 5. Strip any attempt to inject role markers (XML-style or chat-style)
	text = stripRoleMarkers(text)

	return InputValidationResult{Clean: true, Text: text}
}

func stripRoleMarkers(text string) string {
	// Remove anything that looks like it's trying to inject system/assistant/user role boundaries
	replacer := strings.NewReplacer(
		"<system>", "",
		"</system>", "",
		"<assistant>", "",
		"</assistant>", "",
		"<user>", "",
		"</user>", "",
	)
	return replacer.Replace(text)
}

// validateChannelSecurity checks that security config is present and valid.
// Called at startup — engine refuses to start if this fails.
func validateChannelSecurity(cfg *Config) error {
	// Telegram channel must have security configured
	if cfg.Telegram.ChatID != 0 {
		sec := cfg.Telegram.Security
		if len(sec.AllowedUsers) == 0 {
			return fmt.Errorf("STARTUP_BLOCKED: telegram.security.allowed_users is required — specify which user IDs may interact with the bot")
		}
		if sec.MaxInputLength == 0 {
			cfg.Telegram.Security.MaxInputLength = 500 // enforce default
		}
		if sec.RateLimit == 0 {
			cfg.Telegram.Security.RateLimit = 10 // enforce default
		}
	}
	return nil
}

// RateLimiter tracks per-user message rates
type RateLimiter struct {
	windows map[int64][]time.Time // userID → timestamps of recent messages
	limit   int                   // max messages per minute
}

func newRateLimiter(limit int) *RateLimiter {
	if limit == 0 {
		limit = 10
	}
	return &RateLimiter{
		windows: make(map[int64][]time.Time),
		limit:   limit,
	}
}

func (r *RateLimiter) allow(userID int64) bool {
	now := time.Now()
	cutoff := now.Add(-1 * time.Minute)

	// Prune old entries
	recent := []time.Time{}
	for _, t := range r.windows[userID] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}

	if len(recent) >= r.limit {
		return false
	}

	r.windows[userID] = append(recent, now)
	return true
}

// --- LLM Provider (OpenRouter) ---

type CompletionRequest struct {
	Model     string
	Prompt    string
	MaxTokens int
}

type CompletionResponse struct {
	Text         string
	InputTokens  int
	OutputTokens int
	LatencyMs    int64
	CostUSD      float64
	Model        string
}

// httpClient with connection settings tuned for flaky providers
var httpClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        10,
		IdleConnTimeout:     30 * time.Second,
		DisableKeepAlives:   true, // fresh connection per request — avoids stale connection EOF
		TLSHandshakeTimeout: 10 * time.Second,
	},
	Timeout: 30 * time.Second,
}

func callLLM(ctx context.Context, cfg *Config, role string, prompt string) (*CompletionResponse, error) {
	modelName, ok := cfg.Roles[role]
	if !ok {
		return nil, fmt.Errorf("unknown role: %s", role)
	}
	modelCfg, ok := cfg.Models[modelName]
	if !ok {
		return nil, fmt.Errorf("unknown model: %s", modelName)
	}

	reqBody := map[string]interface{}{
		"model": modelCfg.Model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"max_tokens": modelCfg.MaxTokens,
	}

	var respBody []byte
	var latency int64
	var lastErr error

	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			log.Printf("[llm] retry %d after error: %v", attempt, lastErr)
			time.Sleep(time.Duration(attempt) * time.Second)
		}

		body, _ := json.Marshal(reqBody)
		req, err := http.NewRequestWithContext(ctx, "POST", cfg.Provider.BaseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+cfg.Provider.apiKey)

		start := time.Now()
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("LLM request failed: %w", err)
			continue
		}
		latency = time.Since(start).Milliseconds()

		respBody, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("LLM API error %d: %s", resp.StatusCode, string(respBody))
			continue
		}
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("LLM API error %d: %s", resp.StatusCode, string(respBody))
		}
		lastErr = nil
		break
	}
	if lastErr != nil {
		return nil, lastErr
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to parse LLM response: %w", err)
	}
	if len(result.Choices) == 0 {
		return nil, fmt.Errorf("LLM returned no choices")
	}

	cost := float64(result.Usage.PromptTokens)/1000*modelCfg.CostIn +
		float64(result.Usage.CompletionTokens)/1000*modelCfg.CostOut

	return &CompletionResponse{
		Text:         result.Choices[0].Message.Content,
		InputTokens:  result.Usage.PromptTokens,
		OutputTokens: result.Usage.CompletionTokens,
		LatencyMs:    latency,
		CostUSD:      cost,
		Model:        result.Model,
	}, nil
}

// --- Output Validation ---

func toFloat64(v interface{}) *float64 {
	switch n := v.(type) {
	case float64:
		return &n
	case int:
		f := float64(n)
		return &f
	case int64:
		f := float64(n)
		return &f
	default:
		return nil
	}
}

func validateOutput(text string, schema map[string]interface{}) (map[string]interface{}, error) {
	if len(schema) == 0 {
		return nil, nil
	}

	// Strip markdown code fences if present
	cleaned := strings.TrimSpace(text)
	if strings.HasPrefix(cleaned, "```") {
		lines := strings.Split(cleaned, "\n")
		if len(lines) >= 3 {
			cleaned = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		return nil, fmt.Errorf("output is not valid JSON: %w\nRaw: %s", err, text)
	}

	for key, schemaDef := range schema {
		val, exists := parsed[key]
		if !exists {
			return nil, fmt.Errorf("missing required field: %s", key)
		}

		if defMap, ok := schemaDef.(map[string]interface{}); ok {
			if typeName, ok := defMap["type"].(string); ok {
				switch typeName {
				case "int":
					num := toFloat64(val)
					if num == nil {
						return nil, fmt.Errorf("field %s: expected number, got %T", key, val)
					}
					if minVal, ok := defMap["min"]; ok {
						if mv := toFloat64(minVal); mv != nil && *num < *mv {
							return nil, fmt.Errorf("field %s: value %v below min %v", key, *num, *mv)
						}
					}
					if maxVal, ok := defMap["max"]; ok {
						if mv := toFloat64(maxVal); mv != nil && *num > *mv {
							return nil, fmt.Errorf("field %s: value %v above max %v", key, *num, *mv)
						}
					}
				case "bool":
					if _, ok := val.(bool); !ok {
						return nil, fmt.Errorf("field %s: expected bool, got %T", key, val)
					}
				case "string":
					if _, ok := val.(string); !ok {
						return nil, fmt.Errorf("field %s: expected string, got %T", key, val)
					}
				}
			}
		}
	}

	return parsed, nil
}

// --- Operator Channel Interface ---
// Each channel (TG, Slack, etc.) implements this interface.
// The engine doesn't know which channel it's talking to.

type OperatorChannel interface {
	// Send a plain notification (no approval needed)
	Send(text string) error
	// Send a draft for approval with action buttons. Returns the operator's decision.
	SendForApproval(ctx context.Context, draft string) (OperatorDecision, error)
}

type OperatorDecision struct {
	Action string // "approve", "skip", "adjust"
	Text   string // adjustment text (only if Action == "adjust")
}

// --- Telegram Channel ---

type TGBot struct {
	token       string
	chatID      int64
	offset      int
	security    ChannelSecurity
	rateLimiter *RateLimiter
}

// tgClient — dedicated HTTP client for Telegram API with timeouts
var tgClient = &http.Client{Timeout: 10 * time.Second}

func (t *TGBot) apiCall(method string, payload map[string]interface{}) (json.RawMessage, error) {
	body, _ := json.Marshal(payload)
	resp, err := tgClient.Post(
		fmt.Sprintf("https://api.telegram.org/bot%s/%s", t.token, method),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var result struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	json.Unmarshal(respBody, &result)
	if !result.OK {
		return nil, fmt.Errorf("TG %s failed: %s", method, result.Description)
	}
	return result.Result, nil
}

func (t *TGBot) Send(text string) error {
	_, err := t.apiCall("sendMessage", map[string]interface{}{
		"chat_id": t.chatID,
		"text":    text,
	})
	if err != nil {
		log.Printf("[telegram] Send failed: %v", err)
	}
	return err
}

func (t *TGBot) sendTyping() {
	t.apiCall("sendChatAction", map[string]interface{}{
		"chat_id": t.chatID,
		"action":  "typing",
	})
}

// sendWithButtons sends a message with inline keyboard buttons.
// Returns the message ID.
func (t *TGBot) sendWithButtons(text string, buttons [][]map[string]string) (int, error) {
	raw, err := t.apiCall("sendMessage", map[string]interface{}{
		"chat_id":      t.chatID,
		"text":         text,
		"reply_markup": map[string]interface{}{"inline_keyboard": buttons},
	})
	if err != nil {
		return 0, err
	}
	var msg struct {
		MessageID int `json:"message_id"`
	}
	json.Unmarshal(raw, &msg)
	return msg.MessageID, nil
}

// editButtons replaces the inline keyboard on an existing message.
func (t *TGBot) editButtons(msgID int, text string, buttons [][]map[string]string) {
	payload := map[string]interface{}{
		"chat_id":    t.chatID,
		"message_id": msgID,
		"text":       text,
	}
	if buttons != nil {
		payload["reply_markup"] = map[string]interface{}{"inline_keyboard": buttons}
	} else {
		// Remove keyboard
		payload["reply_markup"] = map[string]interface{}{"inline_keyboard": []interface{}{}}
	}
	t.apiCall("editMessageText", payload)
}

func (t *TGBot) answerCallback(callbackID string, text string) {
	t.apiCall("answerCallbackQuery", map[string]interface{}{
		"callback_query_id": callbackID,
		"text":              text,
	})
}

// SendForApproval posts a draft with Approve/Skip/Adjust buttons.
// Waits for the operator to click a button or send a text reply for adjustment.
func (t *TGBot) SendForApproval(ctx context.Context, draft string) (OperatorDecision, error) {
	buttons := [][]map[string]string{
		{
			{"text": "Approve", "callback_data": "approve"},
			{"text": "Skip", "callback_data": "skip"},
			{"text": "Adjust...", "callback_data": "adjust"},
		},
	}

	msgID, err := t.sendWithButtons(draft, buttons)
	if err != nil {
		return OperatorDecision{}, fmt.Errorf("failed to send draft: %w", err)
	}
	log.Printf("[telegram] draft posted (msg_id=%d), waiting for operator action...", msgID)

	// Poll for callback queries (button clicks) or text replies
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	waitingForText := false

	for {
		select {
		case <-ctx.Done():
			t.editButtons(msgID, draft+"\n\n[Timed out]", nil)
			return OperatorDecision{}, fmt.Errorf("approval timeout")
		case <-ticker.C:
			updates, err := t.getUpdates()
			if err != nil {
				continue
			}
			for _, u := range updates {
				// Handle callback query (button click)
				if u.CallbackQuery != nil {
					cb := u.CallbackQuery
					// Security: verify user
					if !t.isAllowedUser(cb.From.ID) {
						t.answerCallback(cb.ID, "") // silent drop
						log.Printf("[security] REJECTED callback from user %d", cb.From.ID)
						continue
					}
					if !t.rateLimiter.allow(cb.From.ID) {
						t.answerCallback(cb.ID, "") // silent drop
						continue
					}
					// Must be for our message
					if cb.Message.MessageID != msgID {
						continue
					}

					switch cb.Data {
					case "approve":
						t.answerCallback(cb.ID, "Approved")
						t.editButtons(msgID, draft+"\n\n[Approved]", nil)
						return OperatorDecision{Action: "approve"}, nil
					case "skip":
						t.answerCallback(cb.ID, "Skipped")
						t.editButtons(msgID, draft+"\n\n[Skipped]", nil)
						return OperatorDecision{Action: "skip"}, nil
					case "adjust":
						t.answerCallback(cb.ID, "Send your adjustment as a text message")
						waitingForText = true
						t.editButtons(msgID, draft+"\n\n[Waiting for adjustment text...]", nil)
					}
				}

				// Handle text message (adjustment)
				if waitingForText && u.Message.Text != "" {
					// Security checks
					if u.Message.Chat.ID != t.chatID {
						continue
					}
					if !t.isAllowedUser(u.Message.From.ID) {
						log.Printf("[security] REJECTED text from user %d", u.Message.From.ID)
						continue
					}
					if !t.rateLimiter.allow(u.Message.From.ID) {
						continue
					}

					// Input validation
					result := validateOperatorInput(u.Message.Text, t.security)
					if !result.Clean {
						log.Printf("[security] %s (user: %d)", result.Reason, u.Message.From.ID)
						// silent drop — don't tell attacker why input was rejected
						continue
					}

					return OperatorDecision{Action: "adjust", Text: result.Text}, nil
				}
			}
		}
	}
}

func (t *TGBot) isAllowedUser(userID int64) bool {
	for _, id := range t.security.AllowedUsers {
		if id == userID {
			return true
		}
	}
	return false
}

type TGUpdate struct {
	UpdateID      int         `json:"update_id"`
	Message       *TGMessage  `json:"message"`
	CallbackQuery *TGCallback `json:"callback_query"`
}

type TGMessage struct {
	MessageID int    `json:"message_id"`
	Text      string `json:"text"`
	From      struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Chat struct {
		ID int64 `json:"id"`
	} `json:"chat"`
}

type TGCallback struct {
	ID   string `json:"id"`
	From struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Message struct {
		MessageID int `json:"message_id"`
	} `json:"message"`
	Data string `json:"data"`
}

func (t *TGBot) getUpdates() ([]TGUpdate, error) {
	reqBody := map[string]interface{}{
		"offset":          t.offset,
		"timeout":         0,
		"allowed_updates": []string{"message", "callback_query"},
	}
	body, _ := json.Marshal(reqBody)
	resp, err := tgClient.Post(
		fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates", t.token),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var result struct {
		OK     bool       `json:"ok"`
		Result []TGUpdate `json:"result"`
	}
	json.Unmarshal(respBody, &result)

	if len(result.Result) > 0 {
		t.offset = result.Result[len(result.Result)-1].UpdateID + 1
	}

	return result.Result, nil
}

// --- Pipeline Engine ---

func runPipeline(cfg *Config, pipeline PipelineConfig, budget *BudgetTracker, ch OperatorChannel, skills *SkillRegistry) error {
	log.Printf("[pipeline:%s] starting", pipeline.Name)
	budget.tokensUsedPipeline = 0

	// Parse pipeline timeout
	pipelineTimeout, _ := time.ParseDuration(cfg.Timeouts.PipelineTotal)
	if pipelineTimeout == 0 {
		pipelineTimeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), pipelineTimeout)
	defer cancel()

	// Pipeline data flows through this map
	data := map[string]interface{}{}

	// Mock test data
	data["input"] = `Title: Senior AI/LLM Engineer - Build RAG Pipeline for Legal Documents
Rate: $80-120/hr
Duration: 3-6 months, 20 hrs/week
Client: $45K spent, 4.92 stars, United States
Skills: RAG, Vector Databases, Claude API, TypeScript
Description: We need an experienced LLM engineer to build a retrieval-augmented generation pipeline for searching and summarizing legal documents. Must have production RAG experience.`

	for _, step := range pipeline.Steps {
		select {
		case <-ctx.Done():
			return fmt.Errorf("pipeline timeout after %s", pipelineTimeout)
		default:
		}

		log.Printf("[pipeline:%s][step:%s] type=%s", pipeline.Name, step.Name, step.Type)

		switch step.Type {
		case "deterministic":
			log.Printf("[pipeline:%s][step:%s] action=%s", pipeline.Name, step.Name, step.Action)
			switch step.Action {
			case "gmail_unread":
				if gmail == nil {
					return fmt.Errorf("[step:%s] gmail connector not configured", step.Name)
				}
				emails, err := gmail.FetchUnread(10)
				if err != nil {
					return fmt.Errorf("[step:%s] gmail fetch failed: %w", step.Name, err)
				}
				if len(emails) == 0 {
					log.Printf("[pipeline:%s][step:%s] no unread emails, skipping pipeline", pipeline.Name, step.Name)
					return nil // nothing to report
				}
				emails = dedupByID(pipeline.Name, "gmail", emails,
					func(e Email) string { return e.ID })
				if len(emails) == 0 {
					log.Printf("[pipeline:%s][step:%s] all unread emails already processed, skipping pipeline", pipeline.Name, step.Name)
					return nil
				}
				data["emails"] = FormatEmailsForPrompt(emails)
				data["email_count"] = fmt.Sprintf("%d", len(emails))
				// Store for /reply reference
				lastEmailsMu.Lock()
				lastEmails = emails
				lastEmailsMu.Unlock()
				log.Printf("[pipeline:%s][step:%s] fetched %d unread emails (new)", pipeline.Name, step.Name, len(emails))

			case "notify":
				// Send ai_output or ai_raw to operator channel
				msg := ""
				if output, ok := data["ai_output"]; ok {
					switch v := output.(type) {
					case map[string]interface{}:
						if digest, ok := v["digest"].(string); ok {
							msg = digest
						} else {
							raw, _ := json.Marshal(v)
							msg = string(raw)
						}
					case string:
						msg = v
					default:
						msg = fmt.Sprintf("%v", v)
					}
				} else if raw, ok := data["ai_raw"]; ok {
					msg = fmt.Sprintf("%v", raw)
				}
				if msg != "" {
					count := data["email_count"]
					header := fmt.Sprintf("[email-digest] %v unread\n\n", count)
					ch.Send(header + msg)
				}

			case "ghl_new_contacts":
				if ghl == nil {
					return fmt.Errorf("[step:%s] ghl connector not configured", step.Name)
				}
				hours := 24
				contacts, err := ghl.FetchRecentContacts(hours, 20)
				if err != nil {
					return fmt.Errorf("[step:%s] ghl fetch contacts failed: %w", step.Name, err)
				}
				if len(contacts) == 0 {
					log.Printf("[pipeline:%s][step:%s] no new contacts, skipping pipeline", pipeline.Name, step.Name)
					return nil
				}
				contacts = dedupByID(pipeline.Name, "ghl_contacts", contacts,
					func(c GHLContact) string { return c.ID })
				if len(contacts) == 0 {
					log.Printf("[pipeline:%s][step:%s] all recent contacts already processed, skipping pipeline", pipeline.Name, step.Name)
					return nil
				}
				data["contacts"] = FormatContactsForPrompt(contacts)
				data["contact_count"] = fmt.Sprintf("%d", len(contacts))
				log.Printf("[pipeline:%s][step:%s] fetched %d new contacts", pipeline.Name, step.Name, len(contacts))

			case "ghl_stale_opportunities":
				if ghl == nil {
					return fmt.Errorf("[step:%s] ghl connector not configured", step.Name)
				}
				pipelineID, _ := step.Vars["pipeline_id"]
				if pipelineID == "" {
					return fmt.Errorf("[step:%s] ghl_stale_opportunities requires pipeline_id var", step.Name)
				}
				staleDays := 7
				opps, err := ghl.FetchStaleOpportunities(pipelineID, staleDays, 20)
				if err != nil {
					return fmt.Errorf("[step:%s] ghl fetch stale opportunities failed: %w", step.Name, err)
				}
				if len(opps) == 0 {
					log.Printf("[pipeline:%s][step:%s] no stale opportunities, skipping pipeline", pipeline.Name, step.Name)
					return nil
				}
				data["opportunities"] = FormatOpportunitiesForPrompt(opps)
				data["opportunity_count"] = fmt.Sprintf("%d", len(opps))
				log.Printf("[pipeline:%s][step:%s] fetched %d stale opportunities", pipeline.Name, step.Name, len(opps))

			case "ghl_unread_conversations":
				if ghl == nil {
					return fmt.Errorf("[step:%s] ghl connector not configured", step.Name)
				}
				convos, err := ghl.FetchUnreadConversations(20)
				if err != nil {
					return fmt.Errorf("[step:%s] ghl fetch conversations failed: %w", step.Name, err)
				}
				if len(convos) == 0 {
					log.Printf("[pipeline:%s][step:%s] no unread conversations, skipping pipeline", pipeline.Name, step.Name)
					return nil
				}
				// Composite key id|lastDate so a new message on an already-seen
				// conversation is treated as a fresh item.
				convos = dedupByID(pipeline.Name, "ghl_conversations", convos,
					func(c GHLConversation) string { return c.ID + "|" + c.LastDate })
				if len(convos) == 0 {
					log.Printf("[pipeline:%s][step:%s] all unread conversations already processed, skipping pipeline", pipeline.Name, step.Name)
					return nil
				}
				data["conversations"] = FormatConversationsForPrompt(convos)
				data["conversation_count"] = fmt.Sprintf("%d", len(convos))
				log.Printf("[pipeline:%s][step:%s] fetched %d unread conversations", pipeline.Name, step.Name, len(convos))

			case "pdf_extract":
				path := step.Vars["path"]
				if path == "" {
					if v, ok := data["pdf_path"].(string); ok {
						path = v
					}
				}
				if path == "" {
					return fmt.Errorf("[step:%s] pdf_extract requires path var or data[pdf_path]", step.Name)
				}
				doc, err := pdfParser.Extract(path)
				if err != nil {
					return fmt.Errorf("[step:%s] pdf extract failed: %w", step.Name, err)
				}
				data["pdf_doc"] = doc
				data["pdf_filename"] = doc.Filename
				data["pdf_text"] = FormatPDFForPrompt(doc)
				data["pdf_page_count"] = fmt.Sprintf("%d", len(doc.Pages))
				log.Printf("[pipeline:%s][step:%s] parsed %s: %d pages", pipeline.Name, step.Name, doc.Filename, len(doc.Pages))

			case "pdf_verify_cite":
				doc, ok := data["pdf_doc"].(*PDFDoc)
				if !ok {
					return fmt.Errorf("[step:%s] pdf_verify_cite needs an earlier pdf_extract step", step.Name)
				}
				raw, _ := data["ai_raw"].(string)
				cites := citeTagRe.FindAllStringSubmatch(raw, -1)
				results := make([]map[string]interface{}, 0, len(cites))
				var unresolved []string
				for _, m := range cites {
					file, pageStr, text := m[1], m[2], m[3]
					page, _ := strconv.Atoi(pageStr)
					entry := map[string]interface{}{
						"file": file, "page": page, "text": text, "resolved": false,
					}
					if file != doc.Filename {
						unresolved = append(unresolved, fmt.Sprintf("%s p%d (wrong file, expected %s)", file, page, doc.Filename))
						results = append(results, entry)
						continue
					}
					if span, found := pdfParser.FindSpan(doc, page, text); found {
						entry["resolved"] = true
						entry["span"] = span
					} else {
						snippet := text
						if len(snippet) > 60 {
							snippet = snippet[:60] + "…"
						}
						unresolved = append(unresolved, fmt.Sprintf("%s p%d: %q", file, page, snippet))
					}
					results = append(results, entry)
				}
				data["citations"] = results
				data["citations_ok"] = len(unresolved) == 0
				log.Printf("[pipeline:%s][step:%s] %d citations, %d unresolved", pipeline.Name, step.Name, len(results), len(unresolved))
				if len(unresolved) > 0 && step.Vars["fail_on_unresolved"] == "true" {
					return fmt.Errorf("[step:%s] unresolved citations: %s", step.Name, strings.Join(unresolved, "; "))
				}

			default:
				// No action — pass-through
			}

		case "ai":
			// Budget pre-flight
			if err := budget.check(cfg.Budgets.PerDayTokens, cfg.Budgets.PerStepTokens); err != nil {
				log.Printf("[pipeline:%s][step:%s] %s", pipeline.Name, step.Name, err)
				return err
			}

			// Resolve skill or use inline prompt
			prompt := step.Prompt
			role := step.Role
			schema := step.OutputSchema

			if step.Skill != "" {
				skill, ok := skills.Get(step.Skill)
				if !ok {
					return fmt.Errorf("[step:%s] unknown skill: %s", step.Name, step.Skill)
				}
				prompt = skill.Prompt
				role = skill.Role
				if len(skill.OutputSchema) > 0 {
					schema = skill.OutputSchema
				}
				log.Printf("[pipeline:%s][step:%s] using skill: %s", pipeline.Name, step.Name, step.Skill)
			}

			// Inject step vars
			for k, v := range step.Vars {
				prompt = strings.ReplaceAll(prompt, "{{"+k+"}}", v)
			}

			// Inject pipeline data
			for k, v := range data {
				prompt = strings.ReplaceAll(prompt, "{{"+k+"}}", fmt.Sprintf("%v", v))
			}

			// AI call with timeout
			aiTimeout, _ := time.ParseDuration(cfg.Timeouts.AICall)
			if aiTimeout == 0 {
				aiTimeout = 30 * time.Second
			}
			aiCtx, aiCancel := context.WithTimeout(ctx, aiTimeout)

			resp, err := callLLM(aiCtx, cfg, role, prompt)
			aiCancel()
			if err != nil {
				return fmt.Errorf("[step:%s] LLM call failed: %w", step.Name, err)
			}

			// Record token usage
			budget.record(resp.InputTokens + resp.OutputTokens)
			log.Printf("[pipeline:%s][step:%s] model=%s tokens=%d+%d cost=$%.4f latency=%dms",
				pipeline.Name, step.Name, resp.Model,
				resp.InputTokens, resp.OutputTokens, resp.CostUSD, resp.LatencyMs)

			// Validate output
			if len(schema) > 0 {
				parsed, err := validateOutput(resp.Text, schema)
				if err != nil {
					log.Printf("[pipeline:%s][step:%s] OUTPUT_INVALID: %s", pipeline.Name, step.Name, err)
					return fmt.Errorf("[step:%s] output validation failed: %w", step.Name, err)
				}
				data["ai_output"] = parsed
				log.Printf("[pipeline:%s][step:%s] output validated: %v", pipeline.Name, step.Name, parsed)
			} else {
				data["ai_output"] = resp.Text
			}
			data["ai_raw"] = resp.Text

		case "approval":
			// Build draft message from AI output
			aiOutput := data["ai_output"]
			var draftMsg string

			switch v := aiOutput.(type) {
			case map[string]interface{}:
				score, _ := v["score"].(float64)
				reason, _ := v["reason"].(string)
				reject, _ := v["reject"].(bool)
				status := "MATCH"
				if reject {
					status = "REJECT"
				}
				draftMsg = fmt.Sprintf("[draftyard] %s - Score: %d/5\n\n%s\n\n%v",
					status, int(score), reason, data["input"])
			default:
				draftMsg = fmt.Sprintf("[draftyard] Draft for review:\n\n%v", v)
			}

			// Send for approval via operator channel (TG buttons, Slack reactions, etc.)
			approvalTimeout, _ := time.ParseDuration(cfg.Timeouts.OperatorApproval)
			if approvalTimeout == 0 {
				approvalTimeout = 4 * time.Hour
			}
			approvalCtx, approvalCancel := context.WithTimeout(ctx, approvalTimeout)
			decision, err := ch.SendForApproval(approvalCtx, draftMsg)
			approvalCancel()
			if err != nil {
				return fmt.Errorf("[step:%s] %w", step.Name, err)
			}

			log.Printf("[pipeline:%s][step:%s] operator decision: %s", pipeline.Name, step.Name, decision.Action)

			switch decision.Action {
			case "approve":
				data["approved"] = true
			case "skip":
				data["approved"] = false
				return nil
			case "adjust":
				log.Printf("[pipeline:%s][step:%s] adjustment: %q", pipeline.Name, step.Name, decision.Text)

				if err := budget.check(cfg.Budgets.PerDayTokens, cfg.Budgets.PerStepTokens); err != nil {
					ch.Send(fmt.Sprintf("Budget exceeded, cannot rewrite: %s", err))
					return err
				}

				adjustPrompt := fmt.Sprintf("Original output:\n%s\n\nOperator feedback:\n%s\n\nRewrite incorporating the feedback. Respond with ONLY valid JSON in the same format.", data["ai_raw"], decision.Text)

				aiTimeout, _ := time.ParseDuration(cfg.Timeouts.AICall)
				if aiTimeout == 0 {
					aiTimeout = 30 * time.Second
				}
				aiCtx, aiCancel := context.WithTimeout(ctx, aiTimeout)
				resp, err := callLLM(aiCtx, cfg, "drafter", adjustPrompt)
				aiCancel()
				if err != nil {
					ch.Send(fmt.Sprintf("Rewrite failed: %s", err))
					return err
				}
				budget.record(resp.InputTokens + resp.OutputTokens)

				// Show revised output and ask for final approval
				revisedDraft := fmt.Sprintf("[draftyard] Revised:\n\n%s", resp.Text)
				approvalCtx2, approvalCancel2 := context.WithTimeout(ctx, approvalTimeout)
				decision2, err := ch.SendForApproval(approvalCtx2, revisedDraft)
				approvalCancel2()
				if err != nil {
					return err
				}
				if decision2.Action == "approve" {
					data["approved"] = true
				} else {
					return nil
				}
			}
		}
	}

	log.Printf("[pipeline:%s] completed. tokens_used=%d", pipeline.Name, budget.tokensUsedPipeline)
	return nil
}

// --- Command Handler ---
// Handles operator commands like /cron, /skills, /run, /status

var gmail *GmailConnector // initialized in main if configured
var ghl *GHLConnector     // initialized in main if configured
var pdfParser *PDFParser  // initialized unconditionally in main (no config required)
var state *StateStore     // SQLite-backed state store; opened in main, closed on shutdown
var lastEmails []Email    // last fetched emails for /reply reference
var lastEmailsMu sync.Mutex

// citeTagRe captures <cite file="X" page="N">verbatim text</cite> emitted by
// AI steps that quote a parsed PDF. The verifier resolves each to a span.
var citeTagRe = regexp.MustCompile(`<cite file="([^"]+)" page="(\d+)">([^<]+)</cite>`)

// --- Chat History ---
// Per-chat conversation buffer. Keeps last N turns for context.

type ChatHistory struct {
	mu       sync.Mutex
	messages []ChatMessage
	maxTurns int
}

type ChatMessage struct {
	Role    string // "user" or "assistant"
	Content string
	Time    time.Time
}

func newChatHistory(maxTurns int) *ChatHistory {
	if maxTurns == 0 {
		maxTurns = 20
	}
	return &ChatHistory{maxTurns: maxTurns}
}

func (h *ChatHistory) Add(role string, content string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, ChatMessage{Role: role, Content: content, Time: time.Now()})
	// Trim to max turns
	if len(h.messages) > h.maxTurns*2 {
		h.messages = h.messages[len(h.messages)-h.maxTurns*2:]
	}
}

func (h *ChatHistory) FormatForPrompt() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.messages) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, m := range h.messages {
		if m.Role == "user" {
			sb.WriteString("Operator: " + m.Content + "\n")
		} else {
			sb.WriteString("Assistant: " + m.Content + "\n")
		}
	}
	return sb.String()
}

func (h *ChatHistory) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.messages)
}

func handleCommand(cmd string, args string, bot *TGBot, sched *Scheduler, skills *SkillRegistry, cfg *Config, budget *BudgetTracker) {
	switch cmd {
	case "/cron":
		handleCron(args, bot, sched)
	case "/skills":
		handleSkills(bot, skills)
	case "/run":
		handleRun(args, bot, sched, cfg, budget, skills)
	case "/status":
		handleStatus(bot, budget, sched)
	case "/emails":
		handleEmails(args, bot, cfg, budget)
	case "/reply":
		handleReply(args, bot, cfg, budget)
	case "/thread":
		handleThread(args, bot, cfg, budget)
	case "/reauth":
		handleReauth(bot)
	case "/authcode":
		handleAuthCode(args, bot)
	case "/help":
		bot.Send("Commands:\n/emails [query] - Check emails\n/reply <number> [text] - Reply to an email\n/cron - Manage pipeline schedules\n/skills - List skills\n/run <pipeline> - Run a pipeline now\n/status - Engine status")
	}
}

func handleEmails(args string, bot *TGBot, cfg *Config, budget *BudgetTracker) {
	if gmail == nil {
		bot.Send("[emails] Gmail connector not configured.")
		return
	}
	query := strings.TrimSpace(args)
	if query == "" {
		query = "is:unread"
	}
	maxResults := 5

	emails, err := gmail.FetchRecent(query, maxResults)
	if err != nil {
		log.Printf("[emails] fetch error: %v", err)
		bot.Send(fmt.Sprintf("[emails] Error: %s", err))
		return
	}

	// If specific query returned nothing, try broader search and filter client-side
	if len(emails) == 0 && query != "" && query != "is:unread" {
		log.Printf("[emails] no results for %q, trying broader search", query)
		// Extract the name/keyword from query for client-side filtering
		filterTerm := ""
		for _, part := range strings.Fields(query) {
			if strings.HasPrefix(part, "to:") {
				filterTerm = strings.TrimPrefix(part, "to:")
			} else if strings.HasPrefix(part, "from:") {
				filterTerm = strings.TrimPrefix(part, "from:")
			}
		}
		// Broaden: just use in:sent or no filter
		broadQuery := ""
		if strings.Contains(query, "in:sent") {
			broadQuery = "in:sent"
		}
		broader, err := gmail.FetchRecent(broadQuery, 20)
		if err == nil && filterTerm != "" {
			// Client-side filter by name in From/To/Subject
			ft := strings.ToLower(filterTerm)
			var filtered []Email
			for _, e := range broader {
				if strings.Contains(strings.ToLower(e.From), ft) ||
					strings.Contains(strings.ToLower(e.To), ft) ||
					strings.Contains(strings.ToLower(e.Subject), ft) {
					filtered = append(filtered, e)
				}
			}
			emails = filtered
		} else if err == nil {
			emails = broader
		}
	}

	// Store for /reply reference
	lastEmailsMu.Lock()
	lastEmails = emails
	lastEmailsMu.Unlock()

	if len(emails) == 0 {
		bot.Send("[emails] No emails found for: " + query)
		return
	}

	// Format and send
	formatted := FormatEmailsForPrompt(emails)
	header := fmt.Sprintf("[emails] %d result(s) for: %s\n\n", len(emails), query)

	// If short enough, send directly. Otherwise summarize with LLM.
	if len(formatted) < 3000 {
		bot.Send(header + formatted)
	} else {
		// Use LLM to summarize
		if err := budget.check(cfg.Budgets.PerDayTokens, 1024); err != nil {
			bot.Send(header + formatted[:2000] + "\n\n[truncated]")
			return
		}
		bot.sendTyping()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		resp, err := callLLM(ctx, cfg, "classifier", fmt.Sprintf(
			"Summarize these emails in a brief list. For each: sender, subject, 1-line summary. Be concise.\n\n%s", formatted))
		cancel()
		if err != nil {
			bot.Send(header + formatted[:2000] + "\n\n[truncated]")
		} else {
			budget.record(resp.InputTokens + resp.OutputTokens)
			bot.Send(header + resp.Text)
		}
	}
}

func handleReply(args string, bot *TGBot, cfg *Config, budget *BudgetTracker) {
	if gmail == nil {
		bot.Send("[reply] Gmail connector not configured.")
		return
	}

	parts := strings.SplitN(strings.TrimSpace(args), " ", 2)
	if len(parts) == 0 || parts[0] == "" {
		bot.Send("[reply] Usage: /reply <number> [your reply text]\nOr: /reply <number> (AI drafts a reply)")
		return
	}

	// Parse email number
	idx := 0
	fmt.Sscanf(parts[0], "%d", &idx)
	if idx < 1 {
		bot.Send("[reply] Invalid email number. Use /emails first, then /reply 1")
		return
	}

	lastEmailsMu.Lock()
	emails := lastEmails
	lastEmailsMu.Unlock()

	// Auto-fetch if no emails cached
	if len(emails) == 0 {
		log.Printf("[reply] no cached emails, auto-fetching...")
		fetched, err := gmail.FetchRecent("is:unread", 10)
		if err != nil {
			bot.Send(fmt.Sprintf("[reply] Failed to fetch emails: %s", err))
			return
		}
		if len(fetched) == 0 {
			fetched, err = gmail.FetchRecent("", 10)
			if err != nil {
				bot.Send(fmt.Sprintf("[reply] Failed to fetch emails: %s", err))
				return
			}
		}
		lastEmailsMu.Lock()
		lastEmails = fetched
		lastEmailsMu.Unlock()
		emails = fetched
	}

	if idx > len(emails) {
		bot.Send(fmt.Sprintf("[reply] Only %d emails available. Try a lower number.", len(emails)))
		return
	}

	target := emails[idx-1]

	// Get full message with thread info for proper reply
	fullEmail, threadID, messageID, references, err := gmail.GetFullMessage(target.ID)
	if err != nil {
		bot.Send(fmt.Sprintf("[reply] Failed to fetch email: %s", err))
		return
	}

	replyTo := ExtractEmailAddress(fullEmail.From)
	subject := fullEmail.Subject

	var replyBody string

	if len(parts) > 1 && parts[1] != "" {
		// User provided reply text directly
		replyBody = parts[1]
	} else {
		// AI drafts a reply
		bot.sendTyping()
		if err := budget.check(cfg.Budgets.PerDayTokens, 1024); err != nil {
			bot.Send("[reply] Budget limit reached.")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		prompt := fmt.Sprintf(`Draft a brief, professional reply to this email. Just the reply body, no subject line or headers.

From: %s
Subject: %s
Body:
%s`, fullEmail.From, fullEmail.Subject, fullEmail.Body)
		resp, err := callLLM(ctx, cfg, "drafter", prompt)
		cancel()
		if err != nil {
			bot.Send(fmt.Sprintf("[reply] Draft failed: %s", err))
			return
		}
		budget.record(resp.InputTokens + resp.OutputTokens)
		replyBody = resp.Text
	}

	// Show draft for HITL approval
	draft := fmt.Sprintf("[reply] Draft reply to: %s\nSubject: Re: %s\n\n%s", replyTo, subject, replyBody)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Hour)
	decision, err := bot.SendForApproval(ctx, draft)
	cancel()
	if err != nil {
		log.Printf("[reply] approval error: %v", err)
		return
	}

	switch decision.Action {
	case "approve":
		// Build references chain
		refs := references
		if refs != "" {
			refs += " " + messageID
		} else {
			refs = messageID
		}

		// Try send, fall back to draft
		if cfg.Gmail.Permission == "send" {
			err = gmail.SendReply(replyTo, subject, messageID, refs, threadID, replyBody)
			if err != nil {
				log.Printf("[reply] send failed: %v", err)
				bot.Send(fmt.Sprintf("[reply] Send failed: %s\nTrying to save as draft...", err))
				err = gmail.CreateDraft(replyTo, subject, messageID, refs, threadID, replyBody)
			}
		} else {
			err = gmail.CreateDraft(replyTo, subject, messageID, refs, threadID, replyBody)
		}

		if err != nil {
			bot.Send(fmt.Sprintf("[reply] Failed: %s\nYou may need to /reauth with broader scopes.", err))
		} else {
			action := "Saved as draft"
			if cfg.Gmail.Permission == "send" {
				action = "Sent"
			}
			bot.Send(fmt.Sprintf("[reply] %s to %s", action, replyTo))
		}

	case "adjust":
		// Use adjusted text as the reply
		replyBody = decision.Text
		bot.Send(fmt.Sprintf("[reply] Updated. Send /reply %d %s to send with this text, or use the buttons.", idx, replyBody))

	case "skip":
		bot.Send("[reply] Cancelled.")
	}
}

const authRedirectPort = 9999

func handleThread(args string, bot *TGBot, cfg *Config, budget *BudgetTracker) {
	if gmail == nil {
		bot.Send("[thread] Gmail connector not configured.")
		return
	}

	parts := strings.Fields(args)
	if len(parts) == 0 || parts[0] == "" {
		bot.Send("[thread] Usage: /thread <number> — show full conversation for email N from last fetch")
		return
	}

	idx := 0
	fmt.Sscanf(parts[0], "%d", &idx)
	if idx < 1 {
		idx = 1
	}

	lastEmailsMu.Lock()
	emails := lastEmails
	lastEmailsMu.Unlock()

	// Auto-fetch if empty
	if len(emails) == 0 {
		fetched, err := gmail.FetchRecent("", 10)
		if err != nil {
			bot.Send(fmt.Sprintf("[thread] Failed to fetch emails: %s", err))
			return
		}
		lastEmailsMu.Lock()
		lastEmails = fetched
		lastEmailsMu.Unlock()
		emails = fetched
	}

	if idx > len(emails) {
		bot.Send(fmt.Sprintf("[thread] Only %d emails available.", len(emails)))
		return
	}

	target := emails[idx-1]

	// Get thread ID
	_, threadID, _, _, err := gmail.GetFullMessage(target.ID)
	if err != nil {
		bot.Send(fmt.Sprintf("[thread] Failed: %s", err))
		return
	}

	// Fetch full thread
	bot.sendTyping()
	threadEmails, err := gmail.FetchThread(threadID)
	if err != nil {
		bot.Send(fmt.Sprintf("[thread] Failed to fetch thread: %s", err))
		return
	}

	threadText := FormatThreadForPrompt(threadEmails, "rio@ramaris.app")

	// Summarize with LLM
	if err := budget.check(cfg.Budgets.PerDayTokens, 2048); err != nil {
		// No budget — send raw
		bot.Send(fmt.Sprintf("[thread] %d messages in thread:\n\n%s", len(threadEmails), threadText[:min(3500, len(threadText))]))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	prompt := fmt.Sprintf(`Summarize this email thread. Show the back-and-forth between sent and received messages. Include key points, decisions, and any action items. Be concise.

Thread (%d messages):
%s`, len(threadEmails), threadText)
	resp, err := callLLM(ctx, cfg, "classifier", prompt)
	cancel()
	if err != nil {
		bot.Send(fmt.Sprintf("[thread] %d messages:\n\n%s", len(threadEmails), threadText[:min(3500, len(threadText))]))
		return
	}
	budget.record(resp.InputTokens + resp.OutputTokens)
	bot.Send(fmt.Sprintf("[thread] %d messages — %s\n\n%s", len(threadEmails), target.Subject, resp.Text))
}

func handleReauth(bot *TGBot) {
	if gmail == nil {
		bot.Send("[reauth] Gmail connector not configured.")
		return
	}
	scopes := []string{
		"https://www.googleapis.com/auth/gmail.readonly",
		"https://www.googleapis.com/auth/gmail.send",
		"https://www.googleapis.com/auth/gmail.compose",
	}
	authURL := GenerateAuthURL(gmail.token.ClientID, scopes, authRedirectPort)
	bot.Send(fmt.Sprintf("[reauth] Open this URL:\n\n%s\n\nAfter authorizing, the page will fail to load. Copy the 'code' parameter from the URL bar and send:\n/authcode <the-code>", authURL))
}

func handleAuthCode(args string, bot *TGBot) {
	code := strings.TrimSpace(args)
	if code == "" {
		bot.Send("[authcode] Usage: /authcode <code>")
		return
	}
	if gmail == nil {
		bot.Send("[authcode] Gmail connector not configured.")
		return
	}
	err := gmail.ExchangeCode(code, authRedirectPort)
	if err != nil {
		bot.Send(fmt.Sprintf("[authcode] Failed: %s", err))
		return
	}
	bot.Send("[authcode] Success. Gmail now has send/compose permissions.")
}

func handleCron(args string, bot *TGBot, sched *Scheduler) {
	parts := strings.Fields(args)

	// /cron with no args — show all pipelines
	if len(parts) == 0 {
		states := sched.GetAll()
		if len(states) == 0 {
			bot.Send("[cron] No pipelines configured.")
			return
		}
		msg := "[cron] Pipeline schedules:\n"
		for _, ps := range states {
			status := "active"
			if ps.Paused {
				status = "PAUSED"
			}
			if ps.Schedule == "manual" || ps.Schedule == "" {
				status = "manual"
			}
			nextStr := "-"
			if !ps.NextRun.IsZero() && !ps.Paused {
				nextStr = ps.NextRun.Format("15:04:05")
			}
			lastStr := "-"
			if !ps.LastRun.IsZero() {
				lastStr = ps.LastRun.Format("15:04:05")
			}
			msg += fmt.Sprintf("\n%s [%s]\n  schedule: %s | last: %s | next: %s",
				ps.Name, status, ps.Schedule, lastStr, nextStr)
		}

		// Show inline buttons for each pipeline
		var buttons [][]map[string]string
		for _, ps := range states {
			if ps.Paused {
				buttons = append(buttons, []map[string]string{
					{"text": "Resume " + ps.Name, "callback_data": "cron:resume:" + ps.Name},
				})
			} else if ps.Schedule != "manual" && ps.Schedule != "" {
				buttons = append(buttons, []map[string]string{
					{"text": "Pause " + ps.Name, "callback_data": "cron:pause:" + ps.Name},
				})
			}
			buttons = append(buttons, []map[string]string{
				{"text": "Run " + ps.Name + " now", "callback_data": "cron:run:" + ps.Name},
			})
		}
		bot.sendWithButtons(msg, buttons)
		return
	}

	// /cron pause <name>
	if parts[0] == "pause" && len(parts) > 1 {
		if sched.Pause(parts[1]) {
			bot.Send(fmt.Sprintf("[cron] Paused: %s", parts[1]))
		} else {
			bot.Send(fmt.Sprintf("[cron] Unknown pipeline: %s", parts[1]))
		}
		return
	}

	// /cron resume <name>
	if parts[0] == "resume" && len(parts) > 1 {
		if sched.Resume(parts[1]) {
			bot.Send(fmt.Sprintf("[cron] Resumed: %s", parts[1]))
		} else {
			bot.Send(fmt.Sprintf("[cron] Unknown pipeline: %s", parts[1]))
		}
		return
	}

	// /cron set <name> <schedule>
	if parts[0] == "set" && len(parts) > 2 {
		if sched.Reschedule(parts[1], parts[2]) {
			bot.Send(fmt.Sprintf("[cron] Rescheduled %s to %s", parts[1], parts[2]))
		} else {
			bot.Send(fmt.Sprintf("[cron] Unknown pipeline: %s", parts[1]))
		}
		return
	}

	bot.Send("[cron] Usage: /cron | /cron pause <name> | /cron resume <name> | /cron set <name> <interval>")
}

func handleSkills(bot *TGBot, skills *SkillRegistry) {
	list := skills.List()
	if len(list) == 0 {
		bot.Send("[skills] No skills loaded.")
		return
	}
	msg := "[skills] Available skills:\n"
	for _, s := range list {
		msg += fmt.Sprintf("\n  %s — %s (role: %s)", s.Name, s.Description, s.Role)
	}
	bot.Send(msg)
}

func handleRun(args string, bot *TGBot, sched *Scheduler, cfg *Config, budget *BudgetTracker, skills *SkillRegistry) {
	name := strings.TrimSpace(args)
	if name == "" {
		bot.Send("[run] Usage: /run <pipeline-name>")
		return
	}
	// Find pipeline config
	var pipeline *PipelineConfig
	for i := range cfg.Pipelines {
		if cfg.Pipelines[i].Name == name {
			pipeline = &cfg.Pipelines[i]
			break
		}
	}
	if pipeline == nil {
		bot.Send(fmt.Sprintf("[run] Unknown pipeline: %s", name))
		return
	}
	bot.Send(fmt.Sprintf("[run] Starting: %s", name))
	sched.SetRunning(name, true)
	go func() {
		defer sched.SetRunning(name, false)
		if err := runPipeline(cfg, *pipeline, budget, bot, skills); err != nil {
			log.Printf("[run] pipeline %s error: %v", name, err)
			bot.Send(fmt.Sprintf("[run] ERROR in %s: %s", name, err))
		}
		sched.MarkRun(name)
	}()
}

func handleStatus(bot *TGBot, budget *BudgetTracker, sched *Scheduler) {
	states := sched.GetAll()
	active := 0
	paused := 0
	for _, ps := range states {
		if ps.Paused {
			paused++
		} else {
			active++
		}
	}
	msg := fmt.Sprintf("[status] Engine running\nPipelines: %d active, %d paused\nTokens today: %d\nBudget day start: %s",
		active, paused, budget.tokensUsedToday, budget.dayStart.Format("15:04:05"))
	bot.Send(msg)
}

// --- Main ---

func main() {
	// Subcommand dispatch. The bare form `draftyard [config.yaml] [skills/]` still runs the engine.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "validate":
			os.Exit(runValidate(os.Args[2:]))
		case "test":
			os.Exit(runTestCmd(os.Args[2:]))
		case "-h", "--help", "help":
			fmt.Println("Draftyard — AI communication management for service businesses.")
			fmt.Println()
			fmt.Println("Usage:")
			fmt.Println("  draftyard [config.yaml] [skills/]   run the engine (default)")
			fmt.Println("  draftyard validate [--strict]       lint config + skills, exit non-zero on errors")
			fmt.Println("  draftyard test <pipeline>           dry-run a pipeline using fixtures/<pipeline>/")
			return
		}
	}

	configPath := "config.yaml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	// Load config
	cfgData, err := os.ReadFile(configPath)
	if err != nil {
		log.Fatalf("failed to read config: %v", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(cfgData, &cfg); err != nil {
		log.Fatalf("failed to parse config: %v", err)
	}

	// Resolve env vars
	cfg.Telegram.token = resolveEnv(cfg.Telegram.TokenEnv, "DRAFTYARD_TG_TOKEN")
	cfg.Provider.apiKey = resolveEnv(cfg.Provider.APIKeyEnv, "OPENROUTER_API_KEY")

	if cfg.Telegram.token == "" {
		log.Fatal("Telegram token not set. Set DRAFTYARD_TG_TOKEN env var.")
	}
	if cfg.Provider.apiKey == "" {
		log.Fatal("OpenRouter API key not set. Set OPENROUTER_API_KEY env var.")
	}

	// Validate channel security — refuse to start without it
	if err := validateChannelSecurity(&cfg); err != nil {
		log.Fatalf("%v", err)
	}

	// Load skills
	skillsDir := "skills"
	if len(os.Args) > 2 {
		skillsDir = os.Args[2]
	}
	skillReg, _ := loadSkills(skillsDir)

	// Init Gmail connector if configured
	if cfg.Gmail.TokenPath != "" {
		var err error
		gmail, err = NewGmailConnector(cfg.Gmail.TokenPath)
		if err != nil {
			log.Printf("[gmail] WARNING: failed to initialize: %v", err)
		}
	}

	// Init GoHighLevel connector if configured
	if cfg.GHL.TokenPath != "" || cfg.GHL.APIKeyEnv != "" {
		var err error
		ghl, err = NewGHLConnector(cfg.GHL)
		if err != nil {
			log.Printf("[ghl] WARNING: failed to initialize: %v", err)
		}
	}

	// PDF connector — stateless, always available
	pdfParser = NewPDFParser()

	// State store — SQLite-backed dedup + run history
	statePath := cfg.State.Path
	if statePath == "" {
		statePath = "./state.db"
	}
	{
		var err error
		state, err = OpenStateStore(statePath)
		if err != nil {
			log.Fatalf("[state] failed to open %s: %v", statePath, err)
		}
		defer state.Close()
		log.Printf("[state] opened %s", statePath)
	}

	// Init scheduler
	sched := newScheduler(cfg.Pipelines)

	log.Printf("draftyard starting — %d pipeline(s), %d skill(s), provider=%s, operator=telegram:%d",
		len(cfg.Pipelines), len(skillReg.skills), cfg.Provider.Type, cfg.Telegram.ChatID)

	bot := &TGBot{
		token:       cfg.Telegram.token,
		chatID:      cfg.Telegram.ChatID,
		security:    cfg.Telegram.Security,
		rateLimiter: newRateLimiter(cfg.Telegram.Security.RateLimit),
	}
	budget := &BudgetTracker{dayStart: time.Now()}
	chatHistory := newChatHistory(20) // keep last 20 turns

	// Drain pending updates
	bot.getUpdates()

	// Startup notification
	// No startup message — don't leak that the bot is running to anyone watching the chat

	// Signal handling for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Main event loop — polls for commands and runs scheduled pipelines
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	log.Printf("draftyard running. Waiting for commands and scheduled pipelines...")

	for {
		select {
		case sig := <-sigCh:
			log.Printf("received %s, shutting down", sig)
			// silent shutdown — no message
			return

		case <-ticker.C:
			// 1. Check for operator commands and callbacks
			updates, err := bot.getUpdates()
			if err != nil {
				continue
			}
			for _, u := range updates {
				// Handle text messages (commands)
				if u.Message != nil {
					if u.Message.Chat.ID != bot.chatID {
						continue
					}
					if !bot.isAllowedUser(u.Message.From.ID) {
						continue
					}
					text := strings.TrimSpace(u.Message.Text)
					if strings.HasPrefix(text, "/") {
						parts := strings.SplitN(text, " ", 2)
						cmd := parts[0]
						args := ""
						if len(parts) > 1 {
							args = parts[1]
						}
						log.Printf("[cmd] %s %s (user: %d)", cmd, args, u.Message.From.ID)
						bot.sendTyping()
						handleCommand(cmd, args, bot, sched, skillReg, &cfg, budget)
					} else if text != "" {
						log.Printf("[msg] %q (user: %d)", text, u.Message.From.ID)
						bot.sendTyping()

						// Intent detection via LLM — classify what the user wants
						if gmail != nil {
							intentCtx, intentCancel := context.WithTimeout(context.Background(), 10*time.Second)

							// Build email context for the classifier
							lastEmailsMu.Lock()
							emailCtx := ""
							if len(lastEmails) > 0 {
								for i, e := range lastEmails {
									from := e.From
									if idx := strings.Index(from, "<"); idx > 0 {
										from = strings.TrimSpace(from[:idx])
									}
									emailCtx += fmt.Sprintf("%d. %s — %s\n", i+1, strings.Trim(from, "\""), e.Subject)
								}
							}
							lastEmailsMu.Unlock()

							intentPrompt := fmt.Sprintf(`Classify this message into one intent. Respond with ONLY valid JSON.

Available emails:
%s
Message: %s

Intents:
- Read/check/fetch/search emails (inbox, sent, from someone): {"intent":"emails","query":"<gmail query or empty>"}
- Reply/respond/send to an email: {"intent":"reply","number":<1-based index>,"body":"<reply text or empty for AI draft>"}
- View full conversation/thread/history for an email: {"intent":"thread","number":<1-based index>}
- Anything else (questions, conversation, commands): {"intent":"chat"}

Rules:
- If they mention a name or sender, match it to the email list and return the number
- "reply to rene" = find email from rene in list, return its number
- "send a reply" with no target = reply to email 1
- "check sent" / "show sent emails" / "what did I send" = {"intent":"emails","query":"in:sent"}
- "emails from john" = {"intent":"emails","query":"from:john"}
- "what did I reply to john" / "my reply to john" = {"intent":"emails","query":"to:john in:sent"}
- "emails to sarah" = {"intent":"emails","query":"to:sarah in:sent"}
- "can you see sent emails" = {"intent":"emails","query":"in:sent"}
- Questions about what you can do = {"intent":"chat"}
- IMPORTANT: "from:" means received FROM someone. "to:" means sent TO someone. If the user asks what THEY sent/replied to someone, use "to:<name> in:sent"`, emailCtx, text)

							intentResp, err := callLLM(intentCtx, &cfg, "classifier", intentPrompt)
							intentCancel()

							if err != nil {
								log.Printf("[intent] classifier error: %v", err)
							} else {
								budget.record(intentResp.InputTokens + intentResp.OutputTokens)
								// Parse intent
								cleaned := strings.TrimSpace(intentResp.Text)
								if strings.HasPrefix(cleaned, "```") {
									lines := strings.Split(cleaned, "\n")
									if len(lines) >= 3 {
										cleaned = strings.Join(lines[1:len(lines)-1], "\n")
									}
								}
								var intent struct {
									Intent string `json:"intent"`
									Query  string `json:"query"`
									Number int    `json:"number"`
									Body   string `json:"body"`
								}
								if err := json.Unmarshal([]byte(cleaned), &intent); err != nil {
									log.Printf("[intent] parse error: %v (raw: %q)", err, cleaned)
								} else {
									log.Printf("[intent] %s (number=%d, query=%q, body=%q)", intent.Intent, intent.Number, intent.Query, intent.Body)
									switch intent.Intent {
									case "emails":
										handleEmails(intent.Query, bot, &cfg, budget)
										continue
									case "reply":
										num := intent.Number
										if num == 0 {
											num = 1
										}
										args := fmt.Sprintf("%d", num)
										if intent.Body != "" {
											args += " " + intent.Body
										}
										handleReply(args, bot, &cfg, budget)
										continue
									case "thread":
										num := intent.Number
										if num == 0 {
											num = 1
										}
										handleThread(fmt.Sprintf("%d", num), bot, &cfg, budget)
										continue
									}
									// "chat" falls through to LLM chat below
								}
							}
						}

						// Regular message — respond via LLM with conversation history
						chatHistory.Add("user", text)

						if err := budget.check(cfg.Budgets.PerDayTokens, 512); err != nil {
							bot.Send("Budget limit reached.")
						} else {
							aiCtx, aiCancel := context.WithTimeout(context.Background(), 15*time.Second)
							var skillList, pipelineList string
							for _, s := range skillReg.List() {
								skillList += fmt.Sprintf("\n- %s: %s", s.Name, s.Description)
							}
							for _, p := range cfg.Pipelines {
								pipelineList += fmt.Sprintf("\n- %s (schedule: %s)", p.Name, p.Schedule)
							}
							gmailStatus := "not configured"
							if gmail != nil {
								gmailStatus = fmt.Sprintf("connected (permission: %s) — can read inbox, sent, search, and reply to emails", cfg.Gmail.Permission)
							}
							history := chatHistory.FormatForPrompt()
							sysPrompt := fmt.Sprintf(`You are draftyard, an AI operations assistant running as a Telegram bot.
You manage automated pipelines and can help the operator with tasks.

Available pipelines:%s

Available skills:%s

Connectors:
- Gmail: %s

Commands the operator can use:
/emails [query] - check emails (unread by default, or custom query)
/cron - view/manage pipeline schedules
/skills - list skills
/run <name> - run a pipeline now
/status - engine status

When the operator asks about emails, you can fetch them directly. When they ask for features not yet available (calendar, Slack, etc), say briefly what's needed and that it's on the roadmap. Be direct, concise, no fluff. Remember the conversation context.

Conversation so far:
%s`, pipelineList, skillList, gmailStatus, history)
							resp, err := callLLM(aiCtx, &cfg, "drafter", sysPrompt)
							aiCancel()
							if err != nil {
								log.Printf("[msg] LLM error: %v", err)
								bot.Send("Commands: /help /cron /skills /run /status")
							} else {
								budget.record(resp.InputTokens + resp.OutputTokens)
								log.Printf("[msg] LLM reply (%d tokens, %dms): %s", resp.InputTokens+resp.OutputTokens, resp.LatencyMs, resp.Text[:min(80, len(resp.Text))])
								chatHistory.Add("assistant", resp.Text)
								if err := bot.Send(resp.Text); err != nil {
									log.Printf("[msg] Send error: %v", err)
								}
							}
						}
					}
				}

				// Handle callback queries (button clicks) — separate from messages
				if u.CallbackQuery != nil {
					cb := u.CallbackQuery
					if !bot.isAllowedUser(cb.From.ID) {
						bot.answerCallback(cb.ID, "") // silent drop
						continue
					}
					log.Printf("[callback] %s (user: %d)", cb.Data, cb.From.ID)
					if strings.HasPrefix(cb.Data, "cron:") {
						parts := strings.SplitN(cb.Data, ":", 3)
						if len(parts) == 3 {
							switch parts[1] {
							case "pause":
								sched.Pause(parts[2])
								bot.answerCallback(cb.ID, "Paused: "+parts[2])
								bot.Send(fmt.Sprintf("[cron] Paused: %s", parts[2]))
							case "resume":
								sched.Resume(parts[2])
								bot.answerCallback(cb.ID, "Resumed: "+parts[2])
								bot.Send(fmt.Sprintf("[cron] Resumed: %s", parts[2]))
							case "run":
								bot.answerCallback(cb.ID, "Starting: "+parts[2])
								handleRun(parts[2], bot, sched, &cfg, budget, skillReg)
							}
						}
					}
				}
			}

			// 2. Check for scheduled pipelines
			due := sched.GetDue()
			for _, name := range due {
				for i := range cfg.Pipelines {
					if cfg.Pipelines[i].Name == name {
						log.Printf("[scheduler] running due pipeline: %s", name)
						sched.SetRunning(name, true)
						go func(p PipelineConfig) {
							defer sched.SetRunning(p.Name, false)
							if err := runPipeline(&cfg, p, budget, bot, skillReg); err != nil {
								log.Printf("[scheduler] pipeline %s error: %v", p.Name, err)
								bot.Send(fmt.Sprintf("[draftyard] ERROR in %s: %s", p.Name, err))
							}
							sched.MarkRun(p.Name)
						}(cfg.Pipelines[i])
						break
					}
				}
			}
		}
	}
}

func resolveEnv(names ...string) string {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}
