package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/renezander030/draftcat/internal/config"
	"github.com/renezander030/draftcat/internal/schedule"
	skillsapi "github.com/renezander030/draftcat/internal/skills"
	statestore "github.com/renezander030/draftcat/internal/state"
	"github.com/renezander030/draftcat/internal/validate"
)

// doctorCheck is one line of `draftcat doctor`. Status is "ok", "warn" or
// "fail"; Hint says what to do when it is not ok.
type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

type doctorReport struct {
	OK     bool          `json:"ok"`
	Checks []doctorCheck `json:"checks"`
}

func (r *doctorReport) add(name, status, detail, hint string) {
	r.Checks = append(r.Checks, doctorCheck{Name: name, Status: status, Detail: detail, Hint: hint})
}

// runDoctor implements `draftcat doctor`: a read-only preflight of everything
// the engine needs at start — config, credentials, operator channel, state
// store, listeners and schedules. It never writes, never creates the state
// store and never contacts a provider. Exit 1 when any check fails.
func runDoctor(args []string, stdout io.Writer) int {
	configPath := "config.yaml"
	skillsDir := "skills"
	jsonOut := false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--json", "-json":
			jsonOut = true
		case "--config", "-config":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "doctor: --config requires a path")
				return 2
			}
			configPath = args[i+1]
			i++
		case "--skills", "-skills":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "doctor: --skills requires a path")
				return 2
			}
			skillsDir = args[i+1]
			i++
		case "-h", "--help", "help":
			_, _ = io.WriteString(stdout, "Usage: draftcat doctor [--config path] [--skills dir] [--json]\n\n"+
				"Read-only preflight: config, credentials, operator channel, state store, listeners, schedules.\n"+
				"Exits 1 when a check fails.\n")
			return 0
		default:
			fmt.Fprintf(os.Stderr, "doctor: unknown option %q\n", a)
			return 2
		}
	}
	// The report is the output; loader chatter would interleave with it.
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	rep := doctor(configPath, skillsDir, time.Now())
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else {
		printDoctor(stdout, rep)
	}
	if !rep.OK {
		return 1
	}
	return 0
}

func doctor(configPath, skillsDir string, now time.Time) (rep doctorReport) {
	defer func() {
		rep.OK = true
		for _, c := range rep.Checks {
			if c.Status == "fail" {
				rep.OK = false
			}
		}
	}()

	// #nosec G304 -- the operator's own --config argument.
	data, err := os.ReadFile(configPath)
	if err != nil {
		rep.add("config", "fail", fmt.Sprintf("cannot read %s: %v", configPath, err), "pass --config <path> or run from the directory holding config.yaml")
		return rep
	}
	var cfg config.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		rep.add("config", "fail", fmt.Sprintf("cannot parse %s: %v", configPath, err), "fix the YAML syntax, then run draftcat validate")
		return rep
	}
	rep.add("config", "ok", fmt.Sprintf("%s: %d pipeline(s)", configPath, len(cfg.Pipelines)), "")

	errs, warns := 0, 0
	var firstErr string
	for _, f := range validate.CheckAtStartup(&cfg, skillsDir) {
		if f.Level == "error" {
			if errs == 0 {
				firstErr = f.Path + ": " + f.Message
			}
			errs++
		} else {
			warns++
		}
	}
	switch {
	case errs > 0:
		rep.add("validate", "fail", fmt.Sprintf("%d error(s), %d warning(s); first: %s", errs, warns, firstErr), "run draftcat validate for the full list")
	case warns > 0:
		rep.add("validate", "warn", fmt.Sprintf("%d warning(s)", warns), "run draftcat validate for details")
	default:
		rep.add("validate", "ok", "no findings", "")
	}

	doctorSkills(&rep, skillsDir)
	doctorChannel(&rep, &cfg)
	doctorProvider(&rep, &cfg)
	doctorSigning(&rep)
	doctorSecretsFile(&rep)
	doctorState(&rep, &cfg, now)
	doctorListeners(&rep, &cfg)
	doctorSchedules(&rep, &cfg, now)
	return rep
}

func envSet(name string) bool { return strings.TrimSpace(os.Getenv(name)) != "" }

func doctorSkills(rep *doctorReport, dir string) {
	reg, err := skillsapi.LoadSkills(dir)
	if err != nil {
		rep.add("skills", "warn", fmt.Sprintf("%s: %v", dir, err), "pass the skills directory as --skills <dir>")
		return
	}
	rep.add("skills", "ok", fmt.Sprintf("%s: %d skill(s)", dir, len(reg.List())), "")
}

func doctorChannel(rep *doctorReport, cfg *config.Config) {
	tokenEnv := firstNonEmpty(cfg.Telegram.TokenEnv, "DRAFTCAT_TG_TOKEN")
	if envSet(tokenEnv) || envSet("DRAFTCAT_TG_TOKEN") {
		rep.add("telegram token", "ok", tokenEnv+" is set", "")
	} else {
		rep.add("telegram token", "fail", tokenEnv+" is empty", "export "+tokenEnv+"=<bot token from @BotFather>; the engine refuses to start without it")
	}
	chat := cfg.Telegram.ChatID != 0 || envSet("DRAFTCAT_TG_CHAT_ID") || len(cfg.Telegram.Security.AllowedUsers) > 0 || envSet("DRAFTCAT_TG_ALLOWED_USERS")
	users := len(cfg.Telegram.Security.AllowedUsers) > 0 || envSet("DRAFTCAT_TG_ALLOWED_USERS")
	switch {
	case !users:
		rep.add("operators", "fail", "no telegram.security.allowed_users", "set allowed_users in config or DRAFTCAT_TG_ALLOWED_USERS=<id,id>; without an operator no approval can be decided")
	case !chat:
		rep.add("operators", "fail", "no chat_id", "set telegram.chat_id or DRAFTCAT_TG_CHAT_ID")
	default:
		rep.add("operators", "ok", "operator chat and allowed users configured", "")
	}
	if cfg.Relay.Enabled() {
		secretEnv := firstNonEmpty(cfg.Relay.SecretEnv, "DRAFTCAT_RELAY_SECRET")
		if envSet(secretEnv) || envSet("DRAFTCAT_RELAY_SECRET") {
			rep.add("relay", "ok", fmt.Sprintf("approvals go to %s; %s is set", cfg.Relay.URL, secretEnv), "")
		} else {
			rep.add("relay", "fail", secretEnv+" is empty", "export the shared HMAC secret the relay was configured with")
		}
	}
}

func doctorProvider(rep *doctorReport, cfg *config.Config) {
	keyEnv := firstNonEmpty(cfg.Provider.APIKeyEnv, "OPENROUTER_API_KEY")
	if envSet(keyEnv) || envSet("OPENROUTER_API_KEY") {
		rep.add("provider key", "ok", keyEnv+" is set", "")
	} else {
		rep.add("provider key", "fail", keyEnv+" is empty", "export "+keyEnv+"=<provider API key>")
	}
	if cfg.GHL.APIKeyEnv != "" && !envSet(cfg.GHL.APIKeyEnv) {
		rep.add("gohighlevel key", "warn", cfg.GHL.APIKeyEnv+" is empty", "GoHighLevel steps fail until it is set")
	}
	if cfg.Observ.OTLP.Enabled && cfg.Observ.OTLP.HeaderEnv != "" && !envSet(cfg.Observ.OTLP.HeaderEnv) {
		rep.add("otlp headers", "warn", cfg.Observ.OTLP.HeaderEnv+" is empty", "the collector may reject unauthenticated spans")
	}
}

func doctorSigning(rep *doctorReport) {
	if envSet("DRAFTCAT_APPROVAL_SECRET") {
		rep.add("receipt signing", "ok", "DRAFTCAT_APPROVAL_SECRET is set; approval receipts are signed", "")
		return
	}
	rep.add("receipt signing", "warn", "DRAFTCAT_APPROVAL_SECRET is empty; approval receipts are recorded unsigned", "export a long random secret so audit-verify and receipts verify can prove each decision")
}

func doctorSecretsFile(rep *doctorReport) {
	fi, err := os.Stat("secrets.yaml")
	if err != nil {
		return
	}
	if fi.Mode().Perm()&0o077 != 0 {
		rep.add("secrets.yaml", "warn", fmt.Sprintf("mode %04o: readable by other users", fi.Mode().Perm()), "chmod 600 secrets.yaml")
		return
	}
	rep.add("secrets.yaml", "ok", fmt.Sprintf("mode %04o", fi.Mode().Perm()), "")
}

// doctorState inspects the state store without creating or migrating it.
func doctorState(rep *doctorReport, cfg *config.Config, now time.Time) {
	path := strings.TrimSpace(os.Getenv("DRAFTCAT_STATE_PATH"))
	if path == "" {
		path = cfg.State.Path
	}
	if path == "" {
		path = "./state.db"
	}
	if _, err := os.Stat(path); err != nil {
		dir := filepath.Dir(path)
		if fi, derr := os.Stat(dir); derr != nil || !fi.IsDir() {
			rep.add("state store", "fail", fmt.Sprintf("%s does not exist and %s is not a directory", path, dir), "create the directory or set state.path / DRAFTCAT_STATE_PATH")
			return
		}
		probe, perr := os.CreateTemp(dir, ".draftcat-doctor-*")
		if perr != nil {
			rep.add("state store", "fail", fmt.Sprintf("%s will be created, but %s is not writable: %v", path, dir, perr), "fix the directory permissions or point state.path at a writable volume")
			return
		}
		name := probe.Name()
		_ = probe.Close()
		_ = os.Remove(name)
		rep.add("state store", "ok", fmt.Sprintf("%s will be created at first start", path), "")
		return
	}
	st, err := statestore.OpenStateStoreReadOnly(path)
	if err != nil {
		rep.add("state store", "fail", fmt.Sprintf("cannot open %s: %v", path, err), "check the file permissions; the engine needs read-write access")
		return
	}
	defer func() { _ = st.Close() }()
	day, err := st.BudgetDay(context.Background(), now)
	if err != nil {
		rep.add("state store", "warn", fmt.Sprintf("%s opened, but the usage ledger is unreadable: %v", path, err), "start the engine once to upgrade an older store")
		return
	}
	rep.add("state store", "ok", fmt.Sprintf("%s: today %d tokens, %.4f spent", path, day.Tokens, day.Cost), "")
	if day.Unsettled > 0 {
		rep.add("budget", "warn", fmt.Sprintf("%d provider call(s) with unsettled usage block new model calls", day.Unsettled), "run draftcat budget status, then draftcat budget reconcile")
	}
	if open, err := st.OpenApprovals(); err == nil && len(open) > 0 {
		rep.add("approvals", "warn", fmt.Sprintf("%d approval gate(s) still open", len(open)), "they are closed out and reported at the next engine start; see draftcat pending")
	}
}

func doctorListeners(rep *doctorReport, cfg *config.Config) {
	if cfg.Webhook.Enabled {
		addr := firstNonEmpty(cfg.Webhook.Addr, "127.0.0.1:8088")
		if cfg.Webhook.SecretEnv == "" || !envSet(cfg.Webhook.SecretEnv) {
			rep.add("webhook", "fail", "webhook.enabled but its secret env is empty", "set webhook.secret_env and export that variable; the engine refuses an unauthenticated trigger")
		} else {
			checkListen(rep, "webhook", addr)
		}
	}
	if cfg.Observ.Prometheus.Enabled {
		checkListen(rep, "metrics", firstNonEmpty(cfg.Observ.Prometheus.Addr, "127.0.0.1:9090"))
	}
}

func checkListen(rep *doctorReport, name, addr string) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		rep.add(name, "warn", fmt.Sprintf("%s is not free: %v", addr, err), "another process (or a running engine) holds the port")
		return
	}
	_ = ln.Close()
	rep.add(name, "ok", addr+" is free", "")
}

func doctorSchedules(rep *doctorReport, cfg *config.Config, now time.Time) {
	for _, p := range cfg.Pipelines {
		if !schedule.IsTimer(p.Schedule) {
			continue
		}
		sc, err := schedule.Parse(p.Schedule, p.Timezone)
		if err != nil {
			rep.add("schedule "+p.Name, "fail", err.Error(), "fix pipelines[].schedule")
			continue
		}
		next := sc.Next(now)
		if next.IsZero() {
			rep.add("schedule "+p.Name, "warn", p.Schedule+" never fires", "check the day and month fields")
			continue
		}
		detail := fmt.Sprintf("%s: next run %s", p.Schedule, next.In(sc.Location()).Format("Mon 02 Jan 2006 15:04 MST"))
		if sc.IsInterval() {
			detail = fmt.Sprintf("every %s (continues from the last recorded run)", sc.Interval())
		}
		rep.add("schedule "+p.Name, "ok", detail, "")
	}
}

func printDoctor(w io.Writer, rep doctorReport) {
	width := 0
	for _, c := range rep.Checks {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	var sb strings.Builder
	fails, warns := 0, 0
	for _, c := range rep.Checks {
		sb.WriteString(fmt.Sprintf("%-4s  %-*s  %s\n", c.Status, width, c.Name, c.Detail))
		if c.Hint != "" && c.Status != "ok" {
			sb.WriteString(fmt.Sprintf("      %-*s  → %s\n", width, "", c.Hint))
		}
		switch c.Status {
		case "fail":
			fails++
		case "warn":
			warns++
		}
	}
	switch {
	case fails > 0:
		sb.WriteString(fmt.Sprintf("\n%d check(s) failed, %d warning(s). The engine will not start cleanly until the failures are fixed.\n", fails, warns))
	case warns > 0:
		sb.WriteString(fmt.Sprintf("\nReady to start, with %d warning(s).\n", warns))
	default:
		sb.WriteString("\nReady to start.\n")
	}
	_, _ = io.WriteString(w, sb.String())
}
