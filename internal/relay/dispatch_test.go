package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Dispatch is the engine-to-relay wire step: a signed hitl/v0 request, and
// the relay's status passed back. These assert on what the relay sees.
func TestDispatchSignsAndReportsStatus(t *testing.T) {
	secret := []byte("s3cret")
	var got Request
	code := http.StatusAccepted
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Type") != "application/json" || r.Header.Get(ProtocolHeader) != Version {
			t.Errorf("headers %v", r.Header)
		}
		if err := VerifySignature(r.Header.Get(SigHeader), body, secret, 60, time.Now()); err != nil {
			t.Errorf("signature: %v", err)
		}
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(code)
	}))
	defer srv.Close()

	status, err := Dispatch(context.Background(), srv.Client(), srv.URL, secret, Request{Protocol: Version, ApprovalID: "a1"})
	if err != nil || status != http.StatusAccepted || got.ApprovalID != "a1" {
		t.Fatalf("status=%d err=%v got=%+v", status, err, got)
	}
	code = http.StatusTeapot
	status, err = Dispatch(context.Background(), srv.Client(), srv.URL, secret, Request{})
	if status != http.StatusTeapot || err == nil || !strings.Contains(err.Error(), "relay returned 418") {
		t.Fatalf("non-2xx: status=%d err=%v", status, err)
	}
	if status, err = Dispatch(context.Background(), srv.Client(), "://bad", secret, Request{}); status != 0 || err == nil {
		t.Fatalf("bad url: status=%d err=%v", status, err)
	}
}
