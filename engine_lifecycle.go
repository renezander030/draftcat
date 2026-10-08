package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/renezander030/draftcat/internal/config"
	"github.com/renezander030/draftcat/internal/obs"
	"github.com/renezander030/draftcat/internal/redact"
	statestore "github.com/renezander030/draftcat/internal/state"
)

const defaultShutdownGrace = 30 * time.Second

// secretEnvNames lists every environment variable the engine reads a
// credential from, labelled by the name the operator configured.
func secretEnvNames(cfg *config.Config) []string {
	names := []string{
		firstNonEmpty(cfg.Telegram.TokenEnv, "DRAFTCAT_TG_TOKEN"),
		firstNonEmpty(cfg.Relay.SecretEnv, "DRAFTCAT_RELAY_SECRET"),
		firstNonEmpty(cfg.Provider.APIKeyEnv, "OPENROUTER_API_KEY"),
		"DRAFTCAT_APPROVAL_SECRET",
	}
	for _, n := range []string{cfg.Webhook.SecretEnv, cfg.GHL.APIKeyEnv, cfg.Observ.OTLP.HeaderEnv} {
		if n != "" {
			names = append(names, n)
		}
	}
	return names
}

// registerSecrets registers the value of every credential variable for
// redaction. OTLP header variables hold "k=v,k2=v2"; each value is
// registered on its own as well as the whole string.
func registerSecrets(cfg *config.Config) {
	for _, name := range secretEnvNames(cfg) {
		v := os.Getenv(name)
		redact.Register(name, v)
		if name == cfg.Observ.OTLP.HeaderEnv {
			for _, hv := range obs.ParseHeaderEnv(v) {
				redact.Register(name, hv)
				// "Bearer <token>" — the token alone may also be echoed.
				if i := strings.IndexByte(hv, ' '); i > 0 {
					redact.Register(name, hv[i+1:])
				}
			}
		}
	}
	// Secrets already resolved into the config, in case they came from a
	// fallback variable.
	redact.Register(firstNonEmpty(cfg.Telegram.TokenEnv, "DRAFTCAT_TG_TOKEN"), cfg.Telegram.Token())
	redact.Register(firstNonEmpty(cfg.Relay.SecretEnv, "DRAFTCAT_RELAY_SECRET"), cfg.Relay.Secret())
	redact.Register(firstNonEmpty(cfg.Provider.APIKeyEnv, "OPENROUTER_API_KEY"), cfg.Provider.APIKey())
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// restoreSchedules continues every pipeline's schedule and failure streak
// from the run history, and tells the operator about pipelines that start
// paused because their recorded streak reached pause_after_failures.
func restoreSchedules(sched *Scheduler, cfg *config.Config, store *statestore.StateStore, ch interface{ Send(string) error }) {
	if store == nil {
		return
	}
	for _, p := range cfg.Pipelines {
		n := p.PauseAfterFailures
		if n < 1 {
			n = 1
		}
		runs, err := store.RecentRuns(p.Name, n)
		if err != nil {
			log.Printf("[scheduler] run history for %s unavailable: %v", p.Name, err)
			continue
		}
		if sched.Restore(p.Name, runs) {
			log.Printf("[scheduler] %s starts paused: last %d runs failed", p.Name, len(runs))
			if ch != nil {
				msg := autoPauseNotice(p.Name, len(runs), nil)
				if len(runs) > 0 && runs[0].Error != "" {
					msg = autoPauseNotice(p.Name, len(runs), errText(runs[0].Error))
				}
				_ = ch.Send(msg)
			}
		}
	}
}

type errText string

func (e errText) Error() string { return string(e) }

// shutdownGrace returns timeouts.shutdown_grace, default 30s.
func shutdownGrace(cfg *config.Config) time.Duration {
	if v := strings.TrimSpace(cfg.Timeouts.ShutdownGrace); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
	}
	return defaultShutdownGrace
}

// drainEngine stops admitting runs, closes the webhook listener after its
// in-flight requests, and waits up to grace for admitted pipeline runs.
// Approval taps keep arriving while it waits, so a run blocked on a decision
// can still finish. An approval gate still open at exit is reconciled at the
// next start.
func drainEngine(sched *Scheduler, webhook *http.Server, grace time.Duration) bool {
	sched.BeginDrain()
	if webhook != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := webhook.Shutdown(ctx); err != nil {
			log.Printf("[shutdown] webhook listener: %v", err)
		}
		cancel()
	}
	running := sched.RunningNames()
	if len(running) == 0 {
		return true
	}
	log.Printf("[shutdown] waiting up to %s for %d running pipeline(s): %s", grace, len(running), strings.Join(running, ", "))
	if sched.Wait(grace) {
		log.Printf("[shutdown] all pipeline runs finished")
		return true
	}
	log.Printf("[shutdown] grace period elapsed with %s still running; open approval gates are reconciled at next start", strings.Join(sched.RunningNames(), ", "))
	return false
}

// redactedError keeps the wrapped error for errors.Is/As while its message
// carries no registered secret.
type redactedError struct{ err error }

func (e redactedError) Error() string { return redact.String(e.err.Error()) }
func (e redactedError) Unwrap() error { return e.err }

// redactErr returns err with registered secrets removed from its message.
func redactErr(err error) error {
	if err == nil {
		return nil
	}
	return redactedError{err}
}

// engineGauges reports current engine state for /metrics: runs in progress,
// paused pipelines, open approval gates and today's usage against the caps.
func engineGauges(sched *Scheduler, budget *BudgetTracker, cfg *config.Config, store *statestore.StateStore) []obs.Gauge {
	var out []obs.Gauge
	running, _ := sched.Counts()
	out = append(out, obs.Gauge{Name: "draftcat_pipelines_running", Help: "Pipeline runs in progress.", Value: float64(running)})
	for _, ps := range sched.GetAll() {
		v := 0.0
		if ps.Paused {
			v = 1
		}
		out = append(out, obs.Gauge{Name: "draftcat_pipeline_paused", Help: "1 when the pipeline's timer is paused.", Labels: []string{"pipeline", ps.Name}, Value: v})
		out = append(out, obs.Gauge{Name: "draftcat_pipeline_consecutive_failures", Help: "Consecutive failed runs of the pipeline.", Labels: []string{"pipeline", ps.Name}, Value: float64(ps.Failures)})
	}
	if store != nil {
		if open, err := store.OpenApprovals(); err == nil {
			out = append(out, obs.Gauge{Name: "draftcat_approvals_open", Help: "Approval gates waiting on a decision.", Value: float64(len(open))})
		}
	}
	if budget != nil {
		snap := budget.snapshot()
		out = append(out,
			obs.Gauge{Name: "draftcat_budget_day_tokens", Help: "Tokens used today (UTC).", Value: float64(snap.tokensUsedToday)},
			obs.Gauge{Name: "draftcat_budget_day_cost", Help: "Cost spent today (UTC), in the unit of the model rates.", Value: snap.costToday},
			obs.Gauge{Name: "draftcat_budget_unsettled_calls", Help: "Provider calls whose usage is not settled yet.", Value: float64(snap.unsettled)},
		)
		if cfg.Budgets.PerDayTokens > 0 {
			out = append(out, obs.Gauge{Name: "draftcat_budget_day_tokens_limit", Help: "Daily token cap.", Value: float64(cfg.Budgets.PerDayTokens)})
		}
		if cfg.Budgets.PerDayCost > 0 {
			out = append(out, obs.Gauge{Name: "draftcat_budget_day_cost_limit", Help: "Daily cost cap.", Value: cfg.Budgets.PerDayCost})
		}
	}
	return out
}
