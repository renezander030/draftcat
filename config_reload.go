package main

import (
	"fmt"
	"log"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"gopkg.in/yaml.v3"

	"github.com/renezander030/draftcat/internal/config"
	skillsapi "github.com/renezander030/draftcat/internal/skills"
	"github.com/renezander030/draftcat/internal/validate"
)

// Config reload.
//
// The engine runs on immutable config snapshots: a run reads the snapshot that
// was current when it was claimed, so a reload never changes a run in
// progress. A reload builds the next snapshot from the file for the sections
// that only shape runs (pipelines, models, roles, budgets, timeouts, model and
// approval policy, skills) and keeps the sections that built long-lived
// resources at start (channels, provider, state, listeners, tool gate,
// observability, voice, the receipt anchor schedule) as they are, naming any that changed so the operator
// knows a restart is needed for them. Two parts of tool_gate are the exception:
// a reload re-reads the content of each rule's definition_file, so approving a
// tool's new definition does not take a restart, and tool_gate.shadow's
// candidate file.

var (
	liveConfig atomic.Pointer[config.Config]
	liveSkills atomic.Pointer[skillsapi.SkillRegistry]
	reloadMu   sync.Mutex
)

// currentConfig returns the live snapshot, or fallback before one is set.
func currentConfig(fallback *config.Config) *config.Config {
	if c := liveConfig.Load(); c != nil {
		return c
	}
	return fallback
}

// currentSkills returns the live skill registry, or fallback before one is set.
func currentSkills(fallback *skillsapi.SkillRegistry) *skillsapi.SkillRegistry {
	if s := liveSkills.Load(); s != nil {
		return s
	}
	return fallback
}

// restartOnlySections lists the config sections a reload does not apply.
func restartOnlySections(c *config.Config) map[string]interface{} {
	return map[string]interface{}{
		"telegram":      c.Telegram,
		"relay":         c.Relay,
		"gmail":         c.Gmail,
		"gohighlevel":   c.GHL,
		"state":         c.State,
		"provider":      c.Provider,
		"webhook":       c.Webhook,
		"tool_gate":     withoutShadow(c.ToolGate),
		"observability": c.Observ,
		"voice":         c.Voice,
		"receipts":      c.Receipts,
	}
}

// withoutShadow is tool_gate less tool_gate.shadow, which a reload applies.
func withoutShadow(g config.ToolGateConfig) config.ToolGateConfig {
	g.Shadow = nil
	return g
}

type reloadResult struct {
	Pipelines     int
	Skills        int
	RestartNeeded []string
	Warnings      int
	// definitions holds every definition_file as re-read, by rule index, for
	// the running rules once the reload is applied.
	definitions map[int]*config.PinnedDefinition
	// Shadow is the re-read tool_gate.shadow candidate (nil = off), or
	// ShadowErr why it was refused.
	Shadow    *shadowCandidate
	ShadowErr error
}

// buildReload reads configPath and skillsDir and returns the next snapshot.
// running is the live snapshot, boot is the file as parsed at start (before
// credentials were resolved). An invalid file returns an error and nothing
// changes.
func buildReload(configPath, skillsDir string, running, boot *config.Config) (*config.Config, *skillsapi.SkillRegistry, reloadResult, error) {
	var res reloadResult
	// #nosec G304 -- the operator's own config path, the same file the engine booted from.
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, nil, res, fmt.Errorf("read %s: %w", configPath, err)
	}
	var raw config.Config
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, nil, res, fmt.Errorf("parse %s: %w", configPath, err)
	}

	next := *running
	next.Models = raw.Models
	next.Roles = raw.Roles
	next.Budgets = raw.Budgets
	next.Timeouts = raw.Timeouts
	next.Policy = raw.Policy
	next.ModelPolicy = raw.ModelPolicy
	next.Pipelines = raw.Pipelines

	// The shadow candidate is judged on its own below: a broken candidate
	// switches the shadow off but does not hold up the reload.
	check := next
	check.ToolGate.Shadow = nil
	verdict := validate.Summarize(validate.CheckAtStartup(&check, skillsDir))
	res.Warnings = verdict.Warnings
	if verdict.Errors > 0 {
		return nil, nil, res, fmt.Errorf("%d error(s); first: %s", verdict.Errors, verdict.FirstError)
	}
	// A reload applies tool_gate.shadow (with the rule definition files below):
	// the candidate is re-read from the path the file names now.
	probe := next
	probe.ToolGate.Shadow = raw.ToolGate.Shadow
	res.Shadow, res.ShadowErr = loadShadowCandidate(&probe)

	skills, err := skillsapi.LoadSkills(skillsDir)
	if err != nil {
		return nil, nil, res, fmt.Errorf("load skills from %s: %w", skillsDir, err)
	}

	// The running rules name the files; a rule changed in the config file is
	// a tool_gate change and waits for a restart like the rest of the section.
	if next.ToolGate.Enabled {
		defs, err := readToolDefinitions(next.ToolGate.Tools)
		if err != nil {
			return nil, nil, res, err
		}
		res.definitions = defs
	}

	if boot != nil {
		was, now := restartOnlySections(boot), restartOnlySections(&raw)
		for name := range now {
			if !reflect.DeepEqual(was[name], now[name]) {
				res.RestartNeeded = append(res.RestartNeeded, name)
			}
		}
		sort.Strings(res.RestartNeeded)
	}
	res.Pipelines = len(next.Pipelines)
	res.Skills = len(skills.List())
	return &next, skills, res, nil
}

// reloadEngine applies a reload and returns the operator-facing summary.
func reloadEngine(configPath, skillsDir string, boot, fallbackCfg *config.Config, sched *Scheduler, budget *BudgetTracker, notify func(string)) string {
	reloadMu.Lock()
	defer reloadMu.Unlock()
	next, skills, res, err := buildReload(configPath, skillsDir, currentConfig(fallbackCfg), boot)
	if err != nil {
		log.Printf("[config] reload refused: %v", err)
		return "[config] Reload refused, the running config is unchanged: " + err.Error()
	}
	changedDefs := applyToolDefinitions(next.ToolGate.Tools, res.definitions)
	liveConfig.Store(next)
	liveSkills.Store(skills)
	wasShadow := liveShadow.Swap(res.Shadow)
	sched.Sync(next.Pipelines)
	if budget != nil {
		budget.configureAlerts(next, notify)
	}
	msg := fmt.Sprintf("[config] Reloaded: %d pipeline(s), %d skill(s)", res.Pipelines, res.Skills)
	if res.Warnings > 0 {
		msg += fmt.Sprintf(", %d warning(s)", res.Warnings)
	}
	msg += ". Runs in progress keep the config they started with."
	if len(changedDefs) > 0 {
		msg += "\nTool definitions re-read and changed: " + strings.Join(changedDefs, ", ")
	}
	if len(res.RestartNeeded) > 0 {
		msg += "\nChanged but applied only at restart: " + strings.Join(res.RestartNeeded, ", ")
	}
	msg += shadowReloadLine(wasShadow, res.Shadow, res.ShadowErr)
	log.Printf("%s", msg)
	return msg
}

// reloadHook performs a reload from the running engine; nil outside it.
var reloadHook func() string
