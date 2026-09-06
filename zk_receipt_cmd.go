package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/renezander030/draftcat/internal/approval"
	"github.com/renezander030/draftcat/internal/config"
	statestore "github.com/renezander030/draftcat/internal/state"
	"github.com/renezander030/draftcat/internal/zkreceipt"
)

func runZKReceiptCmd(args []string) int {
	if len(args) == 0 {
		printZKReceiptUsage()
		return 2
	}
	switch args[0] {
	case "key-id":
		return runZKReceiptKeyID(args[1:])
	case "prove":
		return runZKReceiptProve(args[1:])
	case "verify":
		return runZKReceiptVerify(args[1:])
	case "-h", "--help", "help":
		printZKReceiptUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown zk-receipt command %q\n", args[0])
		printZKReceiptUsage()
		return 2
	}
}

func printZKReceiptUsage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  draftcat zk-receipt key-id")
	fmt.Fprintln(os.Stderr, "  draftcat zk-receipt prove [--config config.yaml] [--out proof.json] <pipeline>")
	fmt.Fprintln(os.Stderr, "  draftcat zk-receipt verify --expect-key <commitment> <proof.json>")
}

func approvalSecret() ([]byte, error) {
	secret := []byte(os.Getenv("DRAFTCAT_APPROVAL_SECRET"))
	if len(secret) < 32 {
		return nil, fmt.Errorf("DRAFTCAT_APPROVAL_SECRET must be set and at least 32 bytes")
	}
	return secret, nil
}

func runZKReceiptKeyID(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "usage: draftcat zk-receipt key-id")
		return 2
	}
	secret, err := approvalSecret()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	commitment, err := zkreceipt.KeyCommitment(secret)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(commitment)
	return 0
}

func runZKReceiptProve(args []string) int {
	flags := flag.NewFlagSet("zk-receipt prove", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", "config.yaml", "Draftcat config path")
	outputPath := flags.String("out", "", "proof bundle path; defaults to stdout")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: draftcat zk-receipt prove [--config config.yaml] [--out proof.json] <pipeline>")
		return 2
	}
	secret, err := approvalSecret()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	pipeline := flags.Arg(0)
	storePath := resolveZKStatePath(*configPath)
	store, err := statestore.OpenStateStore(storePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open state store %s: %v\n", storePath, err)
		return 1
	}
	defer func() { _ = store.Close() }()
	records, err := store.ApprovalsForPipeline(pipeline, 1000)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read approvals: %v\n", err)
		return 1
	}
	record, ok := latestHumanApproval(records)
	if !ok {
		fmt.Fprintf(os.Stderr, "no direct human approval found for pipeline %q\n", pipeline)
		return 1
	}
	fields := approval.Fields{
		Pipeline: record.Pipeline, Step: record.Step, DecidedAt: record.DecidedAt.Unix(),
		Decision: record.Decision, OperatorID: record.OperatorID, PayloadHash: record.PayloadHash,
		QuorumN: record.QuorumN, QuorumGot: record.QuorumGot,
	}
	if record.Signature == "" || !approval.Verify(secret, fields, record.Nonce, record.Signature) {
		fmt.Fprintln(os.Stderr, "latest human approval is unsigned or its receipt was altered; refusing to prove it")
		return 1
	}
	privateRecord := zkreceipt.Record{
		Pipeline: record.Pipeline, Step: record.Step, DecidedAt: record.DecidedAt.Unix(),
		Decision: record.Decision, OperatorID: record.OperatorID, PayloadHash: record.PayloadHash,
		QuorumN: record.QuorumN, QuorumGot: record.QuorumGot, Nonce: record.Nonce,
	}
	bundle, err := zkreceipt.Prove(privateRecord, secret)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create proof: %v\n", err)
		return 1
	}
	encoded, err := zkreceipt.MarshalBundle(bundle)
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode proof: %v\n", err)
		return 1
	}
	encoded = append(encoded, '\n')
	if *outputPath == "" {
		_, _ = os.Stdout.Write(encoded)
	} else if err := os.WriteFile(*outputPath, encoded, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write proof: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "created %d-byte proof in %d ms; approval fields stayed private\n", bundle.ProofBytes, bundle.ProverMS)
	return 0
}

func latestHumanApproval(records []statestore.ApprovalRecord) (statestore.ApprovalRecord, bool) {
	for _, record := range records {
		if record.Decision == "approve" {
			return record, true
		}
	}
	return statestore.ApprovalRecord{}, false
}

func resolveZKStatePath(configPath string) string {
	if value := strings.TrimSpace(os.Getenv("DRAFTCAT_STATE_PATH")); value != "" {
		return value
	}
	// #nosec G304 -- the operator explicitly chooses which local Draftcat config to read.
	if data, err := os.ReadFile(configPath); err == nil {
		var cfg config.Config
		if yaml.Unmarshal(data, &cfg) == nil && strings.TrimSpace(cfg.State.Path) != "" {
			return cfg.State.Path
		}
	}
	return "./state.db"
}

func runZKReceiptVerify(args []string) int {
	flags := flag.NewFlagSet("zk-receipt verify", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	expectedKey := flags.String("expect-key", "", "pinned Draftcat instance-key commitment")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 || *expectedKey == "" {
		fmt.Fprintln(os.Stderr, "usage: draftcat zk-receipt verify --expect-key <commitment> <proof.json>")
		return 2
	}
	// #nosec G304 -- the proof bundle path is the explicit CLI input to verify.
	data, err := os.ReadFile(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "read proof: %v\n", err)
		return 1
	}
	bundle, err := zkreceipt.UnmarshalBundle(data)
	if err == nil {
		err = zkreceipt.Verify(bundle, *expectedKey)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "INVALID: %v\n", err)
		return 1
	}
	fmt.Printf("VALID: a direct human approval met its quorum; private approval fields were not disclosed\n")
	fmt.Printf("receipt commitment: %s\n", bundle.ReceiptCommitment)
	return 0
}
