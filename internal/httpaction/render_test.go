package httpaction

import "testing"

// Render is how a step's request is built; its digest is what an operator's
// approval binds to, so the value is pinned (computed before Request existed).
func TestRenderDigestPinned(t *testing.T) {
	s, ferrs := Parse(map[string]string{"url": "https://api.example.com/v1/x", "method": "POST", "body": "{{ai_output}}",
		"auth_env": "TOK", "auth_scheme": "Bearer", "header.X-Trace": "abc"})
	if len(ferrs) > 0 {
		t.Fatal(ferrs)
	}
	r, err := Render(s, "run-9", "send", map[string]interface{}{"ai_output": map[string]interface{}{"b": 2, "a": "é<"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Digest(); got != "7fe31b676330150bb558772d5c57087aca1832f2787447b7a3469f461d926180" {
		t.Fatalf("digest drifted: %s", got)
	}
	if r.Key != IdempotencyKey("run-9", "send") {
		t.Fatalf("key %q", r.Key)
	}
	if _, err := Render(s, "run-9", "send", map[string]interface{}{}); err == nil {
		t.Fatal("missing ai_output should fail to render")
	}
}
