package main

// `draftcat pending` and the /pending operator command — the gates waiting on
// a human right now.
//
// Every open gate has been written to pending_approvals before its prompt went
// out since v0.4.0, so a restart can reconcile it. Nothing read those rows
// while the engine was live, though: an operator who stepped away had no way
// to ask "what is waiting on me", and an auditor could not see a gate that was
// about to expire. Both surfaces read the same table.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	statestore "github.com/renezander030/draftcat/internal/state"
)

type pendingJSON struct {
	ID          int64  `json:"id"`
	Pipeline    string `json:"pipeline"`
	Step        string `json:"step"`
	PayloadHash string `json:"payload_hash"`
	QuorumN     int    `json:"quorum_n"`
	OpenedAt    string `json:"opened_at"`
	ExpiresAt   string `json:"expires_at"`
	// State is "waiting" while the gate can still be decided, or "expired"
	// once expires_at has passed — the engine denies those on its own clock
	// and reconciles the row; the CLI just reports what it sees.
	State string `json:"state"`
}

func pendingState(p statestore.PendingApproval, now time.Time) string {
	if !p.ExpiresAt.IsZero() && now.After(p.ExpiresAt) {
		return "expired"
	}
	return "waiting"
}

// formatPendingLines renders open gates for the operator channel: one header
// line, then one line per gate, oldest first.
func formatPendingLines(rows []statestore.PendingApproval, now time.Time) []string {
	if len(rows) == 0 {
		return []string{"[pending] No gates open."}
	}
	lines := []string{fmt.Sprintf("[pending] %d gate(s) waiting", len(rows))}
	for _, p := range rows {
		age := now.Sub(p.OpenedAt).Round(time.Second)
		line := fmt.Sprintf("  %s / %s — opened %s ago", p.Pipeline, p.Step, humanDuration(age))
		if pendingState(p, now) == "expired" {
			line += fmt.Sprintf(", expired %s ago", humanDuration(now.Sub(p.ExpiresAt).Round(time.Second)))
		} else if !p.ExpiresAt.IsZero() {
			line += fmt.Sprintf(", %s left", humanDuration(p.ExpiresAt.Sub(now).Round(time.Second)))
		}
		if p.QuorumN > 1 {
			line += fmt.Sprintf(", quorum %d", p.QuorumN)
		}
		lines = append(lines, line)
	}
	return lines
}

// humanDuration is time.Duration.String without the sub-second noise.
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) - h*60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

// handlePending answers the /pending operator command.
func handlePending(bot *TGBot) {
	rows, err := state.OpenApprovals()
	if err != nil {
		_ = bot.Send("[pending] could not read open gates: " + err.Error())
		return
	}
	_ = bot.Send(strings.Join(formatPendingLines(rows, time.Now()), "\n"))
}

func runPendingCmd(args []string) int {
	configPath := "config.yaml"
	jsonOut := false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "-json", "--json":
			jsonOut = true
		case "-config", "--config":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "pending: --config requires a path")
				return 2
			}
			configPath = args[i+1]
			i++
		case "-h", "--help", "help":
			fmt.Println("Usage: draftcat pending [--json] [--config path]")
			fmt.Println("\nLists the approval gates currently waiting on a human, oldest first,")
			fmt.Println("including tool calls held by the tool-call gate. --json prints the archivable form.")
			return 0
		default:
			fmt.Fprintf(os.Stderr, "pending: unknown option %q\n", a)
			return 2
		}
	}

	st, closeStore, code := openStateForCmd(configPath)
	if code != 0 {
		return code
	}
	defer closeStore()

	rows, err := st.OpenApprovals()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pending: read open gates: %v\n", err)
		return 1
	}
	now := time.Now()

	if jsonOut {
		out := make([]pendingJSON, 0, len(rows))
		for _, p := range rows {
			out = append(out, pendingJSON{
				ID: p.ID, Pipeline: p.Pipeline, Step: p.Step, PayloadHash: p.PayloadHash, QuorumN: p.QuorumN,
				OpenedAt: p.OpenedAt.UTC().Format(time.RFC3339), ExpiresAt: p.ExpiresAt.UTC().Format(time.RFC3339),
				State: pendingState(p, now),
			})
		}
		b, mErr := json.MarshalIndent(out, "", "  ")
		if mErr != nil {
			fmt.Fprintf(os.Stderr, "pending: encode: %v\n", mErr)
			return 1
		}
		fmt.Println(string(b))
		return 0
	}

	if len(rows) == 0 {
		fmt.Println("No gates open.")
		return 0
	}
	for _, p := range rows {
		gateState := pendingState(p, now)
		left := "-"
		if gateState == "expired" {
			left = "expired " + humanDuration(now.Sub(p.ExpiresAt)) + " ago"
		} else if !p.ExpiresAt.IsZero() {
			left = humanDuration(p.ExpiresAt.Sub(now)) + " left"
		}
		fmt.Printf("%s  %-22s %-22s %-8s quorum=%d  %s\n",
			p.OpenedAt.UTC().Format(time.RFC3339), p.Pipeline, p.Step, gateState, p.QuorumN, left)
	}
	return 0
}
