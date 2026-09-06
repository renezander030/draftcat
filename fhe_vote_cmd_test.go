package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestFHEVoteCommandEndToEnd(t *testing.T) {
	directory := t.TempDir()
	secretPath := filepath.Join(directory, "campaign.fhe-secret.json")
	publicPath := filepath.Join(directory, "campaign.fhe-public.json")
	tallyPath := filepath.Join(directory, "tally.json")
	context := "invoice-4821"

	if code := runFHEVoteKeygen([]string{"--secret", secretPath, "--public", publicPath}); code != 0 {
		t.Fatalf("keygen exited %d", code)
	}
	for i, vote := range []string{"approve", "reject", "approve"} {
		ballotPath := filepath.Join(directory, fmt.Sprintf("ballot-%d.json", i))
		if code := runFHEVoteEncrypt([]string{
			"--public", publicPath,
			"--context", context,
			"--ballot", fmt.Sprintf("random-invite-%d", i),
			"--vote", vote,
			"--out", ballotPath,
		}); code != 0 {
			t.Fatalf("encrypt %d exited %d", i, code)
		}
	}
	if code := runFHEVoteTally([]string{
		"--public", publicPath,
		"--context", context,
		"--out", tallyPath,
		filepath.Join(directory, "ballot-0.json"),
		filepath.Join(directory, "ballot-1.json"),
		filepath.Join(directory, "ballot-2.json"),
	}); code != 0 {
		t.Fatalf("tally exited %d", code)
	}
	if code := runFHEVoteDecrypt([]string{
		"--secret", secretPath, "--context", context, "--expected", "3", "--quorum", "2", tallyPath,
	}); code != 0 {
		t.Fatalf("decrypt exited %d", code)
	}
	if code := runFHEVoteDecrypt([]string{
		"--secret", secretPath, "--context", context, "--expected", "4", "--quorum", "2", tallyPath,
	}); code != 1 {
		t.Fatalf("partial tally exited %d, want 1", code)
	}

	for _, path := range []string{secretPath, filepath.Join(directory, "ballot-0.json"), tallyPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode = %o, want 600", path, got)
		}
	}
}
