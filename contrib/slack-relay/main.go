// Command slack-relay is a conformant hitl/v0 relay for Slack.
//
// draftcat dispatches a signed approval to /dispatch. This relay checks it,
// posts the draft to one Slack channel as a Block Kit message with a button for
// each decision the dispatch allows, and waits for a click on Slack's
// interactivity URL (/slack/interactivity). A click from a configured approver
// becomes one signed decision envelope, posted back to draftcat; the message is
// then updated with the outcome and its buttons are removed.
//
// draftcat stays the gate. This relay is a presenter and is treated as
// untrusted: draftcat re-checks the signature, the nonce, the payload hash, the
// approver and the expiry of every decision it receives, so a broken or hostile
// relay can stall an approval into a timeout but cannot authorise anything.
//
// Build:  go build -o slack-relay .
// Verify: draftcat hitl verify http://127.0.0.1:8090/dispatch
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// config is read from the environment only. Secrets never reach a log line.
type config struct {
	Addr          string            // RELAY_ADDR
	RelaySecret   []byte            // DRAFTCAT_RELAY_SECRET
	BotToken      string            // SLACK_BOT_TOKEN
	SigningSecret []byte            // SLACK_SIGNING_SECRET
	Channel       string            // SLACK_CHANNEL_ID
	Approvers     map[string]string // SLACK_APPROVERS: Slack user ID -> draftcat identity
	APIBase       string            // SLACK_API_URL
}

const defaultAPIBase = "https://slack.com/api"

func loadConfig(getenv func(string) string) (config, error) {
	c := config{
		Addr:          strings.TrimSpace(getenv("RELAY_ADDR")),
		RelaySecret:   []byte(getenv("DRAFTCAT_RELAY_SECRET")),
		BotToken:      strings.TrimSpace(getenv("SLACK_BOT_TOKEN")),
		SigningSecret: []byte(strings.TrimSpace(getenv("SLACK_SIGNING_SECRET"))),
		Channel:       strings.TrimSpace(getenv("SLACK_CHANNEL_ID")),
		APIBase:       strings.TrimSpace(getenv("SLACK_API_URL")),
	}
	if c.Addr == "" {
		c.Addr = "127.0.0.1:8090"
	}
	if c.APIBase == "" {
		c.APIBase = defaultAPIBase
	}
	var missing []string
	if len(c.RelaySecret) == 0 {
		missing = append(missing, "DRAFTCAT_RELAY_SECRET")
	}
	if c.BotToken == "" {
		missing = append(missing, "SLACK_BOT_TOKEN")
	}
	if len(c.SigningSecret) == 0 {
		missing = append(missing, "SLACK_SIGNING_SECRET")
	}
	if c.Channel == "" {
		missing = append(missing, "SLACK_CHANNEL_ID")
	}
	if strings.TrimSpace(getenv("SLACK_APPROVERS")) == "" {
		missing = append(missing, "SLACK_APPROVERS")
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("missing required env: %s", strings.Join(missing, ", "))
	}
	approvers, err := parseApprovers(getenv("SLACK_APPROVERS"))
	if err != nil {
		return c, err
	}
	c.Approvers = approvers
	if err := checkAPIBase(c.APIBase); err != nil {
		return c, err
	}
	return c, nil
}

var slackUserID = regexp.MustCompile(`^[UW][A-Z0-9]{2,}$`)

// parseApprovers reads SLACK_APPROVERS: Slack user IDs separated by commas or
// whitespace, each optionally mapped to the identity draftcat knows that
// operator by (relay.operators[].identity), e.g.
//
//	U012ABCDEF=alice@example.com,U034GHIJKL=bob@example.com
//
// An ID without "=" is its own identity. Ambiguity is refused rather than
// guessed at: one Slack user cannot map to two identities, and two Slack users
// cannot share one, since the gate counts quorum by identity.
func parseApprovers(raw string) (map[string]string, error) {
	out := map[string]string{}
	byIdentity := map[string]string{}
	for _, entry := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		user, identity, mapped := strings.Cut(entry, "=")
		user, identity = strings.TrimSpace(user), strings.TrimSpace(identity)
		if !mapped {
			identity = user
		}
		if !slackUserID.MatchString(user) {
			return nil, fmt.Errorf("SLACK_APPROVERS: %q is not a Slack user ID (U… or W…)", user)
		}
		if identity == "" {
			return nil, fmt.Errorf("SLACK_APPROVERS: %s maps to an empty identity", user)
		}
		if _, dup := out[user]; dup {
			return nil, fmt.Errorf("SLACK_APPROVERS: %s is listed twice", user)
		}
		if other, dup := byIdentity[identity]; dup {
			return nil, fmt.Errorf("SLACK_APPROVERS: identity %q is mapped from both %s and %s", identity, other, user)
		}
		out[user] = identity
		byIdentity[identity] = user
	}
	if len(out) == 0 {
		return nil, errors.New("SLACK_APPROVERS lists no approvers")
	}
	return out, nil
}

// checkAPIBase keeps the bot token on TLS: the Slack API base must be https,
// or plain http to a loopback address (a local test double).
func checkAPIBase(base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return fmt.Errorf("SLACK_API_URL %q is not a URL", base)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		if ip := net.ParseIP(u.Hostname()); (ip != nil && ip.IsLoopback()) || u.Hostname() == "localhost" {
			return nil
		}
	}
	return fmt.Errorf("SLACK_API_URL must be https (got %s)", u.Scheme)
}

func main() {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		log.Fatalf("slack-relay: %v", err)
	}
	if err := run(cfg); err != nil {
		log.Fatalf("slack-relay: %v", err)
	}
}

func run(cfg config) error {
	rs := newRelay(cfg)
	srv := &http.Server{Addr: cfg.Addr, Handler: rs.routes(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Printf("slack-relay listening on %s (dispatch: POST /dispatch, Slack interactivity: POST /slack/interactivity, %d approver(s))",
		cfg.Addr, len(cfg.Approvers))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// Let clicks already being forwarded finish before exiting.
	rs.wg.Wait()
	return nil
}
