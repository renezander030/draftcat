package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// stubChannel is the in-memory OperatorChannel used by `draftcat test`.
// It records all messages and returns approval decisions from a fixture queue
// or auto-approve, never making a network call.
type stubChannel struct {
	sent        []string
	decisions   []OperatorDecision
	idx         int
	autoApprove bool
}

func (s *stubChannel) Send(text string) error {
	s.sent = append(s.sent, text)
	fmt.Printf("  [send] %s\n", truncateOneLine(text, 200))
	return nil
}

func (s *stubChannel) SendForApproval(ctx context.Context, draft string) (OperatorDecision, error) {
	fmt.Printf("  [approval-draft]\n%s\n", indentBlock(draft, "    "))
	if s.idx < len(s.decisions) {
		d := s.decisions[s.idx]
		s.idx++
		fmt.Printf("  [approval-decision] %s (from fixture)\n", d.Action)
		return d, nil
	}
	if s.autoApprove {
		fmt.Println("  [approval-decision] approve (auto)")
		return OperatorDecision{Action: "approve"}, nil
	}
	fmt.Println("  [approval-decision] skip (auto, --reject)")
	return OperatorDecision{Action: "skip"}, nil
}

// loadFixture reads <dir>/<name>.json into a generic map.
// Returns (data, found, err). Missing files are not errors.
func loadFixture(fixDir, name string) (map[string]interface{}, bool, error) {
	path := filepath.Join(fixDir, name+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, true, fmt.Errorf("%s: %w", path, err)
	}
	return m, true, nil
}

func runTestCmd(args []string) int {
	configPath := "config.yaml"
	skillsDir := "skills"
	fixturesRoot := "fixtures"
	autoApprove := true
	pipelineName := ""

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-config", "--config":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "test: --config requires a path")
				return 2
			}
			configPath = args[i+1]
			i++
		case "-skills", "--skills":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "test: --skills requires a path")
				return 2
			}
			skillsDir = args[i+1]
			i++
		case "-fixtures", "--fixtures":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "test: --fixtures requires a path")
				return 2
			}
			fixturesRoot = args[i+1]
			i++
		case "-reject", "--reject":
			autoApprove = false
		case "-h", "--help", "help":
			fmt.Println("Usage: draftcat test <pipeline> [--config path] [--skills dir] [--fixtures dir] [--reject]")
			fmt.Println("\nDry-runs a pipeline using fixtures/<pipeline>/<step-name>.json.")
			fmt.Println("Deterministic steps load their data map; ai steps load {\"text\": \"...\"};")
			fmt.Println("approval steps load {\"action\": \"approve|skip|adjust\", \"text\": \"...\"}.")
			fmt.Println("Approval steps auto-approve when no fixture is present (use --reject to skip).")
			return 0
		default:
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(os.Stderr, "test: unknown option %q\n", a)
				return 2
			}
			if pipelineName != "" {
				fmt.Fprintf(os.Stderr, "test: unexpected argument %q (pipeline already set to %q)\n", a, pipelineName)
				return 2
			}
			pipelineName = a
		}
	}

	if pipelineName == "" {
		fmt.Fprintln(os.Stderr, "test: pipeline name required")
		fmt.Fprintln(os.Stderr, "Usage: draftcat test <pipeline> [--fixtures dir]")
		return 2
	}

	cfgData, err := os.ReadFile(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "test: read config: %v\n", err)
		return 1
	}
	var cfg Config
	if err := yaml.Unmarshal(cfgData, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "test: parse config: %v\n", err)
		return 1
	}

	skills, _ := loadSkills(skillsDir)

	var pipeline *PipelineConfig
	for i := range cfg.Pipelines {
		if cfg.Pipelines[i].Name == pipelineName {
			pipeline = &cfg.Pipelines[i]
			break
		}
	}
	if pipeline == nil {
		fmt.Fprintf(os.Stderr, "test: pipeline %q not found in %s\n", pipelineName, configPath)
		return 1
	}

	fixDir := filepath.Join(fixturesRoot, pipelineName)
	stub := &stubChannel{autoApprove: autoApprove}

	fmt.Printf("[test] pipeline=%s fixtures=%s\n", pipelineName, fixDir)
	rc := runTestPipeline(&cfg, *pipeline, skills, stub, fixDir)
	if rc != 0 {
		return rc
	}
	fmt.Println("[test] OK")
	return 0
}

// runTestPipeline walks pipeline steps using fixtures instead of real connectors / AI / approval.
func runTestPipeline(cfg *Config, p PipelineConfig, skills *SkillRegistry, ch *stubChannel, fixDir string) int {
	data := map[string]interface{}{}

	// Optional seed fixture: fixtures/<pipeline>/_input.json merged into the data map before any step.
	if seed, found, err := loadFixture(fixDir, "_input"); err != nil {
		fmt.Fprintf(os.Stderr, "  [error] seed fixture: %v\n", err)
		return 1
	} else if found {
		for k, v := range seed {
			data[k] = v
		}
		fmt.Printf("  [seed] %s/_input.json loaded (%d key(s))\n", fixDir, len(seed))
	}

	for _, step := range p.Steps {
		fmt.Printf("[step:%s] type=%s\n", step.Name, step.Type)

		switch step.Type {
		case "deterministic":
			fix, found, err := loadFixture(fixDir, step.Name)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  [error] %v\n", err)
				return 1
			}
			if !found {
				fmt.Printf("  [skip] no fixture %s/%s.json (deterministic step is a no-op in test mode)\n", fixDir, step.Name)
				continue
			}
			// Two shapes accepted: {"data": {...}} or a flat map merged directly.
			if d, ok := fix["data"].(map[string]interface{}); ok {
				for k, v := range d {
					data[k] = v
				}
			} else {
				for k, v := range fix {
					data[k] = v
				}
			}
			fmt.Printf("  [loaded] fixture into data\n")

		case "ai":
			prompt := step.Prompt
			schema := step.OutputSchema
			if step.Skill != "" {
				sk, ok := skills.Get(step.Skill)
				if !ok {
					fmt.Fprintf(os.Stderr, "  [error] unknown skill %q\n", step.Skill)
					return 1
				}
				prompt = sk.Prompt
				if len(sk.OutputSchema) > 0 {
					schema = sk.OutputSchema
				}
			}
			for k, v := range step.Vars {
				prompt = strings.ReplaceAll(prompt, "{{"+k+"}}", v)
			}
			for k, v := range data {
				prompt = strings.ReplaceAll(prompt, "{{"+k+"}}", fmt.Sprintf("%v", v))
			}
			fmt.Printf("  [prompt] %s\n", truncateOneLine(prompt, 200))

			fix, found, err := loadFixture(fixDir, step.Name)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  [error] %v\n", err)
				return 1
			}
			if !found {
				fmt.Fprintf(os.Stderr, "  [error] ai step %q has no fixture %s/%s.json — supply {\"text\": \"...\"}\n", step.Name, fixDir, step.Name)
				return 1
			}
			text, _ := fix["text"].(string)
			if text == "" {
				fmt.Fprintf(os.Stderr, "  [error] fixture %s/%s.json: missing or empty 'text' field\n", fixDir, step.Name)
				return 1
			}

			if len(schema) > 0 {
				parsed, err := validateOutput(text, schema)
				if err != nil {
					fmt.Fprintf(os.Stderr, "  [error] output validation failed: %v\n", err)
					return 1
				}
				data["ai_output"] = parsed
				fmt.Printf("  [output] %v\n", parsed)
			} else {
				data["ai_output"] = text
				fmt.Printf("  [output] %s\n", truncateOneLine(text, 200))
			}
			data["ai_raw"] = text

		case "approval":
			fix, found, err := loadFixture(fixDir, step.Name)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  [error] %v\n", err)
				return 1
			}
			if found {
				action, _ := fix["action"].(string)
				text, _ := fix["text"].(string)
				if action == "" {
					action = "approve"
				}
				ch.decisions = append(ch.decisions, OperatorDecision{Action: action, Text: text})
			}
			aiOutput := data["ai_output"]
			draftMsg := fmt.Sprintf("[test] draft:\n\n%v", aiOutput)
			decision, _ := ch.SendForApproval(context.Background(), draftMsg)
			if decision.Action == "skip" {
				fmt.Println("  [stop] operator skipped — pipeline ends")
				return 0
			}
			data["approved"] = decision.Action == "approve"

		default:
			fmt.Fprintf(os.Stderr, "  [error] unknown step type %q\n", step.Type)
			return 1
		}
	}

	fmt.Println("[final data]")
	if b, err := json.MarshalIndent(data, "  ", "  "); err == nil {
		fmt.Printf("  %s\n", string(b))
	}
	return 0
}

func truncateOneLine(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func indentBlock(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = prefix + ln
	}
	return strings.Join(lines, "\n")
}
