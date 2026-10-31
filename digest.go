package main

// Pending digest: once a day, one message to the operator with only what
// needs a person. When nothing does, nothing is sent.

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	"github.com/renezander030/draftcat/internal/schedule"
	statestore "github.com/renezander030/draftcat/internal/state"
)

const (
	digestPendingAge = time.Hour
	digestFailedWin  = 24 * time.Hour
	digestMaxListed  = 5
	digestRetryAfter = time.Minute
)

// digestSnapshot is everything the digest reads from the store.
type digestSnapshot struct {
	Now    time.Time
	Open   []statestore.PendingApproval // every gate waiting on a person
	Freeze statestore.GateFreeze
	// FailedOutcomes counts tool outcomes reported "failed" in the last 24 h.
	FailedOutcomes int
}

// buildDigest renders the digest. ok is false when nothing needs a person.
func buildDigest(s digestSnapshot) (text string, ok bool) {
	var stale []statestore.PendingApproval
	for _, p := range s.Open {
		if s.Now.Sub(p.OpenedAt) > digestPendingAge {
			stale = append(stale, p)
		}
	}
	sort.SliceStable(stale, func(i, j int) bool { return stale[i].OpenedAt.Before(stale[j].OpenedAt) })

	lines := []string{"[digest] Needs a person:"}
	if len(stale) > 0 {
		lines = append(lines, fmt.Sprintf("%d approval(s) waiting for over 1h (/pending):", len(stale)))
		for i, p := range stale {
			if i == digestMaxListed {
				lines = append(lines, fmt.Sprintf("  … and %d more", len(stale)-digestMaxListed))
				break
			}
			lines = append(lines, fmt.Sprintf("  %s / %s — opened %s ago", p.Pipeline, p.Step, humanDuration(s.Now.Sub(p.OpenedAt))))
		}
	}
	if s.Freeze.Frozen {
		lines = append(lines, describeFreeze(s.Freeze))
	}
	if s.FailedOutcomes > 0 {
		lines = append(lines, fmt.Sprintf("%d tool outcome(s) reported failed in the last 24h", s.FailedOutcomes))
	}
	if len(lines) == 1 {
		return "", false
	}
	return strings.Join(lines, "\n"), true
}

// digestSnapshotAt reads the store.
func digestSnapshotAt(st *statestore.StateStore, now time.Time) (digestSnapshot, error) {
	snap := digestSnapshot{Now: now}
	var err error
	if snap.Open, err = st.OpenApprovals(); err != nil {
		return snap, err
	}
	if snap.Freeze, err = st.GateFreeze(); err != nil {
		return snap, err
	}
	if snap.FailedOutcomes, err = st.FailedToolOutcomesSince(now.Add(-digestFailedWin)); err != nil {
		return snap, err
	}
	return snap, nil
}

// digestJob sends the digest at the configured time. The local date it was
// last handled for is kept in the state store, so a restart on the same day
// does not send it again. The day counts as handled when the digest was
// sent or when there was nothing to say.
type digestJob struct {
	sched schedule.Schedule
	send  func(string) error

	mu      sync.Mutex
	loaded  bool
	lastDay string
	retryAt time.Time
}

// newDigestJob returns nil when telegram.digest is absent.
func newDigestJob(cfg *config.Config, send func(string) error) (*digestJob, error) {
	d := cfg.Telegram.Digest
	if d == nil {
		return nil, nil
	}
	sc, err := schedule.ParseDaily(d.At, d.Timezone)
	if err != nil {
		return nil, fmt.Errorf("telegram.digest: %w", err)
	}
	return &digestJob{sched: sc, send: send}, nil
}

// tick is called from the engine loop; it does its work at most once a day.
func (d *digestJob) tick(now time.Time) {
	if d == nil || state == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	loc := d.sched.Location()
	local := now.In(loc)
	day := local.Format("2006-01-02")
	if d.loaded && d.lastDay == day {
		return
	}
	// The first slot of the local day: the schedule's next run after the
	// minute before midnight.
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	slot := d.sched.Next(midnight.Add(-time.Minute))
	if slot.IsZero() || slot.After(now) || slot.In(loc).Format("2006-01-02") != day || now.Before(d.retryAt) {
		return
	}
	if !d.loaded {
		last, err := state.DigestLastDay()
		if err != nil {
			log.Printf("[digest] last-sent day unreadable: %v", err)
			d.retryAt = now.Add(digestRetryAfter)
			return
		}
		d.loaded, d.lastDay = true, last
		if last == day {
			return
		}
	}
	snap, err := digestSnapshotAt(state, now)
	if err != nil {
		log.Printf("[digest] store unreadable: %v", err)
		d.retryAt = now.Add(digestRetryAfter)
		return
	}
	if text, ok := buildDigest(snap); ok {
		if err := d.send(text); err != nil {
			log.Printf("[digest] send failed, retrying: %v", err)
			d.retryAt = now.Add(digestRetryAfter)
			return
		}
		log.Printf("[digest] sent")
	}
	d.lastDay = day
	if err := state.SetDigestLastDay(day); err != nil {
		log.Printf("[digest] could not record the sent day: %v", err)
	}
}
