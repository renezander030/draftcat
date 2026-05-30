package ghl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// --- GoHighLevel Connector ---
// CRM integration. Reads contacts, opportunities, conversations.
// Sends messages only through HITL approval steps.

const (
	ghlBaseURL    = "https://services.leadconnectorhq.com"
	ghlAPIVersion = "2021-07-28"
)

type GHLConfig struct {
	// OAuth mode (marketplace apps)
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	TokenPath    string `yaml:"token_path"` // stored OAuth tokens

	// Private integration mode (single-location)
	APIKeyEnv  string `yaml:"api_key_env"`
	LocationID string `yaml:"location_id"`

	Permission string `yaml:"permission"` // read | write (write = send messages with HITL)
}

type GHLToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	LocationID   string `json:"locationId"`
	UserID       string `json:"userId"`
}

type GHLConnector struct {
	config      GHLConfig
	token       GHLToken
	accessToken string
	expiresAt   time.Time
	mu          sync.Mutex
}

// --- Data Types ---

type GHLContact struct {
	ID          string           `json:"id"`
	FirstName   string           `json:"firstName"`
	LastName    string           `json:"lastName"`
	Email       string           `json:"email"`
	Phone       string           `json:"phone"`
	Source      string           `json:"source"`
	Tags        []string         `json:"tags"`
	DateAdded   string           `json:"dateAdded"`
	CustomField []GHLCustomField `json:"customFields,omitempty"`
}

type GHLCustomField struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

type GHLOpportunity struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Status        string  `json:"status"`
	PipelineID    string  `json:"pipelineId"`
	StageID       string  `json:"pipelineStageId"`
	StageName     string  `json:"stageName,omitempty"`
	MonetaryValue float64 `json:"monetaryValue"`
	ContactID     string  `json:"contactId"`
	AssignedTo    string  `json:"assignedTo"`
	LastActivity  string  `json:"lastActivity"`
	CreatedAt     string  `json:"createdAt"`
	UpdatedAt     string  `json:"updatedAt"`
}

type GHLConversation struct {
	ID          string `json:"id"`
	ContactID   string `json:"contactId"`
	LastMessage string `json:"lastMessageBody"`
	LastDate    string `json:"lastMessageDate"`
	LastType    string `json:"lastMessageType"` // SMS, Email, WhatsApp, etc.
	Unread      bool   `json:"unreadCount,omitempty"`
}

type GHLMessage struct {
	ID        string `json:"id"`
	Type      int    `json:"type"`      // 1=SMS, 2=Email, 3=WhatsApp, etc.
	Direction string `json:"direction"` // inbound | outbound
	Body      string `json:"body"`
	ContactID string `json:"contactId"`
	DateAdded string `json:"dateAdded"`
}

type GHLPipeline struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	Stages []GHLStage `json:"stages"`
}

type GHLStage struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// --- Constructor ---

func NewGHLConnector(cfg GHLConfig) (*GHLConnector, error) {
	c := &GHLConnector{config: cfg}

	// Private integration token (simpler, no OAuth)
	if cfg.APIKeyEnv != "" {
		apiKey := os.Getenv(cfg.APIKeyEnv)
		if apiKey == "" {
			return nil, fmt.Errorf("ghl: env var %s not set", cfg.APIKeyEnv)
		}
		c.accessToken = apiKey
		c.expiresAt = time.Now().Add(365 * 24 * time.Hour) // private tokens don't expire
		log.Printf("[ghl] initialized with private integration token (location: %s)", cfg.LocationID)
		return c, nil
	}

	// OAuth mode — load stored tokens
	if cfg.TokenPath == "" {
		return nil, fmt.Errorf("ghl: either api_key_env or token_path must be configured")
	}

	data, err := os.ReadFile(cfg.TokenPath)
	if err != nil {
		return nil, fmt.Errorf("ghl: failed to read token file: %w", err)
	}

	if err := json.Unmarshal(data, &c.token); err != nil {
		return nil, fmt.Errorf("ghl: failed to parse token file: %w", err)
	}

	c.accessToken = c.token.AccessToken
	// Tokens expire in ~24h, assume we need to refresh if file is old
	c.expiresAt = time.Now().Add(1 * time.Hour)

	log.Printf("[ghl] initialized with OAuth (location: %s)", c.token.LocationID)
	return c, nil
}

// --- Auth ---

func (c *GHLConnector) ensureToken() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if time.Now().Before(c.expiresAt.Add(-5 * time.Minute)) {
		return nil // token still valid
	}

	// Private tokens don't need refresh
	if c.config.APIKeyEnv != "" {
		return nil
	}

	return c.refreshToken()
}

func (c *GHLConnector) refreshToken() error {
	form := url.Values{}
	form.Set("client_id", c.config.ClientID)
	form.Set("client_secret", c.config.ClientSecret)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", c.token.RefreshToken)

	resp, err := http.PostForm(ghlBaseURL+"/oauth/token", form)
	if err != nil {
		return fmt.Errorf("ghl: token refresh failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ghl: token refresh returned %d: %s", resp.StatusCode, string(body))
	}

	var newToken GHLToken
	if err := json.NewDecoder(resp.Body).Decode(&newToken); err != nil {
		return fmt.Errorf("ghl: failed to decode refreshed token: %w", err)
	}

	c.token = newToken
	c.accessToken = newToken.AccessToken
	c.expiresAt = time.Now().Add(time.Duration(newToken.ExpiresIn) * time.Second)

	// Persist rotated refresh token (GHL uses single-use refresh tokens)
	data, _ := json.MarshalIndent(newToken, "", "  ")
	if err := os.WriteFile(c.config.TokenPath, data, 0600); err != nil {
		log.Printf("[ghl] WARNING: failed to persist refreshed token: %v", err)
	}

	log.Printf("[ghl] token refreshed, expires in %ds", newToken.ExpiresIn)
	return nil
}

// --- HTTP ---

func (c *GHLConnector) request(method, path string, body interface{}) ([]byte, error) {
	if err := c.ensureToken(); err != nil {
		return nil, err
	}

	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("ghl: failed to marshal request: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, ghlBaseURL+path, reqBody)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("Version", ghlAPIVersion)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ghl: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ghl: failed to read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("ghl: %s %s returned %d: %s", method, path, resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// --- Contacts ---

// FetchRecentContacts returns contacts added in the last N hours.
func (c *GHLConnector) FetchRecentContacts(hours int, limit int) ([]GHLContact, error) {
	locationID := c.locationID()
	if locationID == "" {
		return nil, fmt.Errorf("ghl: location_id not configured")
	}

	since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
	path := fmt.Sprintf("/contacts/?locationId=%s&startAfter=%s&limit=%d&sortBy=date_added&sortOrder=desc",
		locationID,
		url.QueryEscape(since.Format("2006-01-02T15:04:05Z")),
		limit,
	)

	data, err := c.request("GET", path, nil)
	if err != nil {
		return nil, err
	}

	var result struct {
		Contacts []GHLContact `json:"contacts"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("ghl: failed to parse contacts: %w", err)
	}

	return result.Contacts, nil
}

// FetchContactByPhone returns the first contact whose phone matches, or nil
// if none. Used by the voice plugin's pre-call lookup adapter.
func (c *GHLConnector) FetchContactByPhone(phone string) (*GHLContact, error) {
	locationID := c.locationID()
	if locationID == "" {
		return nil, fmt.Errorf("ghl: location_id not configured")
	}
	if phone == "" {
		return nil, nil
	}
	path := fmt.Sprintf("/contacts/?locationId=%s&query=%s&limit=1",
		locationID, url.QueryEscape(phone))
	data, err := c.request("GET", path, nil)
	if err != nil {
		return nil, err
	}
	var result struct {
		Contacts []GHLContact `json:"contacts"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("ghl: failed to parse contacts: %w", err)
	}
	if len(result.Contacts) == 0 {
		return nil, nil
	}
	return &result.Contacts[0], nil
}

// GetContact returns a single contact by ID.
func (c *GHLConnector) GetContact(contactID string) (*GHLContact, error) {
	data, err := c.request("GET", "/contacts/"+contactID, nil)
	if err != nil {
		return nil, err
	}

	var result struct {
		Contact GHLContact `json:"contact"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("ghl: failed to parse contact: %w", err)
	}

	return &result.Contact, nil
}

// TagContact adds tags to a contact.
func (c *GHLConnector) TagContact(contactID string, tags []string) error {
	body := map[string]interface{}{
		"tags": tags,
	}
	_, err := c.request("PUT", "/contacts/"+contactID, body)
	return err
}

// --- Opportunities ---

// FetchOpportunities returns opportunities for a pipeline, optionally filtered by stage.
func (c *GHLConnector) FetchOpportunities(pipelineID string, stageID string, limit int) ([]GHLOpportunity, error) {
	locationID := c.locationID()
	path := fmt.Sprintf("/opportunities/search?location_id=%s&pipeline_id=%s&limit=%d",
		locationID, pipelineID, limit,
	)
	if stageID != "" {
		path += "&pipeline_stage_id=" + stageID
	}

	data, err := c.request("GET", path, nil)
	if err != nil {
		return nil, err
	}

	var result struct {
		Opportunities []GHLOpportunity `json:"opportunities"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("ghl: failed to parse opportunities: %w", err)
	}

	return result.Opportunities, nil
}

// FetchStaleOpportunities returns opportunities with no activity in the last N days.
func (c *GHLConnector) FetchStaleOpportunities(pipelineID string, staleDays int, limit int) ([]GHLOpportunity, error) {
	opps, err := c.FetchOpportunities(pipelineID, "", limit)
	if err != nil {
		return nil, err
	}

	cutoff := time.Now().UTC().Add(-time.Duration(staleDays) * 24 * time.Hour)
	var stale []GHLOpportunity

	for _, opp := range opps {
		if opp.Status == "open" {
			lastActivity, err := time.Parse(time.RFC3339, opp.LastActivity)
			if err != nil {
				lastActivity, err = time.Parse("2006-01-02T15:04:05.000Z", opp.LastActivity)
			}
			if err == nil && lastActivity.Before(cutoff) {
				stale = append(stale, opp)
			}
		}
	}

	return stale, nil
}

// UpdateOpportunityStage moves an opportunity to a new pipeline stage.
func (c *GHLConnector) UpdateOpportunityStage(oppID string, stageID string) error {
	body := map[string]interface{}{
		"pipelineStageId": stageID,
	}
	_, err := c.request("PUT", "/opportunities/"+oppID, body)
	return err
}

// FetchPipelines returns all pipelines and their stages.
func (c *GHLConnector) FetchPipelines() ([]GHLPipeline, error) {
	locationID := c.locationID()
	data, err := c.request("GET", "/opportunities/pipelines?locationId="+locationID, nil)
	if err != nil {
		return nil, err
	}

	var result struct {
		Pipelines []GHLPipeline `json:"pipelines"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("ghl: failed to parse pipelines: %w", err)
	}

	return result.Pipelines, nil
}

// --- Conversations ---

// FetchUnreadConversations returns conversations with unread messages.
func (c *GHLConnector) FetchUnreadConversations(limit int) ([]GHLConversation, error) {
	locationID := c.locationID()
	path := fmt.Sprintf("/conversations/search?locationId=%s&limit=%d&sortBy=last_message_date&sortOrder=desc",
		locationID, limit,
	)

	data, err := c.request("GET", path, nil)
	if err != nil {
		return nil, err
	}

	var result struct {
		Conversations []GHLConversation `json:"conversations"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("ghl: failed to parse conversations: %w", err)
	}

	return result.Conversations, nil
}

// FetchMessages returns messages for a conversation.
func (c *GHLConnector) FetchMessages(conversationID string, limit int) ([]GHLMessage, error) {
	path := fmt.Sprintf("/conversations/%s/messages?limit=%d&sortOrder=desc", conversationID, limit)

	data, err := c.request("GET", path, nil)
	if err != nil {
		return nil, err
	}

	var result struct {
		Messages []GHLMessage `json:"messages"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("ghl: failed to parse messages: %w", err)
	}

	return result.Messages, nil
}

// SendMessage sends a message through GHL (SMS, Email, WhatsApp).
// This should ONLY be called after HITL approval.
func (c *GHLConnector) SendMessage(contactID string, msgType string, body string) error {
	if c.config.Permission != "write" {
		return fmt.Errorf("ghl: send requires write permission (current: %s)", c.config.Permission)
	}

	typeMap := map[string]string{
		"sms":      "SMS",
		"email":    "Email",
		"whatsapp": "WhatsApp",
	}

	ghlType, ok := typeMap[strings.ToLower(msgType)]
	if !ok {
		return fmt.Errorf("ghl: unknown message type: %s (supported: sms, email, whatsapp)", msgType)
	}

	payload := map[string]interface{}{
		"type":      ghlType,
		"contactId": contactID,
		"message":   body,
	}

	_, err := c.request("POST", "/conversations/messages", payload)
	return err
}

// --- Webhooks ---

// WebhookPayload represents an inbound GHL webhook event.
type GHLWebhookPayload struct {
	Type       string          `json:"type"` // ContactCreate, OpportunityStageUpdate, InboundMessage, etc.
	LocationID string          `json:"locationId"`
	Body       json.RawMessage `json:"body"`
}

// ParseWebhook parses and validates an inbound GHL webhook payload.
func ParseGHLWebhook(data []byte) (*GHLWebhookPayload, error) {
	var payload GHLWebhookPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("ghl: invalid webhook payload: %w", err)
	}
	if payload.Type == "" {
		return nil, fmt.Errorf("ghl: webhook missing event type")
	}
	return &payload, nil
}

// --- Formatting ---

// FormatContactsForPrompt formats contacts for LLM consumption.
func FormatContactsForPrompt(contacts []GHLContact) string {
	if len(contacts) == 0 {
		return "No contacts."
	}

	var sb strings.Builder
	for i, c := range contacts {
		name := strings.TrimSpace(c.FirstName + " " + c.LastName)
		if name == "" {
			name = "(unnamed)"
		}
		sb.WriteString(fmt.Sprintf("%d. %s", i+1, name))
		if c.Email != "" {
			sb.WriteString(fmt.Sprintf(" <%s>", c.Email))
		}
		if c.Phone != "" {
			sb.WriteString(fmt.Sprintf(" %s", c.Phone))
		}
		if c.Source != "" {
			sb.WriteString(fmt.Sprintf(" [source: %s]", c.Source))
		}
		if len(c.Tags) > 0 {
			sb.WriteString(fmt.Sprintf(" tags: %s", strings.Join(c.Tags, ", ")))
		}
		sb.WriteString(fmt.Sprintf(" added: %s", c.DateAdded))
		sb.WriteString("\n")
	}
	return sb.String()
}

// FormatOpportunitiesForPrompt formats opportunities for LLM consumption.
func FormatOpportunitiesForPrompt(opps []GHLOpportunity) string {
	if len(opps) == 0 {
		return "No opportunities."
	}

	var sb strings.Builder
	for i, o := range opps {
		sb.WriteString(fmt.Sprintf("%d. %s [%s]", i+1, o.Name, o.Status))
		if o.MonetaryValue > 0 {
			sb.WriteString(fmt.Sprintf(" $%.0f", o.MonetaryValue))
		}
		if o.StageName != "" {
			sb.WriteString(fmt.Sprintf(" stage: %s", o.StageName))
		}
		if o.LastActivity != "" {
			sb.WriteString(fmt.Sprintf(" last_activity: %s", o.LastActivity))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// FormatConversationsForPrompt formats conversations for LLM consumption.
func FormatConversationsForPrompt(convos []GHLConversation) string {
	if len(convos) == 0 {
		return "No conversations."
	}

	var sb strings.Builder
	for i, c := range convos {
		sb.WriteString(fmt.Sprintf("%d. [%s] %s", i+1, c.LastType, c.LastMessage))
		if c.LastDate != "" {
			sb.WriteString(fmt.Sprintf(" (%s)", c.LastDate))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// --- Helpers ---

func (c *GHLConnector) locationID() string {
	if c.config.LocationID != "" {
		return c.config.LocationID
	}
	return c.token.LocationID
}
