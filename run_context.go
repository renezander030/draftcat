package main

// Run identity: one ID per pipeline run, carried on the context so every step,
// approval and audit row can be tied back to the run that produced it.
//
// Before this, `action_approvals` recorded pipeline and step but nothing that
// distinguished one run of a pipeline from the next, so the audit trail could
// say "someone approved send-followup at 09:31" but not which run that approval
// released. With a run a day that is a reconstruction job; with a pipeline on a
// five-minute schedule it is guesswork. The gate's strongest claim is that it
// can show what was approved and what that approval let happen, and that claim
// needs a join key.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"
)

type runCtxKey string

const (
	runIDKey    runCtxKey = "draftcat.run_id"
	pipelineKey runCtxKey = "draftcat.pipeline"
	stepKey     runCtxKey = "draftcat.step"
)

// newRunID returns a lexicographically sortable run identifier: a zero-padded
// unix-millisecond prefix so runs sort by start time, plus 64 random bits so
// two runs starting in the same millisecond cannot collide.
func newRunID(now time.Time) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// A run without an ID is worse than a run with a time-only one: the
		// audit trail still needs a join key it can group by.
		return hex.EncodeToString([]byte(now.UTC().Format("20060102150405.000000000")))
	}
	ts := now.UTC().UnixMilli()
	out := make([]byte, 0, 29)
	for shift := 40; shift >= 0; shift -= 8 {
		out = append(out, hexDigits[(ts>>(shift+4))&0xf], hexDigits[(ts>>shift)&0xf])
	}
	return string(out) + "-" + hex.EncodeToString(b)
}

const hexDigits = "0123456789abcdef"

// withRun returns a context carrying the run identity.
func withRun(ctx context.Context, runID, pipeline string) context.Context {
	ctx = context.WithValue(ctx, runIDKey, runID)
	return context.WithValue(ctx, pipelineKey, pipeline)
}

// withStep returns a context carrying the step currently executing.
func withStep(ctx context.Context, step string) context.Context {
	return context.WithValue(ctx, stepKey, step)
}

func runIDFromContext(ctx context.Context) string    { return runCtxString(ctx, runIDKey) }
func pipelineFromContext(ctx context.Context) string { return runCtxString(ctx, pipelineKey) }
func stepFromContext(ctx context.Context) string     { return runCtxString(ctx, stepKey) }

func runCtxString(ctx context.Context, key runCtxKey) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(key).(string); ok {
		return v
	}
	return ""
}

// gateChannel returns the operator channel approval gates must route to. It
// exists so the pipeline-run call sites read the same whether a relay is
// configured or not, and so a nil opChan (unit tests, the standalone approval
// path) falls back to the channel the caller already had.
func gateChannel(fallback OperatorChannel) OperatorChannel {
	if opChan != nil {
		return opChan
	}
	return fallback
}
