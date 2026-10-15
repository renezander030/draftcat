package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	statestore "github.com/renezander030/draftcat/internal/state"
)

const reportUsage = `Usage: draftcat report [--since 30d|YYYY-MM-DD] [--until YYYY-MM-DD] [--pipeline name] [--config path] [--json]

Summarizes human oversight over a period from the receipts in the state store:
decisions by pipeline and step, who decided, skip reasons, reported tool
executions and receipt integrity. Read-only. Exits 1 when a receipt fails its
signature check.`

type reportRow struct {
	Pipeline  string         `json:"pipeline"`
	Step      string         `json:"step"`
	Decisions map[string]int `json:"decisions"`
}

type oversightReport struct {
	From      string                    `json:"from"`
	To        string                    `json:"to"`
	Pipeline  string                    `json:"pipeline,omitempty"`
	Receipts  int                       `json:"receipts"`
	Decisions map[string]int            `json:"decisions"`
	ByPerson  int                       `json:"decided_by_person"`
	Steps     []reportRow               `json:"by_step"`
	Operators map[string]int            `json:"by_operator"`
	Skips     int                       `json:"skips"`
	Reasons   int                       `json:"skips_with_reason"`
	Outcomes  map[string]map[string]int `json:"tool_outcomes"`
	Integrity map[string]int            `json:"integrity"`
	First     string                    `json:"first_receipt,omitempty"`
	Last      string                    `json:"last_receipt,omitempty"`
	// Digest is sha256 over the receipt ids in decided order, one per line,
	// so a receipts export of the same window can be matched to this report.
	Digest string `json:"receipt_ids_sha256"`
}

func runReportCmd(args []string) int { return reportCmd(args, os.Stdout, os.Stderr, time.Now()) }

func reportCmd(args []string, stdout, stderr io.Writer, now time.Time) int {
	configPath, pipeline, since, until, asJSON := "config.yaml", "", "30d", "", false
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func() (string, bool) {
			if i+1 >= len(args) {
				_, _ = fmt.Fprintf(stderr, "report: %s requires a value\n", a)
				return "", false
			}
			i++
			return args[i], true
		}
		var ok bool
		switch a {
		case "--config", "-config":
			configPath, ok = value()
		case "--pipeline":
			pipeline, ok = value()
		case "--since":
			since, ok = value()
		case "--until":
			until, ok = value()
		case "--json":
			asJSON, ok = true, true
		case "-h", "--help", "help":
			_, _ = fmt.Fprintln(stdout, reportUsage)
			return 0
		default:
			_, _ = fmt.Fprintf(stderr, "report: unknown argument %q\n%s\n", a, reportUsage)
			return 2
		}
		if !ok {
			return 2
		}
	}
	from, to, err := reportWindow(since, until, now)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "report: %v\n", err)
		return 2
	}
	st, closeStore, code := openStateForCmd(configPath)
	if code != 0 {
		return code
	}
	defer closeStore()
	rep, err := buildOversightReport(st, from, to, pipeline, []byte(os.Getenv("DRAFTCAT_APPROVAL_SECRET")))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "report: %v\n", err)
		return 1
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else {
		printOversightReport(stdout, rep)
	}
	if rep.Integrity["tampered"] > 0 {
		return 1
	}
	return 0
}

// reportWindow resolves --since (a day count like 30d, or a UTC date) and
// --until (a UTC date, inclusive) into a half-open [from, to) window.
func reportWindow(since, until string, now time.Time) (time.Time, time.Time, error) {
	now = now.UTC()
	to := now
	if until != "" {
		d, err := time.Parse("2006-01-02", until)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("--until wants YYYY-MM-DD (got %q)", until)
		}
		to = d.AddDate(0, 0, 1)
	}
	var from time.Time
	if days, ok := strings.CutSuffix(since, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return time.Time{}, time.Time{}, fmt.Errorf("--since wants a day count like 30d or YYYY-MM-DD (got %q)", since)
		}
		from = to.AddDate(0, 0, -n)
	} else {
		d, err := time.Parse("2006-01-02", since)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("--since wants a day count like 30d or YYYY-MM-DD (got %q)", since)
		}
		from = d
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, fmt.Errorf("--since must be before --until")
	}
	return from, to, nil
}

func buildOversightReport(st *statestore.StateStore, from, to time.Time, pipeline string, secret []byte) (oversightReport, error) {
	rep := oversightReport{
		From: from.Format(time.RFC3339), To: to.Format(time.RFC3339), Pipeline: pipeline,
		Decisions: map[string]int{}, Operators: map[string]int{}, Integrity: map[string]int{},
	}
	rows, err := st.ApprovalsBetween(from, to, pipeline)
	if err != nil {
		return rep, err
	}
	steps := map[[2]string]*reportRow{}
	digest := sha256.New()
	var skipIDs []string
	for _, r := range rows {
		id := receiptID(r)
		_, _ = io.WriteString(digest, id+"\n")
		rep.Receipts++
		rep.Decisions[r.Decision]++
		key := [2]string{r.Pipeline, r.Step}
		row := steps[key]
		if row == nil {
			row = &reportRow{Pipeline: r.Pipeline, Step: r.Step, Decisions: map[string]int{}}
			steps[key] = row
		}
		row.Decisions[r.Decision]++
		if r.OperatorID != 0 {
			rep.ByPerson++
			rep.Operators[strconv.FormatInt(r.OperatorID, 10)]++
		}
		if r.Decision == "skip" {
			rep.Skips++
			skipIDs = append(skipIDs, id)
		}
		rep.Integrity[receiptVerdict(secret, r)]++
		if rep.First == "" {
			rep.First = id
		}
		rep.Last = id
	}
	rep.Digest = "sha256:" + hex.EncodeToString(digest.Sum(nil))
	for _, row := range steps {
		rep.Steps = append(rep.Steps, *row)
	}
	sort.Slice(rep.Steps, func(i, j int) bool {
		if rep.Steps[i].Pipeline != rep.Steps[j].Pipeline {
			return rep.Steps[i].Pipeline < rep.Steps[j].Pipeline
		}
		return rep.Steps[i].Step < rep.Steps[j].Step
	})
	if len(skipIDs) > 0 {
		notes, err := st.ApprovalNotes(skipIDs)
		if err != nil {
			return rep, err
		}
		rep.Reasons = len(notes)
	}
	if pipeline == "" || pipeline == "tool-gate" {
		if rep.Outcomes, err = st.ToolOutcomeCounts(from, to); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

func printOversightReport(w io.Writer, r oversightReport) {
	scope := "all pipelines"
	if r.Pipeline != "" {
		scope = "pipeline " + r.Pipeline
	}
	_, _ = fmt.Fprintf(w, "# Oversight report\n\n%s to %s (UTC), %s\n\n", r.From, r.To, scope)
	if r.Receipts == 0 {
		_, _ = fmt.Fprintln(w, "No decisions were recorded in this period.")
		return
	}
	_, _ = fmt.Fprintf(w, "%d decision(s); %d made by a person, the rest by configured policy or the clock.\n\n", r.Receipts, r.ByPerson)

	decisions := sortedKeys(r.Decisions)
	_, _ = fmt.Fprintln(w, "## Decisions")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "| Decision | Count |")
	_, _ = fmt.Fprintln(w, "|---|---|")
	for _, d := range decisions {
		_, _ = fmt.Fprintf(w, "| %s | %d |\n", d, r.Decisions[d])
	}

	_, _ = fmt.Fprintln(w, "\n## By pipeline and step")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintf(w, "| Pipeline | Step | %s |\n", strings.Join(decisions, " | "))
	_, _ = fmt.Fprintf(w, "|---|---|%s\n", strings.Repeat("---|", len(decisions)))
	for _, row := range r.Steps {
		cells := make([]string, len(decisions))
		for i, d := range decisions {
			cells[i] = strconv.Itoa(row.Decisions[d])
		}
		_, _ = fmt.Fprintf(w, "| %s | %s | %s |\n", row.Pipeline, row.Step, strings.Join(cells, " | "))
	}

	if len(r.Operators) > 0 {
		_, _ = fmt.Fprintln(w, "\n## By operator")
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, "| Operator | Decisions |")
		_, _ = fmt.Fprintln(w, "|---|---|")
		for _, op := range sortedKeys(r.Operators) {
			_, _ = fmt.Fprintf(w, "| %s | %d |\n", op, r.Operators[op])
		}
	}
	if r.Skips > 0 {
		_, _ = fmt.Fprintf(w, "\n%d of %d skip(s) carry a recorded reason.\n", r.Reasons, r.Skips)
	}
	if len(r.Outcomes) > 0 {
		_, _ = fmt.Fprintln(w, "\n## Reported tool executions")
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, "| Tool | Succeeded | Failed |")
		_, _ = fmt.Fprintln(w, "|---|---|---|")
		for _, tool := range sortedKeys(r.Outcomes) {
			_, _ = fmt.Fprintf(w, "| %s | %d | %d |\n", tool, r.Outcomes[tool]["succeeded"], r.Outcomes[tool]["failed"])
		}
	}
	_, _ = fmt.Fprintln(w, "\n## Receipts")
	_, _ = fmt.Fprintln(w)
	var integrity []string
	for _, k := range sortedKeys(r.Integrity) {
		integrity = append(integrity, fmt.Sprintf("%s %d", k, r.Integrity[k]))
	}
	_, _ = fmt.Fprintf(w, "Signatures: %s.\n", strings.Join(integrity, ", "))
	_, _ = fmt.Fprintf(w, "First %s, last %s; receipt ids sha256 %s.\n", r.First, r.Last, r.Digest)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
