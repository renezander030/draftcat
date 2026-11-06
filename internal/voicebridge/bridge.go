//go:build voice

// Package voicebridge wires draftcat's engine to the self-hosted voice plugin:
// it boots the voice webhook server, registers pre-call lookups against the
// engine's existing connectors, and dispatches the voice_*/dograh_* pipeline
// actions. It depends only on the internal connector/store packages and the
// decoded voice config, never on package main — so it lives outside the root.
package voicebridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/renezander030/draftcat/internal/dograh"
	ghlapi "github.com/renezander030/draftcat/internal/ghl"
	statestore "github.com/renezander030/draftcat/internal/state"
	"github.com/renezander030/draftcat/voice"
)

// Bridge owns the running voice server plus the config + dedup store the
// pipeline actions need. A nil *Bridge is the "voice disabled" state; all
// methods are nil-safe so callers can hold a nil bridge without guarding.
type Bridge struct {
	server     *voice.Server
	cfg        voice.Config
	dedupStore *statestore.StateStore
	gate       *callGate
}

// Boot decodes the raw `voice:` config node, and if enabled, starts the voice
// webhook server (sharing the engine's SQLite file), holds its calls to the
// daily call caps in budget, and registers pre-call lookups against the GHL
// connector (when configured). Returns nil when voice is disabled,
// misconfigured, or no state store is available — that nil is a valid
// "no voice" Bridge.
func Boot(raw yaml.Node, st *statestore.StateStore, ghl *ghlapi.GHLConnector, budget CallBudget) *Bridge {
	if st == nil {
		return nil
	}
	var vcfg voice.Config
	if err := raw.Decode(&vcfg); err != nil {
		log.Printf("[voice] config decode failed (skipping voice): %v", err)
		return nil
	}
	if !vcfg.Enabled {
		return nil
	}
	srv, err := voice.NewServer(vcfg, st.DB())
	if err != nil {
		log.Printf("[voice] init failed: %v", err)
		return nil
	}
	if srv == nil {
		return nil
	}
	gate := newCallGate(st, srv.Store(), budget)
	srv.SetCallGate(gate)
	registerLookups(srv, ghl)
	go func() {
		if err := srv.Start(); err != nil && err.Error() != "http: Server closed" {
			log.Printf("[voice] server stopped: %v", err)
		}
	}()
	return &Bridge{server: srv, cfg: vcfg, dedupStore: st, gate: gate}
}

// registerLookups wires draftcat's existing connectors into the voice plugin's
// pre-call lookup runner. Each backend is opt-in: when the underlying connector
// isn't configured, the source is simply not registered and the lookup runner
// emits a warning if a pipeline tries to use it.
func registerLookups(srv *voice.Server, ghl *ghlapi.GHLConnector) {
	if ghl == nil {
		return
	}
	srv.Lookups().Register("ghl", func(ctx context.Context, phone string) (map[string]any, error) {
		c, err := ghl.FetchContactByPhone(phone)
		if err != nil {
			return nil, err
		}
		if c == nil {
			return nil, nil
		}
		out := map[string]any{
			"account_name": strings.TrimSpace(c.FirstName + " " + c.LastName),
			"email":        c.Email,
			"contact_id":   c.ID,
			"source":       c.Source,
		}
		for _, cf := range c.CustomField {
			if cf.ID != "" && cf.Value != "" {
				out["cf_"+cf.ID] = cf.Value
			}
		}
		return out, nil
	})
}

// Shutdown stops the voice server. Safe to call on a nil Bridge.
func (b *Bridge) Shutdown() {
	if b == nil || b.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.server.Shutdown(ctx); err != nil {
		log.Printf("[voice] shutdown error: %v", err)
	}
}

// TryAction handles the voice_*/dograh_* pipeline actions. Returns
// (handled, skipPipeline, err): handled=false means the action isn't ours and
// the caller should keep matching. Safe to call on a nil Bridge.
func (b *Bridge) TryAction(action, pipelineName string, vars map[string]string, data map[string]interface{}) (handled bool, skipPipeline bool, err error) {
	if b == nil {
		return false, false, nil
	}
	// Admin actions: no voice store dependency (git is local, Dograh HTTP is
	// configured separately). Keep them at the top so the guardrail pipeline
	// runs even before any session has been recorded.
	switch action {
	case "git_commit_workflow_update":
		sha, err := dograh.GitCommitWorkflow(vars, data)
		if err != nil {
			return true, false, err
		}
		data["voice_admin_commit_sha"] = sha
		return true, false, nil

	case "dograh_staging_smoke":
		// This run places a call, so the daily call caps apply to it. It is
		// counted when its session_start reaches the voice receivers.
		if b.gate != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := b.gate.Check(ctx)
			cancel()
			if err != nil {
				return true, false, fmt.Errorf("dograh_staging_smoke: %w", err)
			}
		}
		runID, err := dograh.DograhTriggerRun(b.dograhConfig(), setEnv(vars, "staging"), data)
		if err != nil {
			return true, false, err
		}
		data["voice_admin_smoke_run_id"] = runID
		return true, false, nil

	case "dograh_prod_publish":
		if err := dograh.DograhUpdateWorkflow(b.dograhConfig(), vars, data); err != nil {
			return true, false, err
		}
		data["voice_admin_publish_status"] = "ok"
		return true, false, nil
	}

	// Harvest actions: require the voice store.
	if b.server == nil {
		return false, false, nil
	}
	store := b.server.Store()
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
		items = statestore.DedupByID(b.dedupStore, pipelineName, "voice_calls", items,
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
		items = statestore.DedupByID(b.dedupStore, pipelineName, "voice_handoffs", items,
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
		items = statestore.DedupByID(b.dedupStore, pipelineName, "voice_learnings", items,
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

// dograhConfig maps the voice plugin's Dograh settings onto the dograh
// connector's own Config, keeping internal/dograh free of any voice types.
func (b *Bridge) dograhConfig() dograh.Config {
	return dograh.Config{
		BaseURL:    b.cfg.Dograh.BaseURL,
		StagingURL: b.cfg.Dograh.StagingURL,
		APIKeyEnv:  b.cfg.Dograh.APIKeyEnv,
	}
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

// setEnv returns a copy of vars with "env" forced to the given value. Used to
// route the same dograh trigger helper at staging vs prod from different action
// names.
func setEnv(vars map[string]string, env string) map[string]string {
	out := make(map[string]string, len(vars)+1)
	for k, v := range vars {
		out[k] = v
	}
	out["env"] = env
	return out
}

func varInt(vars map[string]string, key string, def int) int {
	if v, ok := vars[key]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
