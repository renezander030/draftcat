package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/renezander030/draftcat/internal/config"
)

const reloadBase = `telegram:
  token_env: RL_TG
  chat_id: 42
  security:
    allowed_users: [42]
    max_input_length: 500
    rate_limit: 10
provider:
  type: openrouter
  api_key_env: RL_KEY
models:
  m: {model: x/y, max_tokens: 64}
roles:
  drafter: m
budgets:
  per_day_tokens: 1000
pipelines:
  - name: a
    schedule: 30m
    steps:
      - {name: s, type: deterministic}
  - name: b
    schedule: manual
    steps:
      - {name: s, type: deterministic}
`

func writeReloadConfig(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func bootFrom(t *testing.T, body string) (*config.Config, *config.Config) {
	t.Helper()
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}
	boot := cfg
	cfg.Provider.SetAPIKey("sk-running")
	cfg.Telegram.SetToken("123:running")
	return &cfg, &boot
}

func resetLive(t *testing.T) {
	t.Helper()
	liveConfig.Store(nil)
	liveSkills.Store(nil)
	t.Cleanup(func() { liveConfig.Store(nil); liveSkills.Store(nil) })
}

func TestReloadAppliesRunSectionsAndKeepsRunningResources(t *testing.T) {
	resetLive(t)
	t.Setenv("RL_TG", "x")
	t.Setenv("RL_KEY", "y")
	dir := t.TempDir()
	running, boot := bootFrom(t, reloadBase)
	sched := newScheduler(running.Pipelines)
	sched.Pause("a")

	changed := strings.Replace(reloadBase, "per_day_tokens: 1000", "per_day_tokens: 5000", 1)
	changed = strings.Replace(changed, "  - name: b\n    schedule: manual", "  - name: c\n    schedule: \"0 8 * * 1-5\"\n    timezone: Europe/Berlin", 1)
	changed = strings.Replace(changed, "type: openrouter", "type: openai", 1)
	path := writeReloadConfig(t, dir, changed)

	msg := reloadEngine(path, filepath.Join(dir, "skills"), boot, running, sched, nil, nil)
	if !strings.Contains(msg, "Reloaded: 2 pipeline(s)") || !strings.Contains(msg, "applied only at restart: provider") {
		t.Fatalf("summary = %q", msg)
	}
	live := currentConfig(running)
	if live == running {
		t.Fatal("snapshot not swapped")
	}
	if live.Budgets.PerDayTokens != 5000 {
		t.Errorf("budgets not applied: %+v", live.Budgets)
	}
	if live.Provider.Type != "openrouter" || live.Provider.APIKey() != "sk-running" || live.Telegram.Token() != "123:running" {
		t.Errorf("running resources replaced: provider=%q key=%q", live.Provider.Type, live.Provider.APIKey())
	}
	if running.Budgets.PerDayTokens != 1000 {
		t.Error("the previous snapshot was mutated; runs in progress would see the change")
	}
	names := map[string]*PipelineState{}
	for _, ps := range sched.GetAll() {
		names[ps.Name] = ps
	}
	if _, ok := names["b"]; ok {
		t.Error("removed pipeline still scheduled")
	}
	if c, ok := names["c"]; !ok || c.NextRun.IsZero() {
		t.Errorf("new cron pipeline not scheduled: %+v", c)
	}
	if !names["a"].Paused {
		t.Error("operator pause lost across reload")
	}
}

func TestReloadRefusesInvalidConfig(t *testing.T) {
	resetLive(t)
	dir := t.TempDir()
	running, boot := bootFrom(t, reloadBase)
	sched := newScheduler(running.Pipelines)
	bad := strings.Replace(reloadBase, "schedule: 30m", "schedule: \"0 25 * * *\"", 1)
	path := writeReloadConfig(t, dir, bad)
	msg := reloadEngine(path, filepath.Join(dir, "skills"), boot, running, sched, nil, nil)
	if !strings.Contains(msg, "Reload refused") || !strings.Contains(msg, "schedule") {
		t.Fatalf("summary = %q", msg)
	}
	if currentConfig(running) != running {
		t.Fatal("invalid config was applied")
	}
	path = writeReloadConfig(t, dir, "pipelines: [")
	if msg := reloadEngine(path, filepath.Join(dir, "skills"), boot, running, sched, nil, nil); !strings.Contains(msg, "Reload refused") {
		t.Fatalf("unparsable config: %q", msg)
	}
}

func TestSyncKeepsRunningPipelineUntilItFinishes(t *testing.T) {
	s, _ := schedulerAt(time.Now(), config.PipelineConfig{Name: "a", Schedule: "5m"}, config.PipelineConfig{Name: "b", Schedule: "manual"})
	if ok, _ := s.TryStartManual("b"); !ok {
		t.Fatal("claim refused")
	}
	s.Sync([]config.PipelineConfig{{Name: "a", Schedule: "5m"}})
	if ok, reason := s.TryStartManual("b"); ok || reason != "unknown pipeline" {
		t.Fatalf("removed pipeline admitted: %v %q", ok, reason)
	}
	running, _ := s.Counts()
	if running != 1 {
		t.Fatalf("running = %d, want the in-flight run counted", running)
	}
	s.Finish("b", nil)
	if !s.Wait(time.Second) {
		t.Fatal("drain still waiting on a removed pipeline")
	}
	if _, ok := s.pipelines["b"]; ok {
		t.Fatal("removed pipeline kept after its run finished")
	}
}

func TestSyncReplansChangedScheduleFromLastRun(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	s, _ := schedulerAt(t0, config.PipelineConfig{Name: "a", Schedule: "1h"})
	s.pipelines["a"].LastRun = t0.Add(-30 * time.Minute)
	s.Sync([]config.PipelineConfig{{Name: "a", Schedule: "2h"}})
	if got, want := s.pipelines["a"].NextRun, t0.Add(90*time.Minute); !got.Equal(want) {
		t.Fatalf("NextRun = %v, want %v", got, want)
	}
	s.Sync([]config.PipelineConfig{{Name: "a", Schedule: "10m"}})
	if got := s.pipelines["a"].NextRun; !got.Equal(t0) {
		t.Fatalf("overdue interval NextRun = %v, want now", got)
	}
	before := s.pipelines["a"].NextRun
	s.Sync([]config.PipelineConfig{{Name: "a", Schedule: "10m", PauseAfterFailures: 3}})
	if !s.pipelines["a"].NextRun.Equal(before) || s.pipelines["a"].pauseAfter != 3 {
		t.Fatal("unchanged schedule was replanned or option not applied")
	}
}
