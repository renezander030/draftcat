package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// --- Gmail Connector ---
// Outbound only. Polls Gmail API via OAuth. Read-only by default.

type GmailConfig struct {
	TokenPath  string `yaml:"token_path"`
	Scopes     string `yaml:"scopes"`     // enforced: gmail.readonly unless explicitly changed
	Permission string `yaml:"permission"` // read | draft | send
}

type GmailToken struct {
	Token        string   `json:"token"`
	RefreshToken string   `json:"refresh_token"`
	TokenURI     string   `json:"token_uri"`
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"`
	Expiry       string   `json:"expiry"`
	Scopes       []string `json:"scopes"`
}

type GmailConnector struct {
	token       GmailToken
	tokenPath   string
	accessToken string
	expiresAt   time.Time
}

type Email struct {
	ID      string
	From    string
	To      string
	Subject string
	Date    string
	Snippet string
	Body    string
	Labels  []string
}

func NewGmailConnector(tokenPath string) (*GmailConnector, error) {
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read gmail token: %w", err)
	}
	var token GmailToken
	if err := json.Unmarshal(data, &token); err != nil {
		return nil, fmt.Errorf("failed to parse gmail token: %w", err)
	}
	g := &GmailConnector{
		token:     token,
		tokenPath: tokenPath,
	}
	// Get initial access token
	if err := g.refreshAccessToken(); err != nil {
		return nil, fmt.Errorf("failed to get access token: %w", err)
	}
	log.Printf("[gmail] connector initialized")
	return g, nil
}

func (g *GmailConnector) refreshAccessToken() error {
	data := url.Values{
		"client_id":     {g.token.ClientID},
		"client_secret": {g.token.ClientSecret},
		"refresh_token": {g.token.RefreshToken},
		"grant_type":    {"refresh_token"},
	}
	resp, err := http.PostForm(g.token.TokenURI, data)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	json.Unmarshal(body, &result)
	if result.Error != "" {
		return fmt.Errorf("token refresh failed: %s", result.Error)
	}
	g.accessToken = result.AccessToken
	g.expiresAt = time.Now().Add(time.Duration(result.ExpiresIn-60) * time.Second)

	// Save refreshed token back — preserve all fields
	g.token.Token = result.AccessToken
	g.saveToken()

	log.Printf("[gmail] access token refreshed, expires in %ds", result.ExpiresIn)
	return nil
}

func (g *GmailConnector) saveToken() {
	// Read existing file first to preserve fields we don't track (e.g. scopes from prior auth)
	existing := make(map[string]interface{})
	if data, err := os.ReadFile(g.tokenPath); err == nil {
		json.Unmarshal(data, &existing)
	}

	// Update only the fields we manage
	existing["token"] = g.token.Token
	existing["refresh_token"] = g.token.RefreshToken
	existing["token_uri"] = g.token.TokenURI
	existing["client_id"] = g.token.ClientID
	existing["client_secret"] = g.token.ClientSecret

	// Only write scopes if we have them (from ExchangeCode)
	if len(g.token.Scopes) > 0 {
		existing["scopes"] = g.token.Scopes
	}

	tokenData, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(g.tokenPath, tokenData, 0600); err != nil {
		log.Printf("[gmail] WARNING: failed to save token: %v", err)
	}
}

func (g *GmailConnector) ensureToken() error {
	if time.Now().After(g.expiresAt) {
		return g.refreshAccessToken()
	}
	return nil
}

func (g *GmailConnector) apiGet(endpoint string) (json.RawMessage, error) {
	if err := g.ensureToken(); err != nil {
		return nil, err
	}
	req, _ := http.NewRequest("GET", "https://gmail.googleapis.com/gmail/v1/users/me/"+endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+g.accessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("gmail API %d: %s", resp.StatusCode, string(body)[:min(200, len(body))])
	}
	return body, nil
}

// FetchRecent fetches the N most recent emails matching a query
func (g *GmailConnector) FetchRecent(query string, maxResults int) ([]Email, error) {
	if maxResults == 0 {
		maxResults = 10
	}
	q := url.QueryEscape(query)
	endpoint := fmt.Sprintf("messages?q=%s&maxResults=%d", q, maxResults)
	raw, err := g.apiGet(endpoint)
	if err != nil {
		return nil, err
	}

	var list struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	json.Unmarshal(raw, &list)

	var emails []Email
	for _, m := range list.Messages {
		email, err := g.GetMessage(m.ID)
		if err != nil {
			log.Printf("[gmail] failed to get message %s: %v", m.ID, err)
			continue
		}
		emails = append(emails, *email)
	}
	return emails, nil
}

// GetMessage fetches a single email by ID
func (g *GmailConnector) GetMessage(id string) (*Email, error) {
	raw, err := g.apiGet("messages/" + id + "?format=full")
	if err != nil {
		return nil, err
	}

	var msg struct {
		ID           string `json:"id"`
		InternalDate string `json:"internalDate"`
		Snippet      string `json:"snippet"`
		Payload      struct {
			Headers []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"headers"`
			Parts []struct {
				MimeType string `json:"mimeType"`
				Body     struct {
					Data string `json:"data"`
				} `json:"body"`
			} `json:"parts"`
			Body struct {
				Data string `json:"data"`
			} `json:"body"`
		} `json:"payload"`
		LabelIds []string `json:"labelIds"`
	}
	json.Unmarshal(raw, &msg)

	email := Email{
		ID:      msg.ID,
		Snippet: msg.Snippet,
		Labels:  msg.LabelIds,
	}

	// Extract headers
	for _, h := range msg.Payload.Headers {
		switch h.Name {
		case "From":
			email.From = h.Value
		case "To":
			email.To = h.Value
		case "Subject":
			email.Subject = h.Value
		}
	}

	// Use internalDate for accurate relative time
	var epoch int64
	fmt.Sscanf(msg.InternalDate, "%d", &epoch)
	email.Date = timeAgo(time.Unix(epoch/1000, 0))

	// Extract body
	if len(msg.Payload.Parts) > 0 {
		for _, part := range msg.Payload.Parts {
			if part.MimeType == "text/plain" && part.Body.Data != "" {
				decoded, _ := base64.URLEncoding.DecodeString(part.Body.Data)
				email.Body = string(decoded)
				break
			}
		}
	} else if msg.Payload.Body.Data != "" {
		decoded, _ := base64.URLEncoding.DecodeString(msg.Payload.Body.Data)
		email.Body = string(decoded)
	}

	// Truncate body for token efficiency
	if len(email.Body) > 500 {
		email.Body = email.Body[:500] + "..."
	}

	return &email, nil
}

// FetchUnread fetches unread emails
func (g *GmailConnector) FetchUnread(maxResults int) ([]Email, error) {
	return g.FetchRecent("is:unread", maxResults)
}

// FetchThread fetches all messages in a thread (sent + received), ordered chronologically
func (g *GmailConnector) FetchThread(threadID string) ([]Email, error) {
	raw, err := g.apiGet("threads/" + threadID + "?format=full")
	if err != nil {
		return nil, err
	}

	var thread struct {
		ID       string `json:"id"`
		Messages []struct {
			ID           string `json:"id"`
			InternalDate string `json:"internalDate"`
			Snippet      string `json:"snippet"`
			Payload      struct {
				Headers []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"headers"`
				Parts []struct {
					MimeType string `json:"mimeType"`
					Body     struct {
						Data string `json:"data"`
					} `json:"body"`
				} `json:"parts"`
				Body struct {
					Data string `json:"data"`
				} `json:"body"`
			} `json:"payload"`
			LabelIds []string `json:"labelIds"`
		} `json:"messages"`
	}
	json.Unmarshal(raw, &thread)

	type msgWithTime struct {
		email Email
		epoch int64
	}
	var msgsWithTime []msgWithTime
	for _, msg := range thread.Messages {
		email := Email{ID: msg.ID, Snippet: msg.Snippet, Labels: msg.LabelIds}
		for _, h := range msg.Payload.Headers {
			switch h.Name {
			case "From":
				email.From = h.Value
			case "To":
				email.To = h.Value
			case "Subject":
				email.Subject = h.Value
			}
		}
		// Use internalDate (epoch ms) for accurate chronological ordering
		var epoch int64
		fmt.Sscanf(msg.InternalDate, "%d", &epoch)
		email.Date = time.Unix(epoch/1000, 0).Format("2006-01-02 15:04")
		// Extract body
		if len(msg.Payload.Parts) > 0 {
			for _, part := range msg.Payload.Parts {
				if part.MimeType == "text/plain" && part.Body.Data != "" {
					decoded, _ := base64.URLEncoding.DecodeString(part.Body.Data)
					email.Body = string(decoded)
					break
				}
			}
		} else if msg.Payload.Body.Data != "" {
			decoded, _ := base64.URLEncoding.DecodeString(msg.Payload.Body.Data)
			email.Body = string(decoded)
		}
		if len(email.Body) > 500 {
			email.Body = email.Body[:500] + "..."
		}
		msgsWithTime = append(msgsWithTime, msgWithTime{email: email, epoch: epoch})
	}

	// Sort chronologically by internalDate
	sort.Slice(msgsWithTime, func(i, j int) bool {
		return msgsWithTime[i].epoch < msgsWithTime[j].epoch
	})

	var emails []Email
	for _, m := range msgsWithTime {
		emails = append(emails, m.email)
	}
	return emails, nil
}

// FormatThreadForPrompt formats a thread as a conversation for LLM summarization
func FormatThreadForPrompt(emails []Email, myAddress string) string {
	var sb strings.Builder
	for _, e := range emails {
		direction := "RECEIVED"
		fromAddr := ExtractEmailAddress(e.From)
		if strings.EqualFold(fromAddr, myAddress) {
			direction = "SENT"
		}
		from := e.From
		if idx := strings.Index(from, "<"); idx > 0 {
			from = strings.TrimSpace(from[:idx])
		}
		from = strings.Trim(from, "\"")

		sb.WriteString(fmt.Sprintf("[%s] %s — %s\n%s\n\n", direction, from, e.Date, e.Body))
	}
	return sb.String()
}

// FormatForPrompt turns emails into a compact string for LLM consumption
func FormatEmailsForPrompt(emails []Email) string {
	if len(emails) == 0 {
		return "No emails found."
	}
	var sb strings.Builder
	for i, e := range emails {
		// Clean up from field
		from := e.From
		if idx := strings.Index(from, "<"); idx > 0 {
			from = strings.TrimSpace(from[:idx])
		}
		from = strings.Trim(from, "\"")

		subject := e.Subject
		if len(subject) > 80 {
			subject = subject[:80] + "..."
		}

		to := e.To
		if idx := strings.Index(to, "<"); idx > 0 {
			to = strings.TrimSpace(to[:idx])
		}
		to = strings.Trim(to, "\"")

		sb.WriteString(fmt.Sprintf("%d. %s | From: %s | To: %s | %s\n   %s\n", i+1, e.Date, from, to, subject, e.Snippet))
		if i < len(emails)-1 {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("Jan 2")
	}
}

// ExtractEmailAddress pulls email from "Name <email>" format
func ExtractEmailAddress(from string) string {
	re := regexp.MustCompile(`<([^>]+)>`)
	matches := re.FindStringSubmatch(from)
	if len(matches) > 1 {
		return matches[1]
	}
	return from
}

// --- Write operations (require gmail.send or gmail.compose scope) ---

func (g *GmailConnector) apiPost(endpoint string, body []byte) (json.RawMessage, error) {
	if err := g.ensureToken(); err != nil {
		return nil, err
	}
	req, _ := http.NewRequest("POST", "https://gmail.googleapis.com/gmail/v1/users/me/"+endpoint, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+g.accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gmail API %d: %s", resp.StatusCode, string(respBody)[:min(300, len(respBody))])
	}
	return respBody, nil
}

// BuildReplyRaw creates a raw RFC 2822 message for replying
func BuildReplyRaw(to, subject, inReplyTo, references, body string) string {
	if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = "Re: " + subject
	}
	msg := fmt.Sprintf("To: %s\r\nSubject: %s\r\nIn-Reply-To: %s\r\nReferences: %s\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s",
		to, subject, inReplyTo, references, body)
	return base64.URLEncoding.EncodeToString([]byte(msg))
}

// CreateDraft creates a draft reply (requires gmail.compose scope)
func (g *GmailConnector) CreateDraft(to, subject, inReplyTo, references, threadID, body string) error {
	raw := BuildReplyRaw(to, subject, inReplyTo, references, body)
	payload := map[string]interface{}{
		"message": map[string]interface{}{
			"raw":      raw,
			"threadId": threadID,
		},
	}
	data, _ := json.Marshal(payload)
	_, err := g.apiPost("drafts", data)
	return err
}

// SendReply sends a reply directly (requires gmail.send scope)
func (g *GmailConnector) SendReply(to, subject, inReplyTo, references, threadID, body string) error {
	raw := BuildReplyRaw(to, subject, inReplyTo, references, body)
	payload := map[string]interface{}{
		"raw":      raw,
		"threadId": threadID,
	}
	data, _ := json.Marshal(payload)
	_, err := g.apiPost("messages/send", data)
	return err
}

// GetFullMessage fetches a message with thread ID and message-id header for replying
func (g *GmailConnector) GetFullMessage(id string) (*Email, string, string, string, error) {
	raw, err := g.apiGet("messages/" + id + "?format=full")
	if err != nil {
		return nil, "", "", "", err
	}

	var msg struct {
		ID       string `json:"id"`
		ThreadId string `json:"threadId"`
		Snippet  string `json:"snippet"`
		Payload  struct {
			Headers []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"headers"`
			Parts []struct {
				MimeType string `json:"mimeType"`
				Body     struct {
					Data string `json:"data"`
				} `json:"body"`
			} `json:"parts"`
			Body struct {
				Data string `json:"data"`
			} `json:"body"`
		} `json:"payload"`
	}
	json.Unmarshal(raw, &msg)

	email := Email{ID: msg.ID, Snippet: msg.Snippet}
	var messageID, references string
	for _, h := range msg.Payload.Headers {
		switch h.Name {
		case "From":
			email.From = h.Value
		case "To":
			email.To = h.Value
		case "Subject":
			email.Subject = h.Value
		case "Date":
			email.Date = h.Value
		case "Message-ID", "Message-Id":
			messageID = h.Value
		case "References":
			references = h.Value
		}
	}

	// Extract body
	if len(msg.Payload.Parts) > 0 {
		for _, part := range msg.Payload.Parts {
			if part.MimeType == "text/plain" && part.Body.Data != "" {
				decoded, _ := base64.URLEncoding.DecodeString(part.Body.Data)
				email.Body = string(decoded)
				break
			}
		}
	} else if msg.Payload.Body.Data != "" {
		decoded, _ := base64.URLEncoding.DecodeString(msg.Payload.Body.Data)
		email.Body = string(decoded)
	}

	return &email, msg.ThreadId, messageID, references, nil
}

// GenerateAuthURL generates an OAuth consent URL with localhost redirect
func GenerateAuthURL(clientID string, scopes []string, port int) string {
	params := url.Values{
		"client_id":     {clientID},
		"redirect_uri":  {fmt.Sprintf("http://localhost:%d", port)},
		"response_type": {"code"},
		"scope":         {strings.Join(scopes, " ")},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
	}
	return "https://accounts.google.com/o/oauth2/v2/auth?" + params.Encode()
}

// ExchangeCode exchanges an auth code for tokens and saves them
func (g *GmailConnector) ExchangeCode(code string, port int) error {
	data := url.Values{
		"client_id":     {g.token.ClientID},
		"client_secret": {g.token.ClientSecret},
		"code":          {code},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {fmt.Sprintf("http://localhost:%d", port)},
	}
	resp, err := http.PostForm(g.token.TokenURI, data)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	json.Unmarshal(body, &result)
	if result.Error != "" {
		return fmt.Errorf("%s: %s", result.Error, result.ErrorDesc)
	}

	g.accessToken = result.AccessToken
	g.expiresAt = time.Now().Add(time.Duration(result.ExpiresIn-60) * time.Second)
	g.token.Token = result.AccessToken
	if result.RefreshToken != "" {
		g.token.RefreshToken = result.RefreshToken
	}
	if result.Scope != "" {
		g.token.Scopes = strings.Split(result.Scope, " ")
	}

	g.saveToken()
	log.Printf("[gmail] re-authorized with scopes: %s", result.Scope)
	return nil
}

// StartAuthCallback starts a temporary HTTP server to catch the OAuth redirect.
// Returns the auth code via the channel, then shuts down.
func StartAuthCallback(port int) chan string {
	codeCh := make(chan string, 1)

	srv := &http.Server{Addr: fmt.Sprintf(":%d", port)}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code != "" {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<h2>Authorization successful</h2><p>You can close this tab.</p>")
			codeCh <- code
			go func() {
				time.Sleep(1 * time.Second)
				srv.Close()
			}()
		} else {
			errMsg := r.URL.Query().Get("error")
			fmt.Fprintf(w, "Error: %s", errMsg)
			codeCh <- ""
			go srv.Close()
		}
	})

	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("[auth] callback server error: %v", err)
		}
	}()

	return codeCh
}
