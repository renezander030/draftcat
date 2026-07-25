package validate

import (
	"strings"
	"testing"

	"github.com/renezander030/draftcat/internal/config"
)

func findingsAt(rep *validateReport, level, pathFragment string) []validateFinding {
	var out []validateFinding
	for _, f := range rep.Findings {
		if f.Level == level && strings.Contains(f.Path, pathFragment) {
			out = append(out, f)
		}
	}
	return out
}

func approvalCfg(step config.StepConfig, allowed ...int64) *config.Config {
	if len(allowed) == 0 {
		allowed = []int64{111, 222, 333}
	}
	return &config.Config{
		Telegram: config.TelegramConfig{
			Security: config.ChannelSecurity{AllowedUsers: allowed},
		},
		Pipelines: []config.PipelineConfig{{Name: "p", Steps: []config.StepConfig{step}}},
	}
}

// The regression this whole change exists for: `channel: slack` used to sit in
// the validator's allow-list while no Slack channel was ever implemented and
// step.Channel was never read at runtime. Validation said OK and approvals went
// to Telegram — an approval gate routing somewhere the operator is not watching
// is worse than no gate, because it still looks like it held.
func TestValidateRejectsUnimplementedChannel(t *testing.T) {
	cfg := approvalCfg(config.StepConfig{
		Name: "gate", Type: "approval", Channel: "slack",
	})
	rep := runCheckPipelines(cfg)

	errs := findingsAt(rep, "error", ".channel")
	if len(errs) == 0 {
		t.Fatalf("channel 'slack' must be a hard error while unimplemented; findings: %+v", rep.Findings)
	}
	if !strings.Contains(errs[0].Message, "not implemented") {
		t.Errorf("message %q should say the channel is not implemented", errs[0].Message)
	}
}

func TestValidateAcceptsImplementedChannel(t *testing.T) {
	cfg := approvalCfg(config.StepConfig{
		Name: "gate", Type: "approval", Channel: "telegram",
	})
	if errs := findingsAt(runCheckPipelines(cfg), "error", ".channel"); len(errs) != 0 {
		t.Fatalf("telegram is implemented and must validate cleanly, got %+v", errs)
	}
}

func TestValidateRejectsTypoedChannel(t *testing.T) {
	cfg := approvalCfg(config.StepConfig{
		Name: "gate", Type: "approval", Channel: "telegramm",
	})
	if errs := findingsAt(runCheckPipelines(cfg), "error", ".channel"); len(errs) == 0 {
		t.Fatal("a typo'd channel name must be an error, not a silent reroute")
	}
}

// --- approver scoping ---

// An id that is not an allowed user silently shrinks the real approver pool,
// which can make a quorum unsatisfiable at 4am. Catch it at config time.
func TestValidateApproverNotInAllowedUsers(t *testing.T) {
	cfg := approvalCfg(config.StepConfig{
		Name: "gate", Type: "approval", Channel: "telegram",
		Approvers: []int64{999},
	}, 111, 222)

	errs := findingsAt(runCheckPipelines(cfg), "error", ".approvers")
	if len(errs) == 0 {
		t.Fatal("an approver absent from allowed_users must be an error")
	}
}

func TestValidateApproversCannotSatisfyQuorum(t *testing.T) {
	cfg := approvalCfg(config.StepConfig{
		Name: "gate", Type: "approval", Channel: "telegram",
		Quorum: 3, Approvers: []int64{111, 222},
	}, 111, 222, 333)

	errs := findingsAt(runCheckPipelines(cfg), "error", ".approvers")
	if len(errs) == 0 {
		t.Fatal("2 approvers cannot satisfy quorum 3 — must error rather than hang until timeout")
	}
}

func TestValidateApproversSatisfyingQuorumIsClean(t *testing.T) {
	cfg := approvalCfg(config.StepConfig{
		Name: "gate", Type: "approval", Channel: "telegram",
		Quorum: 2, Approvers: []int64{111, 222},
	}, 111, 222, 333)

	if errs := findingsAt(runCheckPipelines(cfg), "error", ".approvers"); len(errs) != 0 {
		t.Fatalf("2 valid approvers satisfy quorum 2, got %+v", errs)
	}
}

func TestValidateDuplicateApproverWarnsAndDoesNotCountTwice(t *testing.T) {
	cfg := approvalCfg(config.StepConfig{
		Name: "gate", Type: "approval", Channel: "telegram",
		Quorum: 2, Approvers: []int64{111, 111},
	}, 111, 222)
	rep := runCheckPipelines(cfg)

	if warns := findingsAt(rep, "warn", ".approvers"); len(warns) == 0 {
		t.Error("a duplicated approver id should warn")
	}
	// 111 listed twice is still one human, so quorum 2 is unsatisfiable.
	if errs := findingsAt(rep, "error", ".approvers"); len(errs) == 0 {
		t.Error("a duplicate must not count twice toward quorum")
	}
}

func TestValidateEmptyApproversIsUnscopedAndClean(t *testing.T) {
	cfg := approvalCfg(config.StepConfig{
		Name: "gate", Type: "approval", Channel: "telegram", Quorum: 2,
	}, 111, 222)

	if errs := findingsAt(runCheckPipelines(cfg), "error", ".approvers"); len(errs) != 0 {
		t.Fatalf("no approvers list means unscoped, which is the pre-existing behavior; got %+v", errs)
	}
}

// --- startup gate ---

// CheckAtStartup must run the same checks as the subcommand, so the engine and
// `draftcat validate` can never disagree about what a valid config is.
func TestCheckAtStartupReportsErrors(t *testing.T) {
	cfg := approvalCfg(config.StepConfig{
		Name: "gate", Type: "approval", Channel: "slack",
	})
	findings := CheckAtStartup(cfg, "skills")

	var errs int
	for _, f := range findings {
		if f.Level == "error" {
			errs++
		}
	}
	if errs == 0 {
		t.Fatalf("startup check must surface the unimplemented-channel error; got %+v", findings)
	}
}
