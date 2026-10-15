package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	"github.com/renezander030/draftcat/internal/redact"
)

// telegramAPIBase is the Bot API root every Telegram call goes to.
var telegramAPIBase = "https://api.telegram.org"

// doctorProbeTimeout bounds each --online probe.
var doctorProbeTimeout = 10 * time.Second

// doctorOnline asks each remote dependency whether it accepts the configured
// credentials: the bot token (getMe), the operator chat (getChat), the
// provider key, and whether the relay answers at all. Every call is
// read-only: nothing is sent to the operator and no tokens are spent.
// Credentials are registered for redaction first, so an error that quotes a
// request URL never prints a token.
func doctorOnline(rep *doctorReport, cfg *config.Config) {
	registerSecrets(cfg)
	client := &http.Client{Timeout: doctorProbeTimeout}

	token := resolveEnv(cfg.Telegram.TokenEnv, config.DefaultTelegramTokenEnv)
	if token == "" {
		rep.add("telegram api", "fail", "no bot token to check", "set the telegram token, then run doctor --online again")
	} else {
		probeTelegram(rep, client, token, cfg)
	}

	probeProvider(rep, client, cfg)

	if cfg.Relay.Enabled() {
		status, err := probeGet(client, cfg.Relay.URL, nil)
		if err != nil {
			rep.add("relay reachable", "fail", redact.String(err.Error()), "check relay.url and that the relay is running")
		} else {
			rep.add("relay reachable", "ok", fmt.Sprintf("%s answered HTTP %d", cfg.Relay.URL, status), "")
		}
	}
}

func probeTelegram(rep *doctorReport, client *http.Client, token string, cfg *config.Config) {
	var me struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			Username string `json:"username"`
		} `json:"result"`
	}
	if err := telegramGet(client, token, "getMe", nil, &me); err != nil {
		rep.add("telegram api", "fail", redact.String(err.Error()), "check network access to "+telegramAPIBase)
		return
	}
	if !me.OK {
		rep.add("telegram api", "fail", "token rejected: "+orDash(me.Description), "create a new token with @BotFather and export it")
		return
	}
	rep.add("telegram api", "ok", "token accepted for @"+me.Result.Username, "")

	chatID := cfg.Telegram.ChatID
	if chatID == 0 {
		if v := strings.TrimSpace(os.Getenv("DRAFTCAT_TG_CHAT_ID")); v != "" {
			chatID, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	if chatID == 0 {
		return // the offline operators check names the missing chat
	}
	var chat struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			Type  string `json:"type"`
			Title string `json:"title"`
		} `json:"result"`
	}
	q := url.Values{"chat_id": {strconv.FormatInt(chatID, 10)}}
	if err := telegramGet(client, token, "getChat", q, &chat); err != nil {
		rep.add("telegram chat", "fail", redact.String(err.Error()), "")
		return
	}
	if !chat.OK {
		rep.add("telegram chat", "fail", fmt.Sprintf("chat %d not reachable: %s", chatID, orDash(chat.Description)),
			"start a conversation with the bot (or add it to the group) so it can post approval prompts")
		return
	}
	rep.add("telegram chat", "ok", fmt.Sprintf("bot can reach chat %d (%s)", chatID, chat.Result.Type), "")
}

func telegramGet(client *http.Client, token, method string, q url.Values, out interface{}) error {
	u := fmt.Sprintf("%s/bot%s/%s", telegramAPIBase, token, method)
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	ctx, cancel := context.WithTimeout(context.Background(), doctorProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: HTTP %d, unreadable answer", method, resp.StatusCode)
	}
	return nil
}

// probeProvider checks the key without a completion: OpenRouter's key
// endpoint, or the model list of any other OpenAI-compatible endpoint.
func probeProvider(rep *doctorReport, client *http.Client, cfg *config.Config) {
	base := strings.TrimRight(strings.TrimSpace(cfg.Provider.BaseURL), "/")
	if base == "" {
		rep.add("provider api", "fail", "provider.base_url is empty", "set provider.base_url (OpenRouter: https://openrouter.ai/api/v1)")
		return
	}
	key := resolveEnv(cfg.Provider.APIKeyEnv, config.DefaultProviderKeyEnv)
	if key == "" {
		rep.add("provider api", "fail", "no provider key to check", "set the provider key, then run doctor --online again")
		return
	}
	endpoint := base + "/models"
	if cfg.Provider.IsOpenRouter() {
		endpoint = base + "/key"
	}
	status, err := probeGet(client, endpoint, map[string]string{"Authorization": "Bearer " + key})
	switch {
	case err != nil:
		rep.add("provider api", "fail", redact.String(err.Error()), "check network access to "+base)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		rep.add("provider api", "fail", fmt.Sprintf("key rejected (HTTP %d)", status), "export a valid key in "+cfg.Provider.APIKeyEnvName())
	case status >= 200 && status < 300:
		rep.add("provider api", "ok", fmt.Sprintf("key accepted by %s", base), "")
	default:
		rep.add("provider api", "warn", fmt.Sprintf("%s answered HTTP %d; the key was not confirmed", endpoint, status), "")
	}
}

// probeGet issues one GET and reports the status code; any HTTP answer counts
// as reachable.
func probeGet(client *http.Client, u string, headers map[string]string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), doctorProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
