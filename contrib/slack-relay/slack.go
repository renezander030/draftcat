package main

// The Slack side of the relay: request signing, the three Web API calls it
// makes, and the Block Kit message. No Slack SDK: the surface is three JSON
// POSTs and one HMAC, and a dependency would be more code to audit than that.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	slackTSHeader  = "X-Slack-Request-Timestamp"
	slackSigHeader = "X-Slack-Signature"
	// Slack's own guidance: refuse a request whose timestamp is more than five
	// minutes from now, so a captured click cannot be replayed later.
	slackMaxSkew = 5 * time.Minute

	// Block Kit limits (characters as Slack counts them, UTF-16 units).
	maxSectionText = 3000
	// Slack truncates a message beyond 40,000 characters. The draft gets the
	// room that leaves after the header, facts and footer.
	maxDraftShown = 36000

	actionPrefix  = "draftcat_"
	blockDecide   = "draftcat_decide"
	blockAdjust   = "draftcat_adjust"
	actionAdjustT = "adjust_text"
)

// verifySlackSignature checks Slack's v0 request signature: HMAC-SHA256 with
// the app's signing secret over "v0:<timestamp>:<raw body>".
func verifySlackSignature(secret []byte, tsHeader, sigHeader string, body []byte, now time.Time) error {
	if tsHeader == "" || sigHeader == "" {
		return errors.New("missing slack signature headers")
	}
	ts, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		return errors.New("unparseable slack timestamp")
	}
	skew := now.Sub(time.Unix(ts, 0))
	if skew > slackMaxSkew || skew < -slackMaxSkew {
		return fmt.Errorf("slack timestamp outside the %s window", slackMaxSkew)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("v0:" + tsHeader + ":"))
	mac.Write(body)
	want := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sigHeader)) {
		return errors.New("slack signature mismatch")
	}
	return nil
}

// slackSign is the sender side of verifySlackSignature, used by tests.
func slackSign(secret []byte, ts string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write(body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

type slackClient struct {
	base  string // e.g. https://slack.com/api
	token string
	http  *http.Client
}

type slackResp struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

// call POSTs one Web API method. The token travels only in the Authorization
// header and never appears in a returned error.
func (c *slackClient) call(ctx context.Context, method string, payload any) (slackResp, error) {
	var out slackResp
	body, err := json.Marshal(payload)
	if err != nil {
		return out, fmt.Errorf("slack %s: encode: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.base, "/")+"/"+method, bytes.NewReader(body))
	if err != nil {
		return out, fmt.Errorf("slack %s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return out, fmt.Errorf("slack %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return out, fmt.Errorf("slack %s: read: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("slack %s: HTTP %d", method, resp.StatusCode)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("slack %s: decode: %w", method, err)
	}
	if !out.OK {
		return out, fmt.Errorf("slack %s: %s", method, out.Error)
	}
	return out, nil
}

func (c *slackClient) postMessage(ctx context.Context, channel, text string, blocks []any) (slackResp, error) {
	return c.call(ctx, "chat.postMessage", map[string]any{
		"channel": channel, "text": text, "blocks": blocks,
		"unfurl_links": false, "unfurl_media": false,
	})
}

func (c *slackClient) update(ctx context.Context, channel, ts, text string, blocks []any) error {
	_, err := c.call(ctx, "chat.update", map[string]any{
		"channel": channel, "ts": ts, "text": text, "blocks": blocks,
	})
	return err
}

func (c *slackClient) ephemeral(ctx context.Context, channel, user, text string) error {
	_, err := c.call(ctx, "chat.postEphemeral", map[string]any{
		"channel": channel, "user": user, "text": text,
	})
	return err
}

// --- Block Kit ---

// view is what the message shows besides the dispatch itself.
type view struct {
	buttons bool   // decision buttons (and the adjust box) are live
	status  string // mrkdwn status line under the draft, e.g. who decided
}

// summary is the message's top-level text: what notifications and screen
// readers show. Kept short; the blocks carry the content.
func summary(d *dispatch) string {
	if !d.isApproval() {
		return "draftcat notification"
	}
	return "draftcat approval needed: " + esc(clip(d.Pipeline, 100)) + " / " + esc(clip(d.Step, 100))
}

// renderBlocks builds the message for d. The payload hash line is always
// present, whatever the draft's length: it is what ties the text a human saw to
// the decision draftcat verifies.
func renderBlocks(d *dispatch, v view, mention func(identity string) string) []any {
	var blocks []any
	if d.isApproval() {
		blocks = append(blocks, header("draftcat needs your approval"))
		fields := []any{
			mrkdwn("*Pipeline*\n" + esc(clip(d.Pipeline, 200))),
			mrkdwn("*Step*\n" + esc(clip(d.Step, 200))),
			mrkdwn("*Risk*\n" + esc(clip(orNormal(d.Risk), 40))),
			mrkdwn("*Expires*\n" + slackDate(d.ExpiresAt)),
		}
		if d.Quorum.Required > 1 {
			fields = append(fields, mrkdwn(fmt.Sprintf("*Quorum*\n%d approvals", d.Quorum.Required)))
		}
		blocks = append(blocks, map[string]any{"type": "section", "fields": fields})
		var ctx []string
		if len(d.Approvers) > 0 {
			names := make([]string, 0, len(d.Approvers))
			for i, id := range d.Approvers {
				if i == 20 {
					names = append(names, fmt.Sprintf("and %d more", len(d.Approvers)-20))
					break
				}
				names = append(names, mention(id))
			}
			ctx = append(ctx, "Approvers: "+strings.Join(names, ", "))
		}
		if b := d.Budget; b != nil {
			line := fmt.Sprintf("Spend today %.2f", b.SpentToday)
			if b.CapToday > 0 {
				line += fmt.Sprintf(" of %.2f", b.CapToday)
			}
			if b.Unit != "" {
				line += " " + esc(clip(b.Unit, 10))
			}
			if b.CapPipeline > 0 {
				line += fmt.Sprintf(" · this pipeline %.2f of %.2f", b.SpentPipeline, b.CapPipeline)
			}
			ctx = append(ctx, line)
		}
		if len(ctx) > 0 {
			blocks = append(blocks, contextBlock(clip(strings.Join(ctx, "\n"), maxSectionText)))
		}
	} else {
		blocks = append(blocks, header("draftcat"))
	}

	blocks = append(blocks, draftBlocks(d.Draft.Body)...)

	foot := "payload_hash `" + esc(d.PayloadHash) + "`"
	if d.ApprovalID != "" {
		foot += " · approval `" + esc(clip(d.ApprovalID, 128)) + "`"
	}
	blocks = append(blocks, contextBlock(foot))
	if v.status != "" {
		blocks = append(blocks, map[string]any{"type": "section", "text": mrkdwn(clip(v.status, maxSectionText))})
	}
	if !v.buttons || !d.isApproval() {
		return blocks
	}

	if d.offers(verbAdjust) {
		blocks = append(blocks, map[string]any{
			"type": "input", "block_id": blockAdjust, "optional": true,
			"label": plain("Adjust"),
			"element": map[string]any{
				"type": "plain_text_input", "action_id": actionAdjustT,
				"multiline": true, "max_length": maxSectionText,
				"placeholder": plain("Describe the change, then press Adjust"),
			},
		})
	}
	var buttons []any
	for _, verb := range d.verbs() {
		b := map[string]any{
			"type": "button", "action_id": actionPrefix + verb,
			"value": d.ApprovalID, "text": plain(buttonLabel(verb)),
		}
		switch verb {
		case verbApprove:
			b["style"] = "primary"
		case verbSkip:
			b["style"] = "danger"
		}
		buttons = append(buttons, b)
	}
	return append(blocks, map[string]any{"type": "actions", "block_id": blockDecide, "elements": buttons})
}

// draftBlocks renders the draft as plain_text sections (no mrkdwn: a draft
// containing <!channel> or a link must show as text, not ping or unfurl), split
// at Slack's per-block limit. A draft longer than a message can carry is cut,
// and the cut is said out loud rather than left for the reader to miss.
func draftBlocks(body string) []any {
	if strings.TrimSpace(body) == "" {
		return []any{contextBlock("_The draft is empty._")}
	}
	total := utf16Len(body)
	shown := body
	truncated := total > maxDraftShown
	if truncated {
		shown = cutUTF16(body, maxDraftShown)
	}
	chunks := chunkText(shown, maxSectionText)
	var out []any
	if len(chunks) > 1 || truncated {
		out = append(out, contextBlock(fmt.Sprintf("Draft, %d characters, shown in %d parts.", total, len(chunks))))
	}
	for _, c := range chunks {
		out = append(out, map[string]any{"type": "section", "text": map[string]any{"type": "plain_text", "text": c, "emoji": false}})
	}
	if truncated {
		out = append(out, contextBlock(fmt.Sprintf(
			":warning: *Draft truncated.* Slack shows the first %d of %d characters. The payload hash below covers the full draft, and approving approves all of it.",
			utf16Len(shown), total)))
	}
	return out
}

// chunkText splits s into pieces of at most limit UTF-16 units, preferring to
// break after a newline, then after a space, in the second half of a piece.
func chunkText(s string, limit int) []string {
	var out []string
	for utf16Len(s) > limit {
		head := cutUTF16(s, limit)
		cut := len(head)
		if i := strings.LastIndexByte(head, '\n'); i >= len(head)/2 {
			cut = i + 1
		} else if i := strings.LastIndexByte(head, ' '); i >= len(head)/2 {
			cut = i + 1
		}
		if cut == 0 {
			_, cut = utf8.DecodeRuneInString(s)
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

// cutUTF16 returns the longest prefix of s, on a rune boundary, that is at most
// limit UTF-16 units long.
func cutUTF16(s string, limit int) string {
	n := 0
	for i, r := range s {
		w := utf16.RuneLen(r)
		if w < 0 {
			w = 1
		}
		if n+w > limit {
			return s[:i]
		}
		n += w
	}
	return s
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if w := utf16.RuneLen(r); w > 0 {
			n += w
		} else {
			n++
		}
	}
	return n
}

func header(text string) map[string]any {
	return map[string]any{"type": "header", "text": plain(clip(text, 150))}
}

func contextBlock(md string) map[string]any {
	return map[string]any{"type": "context", "elements": []any{mrkdwn(clip(md, maxSectionText))}}
}

func plain(s string) map[string]any { return map[string]any{"type": "plain_text", "text": s} }

func mrkdwn(s string) map[string]any { return map[string]any{"type": "mrkdwn", "text": s} }

func buttonLabel(verb string) string {
	switch verb {
	case verbApprove:
		return "Approve"
	case verbSkip:
		return "Skip"
	default:
		return "Adjust"
	}
}

// slackDate renders an RFC 3339 time in each reader's own time zone, with the
// raw value as the fallback.
func slackDate(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return esc(clip(rfc3339, 64))
	}
	return fmt.Sprintf("<!date^%d^{date_short_pretty} {time}|%s>", t.Unix(), t.UTC().Format(time.RFC3339))
}

// esc escapes the three characters Slack's mrkdwn treats as control syntax, so
// a pipeline name cannot become a mention or a link.
func esc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// clip shortens s to at most n UTF-16 units, marking the cut.
func clip(s string, n int) string {
	if utf16Len(s) <= n {
		return s
	}
	return cutUTF16(s, n-1) + "…"
}

func orNormal(s string) string {
	if s == "" {
		return "normal"
	}
	return s
}
