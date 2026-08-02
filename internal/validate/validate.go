package validate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/renezander030/draftcat/internal/channels"
	"github.com/renezander030/draftcat/internal/config"
	skillsapi "github.com/renezander030/draftcat/internal/skills"
)

// validKnownActions mirrors the deterministic action switch in runPipeline.
// Keep in sync when new actions are added.
var validKnownActions = map[string]string{
	"":                         "pass-through",
	"whatsapp_intake":          "normalize a WhatsApp inbound payload into input/whatsapp_* fields",
	"gmail_unread":             "fetch unread Gmail messages",
	"notify":                   "send last ai_output to operator channel",
	"ghl_new_contacts":         "fetch new GoHighLevel contacts",
	"ghl_stale_opportunities":  "fetch stale GHL opportunities (requires vars.pipeline_id)",
	"ghl_unread_conversations": "fetch unread GHL conversations",
	"pdf_extract":              "parse a PDF into text + per-fragment bounding boxes (requires vars.path or data.pdf_path)",
	"pdf_verify_cite":          "resolve <cite> tags in ai_raw against the parsed PDF (optional vars.fail_on_unresolved)",
}

var validStepTypes = map[string]bool{
	"deterministic": true,
	"ai":            true,
	"approval":      true,
}

// Implemented operator channels come from internal/channels, which the engine
// reads too. Keeping a second list here is what let `channel: slack` pass
// validation for a channel that was never built.

type validateFinding struct {
	Level   string `json:"level"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

type validateReport struct {
	Findings []validateFinding `json:"findings"`
}

func (r *validateReport) errf(path, format string, args ...interface{}) {
	r.Findings = append(r.Findings, validateFinding{Level: "error", Path: path, Message: fmt.Sprintf(format, args...)})
}

func (r *validateReport) warnf(path, format string, args ...interface{}) {
	r.Findings = append(r.Findings, validateFinding{Level: "warn", Path: path, Message: fmt.Sprintf(format, args...)})
}

func (r *validateReport) errors() int {
	n := 0
	for _, f := range r.Findings {
		if f.Level == "error" {
			n++
		}
	}
	return n
}

// Run is the entry point for `draftcat validate`.
func Run(args []string) int {
	configPath := "config.yaml"
	skillsDir := "skills"
	strict := false
	jsonOut := false

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-strict", "--strict":
			strict = true
		case "-json", "--json":
			jsonOut = true
		case "-config", "--config":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "validate: --config requires a path")
				return 2
			}
			configPath = args[i+1]
			i++
		case "-skills", "--skills":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "validate: --skills requires a path")
				return 2
			}
			skillsDir = args[i+1]
			i++
		case "-h", "--help", "help":
			fmt.Println("Usage: draftcat validate [--config path] [--skills dir] [--strict] [--json]")
			fmt.Println("\nLints config.yaml + skills/*.yaml. Exits 1 on errors, or on any finding under --strict.")
			return 0
		default:
			fmt.Fprintf(os.Stderr, "validate: unknown option %q\n", a)
			return 2
		}
	}

	rep := &validateReport{}

	cfgData, err := os.ReadFile(configPath)
	if err != nil {
		rep.errf("config", "failed to read %s: %v", configPath, err)
		return printValidateReport(rep, jsonOut, strict)
	}
	var cfg config.Config
	if err := yaml.Unmarshal(cfgData, &cfg); err != nil {
		rep.errf("config", "failed to parse %s: %v", configPath, err)
		return printValidateReport(rep, jsonOut, strict)
	}

	runChecks(&cfg, skillsDir, rep)

	return printValidateReport(rep, jsonOut, strict)
}

// runChecks is the shared check body behind both `draftcat validate` and the
// engine's startup gate, so the two can never drift into disagreeing about
// what a valid config is.
func runChecks(cfg *config.Config, skillsDir string, rep *validateReport) {
	skills := loadSkillsForValidate(skillsDir, rep)
	checkConfigSecurity(cfg, rep)
	checkTimeouts(cfg, rep)
	checkRolesToModels(cfg, rep)
	checkEnvVars(cfg, rep)
	checkPipelines(cfg, skills, skillsDir, rep)
	checkOrphanedSkills(cfg, skills, rep)
	checkSecretsHygiene(rep)
}

// Finding is one validation result surfaced to the engine at startup.
type Finding struct {
	Level   string // "error" | "warn"
	Path    string
	Message string
}

// CheckAtStartup runs the same checks as `draftcat validate` against an
// already-parsed config and returns the findings.
//
// Validation used to be opt-in: thorough, but only if someone remembered to run
// it. A config with a typo'd role or an unimplemented channel booted happily and
// failed hours later at step-run time, which is the worst moment to discover it
// — mid-pipeline, with a half-finished action and an operator who did not cause
// the problem. Running it on the boot path turns a 3am runtime failure into a
// startup refusal.
//
// The engine treats errors as fatal and warnings as logged. Returned rather than
// printed so the caller controls presentation.
func CheckAtStartup(cfg *config.Config, skillsDir string) []Finding {
	rep := &validateReport{}
	runChecks(cfg, skillsDir, rep)
	out := make([]Finding, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		out = append(out, Finding(f))
	}
	return out
}

func loadSkillsForValidate(skillsDir string, rep *validateReport) map[string]*skillsapi.SkillDef {
	skills := map[string]*skillsapi.SkillDef{}
	skillFiles, _ := filepath.Glob(filepath.Join(skillsDir, "*.yaml"))
	if len(skillFiles) == 0 {
		rep.warnf("skills/", "no *.yaml files found in %s", skillsDir)
		return skills
	}
	for _, f := range skillFiles {
		base := filepath.Base(f)
		data, err := os.ReadFile(f)
		if err != nil {
			rep.errf("skills/"+base, "read failed: %v", err)
			continue
		}
		var s skillsapi.SkillDef
		if err := yaml.Unmarshal(data, &s); err != nil {
			rep.errf("skills/"+base, "parse failed: %v", err)
			continue
		}
		if s.Name == "" {
			rep.errf("skills/"+base, "missing required 'name'")
			continue
		}
		if s.Prompt == "" {
			rep.errf("skills/"+s.Name, "missing 'prompt'")
		}
		for field, def := range s.OutputSchema {
			dm, ok := def.(map[string]interface{})
			if !ok {
				rep.warnf("skills/"+s.Name, "output_schema.%s: definition is not a map", field)
				continue
			}
			_, hasEnum := dm["enum"]
			t, _ := dm["type"].(string)
			switch t {
			case "int", "number", "bool", "string":
			case "":
				if !hasEnum {
					rep.warnf("skills/"+s.Name, "output_schema.%s: missing 'type'", field)
				}
			default:
				rep.warnf("skills/"+s.Name, "output_schema.%s: unsupported type %q (validator handles int|number|bool|string)", field, t)
			}
			if hasEnum {
				if _, ok := dm["enum"].([]interface{}); !ok {
					rep.warnf("skills/"+s.Name, "output_schema.%s: 'enum' must be a list", field)
				}
			}
		}
		if _, dup := skills[s.Name]; dup {
			rep.errf("skills/"+s.Name, "duplicate skill name (also defined in another file)")
		}
		skills[s.Name] = &s
	}
	return skills
}

func checkConfigSecurity(cfg *config.Config, rep *validateReport) {
	if cfg.Telegram.Security.MaxInputLength <= 0 {
		rep.errf("telegram.security.max_input_length", "must be set and > 0 (engine refuses to start without it)")
	}
	if cfg.Telegram.Security.RateLimit <= 0 {
		rep.errf("telegram.security.rate_limit", "must be set and > 0 (engine refuses to start without it)")
	}
	checkRelay(cfg, rep)
	checkApprovalPolicy(cfg, rep)
	checkToolGate(cfg, rep)
	if len(cfg.Telegram.Security.AllowedUsers) == 0 {
		rep.warnf("telegram.security.allowed_users", "empty — channel will accept no operator")
	}
	if cfg.Webhook.Enabled {
		if cfg.Webhook.SecretEnv == "" {
			rep.errf("webhook.secret_env", "must be set when webhook.enabled (engine refuses to start an unauthenticated trigger)")
		} else if os.Getenv(cfg.Webhook.SecretEnv) == "" {
			rep.warnf("webhook.secret_env", "env var %s is empty (engine will refuse to start at runtime)", cfg.Webhook.SecretEnv)
		}
	}
	if cfg.Observ.OTLP.Enabled && cfg.Observ.OTLP.Endpoint == "" {
		rep.errf("observability.otlp.endpoint", "must be set when observability.otlp.enabled (nothing to export to)")
	}
}

func checkTimeouts(cfg *config.Config, rep *validateReport) {
	for label, val := range map[string]string{
		"timeouts.ai_call":           cfg.Timeouts.AICall,
		"timeouts.operator_approval": cfg.Timeouts.OperatorApproval,
		"timeouts.pipeline_total":    cfg.Timeouts.PipelineTotal,
	} {
		if val == "" {
			continue
		}
		if _, err := time.ParseDuration(val); err != nil {
			rep.errf(label, "invalid duration %q: %v", val, err)
		}
	}
}

func checkRolesToModels(cfg *config.Config, rep *validateReport) {
	for role, model := range cfg.Roles {
		if _, ok := cfg.Models[model]; !ok {
			rep.errf("roles."+role, "model %q is not declared in models:", model)
		}
	}
}

func checkEnvVars(cfg *config.Config, rep *validateReport) {
	if cfg.Provider.APIKeyEnv != "" && os.Getenv(cfg.Provider.APIKeyEnv) == "" {
		rep.warnf("provider.api_key_env", "env var %s is empty (engine will refuse to start at runtime)", cfg.Provider.APIKeyEnv)
	}
	if cfg.Telegram.TokenEnv != "" && os.Getenv(cfg.Telegram.TokenEnv) == "" {
		rep.warnf("telegram.token_env", "env var %s is empty", cfg.Telegram.TokenEnv)
	}
	if cfg.GHL.APIKeyEnv != "" && os.Getenv(cfg.GHL.APIKeyEnv) == "" {
		rep.warnf("gohighlevel.api_key_env", "env var %s is empty", cfg.GHL.APIKeyEnv)
	}
}

func checkPipelines(cfg *config.Config, skills map[string]*skillsapi.SkillDef, skillsDir string, rep *validateReport) {
	seen := map[string]bool{}
	for pi, p := range cfg.Pipelines {
		path := fmt.Sprintf("pipelines[%d:%s]", pi, p.Name)
		if p.Name == "" {
			rep.errf(path, "missing 'name'")
		}
		if seen[p.Name] {
			rep.errf(path, "duplicate pipeline name %q", p.Name)
		}
		seen[p.Name] = true

		// A pipeline with no steps is almost always a half-finished edit or a
		// YAML indentation slip. It schedules, runs, and does nothing, which
		// looks like success — so it is an error, not a warning.
		if len(p.Steps) == 0 {
			rep.errf(path+".steps", "pipeline has no steps — it would run and do nothing")
		}

		switch p.Schedule {
		case "", "manual":
			// operator /run only
		case "webhook":
			if !cfg.Webhook.Enabled {
				rep.warnf(path+".schedule", "schedule 'webhook' but webhook.enabled is false — this pipeline can never be triggered")
			}
		default:
			if _, err := time.ParseDuration(p.Schedule); err != nil {
				rep.errf(path+".schedule", "invalid duration %q (use e.g. '30m', '1h', 'manual', or 'webhook')", p.Schedule)
			}
		}

		aiSteps := 0
		for si, st := range p.Steps {
			spath := fmt.Sprintf("%s.steps[%d:%s]", path, si, st.Name)
			if st.Name == "" {
				rep.errf(spath, "missing 'name'")
			}
			if !validStepTypes[st.Type] {
				rep.errf(spath+".type", "invalid type %q%s (must be one of: deterministic, ai, approval)",
					st.Type, didYouMean(st.Type, stepTypeNames()))
				continue
			}
			switch st.Type {
			case "deterministic":
				if _, ok := validKnownActions[st.Action]; !ok {
					rep.errf(spath+".action", "unknown action %q%s (known: %s)",
						st.Action, didYouMean(st.Action, knownActionNames()), strings.Join(knownActionNames(), ", "))
				}
				if st.Action == "ghl_stale_opportunities" && st.Vars["pipeline_id"] == "" {
					rep.errf(spath+".vars.pipeline_id", "ghl_stale_opportunities requires vars.pipeline_id")
				}
				if strings.HasPrefix(st.Action, "gmail_") && cfg.Gmail.TokenPath == "" {
					rep.warnf(spath, "uses %s but gmail.token_path is empty", st.Action)
				}
				if strings.HasPrefix(st.Action, "ghl_") && cfg.GHL.APIKeyEnv == "" && cfg.GHL.TokenPath == "" {
					rep.warnf(spath, "uses %s but neither gohighlevel.api_key_env nor token_path is set", st.Action)
				}
			case "ai":
				aiSteps++
				if st.Skill == "" && st.Prompt == "" {
					rep.errf(spath, "ai step needs either 'skill' or inline 'prompt'")
				}
				if st.Skill != "" {
					sk, ok := skills[st.Skill]
					if !ok {
						rep.errf(spath+".skill", "skill %q not found in %s/", st.Skill, skillsDir)
					} else {
						for _, v := range extractTemplateVars(sk.Prompt) {
							if _, supplied := st.Vars[v]; supplied {
								continue
							}
							if isCommonDataKey(v) {
								continue
							}
							rep.warnf(spath+".vars", "skill %q references {{%s}}, not in vars and not a known upstream data key", st.Skill, v)
						}
					}
				}
				role := st.Role
				if role == "" && st.Skill != "" {
					if sk, ok := skills[st.Skill]; ok {
						role = sk.Role
					}
				}
				if role != "" {
					if _, ok := cfg.Roles[role]; !ok {
						rep.errf(spath+".role", "role %q is not declared in roles:", role)
					}
				}
			case "approval":
				if st.Mode != "" && st.Mode != "hitl" {
					rep.warnf(spath+".mode", "only 'hitl' is supported (got %q)", st.Mode)
				}
				// An unimplemented channel is an ERROR, not a warning. The
				// engine routes approvals to the one channel it is running;
				// naming any other means the operator would be watching a
				// channel the draft never reaches, while validate said OK.
				if st.Channel == "" {
					rep.errf(spath+".channel", "approval step requires 'channel'")
				} else if !channels.IsImplemented(st.Channel) {
					rep.errf(spath+".channel", "channel %q is not implemented — approvals cannot be routed there (implemented: %s)",
						st.Channel, strings.Join(knownApprovalChannels(), ", "))
				}
				// Quorum (N-of-M) validation, counted against the operator pool
				// of the channel the step actually names — the relay keeps its
				// own operator list, so validating a relay step against the
				// Telegram allow-list would reject satisfiable gates and accept
				// unsatisfiable ones.
				if st.Quorum < 0 {
					rep.errf(spath+".quorum", "quorum must be >= 0 (got %d)", st.Quorum)
				}
				pool := len(cfg.Telegram.Security.AllowedUsers)
				if st.Channel == channels.Relay {
					pool = len(cfg.Relay.Security.AllowedUsers)
				}
				if st.Quorum > pool {
					rep.errf(spath+".quorum", "quorum %d exceeds the %d allowed operator(s) on channel %q — unsatisfiable",
						st.Quorum, pool, st.Channel)
				}
				if st.Quorum >= 2 && st.Channel != channels.Telegram && st.Channel != channels.Relay {
					rep.errf(spath+".quorum", "quorum >= 2 requires channel 'telegram' or 'relay' (only these implement multi-operator approval), got %q", st.Channel)
				}
				if st.Channel == channels.Relay && !cfg.Relay.Enabled() {
					rep.errf(spath+".channel", "step routes to channel 'relay' but no relay is configured (set relay.url)")
				}
				// Approver scoping: a step can only narrow the channel's allowed
				// users, never widen them, and must leave enough approvers to
				// satisfy its own quorum — otherwise the gate hangs until the
				// approval timeout instead of failing at startup.
				checkApprovers(cfg, st, spath, rep)
			}
		}

		if cfg.Budgets.PerStepTokens > 0 && cfg.Budgets.PerPipelineTokens > 0 && aiSteps > 0 {
			if cfg.Budgets.PerStepTokens*aiSteps > cfg.Budgets.PerPipelineTokens {
				rep.warnf(path, "per_step_tokens × %d ai step(s) = %d exceeds per_pipeline_tokens = %d (pipeline may halt mid-run)",
					aiSteps, cfg.Budgets.PerStepTokens*aiSteps, cfg.Budgets.PerPipelineTokens)
			}
		}
	}
}

// checkRelay validates the hitl/v0 relay channel. Every finding here is an
// error rather than a warning for the same reason the channel allow-list is an
// error: a misconfigured relay does not fail loudly at the gate, it fails as an
// approval request nobody receives, which looks identical to an operator who is
// simply slow to answer. See docs/hitl-protocol.md.
func checkRelay(cfg *config.Config, rep *validateReport) {
	if !cfg.Relay.Enabled() {
		// Operators listed with no relay URL is a config half-written; the
		// channel is off and those operators can approve nothing.
		if len(cfg.Relay.Operators) > 0 {
			rep.warnf("relay.url", "relay operators are configured but relay.url is empty — the relay channel is off")
		}
		return
	}
	if strings.TrimSpace(cfg.Relay.SecretEnv) == "" {
		rep.errf("relay.secret_env", "required when relay.url is set — dispatches and decisions are both HMAC-signed")
	} else if os.Getenv(cfg.Relay.SecretEnv) == "" {
		rep.errf("relay.secret_env", "env var %q is empty — the relay secret must be set for the channel to start", cfg.Relay.SecretEnv)
	}
	if strings.TrimSpace(cfg.Relay.PublicURL) == "" {
		rep.errf("relay.public_url", "required when relay.url is set — the relay cannot post a decision back to a loopback address")
	} else if !strings.HasPrefix(cfg.Relay.PublicURL, "http://") && !strings.HasPrefix(cfg.Relay.PublicURL, "https://") {
		rep.errf("relay.public_url", "must be an absolute http(s) URL (got %q)", cfg.Relay.PublicURL)
	}
	if !strings.HasPrefix(cfg.Relay.URL, "http://") && !strings.HasPrefix(cfg.Relay.URL, "https://") {
		rep.errf("relay.url", "must be an absolute http(s) URL (got %q)", cfg.Relay.URL)
	}
	if len(cfg.Relay.Operators) == 0 {
		rep.errf("relay.operators", "at least one operator (id + identity) is required — the gate would admit no approver")
	}
	seenID := map[int64]bool{}
	seenIdent := map[string]bool{}
	for i, op := range cfg.Relay.Operators {
		p := fmt.Sprintf("relay.operators[%d]", i)
		if op.ID == 0 {
			rep.errf(p+".id", "operator id must be non-zero (0 is reserved for system/timeout rows in the audit trail)")
		}
		if strings.TrimSpace(op.Identity) == "" {
			rep.errf(p+".identity", "operator identity is required — it is what the relay reports back and what the gate checks")
		}
		// Duplicates make membership ambiguous: two ids for one identity means
		// the audit trail cannot say which operator decided.
		if op.ID != 0 && seenID[op.ID] {
			rep.errf(p+".id", "duplicate operator id %d", op.ID)
		}
		if op.Identity != "" && seenIdent[op.Identity] {
			rep.errf(p+".identity", "duplicate operator identity %q", op.Identity)
		}
		seenID[op.ID] = true
		seenIdent[op.Identity] = true
	}
	// Every allowed user must map to a wire identity, or the gate silently
	// shrinks: the engine would present to fewer people than configured and a
	// quorum that looks satisfiable would hang until timeout.
	for _, id := range cfg.Relay.Security.AllowedUsers {
		if cfg.Relay.IdentityFor(id) == "" {
			rep.errf("relay.security.allowed_users",
				"operator %d has no matching relay.operators entry — it can never be presented to or counted", id)
		}
	}
	if len(cfg.Relay.Security.AllowedUsers) == 0 {
		rep.errf("relay.security.allowed_users", "empty — the relay channel would accept no operator")
	}
}

// checkApprovalPolicy validates auto-approve rules.
//
// A policy tier is a narrow, declared exemption. The failure mode worth
// guarding is the one where it quietly stops being narrow: a rule with no risk
// scope matches every non-high step in every pipeline, which is "no gate at
// all" written in a way that still looks like governance. That is an error, not
// a warning.
func checkApprovalPolicy(cfg *config.Config, rep *validateReport) {
	for i, rule := range cfg.Policy.AutoApprove {
		p := fmt.Sprintf("approval_policy.auto_approve[%d]", i)
		switch strings.ToLower(strings.TrimSpace(rule.Risk)) {
		case config.RiskLow, config.RiskNormal:
		case "":
			rep.errf(p+".risk", "required — an unscoped auto-approve rule matches every step and removes the gate entirely")
		case config.RiskHigh:
			rep.errf(p+".risk", "high-risk steps can never be auto-approved; remove the rule or lower the step's declared risk deliberately")
		default:
			rep.errf(p+".risk", "must be one of low, normal (got %q)", rule.Risk)
		}
		if rule.MaxCost < 0 {
			rep.errf(p+".max_cost", "must be >= 0 (got %.4f)", rule.MaxCost)
		}
		if rule.Pipeline != "" {
			known := false
			for _, pl := range cfg.Pipelines {
				if pl.Name == rule.Pipeline {
					known = true
					break
				}
			}
			if !known {
				rep.errf(p+".pipeline", "no pipeline named %q — the rule can never match", rule.Pipeline)
			}
		}
		// A rule scoped to normal risk with no pipeline or step narrowing is
		// broad enough to deserve saying out loud, even though it is legal.
		if strings.EqualFold(rule.Risk, config.RiskNormal) && rule.Pipeline == "" && rule.Step == "" && rule.MaxCost == 0 {
			rep.warnf(p, "matches every normal-risk approval step in every pipeline — consider narrowing by pipeline, step or max_cost")
		}
	}
	// Steps must declare a risk value the policy can actually match on.
	for _, pl := range cfg.Pipelines {
		for _, st := range pl.Steps {
			if st.Risk == "" {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(st.Risk)) {
			case config.RiskLow, config.RiskNormal, config.RiskHigh:
			default:
				rep.errf(fmt.Sprintf("pipelines.%s.steps.%s.risk", pl.Name, st.Name),
					"must be one of low, normal, high (got %q)", st.Risk)
			}
		}
	}
}

// checkToolGate validates the tool-call gate allowlist.
func checkToolGate(cfg *config.Config, rep *validateReport) {
	if !cfg.ToolGate.Enabled {
		if len(cfg.ToolGate.Tools) > 0 {
			rep.warnf("tool_gate.enabled", "tools are listed but the tool gate is disabled — the endpoint is not served")
		}
		return
	}
	if !cfg.Webhook.Enabled {
		rep.errf("tool_gate.enabled", "requires webhook.enabled — the gate endpoint is served on the webhook listener")
	}
	if len(cfg.ToolGate.Tools) == 0 {
		rep.warnf("tool_gate.tools", "empty allowlist — every tool call will be denied")
	}
	seen := map[string]bool{}
	for i, t := range cfg.ToolGate.Tools {
		p := fmt.Sprintf("tool_gate.tools[%d]", i)
		if strings.TrimSpace(t.Name) == "" {
			rep.errf(p+".name", "tool name is required")
		}
		if seen[t.Name] {
			rep.errf(p+".name", "duplicate tool %q — the first entry would always win", t.Name)
		}
		seen[t.Name] = true
		switch strings.ToLower(strings.TrimSpace(t.Risk)) {
		case "", config.RiskLow, config.RiskNormal, config.RiskHigh:
		default:
			rep.errf(p+".risk", "must be one of low, normal, high (got %q)", t.Risk)
		}
		// A high-risk tool that anyone can call without a human is the exact
		// shape of the gap this gate exists to close.
		if strings.EqualFold(t.Risk, config.RiskHigh) && !t.RequireApproval {
			rep.errf(p+".require_approval", "tool %q is declared high risk but is allowed without approval", t.Name)
		}
	}
}

// checkApprovers validates a step's approver narrowing. The engine intersects
// step.approvers with the channel's allowed_users and never widens, so an id
// that is not an allowed user is dead weight that silently shrinks the real
// approver pool — worth an error while the operator is still looking at it,
// not a surprise at 4am when the gate cannot be satisfied.
func checkApprovers(cfg *config.Config, st config.StepConfig, spath string, rep *validateReport) {
	if len(st.Approvers) == 0 {
		return
	}
	allowed := map[int64]bool{}
	for _, id := range cfg.Telegram.Security.AllowedUsers {
		allowed[id] = true
	}
	seen := map[int64]bool{}
	valid := 0
	for _, id := range st.Approvers {
		if seen[id] {
			rep.warnf(spath+".approvers", "operator %d listed more than once (counted once)", id)
			continue
		}
		seen[id] = true
		if !allowed[id] {
			rep.errf(spath+".approvers", "operator %d is not in telegram.security.allowed_users — cannot approve anything", id)
			continue
		}
		valid++
	}
	need := st.Quorum
	if need < 1 {
		need = 1
	}
	if valid < need {
		rep.errf(spath+".approvers", "%d valid approver(s) cannot satisfy quorum %d — gate would hang until timeout", valid, need)
	}
}

func checkOrphanedSkills(cfg *config.Config, skills map[string]*skillsapi.SkillDef, rep *validateReport) {
	referenced := map[string]bool{}
	for _, p := range cfg.Pipelines {
		for _, st := range p.Steps {
			if st.Skill != "" {
				referenced[st.Skill] = true
			}
		}
	}
	for name := range skills {
		if !referenced[name] {
			rep.warnf("skills/"+name, "loaded but not referenced by any pipeline")
		}
	}
}

func checkSecretsHygiene(rep *validateReport) {
	if _, err := os.Stat("secrets.yaml"); err != nil {
		return
	}
	gi, err := os.ReadFile(".gitignore")
	if err != nil {
		rep.errf("secrets.yaml", "exists but no .gitignore found — risk of committing credentials")
		return
	}
	if !strings.Contains(string(gi), "secrets.yaml") {
		rep.errf("secrets.yaml", "exists but is not listed in .gitignore — risk of committing credentials")
	}
}

func printValidateReport(rep *validateReport, jsonOut, strict bool) int {
	errs := rep.errors()
	warns := len(rep.Findings) - errs

	if jsonOut {
		out := map[string]interface{}{
			"errors":   errs,
			"warnings": warns,
			"findings": rep.Findings,
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
	} else {
		for _, f := range rep.Findings {
			tag := "ERROR"
			if f.Level == "warn" {
				tag = "WARN "
			}
			fmt.Printf("%s %s: %s\n", tag, f.Path, f.Message)
		}
		if errs == 0 && warns == 0 {
			fmt.Println("OK")
		} else {
			fmt.Printf("\n%d error(s), %d warning(s)\n", errs, warns)
		}
	}

	if errs > 0 {
		return 1
	}
	if strict && warns > 0 {
		return 1
	}
	return 0
}

var templateVarPattern = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)

func extractTemplateVars(s string) []string {
	matches := templateVarPattern.FindAllStringSubmatch(s, -1)
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// isCommonDataKey marks data-map keys produced by deterministic actions in runPipeline.
// Used to suppress false-positive "missing template var" warnings.
func isCommonDataKey(k string) bool {
	switch k {
	case "input", "emails", "email_count", "contacts", "contact_count",
		"opportunities", "opportunity_count", "conversations", "conversation_count",
		"whatsapp_message", "whatsapp_from", "whatsapp_text",
		"voice_calls", "voice_call_count",
		"voice_handoffs", "voice_handoff_count",
		"voice_handoffs_resolved_count",
		"voice_learnings", "voice_learning_count",
		"voice_admin_commit_sha", "voice_admin_smoke_run_id", "voice_admin_publish_status",
		"workflow_id", "workflow_uuid",
		"ai_output", "ai_raw", "approved":
		return true
	}
	return false
}

func knownActionNames() []string {
	var out []string
	for k := range validKnownActions {
		if k != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func knownApprovalChannels() []string { return channels.Names() }

func stepTypeNames() []string {
	out := make([]string, 0, len(validStepTypes))
	for k := range validStepTypes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// didYouMean returns ` — did you mean "ai"?` when got is a near miss for one of
// the candidates, or "" when nothing is close enough. A typo'd step type or
// action is the single most common config mistake, and "invalid type" plus a
// list of twelve valid names makes the reader do the diffing. Naming the likely
// intent turns that into a one-second fix.
//
// The cutoff is deliberately tight (edit distance <= 2, and never more than a
// third of the word): a wrong-but-not-similar value is a different mistake, and
// a confidently wrong suggestion is worse than none.
func didYouMean(got string, candidates []string) string {
	if got == "" {
		return ""
	}
	best, bestDist := "", 1<<30
	for _, c := range candidates {
		if c == "" {
			continue
		}
		d := editDistance(strings.ToLower(got), strings.ToLower(c))
		if d < bestDist {
			best, bestDist = c, d
		}
	}
	limit := len(got) / 3
	if limit > 2 {
		limit = 2
	}
	if limit < 1 {
		limit = 1
	}
	if best == "" || bestDist > limit {
		return ""
	}
	return fmt.Sprintf(" — did you mean %q?", best)
}

// editDistance is Damerau-Levenshtein (optimal string alignment), i.e.
// Levenshtein plus adjacent transposition as a single edit.
//
// Transposition has to count as one edit or the most common typo of all is
// missed: "ia" -> "ai" is two substitutions under plain Levenshtein, which
// pushes it past the cutoff on a short word and loses exactly the suggestion
// the config linter most wants to make.
func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	// Full matrix: the transposition rule needs row i-2.
	d := make([][]int, len(ar)+1)
	for i := range d {
		d[i] = make([]int, len(br)+1)
		d[i][0] = i
	}
	for j := 0; j <= len(br); j++ {
		d[0][j] = j
	}
	for i := 1; i <= len(ar); i++ {
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			d[i][j] = min3(d[i][j-1]+1, d[i-1][j]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && ar[i-1] == br[j-2] && ar[i-2] == br[j-1] {
				if t := d[i-2][j-2] + 1; t < d[i][j] {
					d[i][j] = t
				}
			}
		}
	}
	return d[len(ar)][len(br)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
