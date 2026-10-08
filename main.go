package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/renezander030/draftcat/internal/config"
	skillsapi "github.com/renezander030/draftcat/internal/skills"
	"github.com/renezander030/draftcat/internal/validate"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/renezander030/draftcat/internal/approval"
	"github.com/renezander030/draftcat/internal/channels"
	ghlapi "github.com/renezander030/draftcat/internal/ghl"
	gmailapi "github.com/renezander030/draftcat/internal/gmail"
	"github.com/renezander030/draftcat/internal/obs"
	"github.com/renezander030/draftcat/internal/pdf"
	"github.com/renezander030/draftcat/internal/redact"
	statestore "github.com/renezander030/draftcat/internal/state"
	"github.com/renezander030/draftcat/internal/voicebridge"
	"github.com/renezander030/draftcat/internal/whatsapp"
)

// --- Guardrails ---

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

func validateOperatorInput(text string, sec config.ChannelSecurity) InputValidationResult {
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
func validateChannelSecurity(cfg *config.Config) error {
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
	mu      sync.Mutex
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

// allow is called from every approval waiter and the command loop at once, so
// the window map is locked — an unlocked map written from two gates is a
// runtime fatal, not a wrong answer.
func (r *RateLimiter) allow(userID int64) bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.windows == nil {
		r.windows = make(map[int64][]time.Time)
	}
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

// --- LLM Provider (OpenAI-compatible chat completions; OpenRouter by default) ---

type CompletionRequest struct {
	Model     string
	Prompt    string
	MaxTokens int
}

type CompletionResponse struct {
	Text         string
	InputTokens  int
	OutputTokens int
	// CachedTokens and ReasoningTokens are the provider's breakdown when it
	// reports one (OpenRouter usage accounting); zero otherwise.
	CachedTokens    int
	ReasoningTokens int
	LatencyMs       int64
	CostUSD         float64
	// CostSource says where CostUSD came from: "provider" when the response
	// carried the real charge, "rates" when it was computed from the
	// configured per-1k prices.
	CostSource string
	Model      string
	// Attempts is how many HTTP requests the call took, so a run that limped
	// through rate limits is visible in the log rather than only slow.
	Attempts int
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

// llmRetryable identifies an explicit rate-limit rejection. Timeouts and server
// errors can follow a paid response, so they require reconciliation first.
func llmRetryable(status int) bool {
	return status == http.StatusTooManyRequests
}

// retryDelay is how long to wait before the given retry (1-based). A
// Retry-After header wins when present — that is the provider saying when it
// will accept the call — capped so a provider cannot park a run indefinitely.
// Otherwise exponential backoff with ±25% jitter, so parallel pipelines that
// hit one rate limit together do not all come back together.
func retryDelay(retry int, retryAfter string, now time.Time) time.Duration {
	const capAfter = 30 * time.Second
	if s := strings.TrimSpace(retryAfter); s != "" {
		if secs, err := strconv.Atoi(s); err == nil && secs >= 0 {
			d := time.Duration(secs) * time.Second
			if d > capAfter {
				d = capAfter
			}
			return d
		}
		if at, err := http.ParseTime(s); err == nil {
			d := at.Sub(now)
			if d < 0 {
				d = 0
			}
			if d > capAfter {
				d = capAfter
			}
			return d
		}
	}
	if retry < 1 {
		retry = 1
	}
	base := 500 * time.Millisecond
	for i := 0; i < retry && base < 10*time.Second; i++ {
		base *= 2 // 1s, 2s, 4s, 8s, 10s cap
	}
	if base > 10*time.Second {
		base = 10 * time.Second
	}
	jitter := time.Duration(rand.Int63n(int64(base)/2+1)) - base/4
	return base + jitter
}

func providerCallLLM(ctx context.Context, cfg *config.Config, role string, prompt string, maxTokens int) (*CompletionResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, providerError(err, false)
	}
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
		"max_tokens": maxTokens,
	}
	// OpenRouter reports the real charge (usage.cost) and the cached/reasoning
	// token breakdown when asked. Other OpenAI-compatible servers reject
	// unknown request fields, which is why this is a switch and not a default.
	usageAccounting := cfg.Provider.UsageAccountingOn()
	if usageAccounting {
		reqBody["usage"] = map[string]bool{"include": true}
	}
	body, _ := json.Marshal(reqBody)

	var (
		respBody   []byte
		latency    int64
		lastErr    error
		retryAfter string
		attempts   int
	)
	maxAttempts := cfg.Provider.RetriesOrDefault() + 1
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := retryDelay(attempt, retryAfter, time.Now())
			log.Printf("[llm] retry %d/%d in %s after: %v", attempt, maxAttempts-1, delay.Round(time.Millisecond), lastErr)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, providerError(fmt.Errorf("LLM call abandoned during backoff: %w", ctx.Err()), false)
			}
		}
		attempts++
		retryAfter = ""

		req, err := http.NewRequestWithContext(ctx, "POST", cfg.Provider.BaseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, providerError(err, false)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+cfg.Provider.APIKey())

		start := time.Now()
		resp, err := httpClient.Do(req)
		if err != nil {
			// Dispatch may have reached the provider. Repeating an ambiguous
			// request would spend again without accounting for the first one.
			return nil, providerError(fmt.Errorf("LLM request failed; usage may require reconciliation: %w", err), true)
		}
		latency = time.Since(start).Milliseconds()

		respBody, err = readProviderResponse(ctx, resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, providerError(err, true)
		}
		if resp.StatusCode == http.StatusOK {
			lastErr = nil
			break
		}
		lastErr = fmt.Errorf("LLM API error %d", resp.StatusCode)
		// Only an explicit rate-limit rejection is safe to retry. A timeout
		// or server error can follow a paid completion.
		if !llmRetryable(resp.StatusCode) || providerDeclaresUsage(respBody) {
			return nil, providerError(lastErr, providerDeclaresUsage(respBody) || resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500)
		}
		retryAfter = resp.Header.Get("Retry-After")
	}
	if lastErr != nil {
		return nil, providerError(fmt.Errorf("%w (gave up after %d attempt(s))", lastErr, attempts), false)
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        *int     `json:"prompt_tokens"`
			CompletionTokens    *int     `json:"completion_tokens"`
			Cost                *float64 `json:"cost"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, providerError(fmt.Errorf("failed to parse LLM response: %w", err), true)
	}
	if result.Usage.PromptTokens == nil || result.Usage.CompletionTokens == nil {
		return nil, providerError(fmt.Errorf("LLM response is missing token usage; reconciliation required"), true)
	}
	inputTokens, outputTokens := *result.Usage.PromptTokens, *result.Usage.CompletionTokens
	if inputTokens < 0 || outputTokens < 0 || inputTokens > int(^uint(0)>>1)-outputTokens ||
		result.Usage.PromptTokensDetails.CachedTokens < 0 || result.Usage.CompletionTokensDetails.ReasoningTokens < 0 ||
		(result.Usage.Cost != nil && !validCost(*result.Usage.Cost)) {
		return nil, providerError(fmt.Errorf("LLM response contains invalid usage; reconciliation required"), true)
	}

	// The configured rates are the fallback. When the provider states what it
	// actually charged, the caps are enforced on that number — rates alone
	// undercount reasoning tokens and overcount cached ones.
	cost := float64(inputTokens)/1000*modelCfg.CostIn +
		float64(outputTokens)/1000*modelCfg.CostOut
	source := "rates"
	if usageAccounting && result.Usage.Cost != nil {
		cost = *result.Usage.Cost
		source = "provider"
	}

	if !validCost(cost) {
		return nil, providerError(fmt.Errorf("LLM response cost is invalid; reconciliation required"), true)
	}
	completion := &CompletionResponse{
		InputTokens:     inputTokens,
		OutputTokens:    outputTokens,
		CachedTokens:    result.Usage.PromptTokensDetails.CachedTokens,
		ReasoningTokens: result.Usage.CompletionTokensDetails.ReasoningTokens,
		LatencyMs:       latency,
		CostUSD:         cost,
		CostSource:      source,
		Model:           result.Model,
		Attempts:        attempts,
	}
	if len(result.Choices) == 0 {
		return completion, fmt.Errorf("LLM returned no choices")
	}
	completion.Text = result.Choices[0].Message.Content
	return completion, nil
}

// --- Operator Channel Interface ---
// Each channel (TG, Slack, etc.) implements this interface.
// The engine doesn't know which channel it's talking to.

type OperatorChannel interface {
	// Name is the channel's config name (see internal/channels). The engine
	// compares it against a step's `channel:` so a step can never be routed to
	// a channel that is not the one actually running.
	Name() string
	// Send a plain notification (no approval needed)
	Send(text string) error
	// SendForApproval sends a draft for approval with action buttons and returns
	// the operator's decision. `approvers`, when non-empty, narrows who may
	// decide to that subset of the channel's allowed users; empty = anyone
	// allowed on the channel.
	SendForApproval(ctx context.Context, draft string, approvers []int64) (OperatorDecision, error)
	// SendForQuorumApproval blocks until `need` distinct permitted operators
	// approve, or any of them skips/adjusts, or ctx expires. need<=1 behaves
	// like SendForApproval (single approver). `approvers` narrows the permitted
	// set exactly as above.
	SendForQuorumApproval(ctx context.Context, draft string, need int, approvers []int64) (QuorumDecision, error)
}

type OperatorDecision struct {
	Action     string // "approve", "skip", "adjust"
	Text       string // adjustment text for "adjust"; the operator's reason for "skip" (may be empty)
	ApproverID int64  // user id that approved (only if Action == "approve"); 0 if unknown
	DeciderID  int64  // user id that skipped or adjusted; 0 if unknown
}

// QuorumDecision is the outcome of an N-of-M approval gate.
type QuorumDecision struct {
	Action    string  // "approve" | "skip" | "adjust" | "timeout"
	Text      string  // adjustment text (Action == "adjust"); skip reason (Action == "skip", may be empty)
	Approvers []int64 // distinct user IDs that approved (Action == "approve")
	DeciderID int64   // user id that skipped or adjusted; 0 if unknown
}

// --- Quorum decision logic (pure, testable without Telegram) ---

// approvalEvent is one operator action in arrival order.
type approvalEvent struct {
	userID int64
	action string // "approve" | "skip" | "adjust"
}

// quorumReducer replays operator events against an N-of-M gate and returns the
// terminal decision, or Action=="pending" if the events neither reach quorum
// nor trip a veto. Rules (mirror the live Telegram poll loop):
//   - events from users not in `allowed` are ignored;
//   - any single Skip is an immediate veto (easy to stop, hard to release);
//   - Adjust takes the floor immediately (the rewrite re-enters the gate at 0/N);
//   - Approve adds a DISTINCT user; the same user twice counts once;
//   - reaching `need` distinct approvers finalizes approve. need<=1 = single approver.
func quorumReducer(events []approvalEvent, need int, allowed map[int64]bool) QuorumDecision {
	if need < 1 {
		need = 1
	}
	approved := map[int64]bool{}
	for _, e := range events {
		if !allowed[e.userID] {
			continue
		}
		switch e.action {
		case "skip":
			return QuorumDecision{Action: "skip"}
		case "adjust":
			return QuorumDecision{Action: "adjust"}
		case "approve":
			approved[e.userID] = true
			if len(approved) >= need {
				return QuorumDecision{Action: "approve", Approvers: sortedIDs(approved)}
			}
		}
	}
	return QuorumDecision{Action: "pending"}
}

func sortedIDs(m map[int64]bool) []int64 {
	out := make([]int64, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// --- Telegram Channel ---

// skipReasonWait bounds how long a gate waits for the reason after an
// operator chose "Skip with reason". The skip stands either way.
var skipReasonWait = 10 * time.Minute

// approvalButtons is the keyboard on every approval message.
func approvalButtons() [][]map[string]string {
	return [][]map[string]string{
		{
			{"text": "Approve", "callback_data": "approve"},
			{"text": "Skip", "callback_data": "skip"},
			{"text": "Adjust...", "callback_data": "adjust"},
		},
		{
			{"text": "Skip with reason...", "callback_data": "skip_reason"},
		},
	}
}

type TGBot struct {
	token       string
	chatID      int64
	offset      int
	security    config.ChannelSecurity
	rateLimiter *RateLimiter

	// api and fetch are the network seams; nil means the real Bot API. Tests
	// script both so an approval round trip runs without Telegram.
	api   func(method string, payload map[string]interface{}) (json.RawMessage, error)
	fetch func() ([]TGUpdate, error)

	// The single update pump — see telegram_pump.go. Only it calls getUpdates.
	pumpOnce sync.Once
	updates  *tgPump
}

// tgClient — dedicated HTTP client for Telegram API with timeouts
var tgClient = &http.Client{Timeout: 10 * time.Second}

func (t *TGBot) apiCall(method string, payload map[string]interface{}) (json.RawMessage, error) {
	if t.api != nil {
		return t.api(method, payload)
	}
	body, _ := json.Marshal(payload)
	resp, err := tgClient.Post(
		fmt.Sprintf("https://api.telegram.org/bot%s/%s", t.token, method),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		// The request URL embeds the bot token; transport errors quote it.
		return nil, redactedError{err}
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
		"text":    redact.String(text),
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

// Name identifies this channel to the engine's step routing check.
func (t *TGBot) Name() string { return channels.Telegram }

// SendForApproval posts a draft with Approve/Skip/Adjust buttons.
// Waits for the operator to click a button or send a text reply for adjustment.
//
// Updates reach this waiter through the bot's single update pump (see
// telegram_pump.go): it registers for its own message id and receives exactly
// the callbacks aimed at that message, plus adjustment text once it asked for
// it. It never calls getUpdates itself — two pollers on one Bot API stream
// each consume the other's taps.
func (t *TGBot) SendForApproval(ctx context.Context, draft string, approvers []int64) (OperatorDecision, error) {
	buttons := approvalButtons()

	msgID, err := t.sendWithButtons(draft, buttons)
	if err != nil {
		return OperatorDecision{}, fmt.Errorf("failed to send draft: %w", err)
	}
	log.Printf("[telegram] draft posted (msg_id=%d), waiting for operator action...", msgID)

	w := t.pump().register(msgID)
	defer t.pump().release(msgID)

	// textFor is what a text reply means once a button asked for one:
	// "adjust" (rewrite feedback) or "skip" (the reason for skipping).
	textFor := ""
	var textFrom int64
	var reasonDeadline <-chan time.Time

	for {
		select {
		case <-reasonDeadline:
			t.editButtons(msgID, draft+"\n\n[Skipped]", nil)
			return OperatorDecision{Action: "skip", DeciderID: textFrom}, nil
		case <-ctx.Done():
			if textFor == "skip" {
				t.editButtons(msgID, draft+"\n\n[Skipped]", nil)
				return OperatorDecision{Action: "skip", DeciderID: textFrom}, nil
			}
			t.editButtons(msgID, draft+"\n\n[Timed out]", nil)
			return OperatorDecision{}, fmt.Errorf("approval timeout")
		case u := <-w.ch:
			// Button click on our message
			if u.CallbackQuery != nil {
				cb := u.CallbackQuery
				// Security: verify user is permitted to decide THIS step
				if !t.isApprover(cb.From.ID, approvers) {
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
					return OperatorDecision{Action: "approve", ApproverID: cb.From.ID}, nil
				case "skip":
					t.answerCallback(cb.ID, "Skipped")
					t.editButtons(msgID, draft+"\n\n[Skipped]", nil)
					return OperatorDecision{Action: "skip", DeciderID: cb.From.ID}, nil
				case "skip_reason":
					t.answerCallback(cb.ID, "Send the reason as a text message")
					// The skip is decided now; only the reason is outstanding.
					textFor, textFrom = "skip", cb.From.ID
					reasonDeadline = time.After(skipReasonWait)
					w.wantText(true)
					t.editButtons(msgID, draft+"\n\n[Skipping — waiting for the reason...]", nil)
				case "adjust":
					t.answerCallback(cb.ID, "Send your adjustment as a text message")
					textFor, textFrom = "adjust", cb.From.ID
					w.wantText(true)
					t.editButtons(msgID, draft+"\n\n[Waiting for adjustment text...]", nil)
				}
				continue
			}

			// Adjustment text, routed here only after Adjust was tapped
			if u.Message != nil && w.wantsText() && u.Message.Text != "" {
				// Security checks
				if u.Message.Chat.ID != t.chatID {
					continue
				}
				if !t.isApprover(u.Message.From.ID, approvers) {
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

				if textFor == "skip" {
					if u.Message.From.ID != textFrom {
						continue // the reason comes from the operator who chose to skip
					}
					t.editButtons(msgID, draft+"\n\n[Skipped: "+result.Text+"]", nil)
					return OperatorDecision{Action: "skip", Text: result.Text, DeciderID: textFrom}, nil
				}
				return OperatorDecision{Action: "adjust", Text: result.Text, DeciderID: u.Message.From.ID}, nil
			}
		}
	}
}

// SendForQuorumApproval posts a draft that requires `need` distinct allowed
// operators to approve. It mirrors SendForApproval's security posture
// (allowed-user check, rate limiting, message-ID match, input validation) and
// adds a live tally. Any single Skip vetoes immediately; Adjust takes the floor
// and returns so the caller can rewrite and re-enter the gate.
func (t *TGBot) SendForQuorumApproval(ctx context.Context, draft string, need int, approvers []int64) (QuorumDecision, error) {
	if need <= 1 {
		// Single-approver semantics — delegate so behavior is byte-for-byte today's.
		d, err := t.SendForApproval(ctx, draft, approvers)
		if err != nil {
			return QuorumDecision{Action: "timeout"}, err
		}
		qd := QuorumDecision{Action: d.Action, Text: d.Text, DeciderID: d.DeciderID}
		if d.Action == "approve" && d.ApproverID != 0 {
			qd.Approvers = []int64{d.ApproverID}
		}
		return qd, nil
	}

	buttons := approvalButtons()
	tally := func(got int) string {
		return fmt.Sprintf("%s\n\nApprovals: %d/%d", draft, got, need)
	}

	msgID, err := t.sendWithButtons(tally(0), buttons)
	if err != nil {
		return QuorumDecision{Action: "timeout"}, fmt.Errorf("failed to send draft: %w", err)
	}
	log.Printf("[telegram] quorum draft posted (msg_id=%d), need=%d distinct approvers...", msgID, need)

	w := t.pump().register(msgID)
	defer t.pump().release(msgID)

	approved := map[int64]bool{}
	// vetoBy is set once someone chose "Skip with reason": the gate is
	// decided and only that operator's reason is awaited.
	var vetoBy int64
	var reasonDeadline <-chan time.Time

	for {
		select {
		case <-reasonDeadline:
			t.editButtons(msgID, draft+"\n\n[Skipped]", nil)
			return QuorumDecision{Action: "skip", DeciderID: vetoBy}, nil
		case <-ctx.Done():
			if vetoBy != 0 {
				t.editButtons(msgID, draft+"\n\n[Skipped]", nil)
				return QuorumDecision{Action: "skip", DeciderID: vetoBy}, nil
			}
			t.editButtons(msgID, fmt.Sprintf("%s\n\n[Timed out — %d/%d approvals not reached]", draft, len(approved), need), nil)
			return QuorumDecision{Action: "timeout"}, fmt.Errorf("approval timeout")
		case u := <-w.ch:
			if vetoBy != 0 {
				if m := u.Message; m != nil && m.Text != "" && m.Chat.ID == t.chatID && m.From.ID == vetoBy && t.rateLimiter.allow(m.From.ID) {
					if result := validateOperatorInput(m.Text, t.security); result.Clean {
						t.editButtons(msgID, draft+"\n\n[Skipped: "+result.Text+"]", nil)
						return QuorumDecision{Action: "skip", Text: result.Text, DeciderID: vetoBy}, nil
					}
				}
				continue
			}
			if u.CallbackQuery == nil {
				continue
			}
			cb := u.CallbackQuery
			if !t.isApprover(cb.From.ID, approvers) {
				t.answerCallback(cb.ID, "") // silent drop
				log.Printf("[security] REJECTED callback from user %d", cb.From.ID)
				continue
			}
			if !t.rateLimiter.allow(cb.From.ID) {
				t.answerCallback(cb.ID, "") // silent drop
				continue
			}
			if cb.Message.MessageID != msgID {
				continue
			}

			switch cb.Data {
			case "skip":
				// A single veto stops the action. Easy to stop, hard to release.
				t.answerCallback(cb.ID, "Skipped (veto)")
				t.editButtons(msgID, draft+"\n\n[Skipped]", nil)
				return QuorumDecision{Action: "skip", DeciderID: cb.From.ID}, nil
			case "skip_reason":
				// The veto takes effect now; no approval can land after it.
				t.answerCallback(cb.ID, "Skipped (veto) — send the reason as a text message")
				vetoBy = cb.From.ID
				reasonDeadline = time.After(skipReasonWait)
				w.wantText(true)
				t.editButtons(msgID, draft+"\n\n[Skipping — waiting for the reason...]", nil)
			case "adjust":
				t.answerCallback(cb.ID, "Send your adjustment as a text message")
				t.editButtons(msgID, draft+"\n\n[Adjustment requested — rewrite will re-enter the gate at 0/"+strconv.Itoa(need)+"]", nil)
				return QuorumDecision{Action: "adjust", DeciderID: cb.From.ID}, nil
			case "approve":
				if approved[cb.From.ID] {
					t.answerCallback(cb.ID, "Already counted")
					continue
				}
				approved[cb.From.ID] = true
				got := len(approved)
				if got >= need {
					t.answerCallback(cb.ID, "Approved — quorum reached")
					t.editButtons(msgID, fmt.Sprintf("%s\n\n[Approved %d/%d]", draft, got, need), nil)
					return QuorumDecision{Action: "approve", Approvers: sortedIDs(approved)}, nil
				}
				t.answerCallback(cb.ID, fmt.Sprintf("Approved (%d/%d)", got, need))
				t.editButtons(msgID, tally(got), buttons)
			}
		}
	}
}

func (t *TGBot) isAllowedUser(userID int64) bool {
	return t.isApprover(userID, nil)
}

// isApprover reports whether userID may decide a gate whose step narrowed the
// approver set to `approvers`. The step list is an INTERSECTION with the
// channel's allowed_users, never a widening: a step cannot grant approval
// rights to someone the channel does not already trust, so a typo'd id in a
// step's approvers list fails closed instead of opening the gate. Empty
// approvers = anyone on allowed_users (today's behavior).
func (t *TGBot) isApprover(userID int64, approvers []int64) bool {
	if !containsID(t.security.AllowedUsers, userID) {
		return false
	}
	if len(approvers) == 0 {
		return true
	}
	return containsID(approvers, userID)
}

func containsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
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
		// The request URL embeds the bot token; transport errors quote it.
		return nil, redactedError{err}
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

func runPipeline(cfg *config.Config, pipeline config.PipelineConfig, budget *BudgetTracker, ch OperatorChannel, skills *skillsapi.SkillRegistry, seed map[string]interface{}) (err error) {
	// One identity for this run, minted before any work happens so every
	// approval and audit row produced below can be joined back to it.
	startedAt := time.Now()
	runID := newRunID(startedAt)
	runStore := state
	defer func() {
		if runStore != nil {
			if recordErr := runStore.RecordRunForID(runID, pipeline.Name, startedAt, time.Now(), redactErr(err)); recordErr != nil {
				log.Printf("[pipeline:%s] record run: %v", pipeline.Name, recordErr)
			}
		}
	}()
	log.Printf("[pipeline:%s] starting (run %s)", pipeline.Name, runID)
	budget = budget.newRun(cfg.Budgets.PerPipelineTokens)

	// Observability: one pipeline span (always) + one span per step. Steps that
	// run to completion emit at the bottom of the loop; a step that halts the
	// pipeline (skip or error) never reaches there, so the deferred block emits
	// its terminal span. No-op unless tracing is enabled. See internal/obs.
	pipeSpan := obs.Pipeline(pipeline.Name)
	var (
		stepsCompleted int
		lastStep       string
		lastKind       string
		lastStepStart  time.Time
		allDone        bool
	)
	defer func() {
		status := "ok"
		fields := map[string]interface{}{
			"steps_completed": stepsCompleted,
			"tokens":          budget.snapshot().tokensUsedPipeline,
		}
		if err != nil {
			status = "error"
			fields["error"] = err.Error()
		}
		if !allDone && lastStep != "" {
			stStatus := "skip"
			stFields := map[string]interface{}{}
			if err != nil {
				stStatus = "error"
				stFields["error"] = err.Error()
			}
			obs.EmitStep(pipeline.Name, lastStep, lastKind, lastStepStart, stStatus, stFields)
			fields["halted_at"] = lastStep
		}
		pipeSpan.End(status, fields)
	}()

	// Parse pipeline timeout
	pipelineTimeout, _ := time.ParseDuration(cfg.Timeouts.PipelineTotal)
	if pipelineTimeout == 0 {
		pipelineTimeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(withRun(context.Background(), runID, pipeline.Name), pipelineTimeout)
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

	// Seed values (e.g. a webhook request body) override/extend the defaults so
	// the first step can reference {{webhook_body}} or {{input}}.
	for k, v := range seed {
		data[k] = v
	}

	for _, step := range pipeline.Steps {
		lastStep = step.Name
		lastKind = step.Type
		lastStepStart = time.Now()
		stepFields := map[string]interface{}{}

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
			case "whatsapp_intake":
				raw, _ := data["webhook_body"].(string)
				if raw == "" {
					raw, _ = data["input"].(string)
				}
				if raw == "" {
					return fmt.Errorf("[step:%s] whatsapp_intake requires webhook_body or input JSON", step.Name)
				}
				msg, err := whatsapp.ParseWebhook(raw)
				if err != nil {
					return fmt.Errorf("[step:%s] %w", step.Name, err)
				}
				data["whatsapp_message"] = msg
				data["whatsapp_from"] = msg.From
				data["whatsapp_text"] = msg.Text
				data["input"] = whatsapp.FormatForPrompt(msg)
				log.Printf("[pipeline:%s][step:%s] accepted whatsapp message from %s", pipeline.Name, step.Name, msg.From)

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
				emails = statestore.DedupByID(state, pipeline.Name, "gmail", emails,
					func(e gmailapi.Email) string { return e.ID })
				if len(emails) == 0 {
					log.Printf("[pipeline:%s][step:%s] all unread emails already processed, skipping pipeline", pipeline.Name, step.Name)
					return nil
				}
				data["emails"] = gmailapi.FormatEmailsForPrompt(emails)
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
				contacts = statestore.DedupByID(state, pipeline.Name, "ghl_contacts", contacts,
					func(c ghlapi.GHLContact) string { return c.ID })
				if len(contacts) == 0 {
					log.Printf("[pipeline:%s][step:%s] all recent contacts already processed, skipping pipeline", pipeline.Name, step.Name)
					return nil
				}
				data["contacts"] = ghlapi.FormatContactsForPrompt(contacts)
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
				data["opportunities"] = ghlapi.FormatOpportunitiesForPrompt(opps)
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
				convos = statestore.DedupByID(state, pipeline.Name, "ghl_conversations", convos,
					func(c ghlapi.GHLConversation) string { return c.ID + "|" + c.LastDate })
				if len(convos) == 0 {
					log.Printf("[pipeline:%s][step:%s] all unread conversations already processed, skipping pipeline", pipeline.Name, step.Name)
					return nil
				}
				data["conversations"] = ghlapi.FormatConversationsForPrompt(convos)
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
				data["pdf_text"] = pdf.FormatPDFForPrompt(doc)
				data["pdf_page_count"] = fmt.Sprintf("%d", len(doc.Pages))
				log.Printf("[pipeline:%s][step:%s] parsed %s: %d pages", pipeline.Name, step.Name, doc.Filename, len(doc.Pages))

			case "pdf_verify_cite":
				doc, ok := data["pdf_doc"].(*pdf.PDFDoc)
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
				// Voice plugin actions (no-op in lean builds)
				if handled, skip, err := vbridge.TryAction(step.Action, pipeline.Name, step.Vars, data); handled {
					if err != nil {
						return fmt.Errorf("[step:%s] %w", step.Name, err)
					}
					if skip {
						log.Printf("[pipeline:%s][step:%s] %s: no items, skipping pipeline", pipeline.Name, step.Name, step.Action)
						return nil
					}
				}
				// else pass-through
			}

		case "ai":

			// Resolve skill or use inline prompt
			prompt := step.Prompt
			role := step.Role
			schema := step.OutputSchema

			if step.Skill != "" {
				if skills == nil {
					return fmt.Errorf("[step:%s] skill registry unavailable: %s", step.Name, step.Skill)
				}
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

			resp, err := callLLM(aiCtx, cfg, role, prompt, budget)
			aiCancel()
			if err != nil {
				return fmt.Errorf("[step:%s] LLM call failed: %w", step.Name, err)
			}

			// Record token usage
			log.Printf("[pipeline:%s][step:%s] model=%s tokens=%d+%d cost=$%.4f latency=%dms",
				pipeline.Name, step.Name, resp.Model,
				resp.InputTokens, resp.OutputTokens, resp.CostUSD, resp.LatencyMs)
			stepFields["model"] = resp.Model
			stepFields["tokens"] = resp.InputTokens + resp.OutputTokens
			stepFields["cost_usd"] = resp.CostUSD

			// Validate output
			if len(schema) > 0 {
				parsed, err := validateOutput(resp.Text, schema)
				if err != nil {
					log.Printf("[pipeline:%s][step:%s] OUTPUT_INVALID: %s", pipeline.Name, step.Name, err)
					return fmt.Errorf("[step:%s] output validation failed: %w", step.Name, err)
				}
				data["ai_output"] = parsed
				log.Printf("[pipeline:%s][step:%s] output validated (%d fields)", pipeline.Name, step.Name, len(parsed))
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
				score := outputScore(v["score"])
				reason, _ := v["reason"].(string)
				reject, _ := v["reject"].(bool)
				status := "MATCH"
				if reject {
					status = "REJECT"
				}
				draftMsg = fmt.Sprintf("[draftcat] %s - Score: %s/5\n\n%s\n\n%v",
					status, score, reason, data["input"])
			default:
				draftMsg = fmt.Sprintf("[draftcat] Draft for review:\n\n%v", v)
			}

			// Send for approval via operator channel (TG buttons, Slack reactions, etc.)
			approvalTimeout, _ := time.ParseDuration(cfg.Timeouts.OperatorApproval)
			if approvalTimeout == 0 {
				approvalTimeout = 4 * time.Hour
			}

			// quorumN is the required distinct-approver count (>=1).
			quorumN := step.Quorum
			if quorumN < 1 {
				quorumN = 1
			}

			// A step may name the channel it wants. Only the running channel can
			// serve it, so a mismatch is a hard error rather than a silent
			// reroute to whatever channel happens to be up. `draftcat validate`
			// catches this before boot; this is the runtime backstop.
			if step.Channel != "" && step.Channel != ch.Name() {
				return fmt.Errorf("[step:%s] channel %q is not the running operator channel (%q) — approvals cannot be routed there",
					step.Name, step.Channel, ch.Name())
			}

			// getApproval dispatches to the quorum gate when quorum >= 2, else the
			// single-approver gate (byte-for-byte today's behavior). It normalises
			// both into (action, adjustText, approvers, err).
			var decider int64 // who skipped or adjusted, when the channel knows
			getApproval := func(cctx context.Context, draft string) (string, string, []int64, error) {
				decider = 0
				if step.Quorum >= 2 {
					qd, qerr := ch.SendForQuorumApproval(cctx, draft, step.Quorum, step.Approvers)
					if qerr != nil {
						return "timeout", "", nil, qerr
					}
					decider = qd.DeciderID
					return qd.Action, qd.Text, qd.Approvers, nil
				}
				dec, derr := ch.SendForApproval(cctx, draft, step.Approvers)
				if derr != nil {
					return "timeout", "", nil, derr
				}
				var aps []int64
				if dec.Action == "approve" && dec.ApproverID != 0 {
					aps = []int64{dec.ApproverID}
				}
				decider = dec.DeciderID
				return dec.Action, dec.Text, aps, nil
			}

			// approvalSecret signs the audit rows into tamper-evident receipts.
			// Unset (empty) → rows are recorded unsigned, exactly as before, so
			// this stays backward compatible on instances that don't opt in.
			approvalSecret := []byte(os.Getenv("DRAFTCAT_APPROVAL_SECRET"))

			// recordAudit appends one append-only approval row (who/what/when) and
			// bumps the Prometheus approval counter. The audit table is always on
			// once a state store exists, regardless of exporter settings. It stores
			// only the sha256 of the exact draft shown — never the draft itself.
			// When a signing secret is set, each row also carries an HMAC receipt
			// so a later reader can prove the row wasn't altered after the fact.
			recordAudit := func(decision, payload string, approvers []int64) error {
				obs.RecordApproval(pipeline.Name, step.Name, decision)
				if state == nil {
					return nil
				}
				var opID int64
				got := 0
				if len(approvers) > 0 {
					opID = approvers[0]
					got = len(approvers)
				} else if decision == "approve" {
					got = quorumN
				}
				decidedAt := time.Now()
				sum := sha256.Sum256([]byte(payload))
				payloadHash := hex.EncodeToString(sum[:])

				envelope := newApprovalEnvelope(approvalSecret, runID, pipeline.Name, step,
					decidedAt, decidedAt.Add(approvalTimeout), decision, opID, payloadHash,
					quorumN, got, "human-approval")
				return persistApprovalReceipt(envelope)
			}

			// maxAdjust bounds rewrite cycles: single-approver keeps today's single
			// rewrite; quorum allows up to 3 (each rewrite re-enters the gate at 0/N).
			maxAdjust := 1
			if step.Quorum >= 2 {
				maxAdjust = 3
			}

			currentDraft := draftMsg

			// Policy tier. An operator can decide IN ADVANCE that a declared class
			// of action does not need a fresh tap, because a gate people switch off
			// protects nothing: the observed workaround for approval fatigue is
			// disabling approval wholesale, which trades a narrow audited exemption
			// for a total one. High-risk steps are never eligible (enforced inside
			// AutoApproveRule.Match so no caller can route around it), and every
			// exemption is written to the audit trail as "policy_approve" with the
			// rule that fired, so the trail never conflates a policy release with a
			// human decision.
			if rule := cfg.Policy.AutoApproves(pipeline.Name, step, budget.snapshot().costPipeline); rule != nil {
				sum := sha256.Sum256([]byte(currentDraft))
				ph := hex.EncodeToString(sum[:])
				reason := rule.Reason
				if reason == "" {
					reason = fmt.Sprintf("risk=%s", step.RiskOf())
				}
				log.Printf("[pipeline:%s][step:%s] released by approval_policy (%s) — no operator prompt", pipeline.Name, step.Name, reason)
				obs.RecordApproval(pipeline.Name, step.Name, "policy_approve")
				if state != nil {
					decidedAt := time.Now()
					envelope := newApprovalEnvelope(approvalSecret, runID, pipeline.Name, step,
						decidedAt, decidedAt.Add(approvalTimeout), "policy_approve", 0, ph,
						quorumN, quorumN, reason)
					if e := persistApprovalReceipt(envelope); e != nil {
						return fmt.Errorf("[step:%s] approval receipt unavailable: %w", step.Name, e)
					}
				}
				data["approved"] = true
				break
			}

			adjustCycles := 0
			for {
				// Open the gate durably BEFORE the draft leaves for the operator.
				// If the process dies in the window between sending and deciding,
				// the next boot reconciles this row instead of the gate vanishing
				// without a trace (see reconcileInterruptedApprovals).
				openedAt := time.Now()
				pendingSum := sha256.Sum256([]byte(currentDraft))
				pendingID, perr := state.BeginApproval(pipeline.Name, step.Name,
					hex.EncodeToString(pendingSum[:]), quorumN, openedAt, openedAt.Add(approvalTimeout))
				if perr != nil {
					return fmt.Errorf("[step:%s] pending approval unavailable: %w", step.Name, perr)
				}

				// withStep puts the step name on the context so a channel that
				// dispatches off-process (the relay) can name the gate it is
				// asking about. In-binary channels ignore it.
				approvalCtx, approvalCancel := context.WithTimeout(withStep(ctx, step.Name), approvalTimeout)
				action, adjustText, approvers, aerr := getApproval(approvalCtx, currentDraft)
				approvalCancel()

				// The gate reached a terminal state in-process, whatever it was —
				// close the pending row so boot does not later call it interrupted.
				if rerr := state.ResolveApproval(pendingID); rerr != nil {
					return fmt.Errorf("[step:%s] approval settlement unavailable: %w", step.Name, rerr)
				}

				if aerr != nil {
					if auditErr := recordAudit("timeout", currentDraft, nil); auditErr != nil {
						return fmt.Errorf("[step:%s] approval receipt unavailable: %w", step.Name, auditErr)
					}
					return fmt.Errorf("[step:%s] %w", step.Name, aerr)
				}

				log.Printf("[pipeline:%s][step:%s] operator decision: %s", pipeline.Name, step.Name, action)

				if action == "approve" {
					if auditErr := recordAudit("approve", currentDraft, approvers); auditErr != nil {
						return fmt.Errorf("[step:%s] approval receipt unavailable: %w", step.Name, auditErr)
					}
					data["approved"] = true
					break
				}
				if action == "skip" {
					if auditErr := recordSkip(approvalSecret, runID, pipeline.Name, step, currentDraft, approvalTimeout, quorumN, decider, adjustText); auditErr != nil {
						return fmt.Errorf("[step:%s] approval receipt unavailable: %w", step.Name, auditErr)
					}
					data["approved"] = false
					return nil
				}
				if action != "adjust" {
					// e.g. a quorum "timeout" returned without an error
					if auditErr := recordAudit("timeout", currentDraft, nil); auditErr != nil {
						return fmt.Errorf("[step:%s] approval receipt unavailable: %w", step.Name, auditErr)
					}
					return fmt.Errorf("[step:%s] approval not completed (%s)", step.Name, action)
				}

				// action == "adjust"
				if auditErr := recordAudit("adjust", currentDraft, nil); auditErr != nil {
					return fmt.Errorf("[step:%s] approval receipt unavailable: %w", step.Name, auditErr)
				}
				adjustCycles++
				log.Printf("[pipeline:%s][step:%s] adjustment: %q", pipeline.Name, step.Name, adjustText)
				if adjustCycles > maxAdjust {
					if step.Quorum >= 2 {
						_ = ch.Send(fmt.Sprintf("Adjustment limit (%d) reached without quorum approval — halting.", maxAdjust))
						if auditErr := recordAudit("quorum_fail", currentDraft, nil); auditErr != nil {
							return fmt.Errorf("[step:%s] approval receipt unavailable: %w", step.Name, auditErr)
						}
					}
					data["approved"] = false
					return nil
				}

				adjustPrompt := fmt.Sprintf("Original output:\n%s\n\nOperator feedback:\n%s\n\nRewrite incorporating the feedback. Respond with ONLY valid JSON in the same format.", data["ai_raw"], adjustText)

				aiTimeout, _ := time.ParseDuration(cfg.Timeouts.AICall)
				if aiTimeout == 0 {
					aiTimeout = 30 * time.Second
				}
				aiCtx, aiCancel := context.WithTimeout(ctx, aiTimeout)
				resp, rerr := callLLM(aiCtx, cfg, "drafter", adjustPrompt, budget)
				aiCancel()
				if rerr != nil {
					_ = ch.Send(fmt.Sprintf("Rewrite failed: %s", rerr))
					return rerr
				}

				// Revised draft re-enters the gate (at 0/N for quorum steps).
				currentDraft = fmt.Sprintf("[draftcat] Revised:\n\n%s", resp.Text)
			}
		}

		// Step ran to completion (did not halt the pipeline).
		if step.Type == "deterministic" && step.Action != "" {
			stepFields["action"] = step.Action
		}
		obs.EmitStep(pipeline.Name, step.Name, step.Type, lastStepStart, "ok", stepFields)
		stepsCompleted++
	}
	allDone = true

	log.Printf("[pipeline:%s] completed. tokens_used=%d", pipeline.Name, budget.snapshot().tokensUsedPipeline)
	return nil
}

// reconcileInterruptedApprovals closes out approval gates that were open when
// the process last stopped.
//
// The gate used to live only in an in-process poll loop, so a redeploy or crash
// mid-approval left the operator looking at a live-looking message with buttons
// attached to a run that no longer existed — and, worse, left no record at all:
// nothing was written until a decision arrived, so the compliance queries could
// not see the hole either. Now every gate is written down when it opens, and
// anything still pending at boot is resolved here to `interrupted`: an audit
// row (so UnapprovedActions counts it) plus a message telling the operator the
// action did NOT run and the buttons on the old message are dead.
//
// Failing closed is the whole point. A gate whose outcome is unknown must never
// be treated as an approval.
func reconcileInterruptedApprovals(st *statestore.StateStore, ch OperatorChannel) {
	pending, err := st.InterruptedApprovals()
	if err != nil {
		log.Printf("[approval] could not read pending approvals: %v", err)
		return
	}
	if len(pending) == 0 {
		return
	}

	secret := []byte(os.Getenv("DRAFTCAT_APPROVAL_SECRET"))
	log.Printf("[approval] %d approval gate(s) were open at last shutdown — marking interrupted", len(pending))

	for _, p := range pending {
		decidedAt := time.Now()
		var nonce, sig string
		if len(secret) > 0 {
			if n, nerr := approval.NewNonce(); nerr == nil {
				nonce = n
				sig = approval.Sign(secret, approval.Fields{
					Pipeline: p.Pipeline, Step: p.Step, DecidedAt: decidedAt.Unix(),
					Decision: "interrupted", OperatorID: 0, PayloadHash: p.PayloadHash,
					QuorumN: p.QuorumN, QuorumGot: 0,
				}, nonce)
			} else {
				log.Printf("[approval] nonce failed for %s/%s, recording unsigned: %v", p.Pipeline, p.Step, nerr)
			}
		}
		if e := st.RecordApproval(p.Pipeline, p.Step, decidedAt, "interrupted", 0,
			p.PayloadHash, p.QuorumN, 0, nonce, sig); e != nil {
			log.Printf("[approval] audit write failed for %s/%s: %v", p.Pipeline, p.Step, e)
		}
		if e := st.MarkInterrupted(p.ID); e != nil {
			log.Printf("[approval] could not mark %d interrupted: %v", p.ID, e)
		}
		obs.RecordApproval(p.Pipeline, p.Step, "interrupted")
		log.Printf("[approval] interrupted: pipeline=%s step=%s opened=%s",
			p.Pipeline, p.Step, p.OpenedAt.Format(time.RFC3339))
	}

	if ch != nil {
		var b strings.Builder
		fmt.Fprintf(&b, "[draftcat] %d approval(s) were interrupted by a restart. The action was NOT taken.\n",
			len(pending))
		for _, p := range pending {
			fmt.Fprintf(&b, "\n- %s / %s (opened %s)", p.Pipeline, p.Step, p.OpenedAt.Format("2006-01-02 15:04"))
		}
		b.WriteString("\n\nButtons on those older messages no longer do anything. Re-run the pipeline if the action is still wanted.")
		if e := ch.Send(b.String()); e != nil {
			log.Printf("[approval] could not notify operator of interrupted approvals: %v", e)
		}
	}
}

// --- Command Handler ---
// Handles operator commands like /cron, /skills, /run, /status

var gmail *gmailapi.GmailConnector // initialized in main if configured
var ghl *ghlapi.GHLConnector       // initialized in main if configured
var pdfParser *pdf.PDFParser       // initialized unconditionally in main (no config required)
var state *statestore.StateStore   // SQLite-backed state store; opened in main, closed on shutdown
var vbridge *voicebridge.Bridge    // voice plugin (nil when disabled or built lean)

// opChan is the ONE operator channel approval gates route to this process.
// draftcat has always run a single operator channel — the approval step refuses
// a step whose `channel:` is not the running one, rather than silently
// rerouting — and the relay does not change that. It is set at boot to the
// relay when one is configured, otherwise the Telegram bot.
var opChan OperatorChannel
var lastEmails []gmailapi.Email // last fetched emails for /reply reference
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

func handleCommand(cmd string, args string, bot *TGBot, sched *Scheduler, skills *skillsapi.SkillRegistry, cfg *config.Config, budget *BudgetTracker) {
	switch cmd {
	case "/cron":
		handleCron(args, bot, sched)
	case "/skills":
		handleSkills(bot, skills)
	case "/run":
		handleRun(args, bot, sched, cfg, budget, skills)
	case "/status":
		handleStatus(bot, budget, sched, cfg)
	case "/pending":
		handlePending(bot)
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
	case "/reload":
		if reloadHook != nil {
			_ = bot.Send(reloadHook())
		}
	case "/help":
		_ = bot.Send("Commands:\n/emails [query] - Check emails\n/reply <number> [text] - Reply to an email\n/cron - Manage pipeline schedules\n/skills - List skills\n/run <pipeline> - Run a pipeline now\n/status - Engine status, spend against caps, open gates\n/pending - Approval gates waiting on you\n/reload - Re-read config.yaml and skills/")
	}
}

func handleEmails(args string, bot *TGBot, cfg *config.Config, budget *BudgetTracker) {
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
			var filtered []gmailapi.Email
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
	formatted := gmailapi.FormatEmailsForPrompt(emails)
	header := fmt.Sprintf("[emails] %d result(s) for: %s\n\n", len(emails), query)

	// If short enough, send directly. Otherwise summarize with LLM.
	if len(formatted) < 3000 {
		bot.Send(header + formatted)
	} else {
		// Use LLM to summarize
		bot.sendTyping()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		resp, err := callLLM(ctx, cfg, "classifier", fmt.Sprintf(
			"Summarize these emails in a brief list. For each: sender, subject, 1-line summary. Be concise.\n\n%s", formatted), budget)
		cancel()
		if err != nil {
			bot.Send(header + formatted[:2000] + "\n\n[truncated]")
		} else {
			bot.Send(header + resp.Text)
		}
	}
}

func handleReply(args string, bot *TGBot, cfg *config.Config, budget *BudgetTracker) {
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

	replyTo := gmailapi.ExtractEmailAddress(fullEmail.From)
	subject := fullEmail.Subject

	var replyBody string

	if len(parts) > 1 && parts[1] != "" {
		// User provided reply text directly
		replyBody = parts[1]
	} else {
		// AI drafts a reply
		bot.sendTyping()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		prompt := fmt.Sprintf(`Draft a brief, professional reply to this email. Just the reply body, no subject line or headers.

From: %s
Subject: %s
Body:
%s`, fullEmail.From, fullEmail.Subject, fullEmail.Body)
		resp, err := callLLM(ctx, cfg, "drafter", prompt, budget)
		cancel()
		if err != nil {
			bot.Send(fmt.Sprintf("[reply] Draft failed: %s", err))
			return
		}
		replyBody = resp.Text
	}

	// Show draft for HITL approval
	draft := fmt.Sprintf("[reply] Draft reply to: %s\nSubject: Re: %s\n\n%s", replyTo, subject, replyBody)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Hour)
	// Ad-hoc operator command, not a pipeline step — no per-step approver
	// narrowing applies, so any allowed user may decide.
	decision, err := bot.SendForApproval(ctx, draft, nil)
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

func handleThread(args string, bot *TGBot, cfg *config.Config, budget *BudgetTracker) {
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

	threadText := gmailapi.FormatThreadForPrompt(threadEmails, "rio@ramaris.app")

	// Summarize with LLM

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	prompt := fmt.Sprintf(`Summarize this email thread. Show the back-and-forth between sent and received messages. Include key points, decisions, and any action items. Be concise.

Thread (%d messages):
%s`, len(threadEmails), threadText)
	resp, err := callLLM(ctx, cfg, "classifier", prompt, budget)
	cancel()
	if err != nil {
		bot.Send(fmt.Sprintf("[thread] %d messages:\n\n%s", len(threadEmails), threadText[:min(3500, len(threadText))]))
		return
	}
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
	authURL := gmailapi.GenerateAuthURL(gmail.ClientID(), scopes, authRedirectPort)
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
				if ps.AutoPaused {
					status = fmt.Sprintf("PAUSED after %d failures", ps.Failures)
				}
			}
			if ps.Schedule == "manual" || ps.Schedule == "" {
				status = "manual"
			}
			nextStr := "-"
			if !ps.NextRun.IsZero() && !ps.Paused {
				nextStr = ps.NextRun.Format("Mon 02 Jan 15:04 MST")
			}
			lastStr := "-"
			if !ps.LastRun.IsZero() {
				lastStr = ps.LastRun.Format("Mon 02 Jan 15:04 MST")
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
		spec := strings.Join(parts[2:], " ")
		if err := sched.Reschedule(parts[1], spec); err != nil {
			_ = bot.Send(fmt.Sprintf("[cron] Not rescheduled: %s", err))
		} else {
			_ = bot.Send(fmt.Sprintf("[cron] Rescheduled %s to %s", parts[1], spec))
		}
		return
	}

	_ = bot.Send("[cron] Usage: /cron | /cron pause <name> | /cron resume <name> | /cron set <name> <interval or cron expression>")
}

func handleSkills(bot *TGBot, skills *skillsapi.SkillRegistry) {
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

func handleRun(args string, bot *TGBot, sched *Scheduler, cfg *config.Config, budget *BudgetTracker, skills *skillsapi.SkillRegistry) {
	name := strings.TrimSpace(args)
	if name == "" {
		bot.Send("[run] Usage: /run <pipeline-name>")
		return
	}
	// Find pipeline config
	var pipeline *config.PipelineConfig
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
	if ok, reason := sched.TryStartManual(name); !ok {
		_ = bot.Send(fmt.Sprintf("[run] Not started: %s (%s)", name, reason))
		return
	}
	bot.Send(fmt.Sprintf("[run] Starting: %s", name))
	go func() {
		err := runPipeline(cfg, *pipeline, budget, gateChannel(bot), skills, nil)
		if err != nil {
			log.Printf("[run] pipeline %s error: %v", name, err)
			bot.Send(fmt.Sprintf("[run] ERROR in %s: %s", name, err))
		}
		finishRun(sched, bot, name, err)
	}()
}

func pipelineByName(cfg *config.Config, name string) (config.PipelineConfig, bool) {
	for i := range cfg.Pipelines {
		if cfg.Pipelines[i].Name == name {
			return cfg.Pipelines[i], true
		}
	}
	return config.PipelineConfig{}, false
}

// finishRun releases a run's claim and tells the operator when the failure
// just paused the pipeline.
func finishRun(sched *Scheduler, ch interface{ Send(string) error }, name string, err error) {
	if sched.Finish(name, err) {
		log.Printf("[scheduler] %s paused after %d consecutive failures", name, sched.failureStreak(name))
		_ = ch.Send(autoPauseNotice(name, sched.failureStreak(name), err))
	}
}

func handleStatus(bot *TGBot, budget *BudgetTracker, sched *Scheduler, cfg *config.Config) {
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
	open, oerr := state.OpenApprovals()
	_ = bot.Send(statusReport(active, paused, budget, cfg, open, oerr, time.Now()))
}

// statusReport renders /status. Spend is shown against every cap that is
// configured — the person paying should see how close the day is to its cap
// without waiting for the next approval prompt to tell them — and open gates
// are counted so a stuck approval is one line away instead of invisible.
func statusReport(active, paused int, budget *BudgetTracker, cfg *config.Config, open []statestore.PendingApproval, openErr error, now time.Time) string {
	budget = budget.snapshotAt(now)
	lines := []string{
		"[status] Engine running",
		fmt.Sprintf("Pipelines: %d active, %d paused", active, paused),
	}
	if budget.stateErr != nil {
		lines = append(lines, "Budget ledger unavailable; model calls blocked.")
	} else if budget.unsettled > 0 {
		lines = append(lines, fmt.Sprintf("Provider usage unresolved: %d call(s); reconcile before retrying.", budget.unsettled))
	}
	if cap := cfg.Budgets.PerDayTokens; cap > 0 {
		lines = append(lines, fmt.Sprintf("Tokens today: %d / %d (%.0f%% left)",
			budget.tokensUsedToday, cap, pctLeft(float64(budget.tokensUsedToday), float64(cap))))
	} else {
		lines = append(lines, fmt.Sprintf("Tokens today: %d (no cap)", budget.tokensUsedToday))
	}
	if budget.dayCostLimit > 0 {
		lines = append(lines, fmt.Sprintf("Spend today: %.4f / %.4f (%.0f%% left)",
			budget.costToday, budget.dayCostLimit, pctLeft(budget.costToday, budget.dayCostLimit)))
	} else {
		lines = append(lines, fmt.Sprintf("Spend today: %.4f (no cap)", budget.costToday))
	}
	if budget.pipelineCostLimit > 0 {
		lines = append(lines, fmt.Sprintf("Per-pipeline cap: %.4f", budget.pipelineCostLimit))
	}
	if cap := cfg.Budgets.PerDayCalls; cap > 0 {
		lines = append(lines, fmt.Sprintf("Calls today: %d / %d", budget.callsToday, cap))
	}
	if cap := cfg.Budgets.PerDayCallMinutes; cap > 0 {
		lines = append(lines, fmt.Sprintf("Call minutes today: %d / %d", budget.callMinutesToday, cap))
	}
	switch {
	case openErr != nil:
		lines = append(lines, "Gates open: unknown ("+openErr.Error()+")")
	case len(open) == 0:
		lines = append(lines, "Gates open: 0")
	default:
		oldest := open[0].OpenedAt
		for _, p := range open {
			if p.OpenedAt.Before(oldest) {
				oldest = p.OpenedAt
			}
		}
		lines = append(lines, fmt.Sprintf("Gates open: %d (oldest %s ago — /pending)", len(open), humanDuration(now.Sub(oldest))))
	}
	lines = append(lines, "Budget day start: "+budget.dayStart.Format("15:04:05"))
	return strings.Join(lines, "\n")
}

// --- Main ---

// runAuditVerify re-checks the HMAC receipts on a pipeline's approval audit rows
// and reports any that fail. This is the operator-facing side of tamper-evidence:
// the append-only convention guards draftcat's own writes, and this catches edits
// made directly to the SQLite file. Exit 1 if any row is tampered, 0 if every row
// verifies (or is unsigned), 2 on a usage/secret error.
func runAuditVerify(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: draftcat audit-verify <pipeline> [config.yaml]")
		return 2
	}
	pipeline := args[0]
	configPath := "config.yaml"
	if len(args) > 1 {
		configPath = args[1]
	}

	secret := []byte(os.Getenv("DRAFTCAT_APPROVAL_SECRET"))
	if len(secret) == 0 {
		fmt.Fprintln(os.Stderr, "DRAFTCAT_APPROVAL_SECRET is not set — cannot verify receipts")
		return 2
	}

	// Resolve the state path exactly like the engine does: env override first,
	// then the config's State.Path, then the ./state.db default.
	statePath := strings.TrimSpace(os.Getenv("DRAFTCAT_STATE_PATH"))
	if statePath == "" {
		if data, err := os.ReadFile(configPath); err == nil {
			var cfg config.Config
			if yaml.Unmarshal(data, &cfg) == nil {
				statePath = cfg.State.Path
			}
		}
	}
	if statePath == "" {
		statePath = "./state.db"
	}

	st, err := statestore.OpenStateStoreReadOnly(statePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open state store %s: %v\n", statePath, err)
		return 1
	}
	defer func() { _ = st.Close() }()

	results, err := st.VerifyApprovals(secret, pipeline, 1000)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify: %v\n", err)
		return 1
	}

	tampered := 0
	for _, r := range results {
		if r.Status == "tampered" {
			tampered++
		}
		fmt.Printf("%-9s %s  %-8s op=%d  step=%s\n",
			r.Status, r.Record.DecidedAt.Format(time.RFC3339), r.Record.Decision, r.Record.OperatorID, r.Record.Step)
	}
	fmt.Printf("%d rows checked, %d tampered\n", len(results), tampered)
	if tampered > 0 {
		return 1
	}
	return 0
}

func main() {
	// Subcommand dispatch. The bare form `draftcat [config.yaml] [skills/]` still runs the engine.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println("draftcat " + version)
			return
		case "validate":
			os.Exit(validate.Run(os.Args[2:]))
		case "doctor":
			os.Exit(runDoctor(os.Args[2:], os.Stdout))
		case "test":
			os.Exit(runTestCmd(os.Args[2:]))
		case "audit-verify":
			os.Exit(runAuditVerify(os.Args[2:]))
		case "zk-receipt":
			os.Exit(runZKReceiptCmd(os.Args[2:]))
		case "fhe-vote":
			os.Exit(runFHEVoteCmd(os.Args[2:]))
		case "runs":
			os.Exit(runRunsCmd(os.Args[2:]))
		case "pending":
			os.Exit(runPendingCmd(os.Args[2:]))
		case "budget":
			os.Exit(runBudgetCmd(os.Args[2:]))
		case "receipts":
			os.Exit(runReceiptsCmd(os.Args[2:]))
		case "hitl":
			os.Exit(runHitlCmd(os.Args[2:]))
		case "-h", "--help", "help":
			fmt.Println("Draftcat — AI communication management for service businesses.")
			fmt.Println()
			fmt.Println("Usage:")
			fmt.Println("  draftcat [config.yaml] [skills/]       run the engine (default)")
			fmt.Println("  draftcat validate [--strict]           lint config + skills, exit non-zero on errors")
			fmt.Println("  draftcat doctor [--json]               read-only preflight: credentials, operators, state, ports, schedules")
			fmt.Println("  draftcat test <pipeline>               dry-run a pipeline using fixtures/<pipeline>/")
			fmt.Println("  draftcat runs [pipeline] [--json]      recent runs + the approval decisions in each")
			fmt.Println("  draftcat budget <status|reconcile>      inspect spend or reconcile uncertain provider usage")
			fmt.Println("  draftcat pending [--json]              approval gates waiting on a human right now")
			fmt.Println("  draftcat receipts <list|show|export|verify> inspect, export, or verify receipts")
			fmt.Println("  draftcat audit-verify <pipeline>       check approval-receipt signatures (needs DRAFTCAT_APPROVAL_SECRET)")
			fmt.Println("  draftcat zk-receipt <command>          prove an approval without revealing its private fields")
			fmt.Println("  draftcat fhe-vote <command>             count encrypted approval votes without reading them")
			fmt.Println("  draftcat hitl verify <relay-url>      run the hitl/v0 conformance suite against a relay")
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
	var cfg config.Config
	if err := yaml.Unmarshal(cfgData, &cfg); err != nil {
		log.Fatalf("failed to parse config: %v", err)
	}
	// The file as written, before credentials and defaults are filled in. A
	// reload compares against it to name the sections that need a restart.
	bootRaw := cfg

	// Resolve env vars
	cfg.Telegram.SetToken(resolveEnv(cfg.Telegram.TokenEnv, "DRAFTCAT_TG_TOKEN"))
	cfg.Relay.SetSecret(resolveEnv(cfg.Relay.SecretEnv, "DRAFTCAT_RELAY_SECRET"))
	cfg.Provider.SetAPIKey(resolveEnv(cfg.Provider.APIKeyEnv, "OPENROUTER_API_KEY"))

	// Every credential the engine holds is registered for redaction before
	// anything is logged, so log lines, operator notices, run errors and spans
	// carry a label in its place.
	registerSecrets(&cfg)
	log.SetOutput(redact.Writer(os.Stderr))

	if cfg.Telegram.Token() == "" {
		log.Fatal("Telegram token not set. Set DRAFTCAT_TG_TOKEN env var.")
	}
	if cfg.Provider.APIKey() == "" {
		log.Fatal("OpenRouter API key not set. Set OPENROUTER_API_KEY env var.")
	}

	// Operator identity from env — lets a container boot from env vars alone
	// (e.g. a one-click PaaS deploy) with no secrets file on disk. Values set
	// in config.yaml win; env only fills what is unset.
	if cfg.Telegram.ChatID == 0 {
		if v := strings.TrimSpace(os.Getenv("DRAFTCAT_TG_CHAT_ID")); v != "" {
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				log.Fatalf("DRAFTCAT_TG_CHAT_ID is not a valid integer: %q", v)
			}
			cfg.Telegram.ChatID = id
		}
	}
	if len(cfg.Telegram.Security.AllowedUsers) == 0 {
		if v := os.Getenv("DRAFTCAT_TG_ALLOWED_USERS"); strings.TrimSpace(v) != "" {
			cfg.Telegram.Security.AllowedUsers = parseUserIDs(v)
		}
	}
	// Single-operator convenience: default the send target to the first allowed
	// user when chat_id was not given explicitly.
	if cfg.Telegram.ChatID == 0 && len(cfg.Telegram.Security.AllowedUsers) > 0 {
		cfg.Telegram.ChatID = cfg.Telegram.Security.AllowedUsers[0]
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
	skillReg, _ := skillsapi.LoadSkills(skillsDir)

	// Config validation on the boot path. `draftcat validate` runs the very same
	// checks, but only if someone remembers to; a bad config used to start fine
	// and fail later mid-pipeline. Errors are fatal here, warnings are logged.
	// DRAFTCAT_SKIP_VALIDATE=1 is the escape hatch for an operator who needs to
	// boot a knowingly-imperfect config in an emergency.
	if os.Getenv("DRAFTCAT_SKIP_VALIDATE") == "" {
		findings := validate.CheckAtStartup(&cfg, skillsDir)
		fatal := 0
		for _, f := range findings {
			if f.Level == "error" {
				log.Printf("[config] ERROR %s: %s", f.Path, f.Message)
				fatal++
			} else {
				log.Printf("[config] warn  %s: %s", f.Path, f.Message)
			}
		}
		if fatal > 0 {
			log.Fatalf("[config] %d error(s) in %s — refusing to start. Fix them, or set DRAFTCAT_SKIP_VALIDATE=1 to override.", fatal, configPath)
		}
	} else {
		log.Printf("[config] WARNING: startup validation skipped (DRAFTCAT_SKIP_VALIDATE set)")
	}

	// The relay channel is built here, before the state store and voice bridge
	// register their cleanup, so a relay that cannot start fails while there is
	// still nothing to unwind. A configured relay is not optional: an operator
	// who set one up is watching that surface, and quietly falling back to
	// Telegram would route approvals to a channel nobody is reading, which is
	// the exact failure internal/channels exists to prevent.
	var relayCh *RelayChannel
	if cfg.Relay.Enabled() {
		rc, rerr := NewRelayChannel(cfg.Relay)
		if rerr != nil {
			log.Fatalf("[relay] channel failed to start: %v", rerr)
		}
		relayCh = rc
	}

	// Init Gmail connector if configured
	if cfg.Gmail.TokenPath != "" {
		var err error
		gmail, err = gmailapi.NewGmailConnector(cfg.Gmail.TokenPath)
		if err != nil {
			log.Printf("[gmail] WARNING: failed to initialize: %v", err)
		}
	}

	// Init GoHighLevel connector if configured
	if cfg.GHL.TokenPath != "" || cfg.GHL.APIKeyEnv != "" {
		var err error
		ghl, err = ghlapi.NewGHLConnector(cfg.GHL)
		if err != nil {
			log.Printf("[ghl] WARNING: failed to initialize: %v", err)
		}
	}

	// PDF connector — stateless, always available
	pdfParser = pdf.NewPDFParser()

	// State store — SQLite-backed dedup + run history
	statePath := cfg.State.Path
	if statePath == "" {
		statePath = "./state.db"
	}
	// Env override so a container can point state at a mounted disk (e.g. a
	// PaaS persistent volume) without shadowing the baked config/skills.
	if v := strings.TrimSpace(os.Getenv("DRAFTCAT_STATE_PATH")); v != "" {
		statePath = v
	}
	{
		var err error
		state, err = statestore.OpenStateStore(statePath)
		if err != nil {
			log.Fatalf("[state] failed to open %s: %v", statePath, err)
		}
		defer state.Close()
		log.Printf("[state] opened %s", statePath)
	}

	// Voice plugin (build-tag voice). No-op in lean builds.
	vbridge = voicebridge.Boot(cfg.Voice, state, ghl)
	defer vbridge.Shutdown()

	// Init scheduler
	sched := newScheduler(cfg.Pipelines)

	log.Printf("draftcat starting — %d pipeline(s), %d skill(s), provider=%s, operator=telegram:%d",
		len(cfg.Pipelines), len(skillReg.List()), cfg.Provider.Type, cfg.Telegram.ChatID)

	bot := &TGBot{
		token:       cfg.Telegram.Token(),
		chatID:      cfg.Telegram.ChatID,
		security:    cfg.Telegram.Security,
		rateLimiter: newRateLimiter(cfg.Telegram.Security.RateLimit),
	}

	// Operator channel selection. A configured relay takes the gate; Telegram
	// keeps the command surface either way. The relay was already built and
	// proven startable above, so nothing here can fail.
	opChan = OperatorChannel(bot)
	if relayCh != nil {
		defer func() { _ = relayCh.Close() }()
		opChan = relayCh
		log.Printf("[relay] operator channel active — approvals dispatch to %s", cfg.Relay.URL)
	}
	budget := &BudgetTracker{
		dayStart:          time.Now(),
		dayCostLimit:      cfg.Budgets.PerDayCost,
		pipelineCostLimit: cfg.Budgets.PerPipelineCost,
	}
	if err := budget.attachStore(state); err != nil {
		log.Printf("[budget] usage ledger unavailable; model calls blocked: %v", err)
	}
	budget.configureAlerts(&cfg, func(msg string) { _ = opChan.Send(msg) })
	chatHistory := newChatHistory(20) // keep last 20 turns

	// Any approval gate still marked pending was open when this process last
	// stopped. Close it out — audit row plus an operator notice — before the
	// engine starts, so no gate is left in an unknown state and no stale button
	// looks live. Needs the bot, hence its position after it.
	reconcileInterruptedApprovals(state, opChan)
	restoreSchedules(sched, &cfg, state, bot)
	if err := state.InterruptWebhookAdmissions(time.Now()); err != nil {
		log.Printf("[webhook] reconcile admissions: %v", err)
	}

	// Observability — structured span emission (off unless opted in).
	if cfg.Observ.Spans || os.Getenv("DRAFTCAT_TRACE") != "" {
		obs.Enable(nil)
		log.Printf("[obs] span tracing enabled")
	}
	// Prometheus exporter — pull-based /metrics endpoint. Opens one local port
	// only when enabled (localhost by default, same posture as the webhook). A
	// failed metrics server is logged but never fatal — observability must not
	// take down the engine.
	if cfg.Observ.Prometheus.Enabled {
		obs.EnablePrometheus()
		obs.SetGaugeSource(func() []obs.Gauge { return engineGauges(sched, budget, currentConfig(&cfg), state) })
		if closer, perr := obs.ServePrometheus(cfg.Observ.Prometheus.Addr, cfg.Observ.Prometheus.Path); perr != nil {
			log.Printf("[obs] prometheus exporter failed to start: %v", perr)
		} else {
			defer func() { _ = closer.Close() }()
			addr := cfg.Observ.Prometheus.Addr
			if addr == "" {
				addr = "127.0.0.1:9090"
			}
			log.Printf("[obs] prometheus exporter listening on %s", addr)
		}
	}
	// OTLP/HTTP trace exporter — push-based, fire-and-forget. Header secrets come
	// from an env var, never YAML. An empty endpoint disables it (the validator
	// flags this as a config error at lint time).
	if cfg.Observ.OTLP.Enabled {
		if cfg.Observ.OTLP.Endpoint == "" {
			log.Printf("[obs] observability.otlp.enabled is true but endpoint is empty — OTLP export disabled")
		} else {
			headers := obs.ParseHeaderEnv(os.Getenv(cfg.Observ.OTLP.HeaderEnv))
			obs.EnableOTLP(cfg.Observ.OTLP.Endpoint, headers)
			log.Printf("[obs] OTLP exporter pushing to %s", cfg.Observ.OTLP.Endpoint)
		}
	}

	// Webhook trigger server — opt-in; opens no port unless enabled.
	var webhookSrv *http.Server
	if cfg.Webhook.Enabled {
		cfg.Webhook.SetSecret(resolveEnv(cfg.Webhook.SecretEnv))
		if cfg.Webhook.Secret() == "" {
			log.Fatalf("webhook.enabled is true but secret env %q is empty — refusing to start an unauthenticated trigger", cfg.Webhook.SecretEnv)
		}
		webhookSrv = startWebhookServer(&cfg, sched, budget, bot, skillReg)
	}

	// Drain updates that arrived while the engine was down, then start the
	// single update pump. Every consumer — the command loop below, each
	// approval waiter — reads from the pump; nothing else calls getUpdates.
	bot.getUpdates()
	bot.startPump(2 * time.Second)
	defer bot.stopPump()

	// Startup notification
	// No startup message — don't leak that the bot is running to anyone watching the chat

	// Signal handling for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Config reload: SIGHUP or /reload swaps the snapshot new runs use.
	liveConfig.Store(&cfg)
	liveSkills.Store(skillReg)
	reloadHook = func() string {
		return reloadEngine(configPath, skillsDir, &bootRaw, &cfg, sched, budget, func(m string) { _ = opChan.Send(m) })
	}
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)

	// Main event loop — polls for commands and runs scheduled pipelines
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	log.Printf("draftcat running. Waiting for commands and scheduled pipelines...")

	for {
		select {
		case sig := <-sigCh:
			log.Printf("received %s, shutting down", sig)
			// silent shutdown — no message
			drainEngine(sched, webhookSrv, shutdownGrace(currentConfig(&cfg)))
			return

		case <-hupCh:
			_ = bot.Send(reloadHook())

		case <-ticker.C:
			// 1. Operator commands and callbacks — whatever the update pump
			// queued for the command loop since the last tick. Approval taps
			// never arrive here; the pump routes them to their waiter.
			updates := bot.drainUpdates()
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
						handleCommand(cmd, args, bot, sched, currentSkills(skillReg), currentConfig(&cfg), budget)
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

							intentResp, err := callLLM(intentCtx, &cfg, "classifier", intentPrompt, budget)
							intentCancel()

							if err != nil {
								log.Printf("[intent] classifier error: %v", err)
							} else {
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

						{
							aiCtx, aiCancel := context.WithTimeout(context.Background(), 15*time.Second)
							var skillList, pipelineList string
							for _, s := range currentSkills(skillReg).List() {
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
							sysPrompt := fmt.Sprintf(`You are draftcat, an AI operations assistant running as a Telegram bot.
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
/status - engine status, spend against caps, open gates
/pending - approval gates waiting on the operator

When the operator asks about emails, you can fetch them directly. When they ask for features not yet available (calendar, Slack, etc), say briefly what's needed and that it's on the roadmap. Be direct, concise, no fluff. Remember the conversation context.

Conversation so far:
%s`, pipelineList, skillList, gmailStatus, history)
							resp, err := callLLM(aiCtx, &cfg, "drafter", sysPrompt, budget)
							aiCancel()
							if err != nil {
								log.Printf("[msg] LLM error: %v", err)
								bot.Send("Commands: /help /cron /skills /run /status")
							} else {
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
								handleRun(parts[2], bot, sched, currentConfig(&cfg), budget, currentSkills(skillReg))
							}
						}
					}
				}
			}

			// 2. Check for scheduled pipelines
			for _, name := range sched.ClaimDue() {
				runCfg, runSkills := currentConfig(&cfg), currentSkills(skillReg)
				p, ok := pipelineByName(runCfg, name)
				if !ok {
					sched.SetRunning(name, false)
					continue
				}
				log.Printf("[scheduler] running due pipeline: %s", name)
				go func(p config.PipelineConfig) {
					err := runPipeline(runCfg, p, budget, gateChannel(bot), runSkills, nil)
					if err != nil {
						log.Printf("[scheduler] pipeline %s error: %v", p.Name, err)
						_ = bot.Send(fmt.Sprintf("[draftcat] ERROR in %s: %s", p.Name, err))
					}
					finishRun(sched, bot, p.Name, err)
				}(p)
			}
		}
	}
}

// startWebhookServer launches the opt-in HTTP trigger. POST /hooks/<pipeline>
// with `Authorization: Bearer <secret>` runs a pipeline whose schedule is
// "webhook". This mirrors the dispatch-style async trigger seen in agent
// harnesses, but it ONLY triggers — the pipeline still runs its own approval
// gates, so an inbound request can never make the LLM fire an action. The
// request body is passed to the pipeline as data["webhook_body"] / {{input}}.
//
// The server runs in a background goroutine. A trigger is rejected (409) if the
// pipeline is already running, preserving the at-most-once-concurrent guarantee.
func startWebhookServer(cfg *config.Config, sched *Scheduler, budget *BudgetTracker, bot *TGBot, skills *skillsapi.SkillRegistry) *http.Server {
	addr := cfg.Webhook.Addr
	if addr == "" {
		addr = "127.0.0.1:8088"
	}
	hookable := webhookPipelines(cfg)
	srv := &http.Server{
		Addr:              addr,
		Handler:           newWebhookHandler(cfg, sched, budget, bot, skills),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("[webhook] listening on %s (%d pipeline(s) triggerable)", addr, len(hookable))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[webhook] server stopped: %v", err)
		}
	}()
	return srv
}

// webhookSigHeader carries the body-bound HMAC receipt on inbound triggers.
const webhookSigHeader = "X-Draftcat-Signature"

// verifyWebhookSignature checks the Stripe/GitHub-style receipt
//
//	X-Draftcat-Signature: t=<unix>,v1=<hex hmac-sha256(t + "." + body)>
//
// A bearer token proves only that the caller once saw the token: it does not
// bind the request body, and a captured header replays forever. Since a webhook
// here STARTS a pipeline, that is the one thing an inbound request must not be
// able to do on its own. Signing t+body and bounding the clock skew closes both
// holes; the nonce store below closes replay inside the skew window.
//
// Errors are deliberately specific for the log and generic for the caller — the
// handler returns a bare 401 so a prober learns nothing about which check failed.
func verifyWebhookSignature(header string, body, secret []byte, maxSkewSeconds int64, now time.Time) error {
	sig, err := authenticateWebhookSignature(header, body, secret, maxSkewSeconds, now)
	if err != nil {
		return err
	}
	return claimWebhookSignature(sig, now)
}

func claimWebhookSignature(sig string, now time.Time) error {
	if state != nil {
		won, err := state.TryMarkSeen(webhookReplayScope, "sig", sig, now)
		if err != nil {
			return fmt.Errorf("replay record failed: %w", err)
		}
		if !won {
			return fmt.Errorf("signature already used (replay)")
		}
	}
	return nil
}

func authenticateWebhookSignature(header string, body, secret []byte, maxSkewSeconds int64, now time.Time) (string, error) {
	if header == "" {
		return "", fmt.Errorf("missing %s header", webhookSigHeader)
	}
	var tsPart, sigPart string
	for _, field := range strings.Split(header, ",") {
		field = strings.TrimSpace(field)
		switch {
		case strings.HasPrefix(field, "t="):
			if tsPart != "" {
				return "", fmt.Errorf("duplicate timestamp")
			}
			tsPart = strings.TrimPrefix(field, "t=")
		case strings.HasPrefix(field, "v1="):
			if sigPart != "" {
				return "", fmt.Errorf("duplicate signature")
			}
			sigPart = strings.TrimPrefix(field, "v1=")
		}
	}
	if tsPart == "" || sigPart == "" {
		return "", fmt.Errorf("malformed signature header")
	}
	ts, err := strconv.ParseInt(tsPart, 10, 64)
	if err != nil {
		return "", fmt.Errorf("unparseable timestamp")
	}
	// Compare bounds instead of subtracting an attacker-controlled timestamp.
	if ts < now.Unix()-maxSkewSeconds || ts > now.Unix()+maxSkewSeconds {
		return "", fmt.Errorf("timestamp outside %ds window", maxSkewSeconds)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(tsPart))
	mac.Write([]byte("."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(want), []byte(sigPart)) != 1 {
		return "", fmt.Errorf("signature mismatch")
	}
	return sigPart, nil
}

// webhookReplayScope namespaces spent webhook signatures in seen_items. The
// leading underscores keep it from colliding with a real pipeline name.
const webhookReplayScope = "__webhook"

// webhookPipelines indexes the pipelines that opted into HTTP triggering.
func webhookPipelines(cfg *config.Config) map[string]config.PipelineConfig {
	hookable := map[string]config.PipelineConfig{}
	for _, p := range cfg.Pipelines {
		if p.Schedule == "webhook" {
			hookable[p.Name] = p
		}
	}
	return hookable
}

// newWebhookHandler builds the /hooks/ handler. Split out from startWebhookServer
// so the auth, routing, and concurrency guards are unit-testable without binding
// a port.
func newWebhookHandler(cfg *config.Config, sched *Scheduler, budget *BudgetTracker, bot *TGBot, skills *skillsapi.SkillRegistry) http.Handler {
	maxBody := cfg.Webhook.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = 65536
	}
	secret := []byte(cfg.Webhook.Secret())
	maxSkew := cfg.Webhook.MaxSkewSeconds
	if maxSkew <= 0 {
		maxSkew = 300
	}
	var admissionMu sync.Mutex

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if state == nil || state.Ping(ctx) != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"status\":\"ready\"}\n"))
	})
	// Tool-call gate: an agent harness asks permission for one tool call.
	// Mounted on the same server as the webhook trigger so enabling it opens no
	// additional port. Default-deny lives in the handler.
	if cfg.ToolGate.Enabled {
		gate := newToolGate(cfg, gateChannel(bot))
		gateAuth := func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				const prefix = "Bearer "
				got := r.Header.Get("Authorization")
				if !strings.HasPrefix(got, prefix) ||
					subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(got, prefix)), secret) != 1 {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				if r.Method == http.MethodPost {
					body, ok := readRequestBody(w, r, maxBody)
					if !ok {
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(body))
					sigHeader := r.Header.Get(webhookSigHeader)
					if sigHeader != "" || cfg.Webhook.RequireSignature {
						if err := verifyWebhookSignature(sigHeader, body, secret, maxSkew, time.Now()); err != nil {
							log.Printf("[tool-gate][security] signature rejected: %v", err)
							http.Error(w, "unauthorized", http.StatusUnauthorized)
							return
						}
					}
				}
				next.ServeHTTP(w, r)
			})
		}
		mux.Handle(toolGatePath, gateAuth(http.HandlerFunc(gate.HandleCall)))
		mux.Handle(toolGatePath+"/", gateAuth(http.HandlerFunc(gate.HandleStatus)))
		log.Printf("[tool-gate] enabled — %d tool(s) allowlisted, everything else denied", len(cfg.ToolGate.Tools))
	}

	mux.HandleFunc("/hooks/status/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		const prefix = "Bearer "
		got := r.Header.Get("Authorization")
		if !strings.HasPrefix(got, prefix) ||
			subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(got, prefix)), secret) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/hooks/status/")
		if !validActionID(id) || state == nil {
			http.Error(w, "admission not found", http.StatusNotFound)
			return
		}
		a, err := state.WebhookAdmission(id)
		if err != nil {
			http.Error(w, "admission not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"admission_id": a.ID, "pipeline": a.Pipeline, "body_hash": a.BodyHash,
			"status": a.Status, "error": a.Error,
			"created_at": a.CreatedAt.UTC().Format(time.RFC3339),
			"updated_at": a.UpdatedAt.UTC().Format(time.RFC3339),
		})
	})

	mux.HandleFunc("/hooks/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Constant-time bearer auth.
		const prefix = "Bearer "
		got := r.Header.Get("Authorization")
		if !strings.HasPrefix(got, prefix) ||
			subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(got, prefix)), secret) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// The body must be read before the signature can be checked, since the
		// signature covers it — that binding is the point.
		body, ok := readRequestBody(w, r, maxBody)
		if !ok {
			return
		}

		// A signature is verified whenever it is present, and demanded when
		// require_signature is on. Verifying an unrequested-but-present header
		// means a signer that starts emitting bad signatures fails loudly
		// instead of being silently ignored.
		sigHeader := r.Header.Get(webhookSigHeader)
		var signature string
		if sigHeader != "" || cfg.Webhook.RequireSignature {
			var err error
			signature, err = authenticateWebhookSignature(sigHeader, body, secret, maxSkew, time.Now())
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}

		name := strings.TrimPrefix(r.URL.Path, "/hooks/")
		runCfg, runSkills := currentConfig(cfg), currentSkills(skills)
		p, ok := webhookPipelines(runCfg)[name]
		if !ok {
			http.Error(w, "no webhook-triggerable pipeline named "+name, http.StatusNotFound)
			return
		}

		key := r.Header.Get("Idempotency-Key")
		if key != "" && !validActionID(key) {
			http.Error(w, "Idempotency-Key must be 1-128 URL-safe characters", http.StatusBadRequest)
			return
		}
		keyHash := ""
		if key != "" {
			if state == nil {
				http.Error(w, "state unavailable", http.StatusServiceUnavailable)
				return
			}
			sum := sha256.Sum256([]byte(key))
			keyHash = hex.EncodeToString(sum[:])
		}
		bodySum := sha256.Sum256(body)
		bodyHash := "sha256:" + hex.EncodeToString(bodySum[:])
		admissionMu.Lock()
		defer admissionMu.Unlock()
		if keyHash != "" {
			existing, err := state.WebhookAdmissionForKey(name, keyHash)
			if err == nil {
				if existing.BodyHash != bodyHash {
					http.Error(w, "idempotency key is already bound to another request body", http.StatusConflict)
					return
				}
				writeWebhookAdmission(w, existing)
				return
			}
			if !errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "state unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		ok, reason := sched.TryStart(name)
		if !ok {
			http.Error(w, reason, http.StatusConflict)
			return
		}
		if signature != "" {
			if err := claimWebhookSignature(signature, time.Now()); err != nil {
				sched.SetRunning(name, false)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		admissionID := "wh_" + strings.TrimPrefix(newToolTicketID(), "tc_")
		now := time.Now()
		admission := statestore.WebhookAdmission{ID: admissionID, Pipeline: name, BodyHash: bodyHash, IdempotencyKey: keyHash, Status: "accepted", CreatedAt: now, UpdatedAt: now}
		if state != nil {
			saved, created, err := state.AdmitWebhook(admission)
			if err != nil {
				sched.SetRunning(name, false)
				log.Printf("[webhook] durable admission failed: %v", err)
				if saved.ID != "" {
					http.Error(w, "idempotency key is already bound to another request body", http.StatusConflict)
				} else {
					http.Error(w, "state unavailable", http.StatusServiceUnavailable)
				}
				return
			}
			if !created {
				sched.SetRunning(name, false)
				writeWebhookAdmission(w, saved)
				return
			}
			if err := state.FinishWebhookAdmission(admissionID, "running", "", now); err != nil {
				sched.SetRunning(name, false)
				http.Error(w, "state unavailable", http.StatusServiceUnavailable)
				return
			}
		}

		log.Printf("[webhook] triggering pipeline %s (%d body bytes)", name, len(body))
		go func(p config.PipelineConfig, body []byte, admissionID string) {
			var runErr error
			defer func() { finishRun(sched, bot, p.Name, runErr) }()
			seed := map[string]interface{}{"webhook_body": string(body)}
			if strings.TrimSpace(string(body)) != "" {
				seed["input"] = string(body)
			}
			if err := runPipeline(runCfg, p, budget, gateChannel(bot), runSkills, seed); err != nil {
				runErr = err
				log.Printf("[webhook] pipeline %s error: %v", p.Name, err)
				bot.Send(fmt.Sprintf("[draftcat] ERROR in %s (webhook): %s", p.Name, err))
				if state != nil {
					_ = state.FinishWebhookAdmission(admissionID, "error", redact.String(err.Error()), time.Now())
				}
			} else if state != nil {
				_ = state.FinishWebhookAdmission(admissionID, "completed", "", time.Now())
			}
		}(p, body, admissionID)

		writeWebhookAdmission(w, admission)
	})
	return mux
}

func writeWebhookAdmission(w http.ResponseWriter, a statestore.WebhookAdmission) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"admission_id": a.ID, "status": a.Status, "poll": "/hooks/status/" + a.ID})
}

func resolveEnv(names ...string) string {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}

// parseUserIDs parses a comma-separated list of integer Telegram user IDs
// (e.g. "123,456") from an env var. A non-integer entry is fatal — a
// misconfigured allow-list must not silently fall through to "no one allowed".
func parseUserIDs(s string) []int64 {
	var ids []int64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			log.Fatalf("DRAFTCAT_TG_ALLOWED_USERS contains a non-integer ID: %q", part)
		}
		ids = append(ids, id)
	}
	return ids
}
