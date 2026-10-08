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
// observability, voice) as they are, naming any that changed so the operator
// knows a restart is needed for them.

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
		"tool_gate":     c.ToolGate,
		"observability": c.Observ,
		"voice":         c.Voice,
	}
}

type reloadResult struct {
	Pipelines     int
	Skills        int
	RestartNeeded []string
	Warnings      int
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

	errs := 0
	var first string
	for _, f := range validate.CheckAtStartup(&next, skillsDir) {
		if f.Level == "error" {
			if errs == 0 {
				first = f.Path + ": " + f.Message
			}
			errs++
		} else {
			res.Warnings++
		}
	}
	if errs > 0 {
		return nil, nil, res, fmt.Errorf("%d error(s); first: %s", errs, first)
	}

	skills, err := skillsapi.LoadSkills(skillsDir)
	if err != nil {
		return nil, nil, res, fmt.Errorf("load skills from %s: %w", skillsDir, err)
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
	liveConfig.Store(next)
	liveSkills.Store(skills)
	sched.Sync(next.Pipelines)
	if budget != nil {
		budget.configureAlerts(next, notify)
	}
	msg := fmt.Sprintf("[config] Reloaded: %d pipeline(s), %d skill(s)", res.Pipelines, res.Skills)
	if res.Warnings > 0 {
		msg += fmt.Sprintf(", %d warning(s)", res.Warnings)
	}
	msg += ". Runs in progress keep the config they started with."
	if len(res.RestartNeeded) > 0 {
		msg += "\nChanged but applied only at restart: " + strings.Join(res.RestartNeeded, ", ")
	}
	log.Printf("%s", msg)
	return msg
}

// reloadHook performs a reload from the running engine; nil outside it.
var reloadHook func() string
