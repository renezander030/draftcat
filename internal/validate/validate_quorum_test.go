package validate

import (
	"strings"
	"testing"

	"github.com/renezander030/draftcat/internal/config"
	skillsapi "github.com/renezander030/draftcat/internal/skills"
)

func quorumFindings(rep *validateReport) []validateFinding {
	var out []validateFinding
	for _, f := range rep.Findings {
		if f.Level == "error" && strings.Contains(f.Path, ".quorum") {
			out = append(out, f)
		}
	}
	return out
}

func runCheckPipelines(cfg *config.Config) *validateReport {
	rep := &validateReport{}
	checkPipelines(cfg, map[string]*skillsapi.SkillDef{}, "skills", rep)
	return rep
}

// TestValidateQuorumUnsatisfiable: quorum greater than the number of allowed
// operators is a hard error (unsatisfiable), not a warning.
func TestValidateQuorumUnsatisfiable(t *testing.T) {
	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			Security: config.ChannelSecurity{AllowedUsers: []int64{1, 2, 3}},
		},
		Pipelines: []config.PipelineConfig{{
			Name: "p",
			Steps: []config.StepConfig{
				{Name: "gate", Type: "approval", Channel: "telegram", Quorum: 5},
			},
		}},
	}
	rep := runCheckPipelines(cfg)
	if len(quorumFindings(rep)) == 0 {
		t.Fatalf("expected an unsatisfiable-quorum error, got findings: %+v", rep.Findings)
	}
}

// TestValidateQuorumNonTelegram: quorum >= 2 on a non-telegram channel errors,
// because only telegram implements multi-operator approval.
func TestValidateQuorumNonTelegram(t *testing.T) {
	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			Security: config.ChannelSecurity{AllowedUsers: []int64{1, 2, 3}},
		},
		Pipelines: []config.PipelineConfig{{
			Name: "p",
			Steps: []config.StepConfig{
				{Name: "gate", Type: "approval", Channel: "slack", Quorum: 2},
			},
		}},
	}
	rep := runCheckPipelines(cfg)
	if len(quorumFindings(rep)) == 0 {
		t.Fatalf("expected a non-telegram-quorum error, got findings: %+v", rep.Findings)
	}
}

// TestValidateQuorumSingleApproverOK: quorum 0/1 produces no quorum error.
func TestValidateQuorumSingleApproverOK(t *testing.T) {
	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			Security: config.ChannelSecurity{AllowedUsers: []int64{1}},
		},
		Pipelines: []config.PipelineConfig{{
			Name: "p",
			Steps: []config.StepConfig{
				{Name: "gate", Type: "approval", Channel: "telegram", Quorum: 0},
			},
		}},
	}
	rep := runCheckPipelines(cfg)
	if got := len(quorumFindings(rep)); got != 0 {
		t.Errorf("single-approver step should produce no quorum error, got %d", got)
	}
}
