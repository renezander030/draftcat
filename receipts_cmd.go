package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	statestore "github.com/renezander030/draftcat/internal/state"
)

type receiptJSON struct {
	Version      int    `json:"version"`
	ReceiptID    string `json:"receipt_id"`
	RunID        string `json:"run_id,omitempty"`
	ActionID     string `json:"action_id,omitempty"`
	Pipeline     string `json:"pipeline"`
	Step         string `json:"step"`
	DecidedAt    string `json:"decided_at"`
	Decision     string `json:"decision"`
	OperatorID   int64  `json:"operator_id"`
	PayloadHash  string `json:"payload_hash"`
	Policy       string `json:"policy,omitempty"`
	PolicyHash   string `json:"policy_hash,omitempty"`
	BindingHash  string `json:"binding_hash,omitempty"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	Lifecycle    string `json:"lifecycle"`
	QuorumN      int    `json:"quorum_n"`
	QuorumGot    int    `json:"quorum_got"`
	Nonce        string `json:"nonce,omitempty"`
	Signature    string `json:"signature,omitempty"`
	Verification string `json:"verification"`
}

func receiptID(r statestore.ApprovalRecord) string {
	if r.ReceiptID != "" {
		return r.ReceiptID
	}
	return strconv.FormatInt(r.ID, 10)
}

func receiptView(r statestore.ApprovalRecord, secret []byte) receiptJSON {
	expires := ""
	if !r.ExpiresAt.IsZero() {
		expires = r.ExpiresAt.UTC().Format(time.RFC3339)
	}
	verification := statestore.VerifyApprovalRecord(secret, r)
	if r.Signature != "" && len(secret) == 0 {
		verification = "unverified"
	}
	return receiptJSON{
		Version: r.Version, ReceiptID: receiptID(r), RunID: r.RunID, ActionID: r.ActionID,
		Pipeline: r.Pipeline, Step: r.Step, DecidedAt: r.DecidedAt.UTC().Format(time.RFC3339),
		Decision: r.Decision, OperatorID: r.OperatorID, PayloadHash: r.PayloadHash,
		Policy: r.Policy, PolicyHash: r.PolicyHash, BindingHash: r.BindingHash,
		ExpiresAt: expires, Lifecycle: r.Lifecycle, QuorumN: r.QuorumN, QuorumGot: r.QuorumGot,
		Nonce: r.Nonce, Signature: r.Signature,
		Verification: verification,
	}
}

func runReceiptsCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: draftcat receipts <list|show|export> [options]")
		return 2
	}
	switch args[0] {
	case "list":
		return runReceiptsList(args[1:])
	case "show":
		return runReceiptsShow(args[1:])
	case "export":
		return runReceiptsExport(args[1:])
	case "-h", "--help", "help":
		fmt.Println("Usage: draftcat receipts <list|show|export> [options]")
		fmt.Println("  list   [--pipeline name] [--limit N] [--json] [--config path]")
		fmt.Println("  show   <receipt-id> [--config path]")
		fmt.Println("  export [--pipeline name] [--limit N] [--out path] [--config path]")
		return 0
	default:
		fmt.Fprintf(os.Stderr, "receipts: unknown command %q\n", args[0])
		return 2
	}
}

type receiptOptions struct {
	pipeline  string
	config    string
	out       string
	limit     int
	json      bool
	receiptID string
}

func parseReceiptOptions(args []string, allowID bool) (receiptOptions, int) {
	o := receiptOptions{config: "config.yaml", limit: 100}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--pipeline":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "receipts: --pipeline requires a name")
				return o, 2
			}
			o.pipeline = args[i+1]
			i++
		case "--config", "-config":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "receipts: --config requires a path")
				return o, 2
			}
			o.config = args[i+1]
			i++
		case "--out":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "receipts: --out requires a path")
				return o, 2
			}
			o.out = args[i+1]
			i++
		case "--limit":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "receipts: --limit requires a number")
				return o, 2
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n <= 0 {
				fmt.Fprintln(os.Stderr, "receipts: --limit must be positive")
				return o, 2
			}
			o.limit = n
			i++
		case "--json":
			o.json = true
		default:
			if allowID && !strings.HasPrefix(args[i], "-") && o.receiptID == "" {
				o.receiptID = args[i]
				continue
			}
			fmt.Fprintf(os.Stderr, "receipts: unknown option %q\n", args[i])
			return o, 2
		}
	}
	return o, 0
}

func loadReceipts(o receiptOptions) (*statestore.StateStore, func(), []statestore.ApprovalRecord, int) {
	st, closeStore, code := openStateForCmd(o.config)
	if code != 0 {
		return nil, closeStore, nil, code
	}
	var rows []statestore.ApprovalRecord
	var err error
	if o.pipeline == "" {
		rows, err = st.AllApprovals(o.limit)
	} else {
		rows, err = st.ApprovalsForPipeline(o.pipeline, o.limit)
	}
	if err != nil {
		closeStore()
		fmt.Fprintf(os.Stderr, "receipts: read: %v\n", err)
		return nil, func() {}, nil, 1
	}
	return st, closeStore, rows, 0
}

func runReceiptsList(args []string) int {
	o, code := parseReceiptOptions(args, false)
	if code != 0 {
		return code
	}
	_, closeStore, rows, code := loadReceipts(o)
	if code != 0 {
		return code
	}
	defer closeStore()
	secret := []byte(os.Getenv("DRAFTCAT_APPROVAL_SECRET"))
	views := make([]receiptJSON, 0, len(rows))
	for _, r := range rows {
		views = append(views, receiptView(r, secret))
	}
	if o.json {
		b, _ := json.MarshalIndent(views, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	if len(views) == 0 {
		fmt.Println("No receipts recorded yet.")
		return 0
	}
	for _, r := range views {
		fmt.Printf("%s  %-22s %-20s %-15s %s\n", r.DecidedAt, r.Pipeline, r.Step, r.Decision, r.ReceiptID)
	}
	return 0
}

func runReceiptsShow(args []string) int {
	o, code := parseReceiptOptions(args, true)
	if code != 0 {
		return code
	}
	if o.receiptID == "" {
		fmt.Fprintln(os.Stderr, "receipts show: receipt id required")
		return 2
	}
	st, closeStore, code := openStateForCmd(o.config)
	if code != 0 {
		return code
	}
	defer closeStore()
	r, err := st.ApprovalByReceiptID(o.receiptID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "receipts show: %v\n", err)
		return 1
	}
	b, _ := json.MarshalIndent(receiptView(r, []byte(os.Getenv("DRAFTCAT_APPROVAL_SECRET"))), "", "  ")
	fmt.Println(string(b))
	return 0
}

func runReceiptsExport(args []string) int {
	o, code := parseReceiptOptions(args, false)
	if code != 0 {
		return code
	}
	_, closeStore, rows, code := loadReceipts(o)
	if code != 0 {
		return code
	}
	defer closeStore()
	var dst io.Writer = os.Stdout
	var f *os.File
	if o.out != "" {
		var err error
		f, err = os.OpenFile(o.out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "receipts export: %v\n", err)
			return 1
		}
		defer func() { _ = f.Close() }()
		dst = f
	}
	w := bufio.NewWriter(dst)
	secret := []byte(os.Getenv("DRAFTCAT_APPROVAL_SECRET"))
	for i := len(rows) - 1; i >= 0; i-- {
		b, err := json.Marshal(receiptView(rows[i], secret))
		if err != nil {
			fmt.Fprintf(os.Stderr, "receipts export: row %d: %v\n", rows[i].ID, err)
			continue
		}
		_, _ = w.Write(append(b, '\n'))
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "receipts export: %v\n", err)
		return 1
	}
	return 0
}
