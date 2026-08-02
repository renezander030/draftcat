package main

// Gate context: the risk classification and budget snapshot that travel with an
// approval request, plus the escalation reminder that keeps a gate from dying
// silently.
//
// Two gaps this closes. Cost caps were enforced between calls but never shown
// to the person releasing the action, so the human answering "should this go
// out?" could not see what the run had already spent. And a pending gate ran
// straight to timeout with no nudge, so a run could die waiting on an operator
// who never saw the prompt in the first place.

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/renezander030/draftcat/internal/config"
)

type gateCtxKey string

const (
	riskKey   gateCtxKey = "draftcat.risk"
	budgetKey gateCtxKey = "draftcat.budget"
)

// budgetSnapshot is what the run has spent at the moment the gate opens. Caps
// of 0 mean "no cap configured" and are omitted from what the operator sees,
// rather than shown as a limit of zero.
type budgetSnapshot struct {
	SpentToday float64
	CapToday   float64
	SpentRun   float64
	CapRun     float64
}

// Configured reports whether there is anything worth showing.
func (b budgetSnapshot) Configured() bool {
	return b.CapToday > 0 || b.CapRun > 0 || b.SpentToday > 0 || b.SpentRun > 0
}

func withGateMeta(ctx context.Context, risk string, b budgetSnapshot) context.Context {
	ctx = context.WithValue(ctx, riskKey, risk)
	return context.WithValue(ctx, budgetKey, b)
}

func riskFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(riskKey).(string); ok {
		return v
	}
	return ""
}

func budgetFromContext(ctx context.Context) (budgetSnapshot, bool) {
	if ctx == nil {
		return budgetSnapshot{}, false
	}
	b, ok := ctx.Value(budgetKey).(budgetSnapshot)
	return b, ok && b.Configured()
}

// budgetLine renders the spend context appended to an operator's draft. Returns
// "" when no caps are configured and nothing has been spent, so instances that
// never set a budget see no change.
func budgetLine(spentToday, capToday, spentRun, capRun float64) string {
	b := budgetSnapshot{SpentToday: spentToday, CapToday: capToday, SpentRun: spentRun, CapRun: capRun}
	if !b.Configured() {
		return ""
	}
	var parts []string
	if capToday > 0 {
		parts = append(parts, fmt.Sprintf("today %.4f/%.4f (%.0f%% left)",
			spentToday, capToday, pctLeft(spentToday, capToday)))
	} else if spentToday > 0 {
		parts = append(parts, fmt.Sprintf("today %.4f (no cap)", spentToday))
	}
	if capRun > 0 {
		parts = append(parts, fmt.Sprintf("this run %.4f/%.4f", spentRun, capRun))
	} else if spentRun > 0 {
		parts = append(parts, fmt.Sprintf("this run %.4f", spentRun))
	}
	if len(parts) == 0 {
		return ""
	}
	return "spend — " + strings.Join(parts, " · ")
}

func pctLeft(spent, cap float64) float64 {
	if cap <= 0 {
		return 100
	}
	left := (1 - spent/cap) * 100
	if left < 0 {
		return 0
	}
	return left
}

// startEscalation re-notifies the operator channel once the step's
// escalate_after has passed with no decision, and returns a stop function the
// caller must invoke when the gate resolves.
//
// It deliberately only NOTIFIES. Adding the escalation list to the permitted
// approvers would mean a slow operator silently promotes someone the config
// never authorised to decide — authority is a config decision, not a timer.
func startEscalation(ctx context.Context, ch OperatorChannel, step config.StepConfig, pipeline string, window time.Duration) func() {
	after, err := time.ParseDuration(strings.TrimSpace(step.EscalateAfter))
	if step.EscalateAfter == "" || err != nil || after <= 0 {
		return func() {}
	}
	if after >= window {
		// A reminder at or past the timeout would never fire before the gate
		// closed; say so rather than pretending it is armed.
		log.Printf("[pipeline:%s][step:%s] escalate_after %s is not shorter than the approval window %s — no reminder will fire",
			pipeline, step.Name, after, window)
		return func() {}
	}

	done := make(chan struct{})
	go func() {
		t := time.NewTimer(after)
		defer t.Stop()
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-t.C:
		}
		remaining := window - after
		msg := fmt.Sprintf("[draftcat] Still waiting on approval — %s / %s. About %s left before it times out and the action does not go out.",
			pipeline, step.Name, remaining.Round(time.Minute))
		if len(step.EscalateTo) > 0 {
			msg += fmt.Sprintf("\nEscalating to: %s (notified, not authorised to decide)", formatIDs(step.EscalateTo))
		}
		if serr := ch.Send(msg); serr != nil {
			log.Printf("[pipeline:%s][step:%s] escalation notice failed: %v", pipeline, step.Name, serr)
		}
	}()
	var once bool
	return func() {
		if !once {
			once = true
			close(done)
		}
	}
}

func formatIDs(ids []int64) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, fmt.Sprintf("%d", id))
	}
	return strings.Join(out, ", ")
}
