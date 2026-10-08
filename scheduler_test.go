package main

import (
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fixedClock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fixedClock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

func schedulerAt(t0 time.Time, pipelines ...config.PipelineConfig) (*Scheduler, *fixedClock) {
	clk := &fixedClock{t: t0}
	s := newScheduler(nil)
	s.now = clk.now
	for _, p := range pipelines {
		ps := &PipelineState{Name: p.Name, catchUp: p.CatchUp, pauseAfter: p.PauseAfterFailures, timezone: p.Timezone}
		_ = ps.setSchedule(p.Schedule, t0)
		s.pipelines[p.Name] = ps
	}
	return s, clk
}

type sentLog struct {
	mu   sync.Mutex
	msgs []string
}

func (l *sentLog) Send(text string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, text)
	return nil
}

func TestManualRunCannotOverlapTimerOrWebhookRun(t *testing.T) {
	s, _ := schedulerAt(time.Now(), config.PipelineConfig{Name: "p", Schedule: "5m"})
	if ok, _ := s.TryStart("p"); !ok {
		t.Fatal("first claim refused")
	}
	if ok, reason := s.TryStartManual("p"); ok || reason != "pipeline is already running" {
		t.Fatalf("manual run overlapped a running pipeline: ok=%v reason=%q", ok, reason)
	}
	s.Finish("p", nil)
	if ok, _ := s.TryStartManual("p"); !ok {
		t.Fatal("manual run refused after the first finished")
	}
}

func TestManualRunAllowedWhilePausedWebhookIsNot(t *testing.T) {
	s, _ := schedulerAt(time.Now(), config.PipelineConfig{Name: "p", Schedule: "5m"})
	s.Pause("p")
	if ok, _ := s.TryStart("p"); ok {
		t.Fatal("webhook/timer claim admitted while paused")
	}
	if ok, reason := s.TryStartManual("p"); !ok {
		t.Fatalf("operator /run refused while paused: %s", reason)
	}
}

func TestClaimDueClaimsAtomically(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	s, clk := schedulerAt(t0, config.PipelineConfig{Name: "p", Schedule: "1m"})
	clk.set(t0.Add(2 * time.Minute))
	if got := s.ClaimDue(); len(got) != 1 || got[0] != "p" {
		t.Fatalf("ClaimDue = %v", got)
	}
	if got := s.ClaimDue(); len(got) != 0 {
		t.Fatalf("a claimed pipeline was handed out twice: %v", got)
	}
	if ok, _ := s.TryStart("p"); ok {
		t.Fatal("webhook claim admitted while the timer run holds the pipeline")
	}
}

func TestCalendarScheduleNextRunInZone(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	t0 := time.Date(2026, 10, 9, 9, 0, 0, 0, berlin) // Friday
	s, _ := schedulerAt(t0, config.PipelineConfig{Name: "digest", Schedule: "0 8 * * 1-5", Timezone: "Europe/Berlin"})
	want := time.Date(2026, 10, 12, 8, 0, 0, 0, berlin)
	if got := s.pipelines["digest"].NextRun; !got.Equal(want) {
		t.Fatalf("NextRun = %v, want %v", got, want)
	}
}

func TestRestoreContinuesIntervalFromLastRun(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	s, _ := schedulerAt(t0, config.PipelineConfig{Name: "daily", Schedule: "24h"})
	// Last run ended 20h ago: the next run is 4h from now, not 24h.
	s.Restore("daily", []statestore.RunRecord{{Status: "ok", EndedAt: t0.Add(-20 * time.Hour)}})
	if got, want := s.pipelines["daily"].NextRun, t0.Add(4*time.Hour); !got.Equal(want) {
		t.Fatalf("NextRun = %v, want %v", got, want)
	}
	// Overdue: last run 30h ago runs on the next tick.
	s.Restore("daily", []statestore.RunRecord{{Status: "ok", EndedAt: t0.Add(-30 * time.Hour)}})
	if got := s.pipelines["daily"].NextRun; !got.Equal(t0) {
		t.Fatalf("overdue NextRun = %v, want now", got)
	}
}

func TestRestoreCalendarCatchUp(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	missed := []statestore.RunRecord{{Status: "ok", EndedAt: time.Date(2026, 10, 7, 8, 1, 0, 0, time.UTC)}}

	s, _ := schedulerAt(t0, config.PipelineConfig{Name: "p", Schedule: "0 8 * * *", Timezone: "UTC"})
	s.Restore("p", missed)
	if got, want := s.pipelines["p"].NextRun, time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("without catch_up NextRun = %v, want %v", got, want)
	}

	s, _ = schedulerAt(t0, config.PipelineConfig{Name: "p", Schedule: "0 8 * * *", Timezone: "UTC", CatchUp: true})
	s.Restore("p", missed)
	if got := s.pipelines["p"].NextRun; !got.Equal(t0) {
		t.Fatalf("with catch_up NextRun = %v, want now", got)
	}
}

func TestAutoPauseAfterConsecutiveFailures(t *testing.T) {
	s, _ := schedulerAt(time.Now(), config.PipelineConfig{Name: "p", Schedule: "5m", PauseAfterFailures: 3})
	boom := errors.New("provider down")
	for i := 1; i <= 2; i++ {
		s.TryStart("p")
		if s.Finish("p", boom) {
			t.Fatalf("paused after %d failures", i)
		}
	}
	s.TryStart("p")
	s.Finish("p", nil) // a success resets the streak
	if s.failureStreak("p") != 0 {
		t.Fatalf("success did not reset the streak: %d", s.failureStreak("p"))
	}
	for i := 1; i <= 3; i++ {
		s.TryStart("p")
		paused := s.Finish("p", boom)
		if paused != (i == 3) {
			t.Fatalf("failure %d: paused=%v", i, paused)
		}
	}
	if ps := s.pipelines["p"]; !ps.Paused || !ps.AutoPaused {
		t.Fatalf("state after streak: %+v", ps)
	}
	if got := s.GetDue(); len(got) != 0 {
		t.Fatalf("auto-paused pipeline still due: %v", got)
	}
	s.Resume("p")
	if ps := s.pipelines["p"]; ps.Paused || ps.AutoPaused || ps.Failures != 0 {
		t.Fatalf("resume left state: %+v", ps)
	}
}

func TestFailuresOnUntimedPipelineNeverPause(t *testing.T) {
	s, _ := schedulerAt(time.Now(), config.PipelineConfig{Name: "hook", Schedule: "webhook", PauseAfterFailures: 1})
	s.TryStart("hook")
	if s.Finish("hook", errors.New("x")) {
		t.Fatal("webhook pipeline was auto-paused")
	}
}

func TestRestoreStartsPausedOnRecordedStreak(t *testing.T) {
	s, _ := schedulerAt(time.Now(), config.PipelineConfig{Name: "p", Schedule: "5m", PauseAfterFailures: 2})
	runs := []statestore.RunRecord{{Status: "error", EndedAt: time.Now()}, {Status: "error", EndedAt: time.Now().Add(-5 * time.Minute)}}
	if !s.Restore("p", runs) {
		t.Fatal("recorded streak did not pause the pipeline")
	}
	s2, _ := schedulerAt(time.Now(), config.PipelineConfig{Name: "p", Schedule: "5m", PauseAfterFailures: 2})
	if s2.Restore("p", []statestore.RunRecord{{Status: "error"}, {Status: "ok"}}) {
		t.Fatal("broken streak paused the pipeline")
	}
	if s2.failureStreak("p") != 1 {
		t.Fatalf("streak = %d, want 1", s2.failureStreak("p"))
	}
}

func TestDrainRefusesNewRunsAndWaitsForAdmitted(t *testing.T) {
	s, clk := schedulerAt(time.Now(), config.PipelineConfig{Name: "a", Schedule: "1m"}, config.PipelineConfig{Name: "b", Schedule: "manual"})
	if ok, _ := s.TryStartManual("b"); !ok {
		t.Fatal("claim refused")
	}
	s.BeginDrain()
	clk.set(time.Now().Add(time.Hour))
	if got := s.ClaimDue(); len(got) != 0 {
		t.Fatalf("timer run admitted during drain: %v", got)
	}
	if ok, reason := s.TryStart("a"); ok || reason != "engine is shutting down" {
		t.Fatalf("webhook run admitted during drain: %v %q", ok, reason)
	}
	if s.Wait(20 * time.Millisecond) {
		t.Fatal("Wait returned true with a run still admitted")
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		s.Finish("b", nil)
	}()
	if !s.Wait(2 * time.Second) {
		t.Fatal("Wait timed out after the run finished")
	}
}

func TestDrainEngineWithNothingRunning(t *testing.T) {
	s, _ := schedulerAt(time.Now(), config.PipelineConfig{Name: "a", Schedule: "manual"})
	if !drainEngine(s, nil, time.Second) {
		t.Fatal("drain with no runs reported unfinished work")
	}
}

func TestSetRunningIsIdempotentForTheWaitGroup(t *testing.T) {
	s, _ := schedulerAt(time.Now(), config.PipelineConfig{Name: "a", Schedule: "manual"})
	s.TryStart("a")
	s.SetRunning("a", false)
	s.SetRunning("a", false) // must not panic with a negative counter
	if !s.Wait(time.Second) {
		t.Fatal("released claim still counted")
	}
}

func TestRescheduleAcceptsCronAndRejectsGarbage(t *testing.T) {
	s, _ := schedulerAt(time.Now(), config.PipelineConfig{Name: "p", Schedule: "5m", Timezone: "UTC"})
	if err := s.Reschedule("p", "0 9 * * 1-5"); err != nil {
		t.Fatalf("cron reschedule refused: %v", err)
	}
	if err := s.Reschedule("p", "every tuesday"); err == nil {
		t.Fatal("garbage schedule accepted")
	}
	if s.pipelines["p"].Schedule != "0 9 * * 1-5" {
		t.Fatalf("schedule changed by a refused reschedule: %q", s.pipelines["p"].Schedule)
	}
	if err := s.Reschedule("nope", "5m"); err == nil {
		t.Fatal("unknown pipeline accepted")
	}
}

func TestFinishRunNotifiesOnceOnAutoPause(t *testing.T) {
	s, _ := schedulerAt(time.Now(), config.PipelineConfig{Name: "p", Schedule: "5m", PauseAfterFailures: 2})
	ch := &sentLog{}
	for i := 0; i < 2; i++ {
		s.TryStart("p")
		finishRun(s, ch, "p", errors.New("upstream 503"))
	}
	if len(ch.msgs) != 1 || !strings.Contains(ch.msgs[0], "Paused p after 2 consecutive failed runs") || !strings.Contains(ch.msgs[0], "upstream 503") {
		t.Fatalf("notices = %q", ch.msgs)
	}
}

func TestRestoreSchedulesFromStateStore(t *testing.T) {
	store, err := statestore.OpenStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	for i := 3; i >= 1; i-- {
		started := now.Add(-time.Duration(i) * 10 * time.Minute)
		if err := store.RecordRunForID("r", "flaky", started, started.Add(time.Second), errors.New("timeout")); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{Pipelines: []config.PipelineConfig{{Name: "flaky", Schedule: "10m", PauseAfterFailures: 3}, {Name: "fresh", Schedule: "10m"}}}
	s := newScheduler(cfg.Pipelines)
	ch := &sentLog{}
	restoreSchedules(s, cfg, store, ch)
	if !s.pipelines["flaky"].Paused {
		t.Fatal("pipeline with a recorded failure streak did not start paused")
	}
	if s.pipelines["fresh"].Paused {
		t.Fatal("pipeline without history was paused")
	}
	if len(ch.msgs) != 1 || !strings.Contains(ch.msgs[0], "flaky") || !strings.Contains(ch.msgs[0], "timeout") {
		t.Fatalf("notices = %q", ch.msgs)
	}
}

func TestEngineGaugesReportState(t *testing.T) {
	store, err := statestore.OpenStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := &config.Config{Pipelines: []config.PipelineConfig{{Name: "a", Schedule: "5m"}, {Name: "b", Schedule: "manual"}}}
	cfg.Budgets.PerDayCost = 20
	s := newScheduler(cfg.Pipelines)
	s.Pause("a")
	s.TryStartManual("b")
	b := &BudgetTracker{dayStart: time.Now()}
	if err := b.attachStore(store); err != nil {
		t.Fatal(err)
	}
	b.RecordCost(2.5)
	got := map[string]float64{}
	for _, g := range engineGauges(s, b, cfg, store) {
		key := g.Name
		if len(g.Labels) == 2 {
			key += "/" + g.Labels[1]
		}
		got[key] = g.Value
	}
	want := map[string]float64{
		"draftcat_pipelines_running":     1,
		"draftcat_pipeline_paused/a":     1,
		"draftcat_pipeline_paused/b":     0,
		"draftcat_approvals_open":        0,
		"draftcat_budget_day_cost":       2.5,
		"draftcat_budget_day_cost_limit": 20,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
	if _, ok := got["draftcat_budget_day_tokens_limit"]; ok {
		t.Error("token limit gauge reported without a token cap")
	}
}

func TestDrainEngineClosesListenerAndWaitsForRun(t *testing.T) {
	s, _ := schedulerAt(time.Now(), config.PipelineConfig{Name: "hooked", Schedule: "webhook"})
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()

	if ok, _ := s.TryStart("hooked"); !ok {
		t.Fatal("claim refused")
	}
	finished := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		s.Finish("hooked", nil)
		close(finished)
	}()
	if !drainEngine(s, srv, 2*time.Second) {
		t.Fatal("drain gave up on a run that finished inside the grace period")
	}
	<-finished
	if _, err := (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(t.Context(), "tcp", ln.Addr().String()); err == nil {
		t.Fatal("webhook listener still accepting after drain")
	}
}

func TestDrainEngineGivesUpAfterGrace(t *testing.T) {
	s, _ := schedulerAt(time.Now(), config.PipelineConfig{Name: "slow", Schedule: "manual"})
	s.TryStartManual("slow")
	start := time.Now()
	if drainEngine(s, nil, 30*time.Millisecond) {
		t.Fatal("drain reported success with a run still going")
	}
	if time.Since(start) > time.Second {
		t.Fatal("drain overran its grace period")
	}
}
