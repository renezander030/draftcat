package main

// One update pump per bot.
//
// The Telegram Bot API hands out updates from a single cursor: whoever calls
// getUpdates next receives everything since the last call and advances the
// offset for everyone. Until now every approval waiter polled that endpoint on
// its own ticker, and so did the command loop. Scheduled, webhook-triggered and
// tool-gate approvals all run in their own goroutines, so two or three pollers
// were routinely live at once, racing on one unsynchronized offset. Whichever
// poller happened to fetch the operator's tap kept it: a tap that landed in the
// command loop was dropped (it only understands cron: buttons), the waiter
// never saw it, and the gate eventually timed out — which the operator
// experienced as "I approved that and nothing happened".
//
// Now exactly one goroutine calls getUpdates. It routes each update to the one
// consumer that asked for it:
//
//   - a callback query goes to the waiter registered for that message id;
//   - adjustment text goes to the waiter that most recently asked for it
//     (commands starting with "/" never do — they always reach the command
//     loop, so /status keeps working while a gate waits for a rewrite);
//   - everything else queues for the command loop.
//
// A waiter that is slow to read never blocks the pump: deliveries are
// buffered, and a full buffer drops with a log line rather than stalling every
// other gate.

import (
	"log"
	"strings"
	"sync"
	"time"
)

const (
	tgWaiterBuffer  = 32
	tgCommandBuffer = 256
)

// tgWaiter is one approval prompt waiting for the operator's reaction to a
// specific message.
type tgWaiter struct {
	msgID int
	ch    chan TGUpdate

	mu    sync.Mutex
	text  bool      // asked for adjustment text
	since time.Time // when it asked, to pick the earliest asker
}

func (w *tgWaiter) wantText(on bool) {
	w.mu.Lock()
	w.text = on
	w.since = time.Now()
	w.mu.Unlock()
}

func (w *tgWaiter) wantsText() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.text
}

type tgPump struct {
	mu       sync.Mutex
	waiters  map[int]*tgWaiter
	commands chan TGUpdate
	stop     chan struct{}
	running  bool
}

// pump returns the bot's update pump, creating it on first use so a bot built
// with a struct literal (the tests, the webhook handler) works unchanged.
func (t *TGBot) pump() *tgPump {
	t.pumpOnce.Do(func() {
		t.updates = &tgPump{
			waiters:  map[int]*tgWaiter{},
			commands: make(chan TGUpdate, tgCommandBuffer),
			stop:     make(chan struct{}),
		}
	})
	return t.updates
}

// startPump begins polling the Bot API every interval and routing what comes
// back. Idempotent. The fetch seam lets tests feed scripted updates.
func (t *TGBot) startPump(interval time.Duration) {
	p := t.pump()
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return
	}
	p.running = true
	p.mu.Unlock()

	fetch := t.fetch
	if fetch == nil {
		fetch = t.getUpdates
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-ticker.C:
				updates, err := fetch()
				if err != nil {
					continue
				}
				for _, u := range updates {
					p.route(u)
				}
			}
		}
	}()
}

func (t *TGBot) stopPump() {
	p := t.pump()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		p.running = false
		close(p.stop)
	}
}

// drainUpdates hands the command loop everything queued for it, without
// blocking. Called once per tick, which keeps command latency where it was.
func (t *TGBot) drainUpdates() []TGUpdate {
	p := t.pump()
	var out []TGUpdate
	for {
		select {
		case u := <-p.commands:
			out = append(out, u)
		default:
			return out
		}
	}
}

// register claims the callbacks for one message id until release.
func (p *tgPump) register(msgID int) *tgWaiter {
	w := &tgWaiter{msgID: msgID, ch: make(chan TGUpdate, tgWaiterBuffer)}
	p.mu.Lock()
	p.waiters[msgID] = w
	p.mu.Unlock()
	return w
}

func (p *tgPump) release(msgID int) {
	p.mu.Lock()
	delete(p.waiters, msgID)
	p.mu.Unlock()
}

// waiterCount is how many gates are currently registered — the number of
// prompts whose taps the pump is routing.
func (p *tgPump) waiterCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.waiters)
}

// route delivers one update to exactly one consumer.
func (p *tgPump) route(u TGUpdate) {
	if u.CallbackQuery != nil {
		p.mu.Lock()
		w := p.waiters[u.CallbackQuery.Message.MessageID]
		p.mu.Unlock()
		if w != nil {
			deliver(w, u)
			return
		}
		p.toCommands(u)
		return
	}
	if u.Message != nil {
		text := strings.TrimSpace(u.Message.Text)
		if text != "" && !strings.HasPrefix(text, "/") {
			if w := p.textWaiter(); w != nil {
				deliver(w, u)
				return
			}
		}
		p.toCommands(u)
	}
}

// textWaiter picks the waiter that has been waiting for adjustment text the
// longest — the operator answers prompts in the order they were asked.
func (p *tgPump) textWaiter() *tgWaiter {
	p.mu.Lock()
	defer p.mu.Unlock()
	var best *tgWaiter
	var bestSince time.Time
	for _, w := range p.waiters {
		w.mu.Lock()
		want, since := w.text, w.since
		w.mu.Unlock()
		if !want {
			continue
		}
		if best == nil || since.Before(bestSince) {
			best, bestSince = w, since
		}
	}
	return best
}

func deliver(w *tgWaiter, u TGUpdate) {
	select {
	case w.ch <- u:
	default:
		log.Printf("[telegram] waiter for msg %d is not reading — dropping update %d", w.msgID, u.UpdateID)
	}
}

func (p *tgPump) toCommands(u TGUpdate) {
	select {
	case p.commands <- u:
	default:
		log.Printf("[telegram] command queue full — dropping update %d", u.UpdateID)
	}
}
