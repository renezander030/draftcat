package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// testExpectation is fixtures/<pipeline>/_expect.json: what a dry run must
// end with. Every field is optional; an absent file asserts nothing.
type testExpectation struct {
	// Outcome is "completed" (the last step ran) or "skipped" (an operator
	// skip ended the pipeline).
	Outcome string `json:"outcome"`
	// StoppedAt names the approval step a skip must stop at.
	StoppedAt string `json:"stopped_at"`
	// Data maps a dotted path into the final data map (ai_output.category)
	// to the value it must hold, compared as JSON.
	Data map[string]json.RawMessage `json:"data"`
	// Absent lists paths that must not be set.
	Absent []string `json:"absent"`
	// DraftsContain lists text that must appear in some approval draft.
	DraftsContain []string `json:"drafts_contain"`
	// SentContain lists text that must appear in some operator message.
	SentContain []string `json:"sent_contain"`
}

func loadExpectation(fixDir string) (testExpectation, bool, error) {
	var e testExpectation
	path := filepath.Join(fixDir, "_expect.json")
	// #nosec G304 -- a fixture under the operator's own fixtures directory.
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return e, false, nil
	}
	if err != nil {
		return e, false, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return e, false, fmt.Errorf("%s: %w", path, err)
	}
	if e.Outcome != "" && e.Outcome != "completed" && e.Outcome != "skipped" {
		return e, false, fmt.Errorf("%s: outcome must be completed or skipped (got %q)", path, e.Outcome)
	}
	return e, true, nil
}

// check returns one line per unmet expectation, in a stable order.
func (e testExpectation) check(run testRun, ch *stubChannel) []string {
	var problems []string
	outcome := "completed"
	if run.stoppedAt != "" {
		outcome = "skipped"
	}
	if e.Outcome != "" && e.Outcome != outcome {
		problems = append(problems, fmt.Sprintf("outcome: %s, expected %s", outcome, e.Outcome))
	}
	if e.StoppedAt != "" && e.StoppedAt != run.stoppedAt {
		problems = append(problems, fmt.Sprintf("stopped_at: %q, expected %q", run.stoppedAt, e.StoppedAt))
	}
	paths := make([]string, 0, len(e.Data))
	for p := range e.Data {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		got, ok := lookupPath(run.data, p)
		if !ok {
			problems = append(problems, fmt.Sprintf("data.%s: not set, expected %s", p, compactJSON(e.Data[p])))
			continue
		}
		gotJSON, _ := json.Marshal(got)
		if !jsonEqual(gotJSON, e.Data[p]) {
			problems = append(problems, fmt.Sprintf("data.%s: %s, expected %s", p, gotJSON, compactJSON(e.Data[p])))
		}
	}
	for _, p := range e.Absent {
		if got, ok := lookupPath(run.data, p); ok {
			b, _ := json.Marshal(got)
			problems = append(problems, fmt.Sprintf("data.%s: %s, expected it absent", p, b))
		}
	}
	for _, want := range e.DraftsContain {
		if !anyContains(ch.drafts, want) {
			problems = append(problems, fmt.Sprintf("drafts_contain: no approval draft contains %q", want))
		}
	}
	for _, want := range e.SentContain {
		if !anyContains(ch.sent, want) {
			problems = append(problems, fmt.Sprintf("sent_contain: no operator message contains %q", want))
		}
	}
	return problems
}

// lookupPath walks a dotted path through nested maps. A key that itself
// contains a dot is matched whole before the path is split further.
func lookupPath(data map[string]interface{}, path string) (interface{}, bool) {
	if v, ok := data[path]; ok {
		return v, true
	}
	for i := len(path) - 1; i > 0; i-- {
		if path[i] != '.' {
			continue
		}
		head, rest := path[:i], path[i+1:]
		if v, ok := data[head]; ok {
			if m, ok := v.(map[string]interface{}); ok {
				return lookupPath(m, rest)
			}
		}
	}
	return nil, false
}

func jsonEqual(a, b []byte) bool {
	var x, y interface{}
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ca, _ := json.Marshal(x)
	cb, _ := json.Marshal(y)
	return bytes.Equal(ca, cb)
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}
	return buf.String()
}

func anyContains(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}
