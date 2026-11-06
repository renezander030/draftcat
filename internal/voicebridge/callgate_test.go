//go:build voice

package voicebridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	statestore "github.com/renezander030/draftcat/internal/state"
	"github.com/renezander030/draftcat/voice"
)

type capsFixture struct {
	calls, minutes int
	notices        chan string
}

func (f *capsFixture) budget() CallBudget {
	return CallBudget{
		Caps:   func() (int, int) { return f.calls, f.minutes },
		Notify: func(m string) { f.notices <- m },
	}
}

func openStore(t *testing.T) (*statestore.StateStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := statestore.OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, path
}

// voiceServer is the voice receivers on st with the call gate attached.
func voiceServer(t *testing.T, st *statestore.StateStore, gate *callGate) http.Handler {
	t.Helper()
	srv, err := voice.NewServer(voice.Config{Enabled: true, ListenAddr: "127.0.0.1:0"}, st.DB())
	if err != nil {
		t.Fatal(err)
	}
	srv.SetCallGate(gate)
	return srv.Handler()
}

func postJSON(h http.Handler, path string, body any) *httptest.ResponseRecorder {
	buf, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf)))
	return w
}

func waitNotice(t *testing.T, ch chan string) string {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("no operator notice")
		return ""
	}
}

func noNotice(t *testing.T, ch chan string) {
	t.Helper()
	select {
	case m := <-ch:
		t.Fatalf("unexpected notice %q", m)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCallCapRefusesSessionStartAndNotifiesOnce(t *testing.T) {
	st, _ := openStore(t)
	f := &capsFixture{calls: 2, notices: make(chan string, 4)}
	h := voiceServer(t, st, newCallGate(st, voiceStore(t, st), f.budget()))

	for _, id := range []string{"s1", "s2"} {
		if w := postJSON(h, "/voice/session_start", map[string]any{"id": id}); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", id, w.Code, w.Body)
		}
	}
	// A repeated webhook for an admitted call is not a new call.
	if w := postJSON(h, "/voice/session_start", map[string]any{"id": "s1"}); w.Code != http.StatusOK {
		t.Fatalf("repeat: %d", w.Code)
	}
	w := postJSON(h, "/voice/session_start", map[string]any{"id": "s3"})
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "daily voice call limit 2 reached") {
		t.Fatalf("third call: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(waitNotice(t, f.notices), "voice call cap reached: 2 of 2 calls") {
		t.Fatal("notice text")
	}
	if w := postJSON(h, "/voice/pre_call_context", map[string]any{"caller_phone": "+10000000000"}); w.Code != http.StatusTooManyRequests {
		t.Fatalf("pre-call check: %d", w.Code)
	}
	noNotice(t, f.notices)

	// The refused session is not stored as a call.
	if _, ok, err := voiceStore(t, st).SessionStartedAt(context.Background(), "s3"); err != nil || ok {
		t.Fatalf("refused session stored: ok=%v err=%v", ok, err)
	}
	day, _ := st.BudgetDay(context.Background(), time.Now())
	if day.Calls != 2 {
		t.Fatalf("calls = %d", day.Calls)
	}

	// A restart keeps both the count and the notice mark.
	g2 := newCallGate(st, voiceStore(t, st), f.budget())
	if err := g2.Admit(context.Background(), "s4"); !errors.Is(err, voice.ErrCallRefused) {
		t.Fatalf("after restart: %v", err)
	}
	noNotice(t, f.notices)
}

func TestCallMinutesRecordedAtSessionEnd(t *testing.T) {
	st, _ := openStore(t)
	f := &capsFixture{minutes: 10, notices: make(chan string, 4)}
	h := voiceServer(t, st, newCallGate(st, voiceStore(t, st), f.budget()))

	start := time.Now().Add(-11 * time.Minute)
	if w := postJSON(h, "/voice/session_start", map[string]any{"id": "s1", "started_at_ms": start.UnixMilli()}); w.Code != http.StatusOK {
		t.Fatalf("start: %d", w.Code)
	}
	end := map[string]any{"session_id": "s1", "outcome": map[string]any{"ok": true}, "ended_at_ms": start.Add(10*time.Minute + time.Second).UnixMilli()}
	for i := 0; i < 2; i++ { // a retried end records once
		if w := postJSON(h, "/voice/session_end", end); w.Code != http.StatusOK {
			t.Fatalf("end: %d %s", w.Code, w.Body)
		}
	}
	day, _ := st.BudgetDay(context.Background(), time.Now())
	if day.Calls != 1 || day.CallMinutes != 11 {
		t.Fatalf("10m01s rounds up to 11 minutes once: %+v", day)
	}
	w := postJSON(h, "/voice/session_start", map[string]any{"id": "s2"})
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "call-minute limit 10") {
		t.Fatalf("minute cap: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(waitNotice(t, f.notices), "call-minute cap reached: 11 of 10 minutes") {
		t.Fatal("minute notice")
	}
}

func TestNoCapsAcceptsEveryCallAndCountsIt(t *testing.T) {
	st, _ := openStore(t)
	f := &capsFixture{notices: make(chan string, 1)}
	h := voiceServer(t, st, newCallGate(st, voiceStore(t, st), f.budget()))
	for i := 0; i < 5; i++ {
		if w := postJSON(h, "/voice/session_start", map[string]any{"id": "s" + string(rune('a'+i))}); w.Code != http.StatusOK {
			t.Fatalf("uncapped call refused: %d", w.Code)
		}
	}
	if w := postJSON(h, "/voice/pre_call_context", map[string]any{"caller_phone": "+10000000000"}); w.Code != http.StatusOK {
		t.Fatalf("uncapped pre-call: %d", w.Code)
	}
	day, _ := st.BudgetDay(context.Background(), time.Now())
	if day.Calls != 5 {
		t.Fatalf("calls = %d", day.Calls)
	}
	// A ledger failure without caps leaves the call path as it was.
	if _, err := st.DB().Exec(`CREATE TRIGGER reject_calls BEFORE INSERT ON budget_voice_calls BEGIN SELECT RAISE(ABORT,'ledger down'); END`); err != nil {
		t.Fatal(err)
	}
	if w := postJSON(h, "/voice/session_start", map[string]any{"id": "sz"}); w.Code != http.StatusOK {
		t.Fatalf("uncapped call refused on ledger failure: %d", w.Code)
	}
	if w := postJSON(h, "/voice/session_end", map[string]any{"session_id": "sz", "outcome": map[string]any{}}); w.Code != http.StatusOK {
		t.Fatalf("uncapped end failed on ledger failure: %d", w.Code)
	}
}

func TestCappedLedgerFailureFailsClosed(t *testing.T) {
	st, _ := openStore(t)
	f := &capsFixture{calls: 100, notices: make(chan string, 1)}
	h := voiceServer(t, st, newCallGate(st, voiceStore(t, st), f.budget()))
	if _, err := st.DB().Exec(`CREATE TRIGGER reject_calls BEFORE INSERT ON budget_voice_calls BEGIN SELECT RAISE(ABORT,'ledger down'); END`); err != nil {
		t.Fatal(err)
	}
	w := postJSON(h, "/voice/session_start", map[string]any{"id": "s1"})
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "call budget unavailable") {
		t.Fatalf("ledger failure with caps: %d %s", w.Code, w.Body)
	}
	noNotice(t, f.notices)
}

func TestStagingSmokeRefusedAtCap(t *testing.T) {
	st, _ := openStore(t)
	f := &capsFixture{calls: 1, notices: make(chan string, 1)}
	gate := newCallGate(st, voiceStore(t, st), f.budget())
	if err := gate.Admit(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	hit := false
	dograhSrv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer dograhSrv.Close()
	b := &Bridge{gate: gate, cfg: voice.Config{Dograh: voice.DograhConfig{StagingURL: dograhSrv.URL}}}
	handled, _, err := b.TryAction("dograh_staging_smoke", "p", map[string]string{}, map[string]interface{}{"workflow_uuid": "w1"})
	if !handled || !errors.Is(err, voice.ErrCallRefused) || hit {
		t.Fatalf("smoke run at cap: handled=%v err=%v hit=%v", handled, err, hit)
	}
}

func TestCallMinutesRounding(t *testing.T) {
	base := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]int{0: 0, time.Second: 1, time.Minute: 1, time.Minute + time.Millisecond: 2, -time.Minute: 0} {
		if got := callMinutes(base, base.Add(d)); got != want {
			t.Errorf("%s -> %d, want %d", d, got, want)
		}
	}
	if callMinutes(time.Time{}, base) != 0 {
		t.Error("unknown start must count no minutes")
	}
}

func voiceStore(t *testing.T, st *statestore.StateStore) *voice.Store {
	t.Helper()
	vs, err := voice.NewStore(st.DB())
	if err != nil {
		t.Fatal(err)
	}
	return vs
}
