package main

import (
	"os"
	"path/filepath"
	"testing"
)

// copyFixtures copies the shipped test-screener fixtures into a temp fixtures
// root and writes the given _expect.json (none when empty).
func copyFixtures(t *testing.T, expect string, extra map[string]string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "test-screener")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"mock-input.json", "classify.json", "review.json"} {
		b, err := os.ReadFile(filepath.Join("fixtures", "test-screener", f))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, f), string(b))
	}
	if expect != "" {
		writeFile(t, filepath.Join(dir, "_expect.json"), expect)
	}
	for name, body := range extra {
		writeFile(t, filepath.Join(dir, name), body)
	}
	return root
}

func runScreener(t *testing.T, fixtures string, extra ...string) int {
	t.Helper()
	args := append([]string{"test-screener", "--config", "config.yaml", "--skills", "skills", "--fixtures", fixtures}, extra...)
	return runTestCmd(args)
}

func TestFixtureExpectationsPass(t *testing.T) {
	if code := runScreener(t, "fixtures"); code != 0 {
		t.Fatalf("the shipped example expectation must hold, exit %d", code)
	}
	if code := runScreener(t, copyFixtures(t, "", nil)); code != 0 {
		t.Fatalf("no _expect.json asserts nothing, exit %d", code)
	}
}

func TestFixtureExpectationsCatchDrift(t *testing.T) {
	cases := map[string]string{
		"value":   `{"data": {"ai_output.score": 5}}`,
		"missing": `{"data": {"ai_output.category": "lead"}}`,
		"absent":  `{"absent": ["approved"]}`,
		"outcome": `{"outcome": "skipped"}`,
		"draft":   `{"drafts_contain": ["Weak fit"]}`,
	}
	for name, expect := range cases {
		if code := runScreener(t, copyFixtures(t, expect, nil)); code != 1 {
			t.Errorf("%s: an unmet expectation must exit 1, got %d", name, code)
		}
	}
}

func TestFixtureExpectationsSkipOutcome(t *testing.T) {
	root := copyFixtures(t, `{"outcome": "skipped", "stopped_at": "review", "absent": ["approved"]}`,
		map[string]string{"review.json": `{"action": "skip"}`})
	if code := runScreener(t, root); code != 0 {
		t.Fatalf("a skip that stops at review meets the expectation, exit %d", code)
	}
}

func TestFixtureExpectationsRejectUnknownKeys(t *testing.T) {
	if code := runScreener(t, copyFixtures(t, `{"outcomes": "completed"}`, nil)); code != 1 {
		t.Fatalf("a typo'd key must not silently assert nothing, exit %d", code)
	}
}
