package main

import (
	"fmt"
	"math/big"

	"github.com/renezander030/draftcat/internal/config"
)

// toolPolicyVerdict is what the configured rules decide about one tool call,
// before any runtime state is consulted: the repeat guard, the operator
// channel and the permit lifecycle come after it. The gate endpoint and
// `draftcat gate` both decide through evaluateToolPolicy, so an offline check
// answers exactly what the running gate would.
type toolPolicyVerdict struct {
	// Decision is allow, deny, or ask (a human decides).
	Decision string
	// DecidedBy names who settled an allow or deny: allowlist or policy.
	DecidedBy string
	// Rule names the configuration behind the decision.
	Rule   string
	Reason string
	// Class groups denials for the operator notice dedup: unlisted, mismatch
	// or cap.
	Class string
	// Why is the line an approval prompt shows when the rules send the call
	// to a human for a reason beyond require_approval.
	Why string
	// Grantable marks an ask a standing grant may answer: the rule allows
	// grants and asks only because it requires approval, not because of an
	// argument mismatch or a daily total.
	Grantable bool
	// Totals reports, for each argument with max_per_day and for
	// max_calls_per_day, the running total before this call and the cap: shown
	// to the human who decides.
	Totals []string
}

// toolDailyTotal returns the running total of one tool argument today.
type toolDailyTotal func(tool, arg string) (*big.Rat, error)

// evaluateToolPolicy decides one call against tool_gate. An unlisted tool is
// denied; arguments outside the rule escalate to a human or are refused per
// on_mismatch; a call that would take a max_per_day total or the
// max_calls_per_day count past its cap is handled the same way; a value above review_above goes to a human; anything
// else is allowed on the strength of being listed. A failing total lookup is
// returned as an error, never guessed.
func evaluateToolPolicy(gate config.ToolGateConfig, tool string, args map[string]interface{}, total toolDailyTotal) (toolPolicyVerdict, error) {
	rule, listed := gate.Lookup(tool)
	if !listed {
		return toolPolicyVerdict{
			Decision: "deny", DecidedBy: "allowlist", Rule: "not listed", Class: "unlisted",
			Reason: fmt.Sprintf("tool %q is not in tool_gate.tools — the gate denies by default", tool),
		}, nil
	}

	// A mismatch can only tighten: it either escalates to a human or refuses.
	// It never lets a call through that the rule's base setting would have
	// sent to a human.
	v := toolPolicyVerdict{Decision: "allow", DecidedBy: "allowlist", Rule: "listed", Reason: "allowlisted in tool_gate"}
	needHuman := rule.RequireApproval
	if ok, why := rule.MatchArgs(args); !ok {
		if rule.DeniesOnMismatch() {
			return toolPolicyVerdict{
				Decision: "deny", DecidedBy: "policy", Rule: "args." + why, Class: "mismatch",
				Reason: "arguments outside the rule — " + why,
			}, nil
		}
		needHuman, v.Why = true, "arguments outside the rule — "+why
	} else {
		caps, err := rule.ReadDailyCaps(args, func(arg string) (*big.Rat, error) {
			spent, err := total(tool, arg)
			if err != nil {
				if arg == config.CallCountKey {
					return nil, fmt.Errorf("max_calls_per_day count for %s: %w", tool, err)
				}
				return nil, fmt.Errorf("max_per_day total for %s.%s: %w", tool, arg, err)
			}
			return spent, nil
		})
		if err != nil {
			return toolPolicyVerdict{}, err
		}
		v.Totals = caps.Totals
		if caps.OverArg != "" {
			if rule.DeniesOnMismatch() {
				return toolPolicyVerdict{
					Decision: "deny", DecidedBy: "policy", Rule: caps.Rule, Class: "cap",
					Reason: "daily total over the rule — " + caps.Why, Totals: v.Totals,
				}, nil
			}
			if v.Why == "" {
				v.Why = "daily total over the rule — " + caps.Why
			}
			needHuman = true
		}
	}
	if !needHuman {
		if review, why := rule.ReviewArgs(args); review {
			needHuman, v.Why = true, "above the review threshold — "+why
		}
	}
	if needHuman {
		v.Decision, v.DecidedBy, v.Rule, v.Reason = "ask", "", "", "a human decides"
		v.Grantable = v.Why == "" && rule.Grantable()
	}
	return v, nil
}
