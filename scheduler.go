package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	"github.com/renezander030/draftcat/internal/schedule"
	statestore "github.com/renezander030/draftcat/internal/state"
)

// --- Scheduler ---
//
// Every run of a pipeline, whatever started it (timer, /run, the Run-now
// button, a webhook), is admitted through one claim under the scheduler lock.
// The claim is what keeps a pipeline at one concurrent run, what lets a
// shutdown refuse new work and wait for the runs it already admitted, and
// what counts consecutive failures for the auto-pause.

type PipelineState struct {
	Name     string
	Schedule string
	Paused   bool
	Running  bool
	LastRun  time.Time
	NextRun  time.Time
	// Failures counts consecutive failed timer runs; any success resets it.
	Failures int
	// AutoPaused is set when the pipeline was paused by pause_after_failures
	// rather than by an operator.
	AutoPaused bool

	timer      schedule.Schedule
	hasTimer   bool
	catchUp    bool
	pauseAfter int
	timezone   string
}

type Scheduler struct {
	mu        sync.Mutex
	pipelines map[string]*PipelineState
	inflight  sync.WaitGroup
	draining  bool
	now       func() time.Time
}

// isAutoSchedule reports whether a schedule string drives automatic timer runs.
// "manual" (operator /run only), "webhook" (HTTP trigger only), and "" are not
// timer-driven.
func isAutoSchedule(spec string) bool { return schedule.IsTimer(spec) }

func newScheduler(pipelines []config.PipelineConfig) *Scheduler {
	s := &Scheduler{pipelines: make(map[string]*PipelineState), now: time.Now}
	now := s.now()
	for _, p := range pipelines {
		ps := &PipelineState{
			Name:       p.Name,
			Schedule:   p.Schedule,
			catchUp:    p.CatchUp,
			pauseAfter: p.PauseAfterFailures,
			timezone:   p.Timezone,
		}
		_ = ps.setSchedule(p.Schedule, now) // validation reports a bad schedule before start
		s.pipelines[p.Name] = ps
	}
	return s
}

// setSchedule parses spec and plans the next run from now. An unparsable
// timer schedule never fires; validation reports it before the engine starts.
func (ps *PipelineState) setSchedule(spec string, now time.Time) error {
	ps.Schedule = spec
	ps.hasTimer = false
	ps.NextRun = time.Time{}
	if !schedule.IsTimer(spec) {
		return nil
	}
	sc, err := schedule.Parse(spec, ps.timezone)
	if err != nil {
		return err
	}
	ps.timer = sc
	ps.hasTimer = true
	ps.NextRun = sc.Next(now)
	return nil
}

// Restore continues a pipeline's schedule and failure count from its recorded
// runs (newest first), so a restart neither resets an interval nor forgets a
// failing streak. It reports whether the pipeline starts paused because the
// recorded streak already reached pause_after_failures.
func (s *Scheduler) Restore(name string, runs []statestore.RunRecord) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, ok := s.pipelines[name]
	if !ok || len(runs) == 0 {
		return false
	}
	now := s.now()
	ps.LastRun = runs[0].EndedAt
	if ps.hasTimer {
		if ps.timer.IsInterval() {
			ps.NextRun = ps.timer.Next(ps.LastRun)
			if ps.NextRun.Before(now) {
				ps.NextRun = now
			}
		} else if next := ps.timer.Next(ps.LastRun); !next.IsZero() && next.Before(now) {
			if ps.catchUp {
				ps.NextRun = now
			} else {
				ps.NextRun = ps.timer.Next(now)
			}
		} else {
			ps.NextRun = next
		}
	}
	streak := 0
	for _, r := range runs {
		if r.Status != "error" {
			break
		}
		streak++
	}
	ps.Failures = streak
	if ps.hasTimer && ps.pauseAfter > 0 && streak >= ps.pauseAfter {
		ps.Paused = true
		ps.AutoPaused = true
		return true
	}
	return false
}

func (s *Scheduler) GetAll() []*PipelineState {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*PipelineState
	for _, ps := range s.pipelines {
		cp := *ps
		out = append(out, &cp)
	}
	return out
}

func (s *Scheduler) Pause(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ps, ok := s.pipelines[name]; ok {
		ps.Paused = true
		return true
	}
	return false
}

// Resume clears a pause, operator or automatic, together with the failure
// streak, and plans the next run from now.
func (s *Scheduler) Resume(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ps, ok := s.pipelines[name]; ok {
		ps.Paused = false
		ps.AutoPaused = false
		ps.Failures = 0
		if ps.hasTimer {
			ps.NextRun = ps.timer.Next(s.now())
		}
		return true
	}
	return false
}

// claimLocked admits one run. manual runs (an operator's /run or Run-now) are
// allowed while the timer is paused; webhook and timer runs are not.
func (s *Scheduler) claimLocked(name string, manual bool) (bool, string) {
	ps, ok := s.pipelines[name]
	if !ok {
		return false, "unknown pipeline"
	}
	if s.draining {
		return false, "engine is shutting down"
	}
	if ps.Paused && !manual {
		return false, "pipeline is paused"
	}
	if ps.Running {
		return false, "pipeline is already running"
	}
	ps.Running = true
	s.inflight.Add(1)
	return true, ""
}

// TryStart atomically claims a pipeline for execution. It returns false (with a
// reason) if the pipeline is unknown, paused, already running, or the engine is
// shutting down. Used by the webhook trigger.
func (s *Scheduler) TryStart(name string) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claimLocked(name, false)
}

// TryStartManual is TryStart for an operator's explicit run request, which may
// run a pipeline whose timer is paused.
func (s *Scheduler) TryStartManual(name string) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claimLocked(name, true)
}

// Reschedule replaces a pipeline's schedule. The new value is parsed with the
// pipeline's time zone and refused if invalid.
func (s *Scheduler) Reschedule(name string, spec string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, ok := s.pipelines[name]
	if !ok {
		return fmt.Errorf("unknown pipeline: %s", name)
	}
	if schedule.IsTimer(spec) {
		if _, err := schedule.Parse(spec, ps.timezone); err != nil {
			return err
		}
	}
	return ps.setSchedule(spec, s.now())
}

// Finish records the end of a run that was claimed through the scheduler and
// releases the claim. It returns true when this failure completed a streak of
// pause_after_failures and the pipeline has just been paused.
func (s *Scheduler) Finish(name string, runErr error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, ok := s.pipelines[name]
	if !ok {
		return false
	}
	now := s.now()
	ps.LastRun = now
	if ps.hasTimer {
		ps.NextRun = ps.timer.Next(now)
	}
	if ps.Running {
		ps.Running = false
		s.inflight.Done()
	}
	if runErr == nil {
		ps.Failures = 0
		return false
	}
	ps.Failures++
	if ps.hasTimer && ps.pauseAfter > 0 && ps.Failures >= ps.pauseAfter && !ps.Paused {
		ps.Paused = true
		ps.AutoPaused = true
		return true
	}
	return false
}

// MarkRun records a successful end of a run. Kept for call sites that do not
// track the run's error.
func (s *Scheduler) MarkRun(name string) { s.Finish(name, nil) }

// SetRunning sets or clears the running flag directly. Clearing releases a
// claim taken by TryStart when the run is abandoned before it starts.
func (s *Scheduler) SetRunning(name string, running bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, ok := s.pipelines[name]
	if !ok || ps.Running == running {
		return
	}
	ps.Running = running
	if running {
		s.inflight.Add(1)
	} else {
		s.inflight.Done()
	}
}

// GetDue lists timer pipelines whose next run has passed, without claiming them.
func (s *Scheduler) GetDue() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dueLocked()
}

func (s *Scheduler) dueLocked() []string {
	var due []string
	now := s.now()
	for name, ps := range s.pipelines {
		if ps.Paused || ps.Running || !ps.hasTimer {
			continue
		}
		if !ps.NextRun.IsZero() && now.After(ps.NextRun) {
			due = append(due, name)
		}
	}
	return due
}

// ClaimDue lists due timer pipelines and claims each one in the same critical
// section, so no other trigger can start the same pipeline in between.
func (s *Scheduler) ClaimDue() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return nil
	}
	var claimed []string
	for _, name := range s.dueLocked() {
		if ok, _ := s.claimLocked(name, false); ok {
			claimed = append(claimed, name)
		}
	}
	return claimed
}

// BeginDrain refuses every new run from now on.
func (s *Scheduler) BeginDrain() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.draining = true
}

// Wait blocks until every admitted run has finished or timeout passes. It
// reports whether all runs finished.
func (s *Scheduler) Wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.inflight.Wait()
		close(done)
	}()
	if timeout <= 0 {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// Counts returns how many pipelines are running and how many are paused.
func (s *Scheduler) Counts() (running, paused int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ps := range s.pipelines {
		if ps.Running {
			running++
		}
		if ps.Paused {
			paused++
		}
	}
	return running, paused
}

// RunningNames lists the pipelines with an admitted run.
func (s *Scheduler) RunningNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for name, ps := range s.pipelines {
		if ps.Running {
			out = append(out, name)
		}
	}
	return out
}

// autoPauseNotice is the operator message sent when pause_after_failures
// stops a pipeline.
func autoPauseNotice(name string, failures int, runErr error) string {
	msg := fmt.Sprintf("[cron] Paused %s after %d consecutive failed runs.", name, failures)
	if runErr != nil {
		msg += "\nLast error: " + runErr.Error()
	}
	return msg + "\nResume with /cron resume " + name
}

// failureStreak returns the current consecutive failure count of a pipeline.
func (s *Scheduler) failureStreak(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ps, ok := s.pipelines[name]; ok {
		return ps.Failures
	}
	return 0
}
