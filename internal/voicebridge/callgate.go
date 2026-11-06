//go:build voice

package voicebridge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	statestore "github.com/renezander030/draftcat/internal/state"
	"github.com/renezander030/draftcat/voice"
)

// callGate holds voice calls to the daily call caps, on the state store's
// budget ledger. Every call is counted, capped or not; only a configured cap
// refuses a call, and without one a ledger failure is logged and the call goes
// ahead exactly as it did before the caps were enforced.
type callGate struct {
	store    *statestore.StateStore
	sessions sessionStarts
	budget   CallBudget
	now      func() time.Time

	mu       sync.Mutex
	notified map[string]bool
}

// sessionStarts finds when a session started (the voice store).
type sessionStarts interface {
	SessionStartedAt(ctx context.Context, id string) (int64, bool, error)
}

func newCallGate(st *statestore.StateStore, sessions sessionStarts, budget CallBudget) *callGate {
	return &callGate{store: st, sessions: sessions, budget: budget, now: time.Now, notified: map[string]bool{}}
}

func (g *callGate) caps() statestore.VoiceCallCaps {
	if g.budget.Caps == nil {
		return statestore.VoiceCallCaps{}
	}
	calls, minutes := g.budget.Caps()
	return statestore.VoiceCallCaps{Calls: calls, Minutes: minutes}
}

// Check answers whether a new call may start now, without counting one.
func (g *callGate) Check(ctx context.Context) error {
	caps := g.caps()
	if !caps.Capped() {
		return nil
	}
	return g.refusal(g.store.CheckVoiceCall(ctx, g.now(), caps))
}

// Admit counts the call or refuses it.
func (g *callGate) Admit(ctx context.Context, sessionID string) error {
	caps := g.caps()
	_, err := g.store.AdmitVoiceCall(ctx, sessionID, g.now(), caps)
	if err != nil && !caps.Capped() {
		log.Printf("[voice] call %s not counted: %v", sessionID, err)
		return nil
	}
	return g.refusal(err)
}

// End records the call's minutes, from its session_start to endedAt, rounded
// up to whole minutes. A call whose start never arrived has no known length
// and counts with no minutes.
func (g *callGate) End(ctx context.Context, sessionID string, endedAt time.Time) error {
	var startedAt time.Time
	ms, ok, err := g.sessions.SessionStartedAt(ctx, sessionID)
	if err == nil {
		if ok {
			startedAt = time.UnixMilli(ms)
		}
		_, err = g.store.EndVoiceCall(ctx, sessionID, endedAt, callMinutes(startedAt, endedAt))
	}
	if err != nil && !g.caps().Capped() {
		log.Printf("[voice] call %s minutes not recorded: %v", sessionID, err)
		return nil
	}
	return err
}

func callMinutes(startedAt, endedAt time.Time) int {
	if startedAt.IsZero() || !endedAt.After(startedAt) {
		return 0
	}
	return int((endedAt.Sub(startedAt) + time.Minute - 1) / time.Minute)
}

// refusal turns a cap error into a voice.ErrCallRefused and tells the
// operator once per cap and UTC day; any other error is the gate unable to
// decide, which the receivers answer by refusing too.
func (g *callGate) refusal(err error) error {
	if err == nil {
		return nil
	}
	var capErr *statestore.VoiceCapError
	if !errors.As(err, &capErr) {
		return fmt.Errorf("call budget unavailable: %w", err)
	}
	g.notifyOnce(capErr)
	return fmt.Errorf("%w: %s", voice.ErrCallRefused, capErr.Error())
}

func (g *callGate) notifyOnce(capErr *statestore.VoiceCapError) {
	if g.budget.Notify == nil {
		return
	}
	key := capErr.Day + ":" + capErr.Cap
	g.mu.Lock()
	seen := g.notified[key]
	g.notified[key] = true
	g.mu.Unlock()
	if seen {
		return
	}
	// The mark in the store keeps a restart from repeating the notice.
	if first, err := g.store.TryMarkSeen("_budget", "voice_cap", key, g.now()); err != nil {
		log.Printf("[voice] cap notice mark %s: %v", key, err)
	} else if !first {
		return
	}
	what := fmt.Sprintf("voice call cap reached: %d of %d calls", capErr.Used, capErr.Limit)
	if capErr.Cap == "per_day_call_minutes" {
		what = fmt.Sprintf("call-minute cap reached: %d of %d minutes", capErr.Used, capErr.Limit)
	}
	msg := fmt.Sprintf("[budget] Daily %s (UTC day %s). New calls are refused until the next UTC day.", what, capErr.Day)
	go g.budget.Notify(msg)
}
