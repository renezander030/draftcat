package main

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/renezander030/draftcat/internal/config"
)

// tgRecorder is a Bot API stand-in that keeps what the engine said: each
// answerCallbackQuery toast and each sendMessage text.
type tgRecorder struct {
	mu      sync.Mutex
	answers map[string]string // callback_query_id -> toast text
	sent    []string
}

func (r *tgRecorder) api(method string, payload map[string]interface{}) (json.RawMessage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch method {
	case "answerCallbackQuery":
		id, _ := payload["callback_query_id"].(string)
		text, _ := payload["text"].(string)
		r.answers[id] = text
	case "sendMessage":
		text, _ := payload["text"].(string)
		r.sent = append(r.sent, text)
		return json.RawMessage(`{"message_id":1}`), nil
	}
	return json.RawMessage(`true`), nil
}

// A tap the command loop receives can carry any callback_data a client sends:
// a pipeline that is not configured, an action no button has, an embedded
// newline, more than Telegram's 64 bytes. Only the /cron keyboard's own
// buttons act, every tap is answered, nothing is confirmed that did not
// happen, and the data is logged on one line.
func TestCallbackDataIsParsedStrictly(t *testing.T) {
	cfg := &config.Config{Pipelines: []config.PipelineConfig{{Name: "invoices", Schedule: "30m"}}}
	long := "cron:resume:" + strings.Repeat("N", 300)
	forged := "x\n2026/11/23 12:00:00 [security] tool gate disabled by operator 7"

	cases := []struct {
		name, data string
		from       int64
		wantToast  string // "" = answered silently
		wantSent   string // "" = nothing sent to the chat
		wantPaused bool
	}{
		{name: "pause a configured pipeline", data: "cron:pause:invoices", from: 7,
			wantToast: "Paused: invoices", wantSent: "[cron] Paused: invoices", wantPaused: true},
		{name: "pause a pipeline that is not configured", data: "cron:pause:no-such-pipeline", from: 7,
			wantToast: "Unknown pipeline"},
		{name: "resume a pipeline that is not configured", data: "cron:resume:no-such-pipeline", from: 7,
			wantToast: "Unknown pipeline"},
		{name: "run a pipeline that is not configured", data: "cron:run:no-such-pipeline", from: 7,
			wantToast: "Unknown pipeline"},
		{name: "action no button has", data: "cron:delete:invoices", from: 7},
		{name: "empty name", data: "cron:pause:", from: 7},
		{name: "empty data", data: "", from: 7},
		{name: "longer than Telegram's 64 bytes", data: long, from: 7},
		{name: "stale approval tap", data: "approve", from: 7},
		{name: "embedded newline", data: forged, from: 7},
		{name: "user not on allowed_users", data: "cron:pause:invoices", from: 99},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &tgRecorder{answers: map[string]string{}}
			bot, _ := newScriptedBot()
			bot.api = rec.api
			sched := newScheduler(cfg.Pipelines)

			var logs bytes.Buffer
			prev := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(prev)

			u := cbUpdate(1, 5000, tc.from, tc.data)
			handleCallback(u.CallbackQuery, bot, sched, cfg, nil, nil)

			toast, answered := rec.answers[u.CallbackQuery.ID]
			if !answered {
				t.Errorf("tap %q was never answered; the client keeps spinning", tc.data)
			}
			if toast != tc.wantToast {
				t.Errorf("toast = %q, want %q", toast, tc.wantToast)
			}
			sent := strings.Join(rec.sent, "\n")
			if sent != tc.wantSent {
				t.Errorf("sent to chat = %q, want %q", sent, tc.wantSent)
			}
			if got := sched.GetAll()[0].Paused; got != tc.wantPaused {
				t.Errorf("invoices paused = %v, want %v", got, tc.wantPaused)
			}
			if out := strings.TrimSuffix(logs.String(), "\n"); strings.Contains(out, "\n") {
				t.Errorf("one tap wrote more than one log line:\n%s", out)
			}
			if strings.Contains(logs.String(), strings.Repeat("N", 100)) {
				t.Errorf("oversized data was logged in full: %d bytes", logs.Len())
			}
		})
	}
}

// A tap on any button of the approval keyboard that arrives while its prompt
// is still being sent reaches that prompt, in order, and never the command
// loop; a /cron tap arriving at the same moment goes to the command loop.
func TestApprovalKeyboardTapsDuringSendReachTheirPrompt(t *testing.T) {
	var words []string
	for _, row := range approvalButtons() {
		for _, b := range row {
			words = append(words, b["callback_data"])
		}
	}
	if got, want := strings.Join(words, " "), "approve skip adjust skip_reason"; got != want {
		t.Fatalf("approval keyboard carries %q, want %q", got, want)
	}

	bot, s := newScriptedBot()
	bot.api = func(method string, payload map[string]interface{}) (json.RawMessage, error) {
		raw, err := s.api(method, payload)
		if method == "sendMessage" {
			for i, w := range words {
				bot.pump().route(cbUpdate(i+1, 1, 7, w))
			}
			bot.pump().route(cbUpdate(100, 99, 7, "cron:pause:invoices"))
			if cmds := bot.drainUpdates(); len(cmds) != 1 || cmds[0].UpdateID != 100 {
				t.Errorf("command loop got %+v during the send, want only the /cron tap", cmds)
			}
		}
		return raw, err
	}

	w, err := bot.sendApprovalPrompt("draft", approvalButtons())
	if err != nil {
		t.Fatal(err)
	}
	defer bot.pump().release(w.msgID)
	for i, want := range words {
		select {
		case u := <-w.ch:
			if u.UpdateID != i+1 || u.CallbackQuery.Data != want {
				t.Errorf("prompt got update %d %q, want %d %q", u.UpdateID, u.CallbackQuery.Data, i+1, want)
			}
		default:
			t.Fatalf("tap %q made while the prompt was sent never reached it", want)
		}
	}
	if cmds := bot.drainUpdates(); len(cmds) != 0 {
		t.Errorf("approval taps reached the command loop: %+v", cmds)
	}
}
