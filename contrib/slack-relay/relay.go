package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	maxDispatchBytes = 1 << 20
	// Slack's interaction payload carries the whole message back, draft
	// included, so it is allowed more room than a dispatch.
	maxInteractionBytes = 2 << 20
	// Closed approvals are remembered this long past their expiry, so a late
	// click is told what happened instead of "unknown".
	keepClosed = 24 * time.Hour
)

type phase int

const (
	phasePosting  phase = iota // reserved while chat.postMessage is in flight
	phaseOpen                  // buttons live
	phaseDeciding              // one decision is on its way to draftcat
	phaseClosed                // resolved, expired or refused for good
)

// approval is one dispatched approval as this relay sees it. In memory on
// purpose: draftcat owns durability. A restarted relay forgets its approvals,
// a click on an old message is answered with "no longer open", and the gate
// on draftcat's side times out, which does not fire the action.
type approval struct {
	d        dispatch
	channel  string
	ts       string
	expires  time.Time
	phase    phase
	approved []string // Slack users whose approve the gate recorded (quorum)
	outcome  string   // mrkdwn status once closed
	timer    *time.Timer
}

type relayServer struct {
	cfg   config
	slack *slackClient
	http  *http.Client
	now   func() time.Time

	mu        sync.Mutex
	approvals map[string]*approval // approval_id -> state

	// wg tracks click handling that runs after Slack has been answered.
	wg sync.WaitGroup
}

func newRelay(cfg config) *relayServer {
	client := &http.Client{Timeout: 15 * time.Second}
	return &relayServer{
		cfg:       cfg,
		slack:     &slackClient{base: cfg.APIBase, token: cfg.BotToken, http: client},
		http:      client,
		now:       time.Now,
		approvals: map[string]*approval{},
	}
}

func (r *relayServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/dispatch", r.handleDispatch)
	mux.HandleFunc("/slack/interactivity", r.handleInteractivity)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// --- dispatch: draftcat -> relay -> Slack ---

func (r *relayServer) handleDispatch(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxDispatchBytes+1))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	if len(body) > maxDispatchBytes {
		http.Error(w, "dispatch too large", http.StatusRequestEntityTooLarge)
		return
	}
	// Verify draftcat really sent this before reading a byte of it. A relay
	// that renders unsigned dispatches can be used to show an approver a draft
	// draftcat never staged.
	if err := verifySignature(r.cfg.RelaySecret, req.Header.Get(sigHeader), body, r.now()); err != nil {
		log.Printf("dispatch rejected: %v", err)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var d dispatch
	if err := json.Unmarshal(body, &d); err != nil {
		http.Error(w, "malformed json", http.StatusBadRequest)
		return
	}
	expires, err := r.checkDispatch(&d)
	if err != nil {
		log.Printf("dispatch %s rejected: %v", logID(d.ApprovalID), err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if !d.isApproval() {
		if _, err := r.slack.postMessage(req.Context(), r.cfg.Channel, summary(&d), renderBlocks(&d, view{}, r.mention)); err != nil {
			log.Printf("notification not posted: %v", err)
			http.Error(w, "slack refused the message", http.StatusBadGateway)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "accepted"})
		return
	}

	// One message per approval. A repeated dispatch (a retry, or a replay
	// inside the signature window) is acknowledged without posting again.
	r.mu.Lock()
	r.pruneLocked()
	if prev, seen := r.approvals[d.ApprovalID]; seen {
		same := prev.d.Callback.Nonce == d.Callback.Nonce && prev.d.PayloadHash == d.PayloadHash
		r.mu.Unlock()
		if !same {
			http.Error(w, "approval_id already dispatched with a different nonce or payload", http.StatusConflict)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "accepted", "duplicate": true})
		return
	}
	a := &approval{d: d, expires: expires, phase: phasePosting}
	r.approvals[d.ApprovalID] = a
	r.mu.Unlock()

	// Post before acknowledging. Slack answers in well under a second, and a
	// refused post then fails draftcat's dispatch at once instead of leaving a
	// gate open that nobody can see until it times out.
	posted, err := r.slack.postMessage(req.Context(), r.cfg.Channel, summary(&d),
		renderBlocks(&d, view{buttons: true}, r.mention))
	r.mu.Lock()
	if err != nil {
		delete(r.approvals, d.ApprovalID)
		r.mu.Unlock()
		log.Printf("approval %s not posted: %v", logID(d.ApprovalID), err)
		http.Error(w, "slack refused the message", http.StatusBadGateway)
		return
	}
	a.channel, a.ts, a.phase = posted.Channel, posted.TS, phaseOpen
	id := d.ApprovalID
	a.timer = time.AfterFunc(time.Until(expires), func() { r.expire(id) })
	r.mu.Unlock()

	log.Printf("approval %s (%s/%s risk=%s) posted to Slack, awaiting a click",
		logID(id), oneLine(d.Pipeline), oneLine(d.Step), orNormal(oneLine(d.Risk)))
	writeJSON(w, http.StatusOK, map[string]any{"status": "accepted"})
}

// checkDispatch applies the checks the signature alone does not cover. The
// payload hash must match the draft: rendering a draft whose hash does not
// match would let a decision claim a human saw text they never saw.
func (r *relayServer) checkDispatch(d *dispatch) (time.Time, error) {
	if d.Protocol != protocolVer {
		return time.Time{}, fmt.Errorf("protocol %q is not %s", d.Protocol, protocolVer)
	}
	if d.PayloadHash != hashPayload(d.Draft.Body) {
		return time.Time{}, errors.New("payload_hash does not match draft.body")
	}
	if !d.isApproval() {
		if len(d.Actions) > 0 {
			return time.Time{}, errors.New("actions without a callback nonce cannot be answered")
		}
		return time.Time{}, nil
	}
	if d.ApprovalID == "" || len(d.ApprovalID) > 128 {
		return time.Time{}, errors.New("approval_id is missing or longer than 128 bytes")
	}
	cb, err := url.Parse(d.Callback.URL)
	if err != nil || (cb.Scheme != "http" && cb.Scheme != "https") || cb.Host == "" {
		return time.Time{}, errors.New("callback.url is not an http(s) URL")
	}
	expires, err := time.Parse(time.RFC3339, d.ExpiresAt)
	if err != nil {
		return time.Time{}, errors.New("expires_at is not an RFC 3339 time")
	}
	if !expires.After(r.now()) {
		return time.Time{}, errors.New("approval already expired")
	}
	if len(d.verbs()) == 0 {
		return time.Time{}, errors.New("dispatch offers no decision this relay can render")
	}
	return expires, nil
}

// expire closes an approval nobody decided once the gate's own clock has run
// out, so the message stops offering buttons that can no longer count.
func (r *relayServer) expire(id string) {
	r.mu.Lock()
	a, ok := r.approvals[id]
	if !ok || a.phase != phaseOpen {
		r.mu.Unlock()
		return
	}
	a.phase = phaseClosed
	a.outcome = ":hourglass: *Expired* with no decision. draftcat resolves it as a timeout, and nothing is sent."
	channel, ts, blocks := a.channel, a.ts, renderBlocks(&a.d, view{status: a.outcome}, r.mention)
	text := summary(&a.d)
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := r.slack.update(ctx, channel, ts, text, blocks); err != nil {
		log.Printf("approval %s: expiry not shown in Slack: %v", logID(id), err)
	}
}

func (r *relayServer) pruneLocked() {
	cutoff := r.now().Add(-keepClosed)
	for id, a := range r.approvals {
		if a.phase == phaseClosed && a.expires.Before(cutoff) {
			delete(r.approvals, id)
		}
	}
}

// --- interactivity: Slack click -> relay -> draftcat ---

type interaction struct {
	Type string `json:"type"`
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Name     string `json:"name"`
	} `json:"user"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Container struct {
		ChannelID string `json:"channel_id"`
		MessageTS string `json:"message_ts"`
	} `json:"container"`
	Actions []struct {
		ActionID string `json:"action_id"`
		BlockID  string `json:"block_id"`
		Value    string `json:"value"`
	} `json:"actions"`
	State struct {
		Values map[string]map[string]struct {
			Value *string `json:"value"`
		} `json:"values"`
	} `json:"state"`
}

type click struct {
	verb       string
	approvalID string
	user       string
	display    string
	channel    string
	ts         string
	adjustText string
}

func (r *relayServer) handleInteractivity(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxInteractionBytes+1))
	if err != nil || len(body) > maxInteractionBytes {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := verifySlackSignature(r.cfg.SigningSecret, req.Header.Get(slackTSHeader), req.Header.Get(slackSigHeader), body, r.now()); err != nil {
		log.Printf("interaction rejected: %v", err)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var p interaction
	if err := json.Unmarshal([]byte(form.Get("payload")), &p); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	c, ok := parseClick(&p)
	// Slack wants an answer within three seconds; forwarding to draftcat may
	// take longer, so the click is handled after this response.
	w.WriteHeader(http.StatusOK)
	if !ok {
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.handleClick(c)
	}()
}

func parseClick(p *interaction) (click, bool) {
	if p.Type != "block_actions" {
		return click{}, false
	}
	for _, a := range p.Actions {
		verb, ours := strings.CutPrefix(a.ActionID, actionPrefix)
		if !ours || a.BlockID != blockDecide {
			continue
		}
		switch verb {
		case verbApprove, verbSkip, verbAdjust:
		default:
			continue
		}
		c := click{
			verb: verb, approvalID: a.Value, user: p.User.ID,
			display: p.User.Name, channel: p.Container.ChannelID, ts: p.Container.MessageTS,
		}
		if c.display == "" {
			c.display = p.User.Username
		}
		if c.channel == "" {
			c.channel = p.Channel.ID
		}
		if v := p.State.Values[blockAdjust][actionAdjustT].Value; v != nil {
			c.adjustText = strings.TrimSpace(*v)
		}
		return c, true
	}
	return click{}, false
}

// handleClick turns one click into at most one decision. Every refusal is an
// ephemeral note to the person who clicked, and none of them reaches draftcat.
func (r *relayServer) handleClick(c click) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tell := func(channel, text string) {
		if err := r.slack.ephemeral(ctx, channel, c.user, text); err != nil {
			log.Printf("ephemeral to %s not sent: %v", c.user, err)
		}
	}

	identity, allowed := r.cfg.Approvers[c.user]
	if !allowed {
		log.Printf("click from %s refused: not a configured approver", c.user)
		tell(c.channel, "You are not an approver for draftcat on this relay. Nothing was sent.")
		return
	}

	r.mu.Lock()
	a, known := r.approvals[c.approvalID]
	if !known || a.phase == phasePosting {
		r.mu.Unlock()
		tell(c.channel, "This approval is no longer open on this relay. Nothing was sent.")
		return
	}
	if a.channel != c.channel || a.ts != c.ts {
		r.mu.Unlock()
		log.Printf("click on %s refused: it came from a message this relay did not post", logID(c.approvalID))
		return
	}
	switch {
	case a.phase == phaseClosed:
		outcome := a.outcome
		r.mu.Unlock()
		tell(a.channel, "This approval is closed, and this click changed nothing. "+outcome)
		return
	case a.phase == phaseDeciding:
		r.mu.Unlock()
		tell(a.channel, "Another decision on this approval is being recorded. Nothing was sent.")
		return
	case len(a.d.Approvers) > 0 && !contains(a.d.Approvers, identity):
		r.mu.Unlock()
		tell(a.channel, "You are not an approver for this step. Nothing was sent.")
		return
	case !a.d.offers(c.verb):
		r.mu.Unlock()
		tell(a.channel, "This approval does not offer that decision. Nothing was sent.")
		return
	case c.verb == verbApprove && contains(a.approved, c.user):
		r.mu.Unlock()
		tell(a.channel, "Your approval is already recorded. It is waiting for the other approvers.")
		return
	case c.verb == verbAdjust && c.adjustText == "":
		r.mu.Unlock()
		tell(a.channel, "Describe the change in the Adjust box first, then press Adjust. Nothing was sent.")
		return
	}
	a.phase = phaseDeciding
	d := a.d
	r.mu.Unlock()

	dec := buildDecision(&d, c, identity, r.now())
	status, resolved, err := r.postDecision(ctx, d.Callback.URL, dec)

	accepted := status >= 200 && status <= 299
	r.mu.Lock()
	var note string
	switch {
	case err != nil:
		a.phase = phaseOpen
		note = "draftcat could not be reached, so this decision was not recorded. Try again."
		log.Printf("approval %s: decision not delivered: %v", logID(d.ApprovalID), err)
	case accepted && !resolved && c.verb == verbApprove:
		a.phase = phaseOpen
		a.approved = append(a.approved, c.user)
	case accepted:
		a.phase = phaseClosed
		if c.verb == verbApprove {
			a.approved = append(a.approved, c.user)
		}
		a.outcome = outcomeLine(c, a.approved)
	case status == http.StatusConflict:
		a.phase = phaseClosed
		a.outcome = ":lock: *Already resolved* at the gate. This click changed nothing."
	case status == http.StatusGone:
		a.phase = phaseClosed
		a.outcome = ":hourglass: *Expired* before this decision reached draftcat. Nothing is sent."
	default:
		// draftcat refused it (signature, nonce, hash or approver). The gate is
		// still open, so another approver may still decide.
		a.phase = phaseOpen
		note = fmt.Sprintf("draftcat refused this decision (HTTP %d), so it was not recorded.", status)
		log.Printf("approval %s: gate refused the decision with HTTP %d", logID(d.ApprovalID), status)
	}
	// The expiry timer skips an approval while a decision is in flight. If the
	// gate's clock ran out meanwhile, close the reopened message now.
	lapsed := a.phase == phaseOpen && !r.now().Before(a.expires)
	if note != "" {
		r.mu.Unlock()
		tell(a.channel, note)
		if lapsed {
			r.expire(d.ApprovalID)
		}
		return
	}
	v := view{buttons: a.phase == phaseOpen, status: a.outcome}
	if a.phase == phaseOpen {
		v.status = progressLine(a.approved, d.Quorum.Required)
	} else if a.timer != nil {
		a.timer.Stop()
	}
	channel, ts, blocks := a.channel, a.ts, renderBlocks(&d, v, r.mention)
	r.mu.Unlock()

	log.Printf("approval %s: %s by %s, gate answered %d", logID(d.ApprovalID), c.verb, c.user, status)
	if err := r.slack.update(ctx, channel, ts, summary(&d), blocks); err != nil {
		log.Printf("approval %s: outcome not shown in Slack: %v", logID(d.ApprovalID), err)
	}
	if lapsed {
		r.expire(d.ApprovalID)
	}
}

// buildDecision is the envelope the protocol requires, bound to the dispatch
// it answers by approval_id, nonce and payload hash.
func buildDecision(d *dispatch, c click, identity string, now time.Time) decision {
	var dec decision
	dec.Protocol = protocolVer
	dec.ApprovalID = d.ApprovalID
	dec.Nonce = d.Callback.Nonce
	dec.Decision = c.verb
	// Echo the hash draftcat sent, unchanged. Recomputing it here would defeat
	// the point: the echo is what proves the human saw the staged draft.
	dec.PayloadHash = d.PayloadHash
	dec.Approver.ID = identity
	dec.Approver.Display = c.display
	dec.Approver.Channel = "slack"
	if c.verb == verbAdjust {
		dec.AdjustText = c.adjustText
	}
	dec.DecidedAt = now.UTC().Format(time.RFC3339)
	return dec
}

// postDecision signs and sends one decision. resolved reports whether the gate
// said the decision closed it (false while a quorum still needs approvers).
func (r *relayServer) postDecision(ctx context.Context, callback string, dec decision) (status int, resolved bool, err error) {
	body, err := json.Marshal(dec)
	if err != nil {
		return 0, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, callback, bytes.NewReader(body))
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(protoHeader, protocolVer)
	req.Header.Set(sigHeader, sign(r.cfg.RelaySecret, body, r.now()))
	resp, err := r.http.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	var ans struct {
		Resolved *bool `json:"resolved"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resolved = true
	if json.Unmarshal(raw, &ans) == nil && ans.Resolved != nil {
		resolved = *ans.Resolved
	}
	return resp.StatusCode, resolved, nil
}

// mention shows a draftcat identity as a Slack mention when it maps to a
// configured approver, so the people who can decide are notified.
func (r *relayServer) mention(identity string) string {
	for user, id := range r.cfg.Approvers {
		if id == identity {
			return "<@" + user + ">"
		}
	}
	return esc(clip(identity, 100))
}

func outcomeLine(c click, approvedBy []string) string {
	switch c.verb {
	case verbApprove:
		return ":white_check_mark: *Approved* by " + mentions(approvedBy) + "."
	case verbSkip:
		return ":no_entry_sign: *Skipped* by <@" + c.user + ">. Nothing is sent."
	default:
		return ":pencil2: *Adjust* requested by <@" + c.user + ">. draftcat rewrites the draft with this note and asks again:\n>" +
			esc(clip(strings.ReplaceAll(c.adjustText, "\n", " "), 500))
	}
}

func progressLine(approvedBy []string, required int) string {
	return fmt.Sprintf("Approved so far by %s (%d of %d).", mentions(approvedBy), len(approvedBy), required)
}

func mentions(users []string) string {
	out := make([]string, len(users))
	for i, u := range users {
		out[i] = "<@" + u + ">"
	}
	return strings.Join(out, ", ")
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// logID keeps an attacker-chosen id from writing arbitrary text into the log.
func logID(id string) string { return oneLine(clip(id, 64)) }

func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, clip(s, 120))
}
