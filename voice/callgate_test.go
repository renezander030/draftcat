//go:build voice

package voice

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

type fakeGate struct {
	checkErr, admitErr, endErr error
	ended                      []string
	endedAt                    time.Time
}

func (g *fakeGate) Check(context.Context) error         { return g.checkErr }
func (g *fakeGate) Admit(context.Context, string) error { return g.admitErr }
func (g *fakeGate) End(_ context.Context, id string, endedAt time.Time) error {
	g.ended = append(g.ended, id)
	g.endedAt = endedAt
	return g.endErr
}

func TestSessionStartRefusedIsNotStored(t *testing.T) {
	s := newTestServer(t)
	s.SetCallGate(&fakeGate{admitErr: fmt.Errorf("%w: cap reached", ErrCallRefused)})
	w := post(t, s, "/voice/session_start", map[string]any{"id": "s1"}, "secret")
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), `"refused":true`) {
		t.Fatalf("want 429 refusal, got %d %s", w.Code, w.Body)
	}
	if _, ok, _ := s.Store().SessionStartedAt(context.Background(), "s1"); ok {
		t.Fatal("a refused call was stored")
	}
}

func TestGateFailureRefusesWithoutDetail(t *testing.T) {
	s := newTestServer(t)
	s.SetCallGate(&fakeGate{admitErr: errors.New("database is locked"), checkErr: errors.New("database is locked")})
	for _, path := range []string{"/voice/session_start", "/voice/pre_call_context"} {
		w := post(t, s, path, map[string]any{"id": "s1", "caller_phone": "+10000000000"}, "secret")
		if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "locked") {
			t.Fatalf("%s: want 503 without detail, got %d %s", path, w.Code, w.Body)
		}
	}
}

func TestSessionEndRecordsUsageAndFailsWhenNotRecorded(t *testing.T) {
	s := newTestServer(t)
	g := &fakeGate{}
	s.SetCallGate(g)
	start := time.UnixMilli(1_700_000_000_000)
	post(t, s, "/voice/session_start", map[string]any{"id": "s1", "started_at_ms": start.UnixMilli()}, "secret")
	if at, ok, err := s.Store().SessionStartedAt(context.Background(), "s1"); err != nil || !ok || at != start.UnixMilli() {
		t.Fatalf("start lookup: %d %v %v", at, ok, err)
	}
	if _, ok, err := s.Store().SessionStartedAt(context.Background(), "s9"); err != nil || ok {
		t.Fatalf("unknown session: %v %v", ok, err)
	}
	ended := start.Add(5 * time.Minute)
	end := map[string]any{"session_id": "s1", "outcome": map[string]any{"ok": true}, "ended_at_ms": ended.UnixMilli()}
	if w := post(t, s, "/voice/session_end", end, "secret"); w.Code != http.StatusOK || len(g.ended) != 1 || !g.endedAt.Equal(ended) {
		t.Fatalf("end: %d ended=%v at=%v", w.Code, g.ended, g.endedAt)
	}
	g.endErr = errors.New("ledger down")
	if w := post(t, s, "/voice/session_end", end, "secret"); w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 so the end is retried, got %d", w.Code)
	}
}
