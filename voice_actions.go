//go:build voice

package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/renezander030/draftyard/voice"
)

func tryVoiceAction(action, pipelineName string, vars map[string]string, data map[string]interface{}) (handled bool, skipPipeline bool, err error) {
	if voiceServer == nil {
		return false, false, nil
	}
	store := voiceServer.Store()
	if store == nil {
		return false, false, nil
	}

	switch action {
	case "voice_calls_completed":
		limit := varInt(vars, "limit", 50)
		items, err := store.FetchCompletedSessions(limit)
		if err != nil {
			return true, false, fmt.Errorf("voice_calls_completed: %w", err)
		}
		items = dedupByID(pipelineName, "voice_calls", items,
			func(c voice.CompletedSession) string { return c.ID })
		if len(items) == 0 {
			return true, true, nil
		}
		data["voice_calls"] = formatCallsForPrompt(items)
		data["voice_call_count"] = fmt.Sprintf("%d", len(items))
		return true, false, nil

	case "voice_handoffs_pending":
		limit := varInt(vars, "limit", 50)
		items, err := store.FetchPendingHandoffs(limit)
		if err != nil {
			return true, false, fmt.Errorf("voice_handoffs_pending: %w", err)
		}
		items = dedupByID(pipelineName, "voice_handoffs", items,
			func(h voice.PendingHandoff) string { return strconv.FormatInt(h.ID, 10) })
		if len(items) == 0 {
			return true, true, nil
		}
		data["voice_handoffs"] = formatHandoffsForPrompt(items)
		data["voice_handoff_count"] = fmt.Sprintf("%d", len(items))
		return true, false, nil

	case "voice_learnings_new":
		limit := varInt(vars, "limit", 50)
		items, err := store.FetchNewLearnings(limit)
		if err != nil {
			return true, false, fmt.Errorf("voice_learnings_new: %w", err)
		}
		items = dedupByID(pipelineName, "voice_learnings", items,
			func(l voice.NewLearning) string { return strconv.FormatInt(l.ID, 10) })
		if len(items) == 0 {
			return true, true, nil
		}
		data["voice_learnings"] = formatLearningsForPrompt(items)
		data["voice_learning_count"] = fmt.Sprintf("%d", len(items))
		return true, false, nil

	case "voice_handoffs_resolve":
		resolutions, parseErr := extractResolutions(data)
		if parseErr != nil {
			return true, false, fmt.Errorf("voice_handoffs_resolve: %w", parseErr)
		}
		if len(resolutions) == 0 {
			return true, true, nil
		}
		now := time.Now().UnixMilli()
		var resolved int
		for _, r := range resolutions {
			if r.HandoffID == 0 || r.Target == "" {
				continue
			}
			if err := store.ResolveHandoff(r.HandoffID, r.Target, now); err != nil {
				return true, false, fmt.Errorf("resolve handoff %d: %w", r.HandoffID, err)
			}
			resolved++
		}
		data["voice_handoffs_resolved_count"] = fmt.Sprintf("%d", resolved)
		return true, false, nil
	}

	return false, false, nil
}

type handoffResolution struct {
	HandoffID int64  `json:"handoff_id"`
	Target    string `json:"target"`
}

// extractResolutions pulls handoff resolutions out of data["ai_output"]. The
// AI step's output schema must expose a "resolutions" array of {handoff_id,
// target}. Returns an empty slice (no error) when ai_output is missing or has
// no resolutions, so the action can early-exit cleanly.
func extractResolutions(data map[string]interface{}) ([]handoffResolution, error) {
	raw, ok := data["ai_output"]
	if !ok {
		return nil, nil
	}
	var holder struct {
		Resolutions []handoffResolution `json:"resolutions"`
	}
	buf, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("ai_output not JSON-serializable: %w", err)
	}
	if err := json.Unmarshal(buf, &holder); err != nil {
		return nil, fmt.Errorf("ai_output.resolutions: %w", err)
	}
	return holder.Resolutions, nil
}

func formatCallsForPrompt(items []voice.CompletedSession) string {
	var sb strings.Builder
	for i, it := range items {
		fmt.Fprintf(&sb, "## Call %d\n", i+1)
		fmt.Fprintf(&sb, "session: %s\nworkflow: %s\nfrom: %s\n",
			it.ID, it.Workflow, it.CallerPhone)
		fmt.Fprintf(&sb, "started: %s\nended: %s\ncost_cents: %d\n",
			time.UnixMilli(it.StartedAt).UTC().Format(time.RFC3339),
			time.UnixMilli(it.EndedAt).UTC().Format(time.RFC3339),
			it.CostCents)
		if it.RecordingURL != "" {
			fmt.Fprintf(&sb, "recording: %s\n", it.RecordingURL)
		}
		fmt.Fprintf(&sb, "outcome: %s\n\n", it.OutcomeJSON)
	}
	return sb.String()
}

func formatHandoffsForPrompt(items []voice.PendingHandoff) string {
	var sb strings.Builder
	for i, h := range items {
		fmt.Fprintf(&sb, "## Handoff %d\n", i+1)
		fmt.Fprintf(&sb, "handoff_id: %d\nsession: %s\nrequested: %s\nreason: %s\n\n",
			h.ID, h.SessionID,
			time.UnixMilli(h.RequestedAt).UTC().Format(time.RFC3339),
			h.Reason)
	}
	return sb.String()
}

func formatLearningsForPrompt(items []voice.NewLearning) string {
	var sb strings.Builder
	for i, l := range items {
		fmt.Fprintf(&sb, "## Learning %d\n", i+1)
		fmt.Fprintf(&sb, "learning_id: %d\nsession: %s\ncategory: %s\nseverity: %s\n",
			l.ID, l.SessionID, l.Category, l.Severity)
		fmt.Fprintf(&sb, "ts: %s\n", time.UnixMilli(l.TS).UTC().Format(time.RFC3339))
		fmt.Fprintf(&sb, "description: %s\n\n", l.Description)
	}
	return sb.String()
}

func varInt(vars map[string]string, key string, def int) int {
	if v, ok := vars[key]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
