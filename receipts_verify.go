package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	statestore "github.com/renezander030/draftcat/internal/state"
)

type receiptCheck struct {
	Line         int    `json:"line"`
	ReceiptID    string `json:"receipt_id"`
	Verification string `json:"verification"`
}

func exportedReceiptRecord(v receiptJSON) (statestore.ApprovalRecord, error) {
	if v.Version != 1 && v.Version != 2 {
		return statestore.ApprovalRecord{}, fmt.Errorf("unsupported receipt version %d", v.Version)
	}
	decided, err := time.Parse(time.RFC3339, v.DecidedAt)
	if err != nil {
		return statestore.ApprovalRecord{}, fmt.Errorf("invalid decided_at: %w", err)
	}
	var expires time.Time
	if v.ExpiresAt != "" {
		expires, err = time.Parse(time.RFC3339, v.ExpiresAt)
		if err != nil {
			return statestore.ApprovalRecord{}, fmt.Errorf("invalid expires_at: %w", err)
		}
	}
	return statestore.ApprovalRecord{
		Version: v.Version, ReceiptID: v.ReceiptID, RunID: v.RunID, ActionID: v.ActionID,
		Pipeline: v.Pipeline, Step: v.Step, DecidedAt: decided, Decision: v.Decision,
		OperatorID: v.OperatorID, PayloadHash: v.PayloadHash, Policy: v.Policy, PolicyHash: v.PolicyHash,
		BindingHash: v.BindingHash, ExpiresAt: expires, Lifecycle: v.Lifecycle,
		QuorumN: v.QuorumN, QuorumGot: v.QuorumGot, Nonce: v.Nonce, Signature: v.Signature,
	}, nil
}

func verifyReceiptStream(src io.Reader, secret []byte) ([]receiptCheck, error) {
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	checks := []receiptCheck{}
	for line := 1; scanner.Scan(); line++ {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var v receiptJSON
		if err := decodeStrictJSON(scanner.Bytes(), &v); err != nil {
			return checks, fmt.Errorf("line %d: %w", line, err)
		}
		record, err := exportedReceiptRecord(v)
		if err != nil {
			return checks, fmt.Errorf("line %d: %w", line, err)
		}
		verdict := statestore.VerifyApprovalRecord(secret, record)
		if v.Signature != "" && len(secret) == 0 {
			verdict = "unverified"
		}
		checks = append(checks, receiptCheck{Line: line, ReceiptID: v.ReceiptID, Verification: verdict})
	}
	if err := scanner.Err(); err != nil {
		return checks, err
	}
	if len(checks) == 0 {
		return checks, fmt.Errorf("no receipts in input")
	}
	return checks, nil
}

func runReceiptsVerify(args []string) int {
	path := ""
	jsonOut := false
	for _, arg := range args {
		switch arg {
		case "--json":
			jsonOut = true
		case "--help", "-h":
			fmt.Println("Usage: draftcat receipts verify <file.jsonl|-> [--json]")
			return 0
		default:
			if path != "" || (strings.HasPrefix(arg, "-") && arg != "-") {
				fmt.Fprintln(os.Stderr, "receipts verify: expected one JSONL file or -")
				return 2
			}
			path = arg
		}
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "Usage: draftcat receipts verify <file.jsonl|-> [--json]")
		return 2
	}
	secret := []byte(os.Getenv("DRAFTCAT_APPROVAL_SECRET"))
	if len(secret) == 0 {
		fmt.Fprintln(os.Stderr, "DRAFTCAT_APPROVAL_SECRET is not set — cannot verify receipts")
		return 2
	}
	var src io.Reader = os.Stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "receipts verify: %v\n", err)
			return 1
		}
		defer func() { _ = f.Close() }()
		src = f
	}
	checks, err := verifyReceiptStream(src, secret)
	if err != nil {
		fmt.Fprintf(os.Stderr, "receipts verify: %v\n", err)
		return 1
	}
	code := 0
	for _, c := range checks {
		if c.Verification != "ok" {
			code = 1
		}
	}
	if jsonOut {
		if err := json.NewEncoder(os.Stdout).Encode(checks); err != nil {
			fmt.Fprintf(os.Stderr, "receipts verify: %v\n", err)
			return 1
		}
	} else {
		for _, c := range checks {
			fmt.Printf("%-9s line=%d receipt=%s\n", c.Verification, c.Line, c.ReceiptID)
		}
	}
	return code
}
