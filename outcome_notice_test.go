package main

import (
	"strings"
	"testing"

	"github.com/renezander030/draftcat/internal/config"
)

func runToCompletion(t *testing.T, g *toolGate, id, status string) {
	t.Helper()
	_, allowed := postGate(t, g, `{"action_id":"`+id+`","tool":"send","agent":"harness-1"}`)
	if code, _ := consumeGate(t, g, id, allowed.BindingHash); code != 200 {
		t.Fatalf("consume %s: %d", id, code)
	}
	hash := "sha256:" + strings.Repeat("b", 64)
	for i := 0; i < 2; i++ { // the retry must not notify twice
		if code, _ := lifecyclePost(t, g, id, "complete", completeBody(allowed.BindingHash, status, hash)); code != 200 {
			t.Fatalf("complete %s: %d", id, code)
		}
	}
}

func TestOutcomeNoticesFollowNotifyOutcomes(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		notices int // after one success and one failure
	}{{"", 0}, {"off", 0}, {"failed", 1}, {"all", 2}} {
		t.Run("mode="+tc.mode, func(t *testing.T) {
			lifecycleHTTPStore(t)
			cfg := gateCfg(config.ToolRule{Name: "send"})
			cfg.ToolGate.NotifyOutcomes = tc.mode
			ch := &countingChannel{}
			g := newToolGate(cfg, ch)
			runToCompletion(t, g, "ok-1", "succeeded")
			runToCompletion(t, g, "bad-1", "failed")
			if len(ch.notices) != tc.notices {
				t.Fatalf("notices: %q", ch.notices)
			}
			if tc.notices > 0 {
				last := ch.notices[len(ch.notices)-1]
				if !strings.Contains(last, "[tool-gate] send FAILED (agent: harness-1)") || !strings.Contains(last, "action: bad-1") {
					t.Fatalf("failure notice: %q", last)
				}
			}
		})
	}
}
