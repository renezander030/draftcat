package main

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
)

// validKnownActions mirrors the deterministic action switch in runPipeline.
// Keep in sync when new actions are added.
var validKnownActions = map[string]string{
	"":                         "pass-through",
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

var validApprovalChannels = map[string]bool{
	"telegram": true,
	"slack":    true,
}

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

// runValidate is the entry point for `draftyard validate`.
func runValidate(args []string) int {
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
			fmt.Println("Usage: draftyard validate [--config path] [--skills dir] [--strict] [--json]")
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
	var cfg Config
	if err := yaml.Unmarshal(cfgData, &cfg); err != nil {
		rep.errf("config", "failed to parse %s: %v", configPath, err)
		return printValidateReport(rep, jsonOut, strict)
	}

	skills := loadSkillsForValidate(skillsDir, rep)
	checkConfigSecurity(&cfg, rep)
	checkTimeouts(&cfg, rep)
	checkRolesToModels(&cfg, rep)
	checkEnvVars(&cfg, rep)
	checkPipelines(&cfg, skills, skillsDir, rep)
	checkOrphanedSkills(&cfg, skills, rep)
	checkSecretsHygiene(rep)

	return printValidateReport(rep, jsonOut, strict)
}

func loadSkillsForValidate(skillsDir string, rep *validateReport) map[string]*SkillDef {
	skills := map[string]*SkillDef{}
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
		var s SkillDef
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
			t, _ := dm["type"].(string)
			switch t {
			case "int", "bool", "string":
			case "":
				rep.warnf("skills/"+s.Name, "output_schema.%s: missing 'type'", field)
			default:
				rep.warnf("skills/"+s.Name, "output_schema.%s: unsupported type %q (validator handles int|bool|string)", field, t)
			}
		}
		if _, dup := skills[s.Name]; dup {
			rep.errf("skills/"+s.Name, "duplicate skill name (also defined in another file)")
		}
		skills[s.Name] = &s
	}
	return skills
}

func checkConfigSecurity(cfg *Config, rep *validateReport) {
	if cfg.Telegram.Security.MaxInputLength <= 0 {
		rep.errf("telegram.security.max_input_length", "must be set and > 0 (engine refuses to start without it)")
	}
	if cfg.Telegram.Security.RateLimit <= 0 {
		rep.errf("telegram.security.rate_limit", "must be set and > 0 (engine refuses to start without it)")
	}
	if len(cfg.Telegram.Security.AllowedUsers) == 0 {
		rep.warnf("telegram.security.allowed_users", "empty — channel will accept no operator")
	}
}

func checkTimeouts(cfg *Config, rep *validateReport) {
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

func checkRolesToModels(cfg *Config, rep *validateReport) {
	for role, model := range cfg.Roles {
		if _, ok := cfg.Models[model]; !ok {
			rep.errf("roles."+role, "model %q is not declared in models:", model)
		}
	}
}

func checkEnvVars(cfg *Config, rep *validateReport) {
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

func checkPipelines(cfg *Config, skills map[string]*SkillDef, skillsDir string, rep *validateReport) {
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

		if p.Schedule != "" && p.Schedule != "manual" {
			if _, err := time.ParseDuration(p.Schedule); err != nil {
				rep.errf(path+".schedule", "invalid duration %q (use e.g. '30m', '1h', or 'manual')", p.Schedule)
			}
		}

		aiSteps := 0
		for si, st := range p.Steps {
			spath := fmt.Sprintf("%s.steps[%d:%s]", path, si, st.Name)
			if st.Name == "" {
				rep.errf(spath, "missing 'name'")
			}
			if !validStepTypes[st.Type] {
				rep.errf(spath+".type", "invalid type %q (must be one of: deterministic, ai, approval)", st.Type)
				continue
			}
			switch st.Type {
			case "deterministic":
				if _, ok := validKnownActions[st.Action]; !ok {
					rep.errf(spath+".action", "unknown action %q (known: %s)", st.Action, strings.Join(knownActionNames(), ", "))
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
				if st.Channel == "" {
					rep.errf(spath+".channel", "approval step requires 'channel'")
				} else if !validApprovalChannels[st.Channel] {
					rep.warnf(spath+".channel", "unknown channel %q (known: telegram, slack)", st.Channel)
				}
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

func checkOrphanedSkills(cfg *Config, skills map[string]*SkillDef, rep *validateReport) {
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
		"voice_calls", "voice_call_count",
		"voice_handoffs", "voice_handoff_count",
		"voice_handoffs_resolved_count",
		"voice_learnings", "voice_learning_count",
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
