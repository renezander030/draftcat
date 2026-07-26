package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
)

// `draftcat runs` — read the audit trail back out.
//
// Every run and every approval decision has been recorded in SQLite for a
// while, but nothing could read it without an sqlite3 client. An audit trail
// you cannot query is only half an audit trail, which is what issue #4 was
// actually asking for ("easy to review, search, and archive").
//
// It asked for a JSON file per run under logs/. That would duplicate state.db
// into a second, unindexed copy and add a retention problem, so this exposes
// the existing store instead: `--json` gives the same archivable output on
// stdout, pipeable into a file, jq, or a log shipper.
//
// Per-step timings and token counts are NOT here: those live in the
// observability spans (`observability.spans`, OTLP/Prometheus), which is the
// right home for them. This is the durable governance record — what ran, what
// was decided, by whom.

type runJSON struct {
	Pipeline  string         `json:"pipeline"`
	StartedAt string         `json:"started_at"`
	EndedAt   string         `json:"ended_at"`
	Duration  float64        `json:"duration_seconds"`
	Status    string         `json:"status"`
	Error     string         `json:"error,omitempty"`
	Approvals []approvalJSON `json:"approvals"`
}

type approvalJSON struct {
	Step       string `json:"step"`
	DecidedAt  string `json:"decided_at"`
	Decision   string `json:"decision"`
	OperatorID int64  `json:"operator_id"`
	QuorumN    int    `json:"quorum_n"`
	QuorumGot  int    `json:"quorum_got"`
	Signed     bool   `json:"signed"`
}

func runRunsCmd(args []string) int {
	pipeline := ""
	configPath := "config.yaml"
	limit := 20
	jsonOut := false

	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "-json", "--json":
			jsonOut = true
		case "-limit", "--limit":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "runs: --limit requires a number")
				return 2
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n <= 0 {
				fmt.Fprintf(os.Stderr, "runs: --limit must be a positive integer, got %q\n", args[i+1])
				return 2
			}
			limit = n
			i++
		case "-config", "--config":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "runs: --config requires a path")
				return 2
			}
			configPath = args[i+1]
			i++
		case "-h", "--help", "help":
			fmt.Println("Usage: draftcat runs [pipeline] [--limit N] [--json] [--config path]")
			fmt.Println("\nShows recent pipeline runs and the approval decisions recorded during each.")
			fmt.Println("Omit the pipeline name to see every pipeline. --json prints the archivable form.")
			return 0
		default:
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(os.Stderr, "runs: unknown option %q\n", a)
				return 2
			}
			if pipeline != "" {
				fmt.Fprintf(os.Stderr, "runs: unexpected argument %q\n", a)
				return 2
			}
			pipeline = a
		}
	}

	st, closeStore, code := openStateForCmd(configPath)
	if code != 0 {
		return code
	}
	defer closeStore()

	var runs []statestore.RunRecord
	var err error
	if pipeline == "" {
		runs, err = st.AllRecentRuns(limit)
	} else {
		runs, err = st.RecentRuns(pipeline, limit)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "runs: read run history: %v\n", err)
		return 1
	}

	out := make([]runJSON, 0, len(runs))
	for _, r := range runs {
		out = append(out, runJSON{
			Pipeline:  r.Pipeline,
			StartedAt: r.StartedAt.Format(time.RFC3339),
			EndedAt:   r.EndedAt.Format(time.RFC3339),
			Duration:  r.EndedAt.Sub(r.StartedAt).Seconds(),
			Status:    r.Status,
			Error:     r.Error,
			Approvals: approvalsDuring(st, r),
		})
	}

	if jsonOut {
		b, mErr := json.MarshalIndent(out, "", "  ")
		if mErr != nil {
			fmt.Fprintf(os.Stderr, "runs: encode: %v\n", mErr)
			return 1
		}
		fmt.Println(string(b))
		return 0
	}

	if len(out) == 0 {
		if pipeline == "" {
			fmt.Println("No runs recorded yet.")
		} else {
			fmt.Printf("No runs recorded for %q.\n", pipeline)
		}
		return 0
	}
	for _, r := range out {
		line := fmt.Sprintf("%s  %-22s %-7s %6.1fs", r.StartedAt, r.Pipeline, r.Status, r.Duration)
		if r.Error != "" {
			line += "  " + truncateOneLine(r.Error, 60)
		}
		fmt.Println(line)
		for _, a := range r.Approvals {
			who := "system"
			if a.OperatorID != 0 {
				who = strconv.FormatInt(a.OperatorID, 10)
			}
			receipt := ""
			if a.Signed {
				receipt = " [signed]"
			}
			fmt.Printf("    %-20s %-11s by %s (%d/%d)%s\n",
				a.Step, a.Decision, who, a.QuorumGot, a.QuorumN, receipt)
		}
	}
	return 0
}

// approvalsDuring attaches the approval decisions recorded inside a run's
// window. There is no run_id on action_approvals, but the scheduler refuses to
// start a pipeline that is already running, so runs of one pipeline never
// overlap and the timestamp window is an unambiguous join.
func approvalsDuring(st *statestore.StateStore, r statestore.RunRecord) []approvalJSON {
	recs, err := st.ApprovalsForPipeline(r.Pipeline, 1000)
	if err != nil {
		return nil
	}
	var out []approvalJSON
	for _, a := range recs {
		if a.DecidedAt.Before(r.StartedAt) || a.DecidedAt.After(r.EndedAt) {
			continue
		}
		out = append(out, approvalJSON{
			Step:       a.Step,
			DecidedAt:  a.DecidedAt.Format(time.RFC3339),
			Decision:   a.Decision,
			OperatorID: a.OperatorID,
			QuorumN:    a.QuorumN,
			QuorumGot:  a.QuorumGot,
			Signed:     a.Signature != "",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DecidedAt < out[j].DecidedAt })
	return out
}

// openStateForCmd resolves the state path exactly like the engine does — env
// override, then config, then ./state.db — and opens it read-only enough for a
// reporting command.
func openStateForCmd(configPath string) (*statestore.StateStore, func(), int) {
	statePath := strings.TrimSpace(os.Getenv("DRAFTCAT_STATE_PATH"))
	if statePath == "" {
		// #nosec G304 -- configPath is the operator's own --config argument on
		// their own machine; reading the file they named is the feature. Same
		// pattern as runAuditVerify.
		if data, err := os.ReadFile(configPath); err == nil {
			var cfg config.Config
			if yaml.Unmarshal(data, &cfg) == nil {
				statePath = cfg.State.Path
			}
		}
	}
	if statePath == "" {
		statePath = "./state.db"
	}
	st, err := statestore.OpenStateStore(statePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runs: open state store %s: %v\n", statePath, err)
		return nil, func() {}, 1
	}
	return st, func() { _ = st.Close() }, 0
}
