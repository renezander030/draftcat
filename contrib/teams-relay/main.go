// Command teams-relay is a conformant hitl/v0 relay for Microsoft Teams.
//
// It exists because the in-binary routes to Teams are all closed: incoming
// webhooks and O365 connectors were disabled in May 2026, Graph chatMessage
// cannot receive a card submit, the Graph Approvals API is beta, and the Python
// Bot Framework SDK is archived. What is left is a registered bot with an Azure
// app registration and tenant admin consent — per deployment, per vendor.
//
// So draftcat does not do any of that. It dispatches a signed approval to this
// relay, this relay renders it wherever the operator already reads messages,
// and posts one signed decision back. draftcat stays the gate; this is only a
// presenter, and it is treated as untrusted: it cannot authorise anything the
// gate would not have authorised anyway.
//
// Build:  go build -o teams-relay .
// Verify: draftcat hitl verify http://127.0.0.1:8090/dispatch
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	sigHeader      = "X-Draftcat-Signature"
	maxSkewSeconds = 300
	protocolVer    = "hitl/v0"
)

// dispatch is the subset of the hitl/v0 request this relay needs. Unknown
// fields are ignored by design, so a newer draftcat can add context without
// breaking a relay already deployed.
type dispatch struct {
	Protocol    string   `json:"protocol"`
	ApprovalID  string   `json:"approval_id"`
	RunID       string   `json:"run_id"`
	Pipeline    string   `json:"pipeline"`
	Step        string   `json:"step"`
	ExpiresAt   string   `json:"expires_at"`
	Risk        string   `json:"risk"`
	Approvers   []string `json:"approvers"`
	PayloadHash string   `json:"payload_hash"`
	Draft       struct {
		Body string `json:"body"`
	} `json:"draft"`
	Budget *struct {
		Unit       string  `json:"unit"`
		SpentToday float64 `json:"spent_today"`
		CapToday   float64 `json:"cap_today"`
	} `json:"budget"`
	Callback struct {
		URL   string `json:"url"`
		Nonce string `json:"nonce"`
	} `json:"callback"`
}

type decision struct {
	Protocol    string `json:"protocol"`
	ApprovalID  string `json:"approval_id"`
	Nonce       string `json:"nonce"`
	Decision    string `json:"decision"`
	PayloadHash string `json:"payload_hash"`
	Approver    struct {
		ID      string `json:"id"`
		Display string `json:"display,omitempty"`
		Channel string `json:"channel,omitempty"`
	} `json:"approver"`
	AdjustText string `json:"adjust_text,omitempty"`
	DecidedAt  string `json:"decided_at"`
}

var secret []byte

func main() {
	s := os.Getenv("DRAFTCAT_RELAY_SECRET")
	if s == "" {
		log.Fatal("DRAFTCAT_RELAY_SECRET is required — it signs decisions and verifies dispatches")
	}
	secret = []byte(s)

	addr := os.Getenv("RELAY_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8090"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/dispatch", handleDispatch)
	mux.HandleFunc("/decide", handleHumanDecision)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("teams-relay listening on %s (dispatch: POST /dispatch)", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
}

// pending holds dispatches awaiting a human. In-memory on purpose: draftcat
// owns durability. If this relay restarts, the gate on draftcat's side is still
// open and still expires on its own clock — a lost dispatch becomes a timeout,
// and a timeout does not fire the action.
var pending = struct {
	m map[string]dispatch
}{m: map[string]dispatch{}}

func handleDispatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "unreadable", http.StatusBadRequest)
		return
	}
	// Verify draftcat really sent this. A relay that renders unverified
	// dispatches can be used to show an operator a draft draftcat never staged.
	if err := verifySignature(r.Header.Get(sigHeader), body, time.Now()); err != nil {
		log.Printf("dispatch rejected: %v", err)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var d dispatch
	if err := json.Unmarshal(body, &d); err != nil {
		http.Error(w, "malformed json", http.StatusBadRequest)
		return
	}
	if d.Protocol != protocolVer {
		http.Error(w, "unsupported protocol", http.StatusBadRequest)
		return
	}

	// Ack immediately. Holding this connection for the human is the mistake
	// that makes a four-hour approval window impossible.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"accepted"}`))

	if len(d.Actions()) == 0 {
		log.Printf("notification (no decision needed): %s", oneLine(d.Draft.Body))
		return
	}
	pending.m[d.ApprovalID] = d
	log.Printf("approval %s (%s/%s risk=%s) awaiting a human — decide at /decide?id=%s",
		d.ApprovalID, d.Pipeline, d.Step, orNormal(d.Risk), d.ApprovalID)
	renderCard(d)
}

// Actions reports whether this dispatch asks for a decision at all.
func (d dispatch) Actions() []string {
	if d.Callback.Nonce == "" {
		return nil
	}
	return []string{"approve", "skip", "adjust"}
}

// renderCard is where a real deployment posts the Adaptive Card. Left as a log
// line plus an optional webhook POST so the relay is runnable and verifiable
// without a tenant; swap this for your Graph or connector call.
func renderCard(d dispatch) {
	card := map[string]any{
		"pipeline": d.Pipeline, "step": d.Step, "risk": orNormal(d.Risk),
		"expires_at": d.ExpiresAt, "draft": d.Draft.Body,
		"approvers": d.Approvers, "approval_id": d.ApprovalID,
	}
	if d.Budget != nil {
		card["spend_today"] = fmt.Sprintf("%.4f of %.4f", d.Budget.SpentToday, d.Budget.CapToday)
	}
	target := os.Getenv("TEAMS_WEBHOOK_URL")
	if target == "" {
		log.Printf("card: %s", mustJSON(card))
		return
	}
	body := mustJSON(card)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		log.Printf("card post failed: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("card post failed: %v", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
}

// handleHumanDecision is the local decision surface: whatever renders the card
// calls this, and it signs and forwards a conformant decision to draftcat.
//
//	POST /decide?id=<approval_id>&decision=approve&approver=alice@example.com
func handleHumanDecision(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	d, ok := pending.m[id]
	if !ok {
		http.Error(w, "unknown or already-decided approval", http.StatusNotFound)
		return
	}
	verb := r.URL.Query().Get("decision")
	switch verb {
	case "approve", "skip", "adjust":
	default:
		http.Error(w, "decision must be approve, skip or adjust", http.StatusBadRequest)
		return
	}
	approver := r.URL.Query().Get("approver")
	if approver == "" && len(d.Approvers) > 0 {
		approver = d.Approvers[0]
	}

	var dec decision
	dec.Protocol = protocolVer
	dec.ApprovalID = d.ApprovalID
	dec.Nonce = d.Callback.Nonce
	dec.Decision = verb
	// Echo the hash draftcat gave us, unchanged. Recomputing it here would
	// defeat the point: the echo is what proves the human saw the staged draft.
	dec.PayloadHash = d.PayloadHash
	dec.Approver.ID = approver
	dec.Approver.Channel = "teams"
	dec.AdjustText = r.URL.Query().Get("adjust_text")
	dec.DecidedAt = time.Now().UTC().Format(time.RFC3339)

	status, err := postDecision(d.Callback.URL, dec)
	if err != nil {
		log.Printf("decision post failed: %v", err)
		http.Error(w, "forwarding failed", http.StatusBadGateway)
		return
	}
	delete(pending.m, id)
	log.Printf("decision %s for %s -> gate answered %d", verb, id, status)
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"forwarded":true,"gate_status":%d}`, status)
}

func postDecision(url string, dec decision) (int, error) {
	body, err := json.Marshal(dec)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(sigHeader, sign(body, time.Now()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

func sign(body []byte, now time.Time) string {
	ts := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func verifySignature(header string, body []byte, now time.Time) error {
	if header == "" {
		return fmt.Errorf("missing %s", sigHeader)
	}
	var tsPart, sigPart string
	for _, f := range strings.Split(header, ",") {
		f = strings.TrimSpace(f)
		switch {
		case strings.HasPrefix(f, "t="):
			tsPart = strings.TrimPrefix(f, "t=")
		case strings.HasPrefix(f, "v1="):
			sigPart = strings.TrimPrefix(f, "v1=")
		}
	}
	if tsPart == "" || sigPart == "" {
		return fmt.Errorf("malformed signature header")
	}
	ts, err := strconv.ParseInt(tsPart, 10, 64)
	if err != nil {
		return fmt.Errorf("bad timestamp")
	}
	if skew := now.Unix() - ts; skew > maxSkewSeconds || skew < -maxSkewSeconds {
		return fmt.Errorf("timestamp outside %ds window", maxSkewSeconds)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(tsPart))
	mac.Write([]byte("."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(want), []byte(sigPart)) != 1 {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

func orNormal(s string) string {
	if s == "" {
		return "normal"
	}
	return s
}
