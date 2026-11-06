package main

import (
	"context"
	"net"
	"testing"
	"time"
)

// A callback port that cannot be bound fails the suite at once, rather than
// dispatching and then waiting out the whole timeout for a decision that has
// nowhere to land.
func TestHitlVerifyFailsFastWhenCallbackPortIsTaken(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind a local port: %v", err)
	}
	defer func() { _ = ln.Close() }()
	t.Setenv("DRAFTCAT_RELAY_SECRET", "test-secret")

	start := time.Now()
	code := runHitlCmd([]string{"verify", "http://127.0.0.1:1/dispatch", "--addr", ln.Addr().String(), "--timeout", "60s", "--json"})
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("took %s; a taken port must fail before the dispatch", took)
	}
}
