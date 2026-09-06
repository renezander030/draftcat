package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/renezander030/draftcat/internal/fhevote"
)

func runFHEVoteCmd(args []string) int {
	if len(args) == 0 {
		printFHEVoteUsage()
		return 2
	}
	switch args[0] {
	case "keygen":
		return runFHEVoteKeygen(args[1:])
	case "encrypt":
		return runFHEVoteEncrypt(args[1:])
	case "tally":
		return runFHEVoteTally(args[1:])
	case "decrypt":
		return runFHEVoteDecrypt(args[1:])
	case "-h", "--help", "help":
		printFHEVoteUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown fhe-vote command %q\n", args[0])
		printFHEVoteUsage()
		return 2
	}
}

func printFHEVoteUsage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  draftcat fhe-vote keygen [--secret fhe-secret.json] [--public fhe-public.json]")
	fmt.Fprintln(os.Stderr, "  draftcat fhe-vote encrypt --public fhe-public.json --context <value> --ballot <unique-token> --vote approve|reject --out vote.json")
	fmt.Fprintln(os.Stderr, "  draftcat fhe-vote tally --public fhe-public.json --context <value> --out tally.json <vote.json>...")
	fmt.Fprintln(os.Stderr, "  draftcat fhe-vote decrypt --secret fhe-secret.json --context <value> --expected <n> --quorum <n> <tally.json>")
}

func runFHEVoteKeygen(args []string) int {
	flags := flag.NewFlagSet("fhe-vote keygen", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	secretPath := flags.String("secret", "fhe-secret.json", "secret decryption key path")
	publicPath := flags.String("public", "fhe-public.json", "shareable encryption key path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *secretPath == *publicPath {
		fmt.Fprintln(os.Stderr, "keygen requires different --secret and --public paths and no positional arguments")
		return 2
	}
	secret, public, err := fhevote.GenerateKeys()
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate FHE keys: %v\n", err)
		return 1
	}
	secretJSON, err := fhevote.Marshal(secret)
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode secret key: %v\n", err)
		return 1
	}
	publicJSON, err := fhevote.Marshal(public)
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode public key: %v\n", err)
		return 1
	}
	if err := writeExclusive(*secretPath, append(secretJSON, '\n'), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write secret key: %v\n", err)
		return 1
	}
	if err := writeExclusive(*publicPath, append(publicJSON, '\n'), 0o644); err != nil {
		_ = os.Remove(*secretPath)
		fmt.Fprintf(os.Stderr, "write public key: %v\n", err)
		return 1
	}
	fmt.Printf("created encrypted-vote keys; share %s and keep %s private\n", *publicPath, *secretPath)
	fmt.Printf("key ID: %s\n", public.KeyID)
	return 0
}

func runFHEVoteEncrypt(args []string) int {
	flags := flag.NewFlagSet("fhe-vote encrypt", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	publicPath := flags.String("public", "", "shareable encryption key path")
	context := flags.String("context", "", "workflow/action context; only its hash is stored")
	ballot := flags.String("ballot", "", "unique random ballot token; only its hash is stored")
	vote := flags.String("vote", "", "approve or reject")
	outputPath := flags.String("out", "", "encrypted ballot path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *publicPath == "" || *context == "" || *ballot == "" || *outputPath == "" || (*vote != "approve" && *vote != "reject") {
		fmt.Fprintln(os.Stderr, "usage: draftcat fhe-vote encrypt --public <key.json> --context <value> --ballot <unique-token> --vote approve|reject --out <vote.json>")
		return 2
	}
	public, err := readPublicKey(*publicPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	encrypted, err := fhevote.EncryptVote(public, *context, *ballot, *vote == "approve")
	if err != nil {
		fmt.Fprintf(os.Stderr, "encrypt vote: %v\n", err)
		return 1
	}
	encoded, err := fhevote.Marshal(encrypted)
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode vote: %v\n", err)
		return 1
	}
	if err := writeExclusive(*outputPath, append(encoded, '\n'), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write encrypted vote: %v\n", err)
		return 1
	}
	fmt.Printf("encrypted one vote to %s; the bundle contains no readable approve/reject value\n", *outputPath)
	return 0
}

func runFHEVoteTally(args []string) int {
	flags := flag.NewFlagSet("fhe-vote tally", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	publicPath := flags.String("public", "", "pinned encryption key path")
	context := flags.String("context", "", "workflow/action context")
	outputPath := flags.String("out", "", "encrypted tally path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *publicPath == "" || *context == "" || *outputPath == "" || flags.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: draftcat fhe-vote tally --public <key.json> --context <value> --out <tally.json> <vote.json>...")
		return 2
	}
	public, err := readPublicKey(*publicPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ballots := make([]fhevote.Ballot, 0, flags.NArg())
	for _, path := range flags.Args() {
		data, err := os.ReadFile(path) // #nosec G304 -- explicit CLI input.
		if err != nil {
			fmt.Fprintf(os.Stderr, "read encrypted ballot %s: %v\n", path, err)
			return 1
		}
		ballot, err := fhevote.ParseBallot(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "parse encrypted ballot %s: %v\n", path, err)
			return 1
		}
		ballots = append(ballots, ballot)
	}
	tally, err := fhevote.Aggregate(public, *context, ballots)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tally encrypted votes: %v\n", err)
		return 1
	}
	encoded, err := fhevote.Marshal(tally)
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode tally: %v\n", err)
		return 1
	}
	if err := writeExclusive(*outputPath, append(encoded, '\n'), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write encrypted tally: %v\n", err)
		return 1
	}
	fmt.Printf("combined %d encrypted votes into %s without decrypting any vote\n", tally.Ballots, *outputPath)
	return 0
}

func runFHEVoteDecrypt(args []string) int {
	flags := flag.NewFlagSet("fhe-vote decrypt", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	secretPath := flags.String("secret", "", "secret decryption key path")
	context := flags.String("context", "", "workflow/action context")
	expected := flags.Int("expected", 0, "exact number of eligible ballots expected")
	quorum := flags.Int("quorum", 0, "approvals required")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *secretPath == "" || *context == "" || *expected < 3 || *quorum < 1 || *quorum > *expected || flags.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: draftcat fhe-vote decrypt --secret <key.json> --context <value> --expected <n>=3+ --quorum <n> <tally.json>")
		return 2
	}
	secretData, err := os.ReadFile(*secretPath) // #nosec G304 -- explicit CLI input.
	if err != nil {
		fmt.Fprintf(os.Stderr, "read secret key: %v\n", err)
		return 1
	}
	secret, err := fhevote.ParseSecretKey(secretData)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse secret key: %v\n", err)
		return 1
	}
	tallyData, err := os.ReadFile(flags.Arg(0)) // #nosec G304 -- explicit CLI input.
	if err != nil {
		fmt.Fprintf(os.Stderr, "read tally: %v\n", err)
		return 1
	}
	tally, err := fhevote.ParseTally(tallyData)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse tally: %v\n", err)
		return 1
	}
	result, err := fhevote.Decrypt(secret, *context, tally)
	if err != nil {
		fmt.Fprintf(os.Stderr, "decrypt tally: %v\n", err)
		return 1
	}
	if result.Ballots != *expected {
		fmt.Fprintf(os.Stderr, "expected %d eligible ballots but the tally contains %d; refusing the result\n", *expected, result.Ballots)
		return 1
	}
	if result.Approvals >= *quorum {
		fmt.Printf("QUORUM MET: %d of %d encrypted votes approved (required %d)\n", result.Approvals, result.Ballots, *quorum)
		return 0
	}
	fmt.Printf("QUORUM NOT MET: %d of %d encrypted votes approved (required %d)\n", result.Approvals, result.Ballots, *quorum)
	return 3
}

func readPublicKey(path string) (fhevote.PublicKeyFile, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- explicit CLI input.
	if err != nil {
		return fhevote.PublicKeyFile{}, fmt.Errorf("read public key: %w", err)
	}
	public, err := fhevote.ParsePublicKey(data)
	if err != nil {
		return fhevote.PublicKeyFile{}, fmt.Errorf("parse public key: %w", err)
	}
	return public, nil
}

func writeExclusive(path string, data []byte, mode os.FileMode) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("output path must not be empty")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode) // #nosec G304 -- explicit CLI output.
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
