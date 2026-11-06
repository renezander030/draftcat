package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestConformanceWithDraftcat runs the real `draftcat hitl verify` against this
// relay, with a local stand-in for the Slack Web API and a signed Slack click
// standing in for the human. It builds draftcat from the enclosing repository,
// or uses the binary named by DRAFTCAT_BIN.
func TestConformanceWithDraftcat(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs draftcat; skipped in -short")
	}
	bin := draftcatBinary(t)

	const identity = "conformance@example.com"
	h := newHarnessWith(t, map[string]string{"U0APPROVER": identity})

	addr := freeAddr(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "hitl", "verify", h.relay.URL+"/dispatch",
		"--addr", addr, "--identity", identity, "--timeout", "60s", "--json")
	cmd.Env = append(os.Environ(), "DRAFTCAT_RELAY_SECRET="+testRelaySecret)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-h.slack.posted:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("the probe never reached Slack\nstderr: %s", stderr.String())
	}
	id := buttonValues(blocksOf(h.slack.byMethod("chat.postMessage")[0]))[actionPrefix+verbApprove]
	if id == "" {
		t.Fatal("the probe message has no approve button")
	}
	// A human takes seconds to click; this test takes milliseconds. Wait until
	// the suite's callback listener is up so the decision has somewhere to go.
	waitListening(t, addr)
	h.click("U0APPROVER", verbApprove, id, "")

	err := cmd.Wait()
	var report struct {
		Conformant bool `json:"conformant"`
		Checks     []struct {
			Check string `json:"check"`
			OK    bool   `json:"ok"`
			Note  string `json:"note"`
		} `json:"checks"`
	}
	if jerr := json.Unmarshal(stdout.Bytes(), &report); jerr != nil {
		t.Fatalf("unreadable report (%v): %s\nstderr: %s", jerr, stdout.String(), stderr.String())
	}
	for _, c := range report.Checks {
		t.Logf("%-5v %s %s", c.OK, c.Check, c.Note)
	}
	if err != nil || !report.Conformant {
		t.Fatalf("not conformant (exit: %v)", err)
	}
}

func draftcatBinary(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("DRAFTCAT_BIN"); bin != "" {
		return bin
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil || !strings.HasPrefix(string(mod), "module github.com/renezander030/draftcat\n") {
		t.Skip("not inside the draftcat repository and DRAFTCAT_BIN is not set")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH and DRAFTCAT_BIN is not set")
	}
	bin := filepath.Join(t.TempDir(), "draftcat")
	build := exec.CommandContext(context.Background(), goBin, "build", "-o", bin, ".")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building draftcat: %v\n%s", err, out)
	}
	return bin
}

func waitListening(t *testing.T, addr string) {
	t.Helper()
	d := net.Dialer{Timeout: time.Second}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if c, err := d.DialContext(context.Background(), "tcp", addr); err == nil {
			_ = c.Close()
			return
		}
	}
	t.Fatalf("draftcat's callback listener on %s never came up", addr)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return "127.0.0.1:" + strconv.Itoa(port)
}
