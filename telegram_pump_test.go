package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
)

// scriptedTG stands in for the Bot API: it mints message ids for sendMessage,
// accepts everything else, and hands the pump whatever updates a test pushed.
type scriptedTG struct {
	mu        sync.Mutex
	nextMsgID int
	sent      int
	queue     []TGUpdate
}

func (s *scriptedTG) api(method string, payload map[string]interface{}) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if method == "sendMessage" {
		s.nextMsgID++
		s.sent++
		return json.RawMessage(fmt.Sprintf(`{"message_id":%d}`, s.nextMsgID)), nil
	}
	_ = payload
	return json.RawMessage(`true`), nil
}

func (s *scriptedTG) fetch() ([]TGUpdate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.queue
	s.queue = nil
	return out, nil
}

func (s *scriptedTG) push(u ...TGUpdate) {
	s.mu.Lock()
	s.queue = append(s.queue, u...)
	s.mu.Unlock()
}

func (s *scriptedTG) sentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent
}

func cbUpdate(id, msgID int, from int64, data string) TGUpdate {
	u := TGUpdate{UpdateID: id, CallbackQuery: &TGCallback{ID: fmt.Sprintf("cb%d", id), Data: data}}
	u.CallbackQuery.From.ID = from
	u.CallbackQuery.Message.MessageID = msgID
	return u
}

func textUpdate(id int, from, chat int64, text string) TGUpdate {
	u := TGUpdate{UpdateID: id, Message: &TGMessage{MessageID: 1000 + id, Text: text}}
	u.Message.From.ID = from
	u.Message.Chat.ID = chat
	return u
}

func newScriptedBot() (*TGBot, *scriptedTG) {
	s := &scriptedTG{}
	bot := &TGBot{
		chatID:      100,
		security:    config.ChannelSecurity{AllowedUsers: []int64{7, 8}},
		rateLimiter: newRateLimiter(100),
		api:         s.api,
		fetch:       s.fetch,
	}
	return bot, s
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- routing ---

func TestPump_CallbackGoesToItsWaiterEverythingElseToCommands(t *testing.T) {
	bot, _ := newScriptedBot()
	p := bot.pump()
	w := p.register(42)
	defer p.release(42)

	p.route(cbUpdate(1, 42, 7, "approve"))
	p.route(cbUpdate(2, 99, 7, "cron:pause:invoices"))

	select {
	case u := <-w.ch:
		if u.UpdateID != 1 {
			t.Fatalf("waiter got update %d, want 1", u.UpdateID)
		}
	default:
		t.Fatal("waiter did not receive the callback aimed at its message")
	}
	cmds := bot.drainUpdates()
	if len(cmds) != 1 || cmds[0].UpdateID != 2 {
		t.Fatalf("command loop got %+v, want only the cron callback", cmds)
	}
}

func TestPump_AdjustTextGoesToTheAskerButSlashCommandsDoNot(t *testing.T) {
	bot, _ := newScriptedBot()
	p := bot.pump()
	w := p.register(42)
	defer p.release(42)

	// Nobody asked for text yet → the command loop gets it.
	p.route(textUpdate(1, 7, 100, "hello"))
	if cmds := bot.drainUpdates(); len(cmds) != 1 {
		t.Fatalf("command loop got %d update(s), want the chat message", len(cmds))
	}

	w.wantText(true)
	p.route(textUpdate(2, 7, 100, "/status"))
	p.route(textUpdate(3, 7, 100, "tighten the CTA"))

	select {
	case u := <-w.ch:
		if u.Message.Text != "tighten the CTA" {
			t.Fatalf("waiter got %q, want the adjustment text", u.Message.Text)
		}
	default:
		t.Fatal("adjustment text did not reach the waiter")
	}
	cmds := bot.drainUpdates()
	if len(cmds) != 1 || cmds[0].Message.Text != "/status" {
		t.Fatalf("command loop got %+v, want /status even while a gate waits for text", cmds)
	}
}

// --- the bug this exists for ---

// An approval waiter and a busy command loop share one update stream. Before
// the pump, the loop's own getUpdates could swallow the operator's tap. Now
// the tap must reach the waiter no matter how eagerly the loop drains.
func TestPump_TapIsNotSwallowedByABusyCommandLoop(t *testing.T) {
	bot, s := newScriptedBot()
	bot.startPump(2 * time.Millisecond)
	defer bot.stopPump()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				bot.drainUpdates()
				time.Sleep(time.Millisecond)
			}
		}
	}()

	type out struct {
		dec OperatorDecision
		err error
	}
	res := make(chan out, 1)
	go func() {
		d, err := bot.SendForApproval(context.Background(), "draft", nil)
		res <- out{d, err}
	}()
	waitFor(t, "the prompt to be posted", func() bool { return s.sentCount() == 1 })
	s.push(cbUpdate(1, 1, 7, "approve"))

	select {
	case r := <-res:
		if r.err != nil || r.dec.Action != "approve" || r.dec.ApproverID != 7 {
			t.Fatalf("decision = %+v err=%v, want approve by 7", r.dec, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the operator's tap never reached the waiter")
	}
}

func TestPump_ConcurrentGatesEachGetTheirOwnTap(t *testing.T) {
	bot, s := newScriptedBot()
	bot.startPump(2 * time.Millisecond)
	defer bot.stopPump()

	first := make(chan OperatorDecision, 1)
	second := make(chan OperatorDecision, 1)
	go func() { d, _ := bot.SendForApproval(context.Background(), "first", nil); first <- d }()
	waitFor(t, "first prompt", func() bool { return s.sentCount() == 1 })
	go func() { d, _ := bot.SendForApproval(context.Background(), "second", nil); second <- d }()
	waitFor(t, "second prompt", func() bool { return s.sentCount() == 2 })

	// Answer the second prompt first, then the first — order must not matter.
	s.push(cbUpdate(1, 2, 8, "approve"), cbUpdate(2, 1, 7, "skip"))

	select {
	case d := <-second:
		if d.Action != "approve" || d.ApproverID != 8 {
			t.Fatalf("second gate = %+v, want approve by 8", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second gate never resolved")
	}
	select {
	case d := <-first:
		if d.Action != "skip" {
			t.Fatalf("first gate = %+v, want skip", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first gate never resolved")
	}
	if stray := bot.drainUpdates(); len(stray) != 0 {
		t.Fatalf("command loop received %d stray update(s): %+v", len(stray), stray)
	}
}

func TestPump_AdjustReturnsTheOperatorsText(t *testing.T) {
	bot, s := newScriptedBot()
	bot.startPump(2 * time.Millisecond)
	defer bot.stopPump()

	res := make(chan OperatorDecision, 1)
	go func() { d, _ := bot.SendForApproval(context.Background(), "draft", nil); res <- d }()
	waitFor(t, "prompt", func() bool { return s.sentCount() == 1 })

	s.push(cbUpdate(1, 1, 7, "adjust"))
	time.Sleep(20 * time.Millisecond)
	// A text from a stranger, then from another chat, then the real one.
	s.push(textUpdate(2, 999, 100, "evil edit"), textUpdate(3, 7, 555, "wrong chat"), textUpdate(4, 7, 100, "Hi Anna, shorter version"))

	select {
	case d := <-res:
		if d.Action != "adjust" || d.Text != "Hi Anna, shorter version" {
			t.Fatalf("decision = %+v, want the operator's adjustment", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("adjustment never resolved the gate")
	}
}

func TestPump_QuorumCountsDistinctApprovers(t *testing.T) {
	bot, s := newScriptedBot()
	bot.startPump(2 * time.Millisecond)
	defer bot.stopPump()

	res := make(chan QuorumDecision, 1)
	go func() { d, _ := bot.SendForQuorumApproval(context.Background(), "draft", 2, nil); res <- d }()
	waitFor(t, "prompt", func() bool { return s.sentCount() == 1 })

	s.push(cbUpdate(1, 1, 7, "approve"), cbUpdate(2, 1, 7, "approve"), cbUpdate(3, 1, 8, "approve"))
	select {
	case d := <-res:
		if d.Action != "approve" || len(d.Approvers) != 2 {
			t.Fatalf("quorum decision = %+v, want approve by two distinct operators", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quorum gate never resolved")
	}
}

func TestPump_TimeoutStillDenies(t *testing.T) {
	bot, _ := newScriptedBot()
	bot.startPump(2 * time.Millisecond)
	defer bot.stopPump()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := bot.SendForApproval(ctx, "draft", nil); err == nil {
		t.Fatal("a gate nobody answered must time out, not resolve")
	}
	if n := bot.pump().waiterCount(); n != 0 {
		t.Fatalf("%d waiter(s) still registered after the gate closed", n)
	}
}
