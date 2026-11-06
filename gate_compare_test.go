package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

const compareConfigOld = `tool_gate:
  enabled: true
  tools:
    - name: create_invoice
      on_mismatch: deny
      args:
        amount: {max: 5000}
    - name: read_calendar
`

const compareConfigTight = `tool_gate:
  enabled: true
  tools:
    - name: create_invoice
      on_mismatch: deny
      args:
        amount: {max: 500}
    - name: read_calendar
`

const compareCases = `cases:
  - name: small invoice
    tool: create_invoice
    args: {amount: 100}
    expect: allow
  - name: mid invoice
    tool: create_invoice
    args: {amount: 800}
    expect: deny
  - name: calendar
    tool: read_calendar
    expect: allow
`

func compareFixture(t *testing.T) (dir, cases string) {
	t.Helper()
	dir = t.TempDir()
	cases = filepath.Join(dir, "cases.yaml")
	writeFile(t, cases, compareCases)
	writeFile(t, filepath.Join(dir, "old.yaml"), compareConfigOld)
	writeFile(t, filepath.Join(dir, "same.yaml"), compareConfigOld)
	writeFile(t, filepath.Join(dir, "tight.yaml"), compareConfigTight)
	return dir, cases
}

func TestGateTestCompareIdenticalConfigs(t *testing.T) {
	dir, cases := compareFixture(t)
	var out, errOut bytes.Buffer
	code := gateCmd([]string{"test", cases, "--config", filepath.Join(dir, "same.yaml"), "--compare", filepath.Join(dir, "old.yaml")}, &out, &errOut)
	if code != 0 || strings.TrimSpace(out.String()) != "0 of 3 cases decide differently" {
		t.Fatalf("exit %d, out %q, err %q", code, out.String(), errOut.String())
	}
}

// The cases file's expect: is not checked in compare mode, so a case that
// would fail a plain run does not fail the comparison.
func TestGateTestCompareTightenedCeiling(t *testing.T) {
	dir, cases := compareFixture(t)
	var out, errOut bytes.Buffer
	code := gateCmd([]string{"test", cases, "--config", filepath.Join(dir, "tight.yaml"), "--compare", filepath.Join(dir, "old.yaml")}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit %d, out %q, err %q", code, out.String(), errOut.String())
	}
	want := "mid invoice: allow -> deny (args.amount: 800 exceeds max 500)\n1 of 3 cases decide differently\n"
	if out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
}

func TestGateTestCompareJSON(t *testing.T) {
	dir, cases := compareFixture(t)
	var out, errOut bytes.Buffer
	code := gateCmd([]string{"test", cases, "--config", filepath.Join(dir, "tight.yaml"), "--compare", filepath.Join(dir, "old.yaml"), "--json"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var diffs []gateDiff
	if err := json.Unmarshal(out.Bytes(), &diffs); err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || diffs[0].Name != "mid invoice" || diffs[0].Tool != "create_invoice" ||
		diffs[0].Old.Decision != "allow" || diffs[0].New.Decision != "deny" || diffs[0].New.Rule != "args.amount: 800 exceeds max 500" {
		t.Fatalf("diffs: %+v", diffs)
	}
	out.Reset()
	if code := gateCmd([]string{"test", cases, "--config", filepath.Join(dir, "same.yaml"), "--compare", filepath.Join(dir, "old.yaml"), "--json"}, &out, &errOut); code != 0 || strings.TrimSpace(out.String()) != "[]" {
		t.Fatalf("no differences: exit %d, out %q", code, out.String())
	}
}

func TestGateCompareBelongsToTest(t *testing.T) {
	dir, _ := compareFixture(t)
	var out, errOut bytes.Buffer
	if code := gateCmd([]string{"check", "read_calendar", "--config", filepath.Join(dir, "old.yaml"), "--compare", filepath.Join(dir, "old.yaml")}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d", code)
	}
}

// A cases file is read the same way with and without --compare: an unnamed
// case is called "case N" by its position, and an unusable file stops the
// command before any case is decided.
func TestGateTestCasesFile(t *testing.T) {
	dir, _ := compareFixture(t)
	unnamed := filepath.Join(dir, "unnamed.yaml")
	writeFile(t, unnamed, "cases:\n  - tool: read_calendar\n    expect: allow\n  - tool: create_invoice\n    args: {amount: 800}\n    expect: allow\n")
	var out, errOut bytes.Buffer
	if code := gateCmd([]string{"test", unnamed, "--config", filepath.Join(dir, "tight.yaml")}, &out, &errOut); code != 1 ||
		!strings.Contains(out.String(), "PASS  case 1: ALLOW  read_calendar") || !strings.Contains(out.String(), "FAIL  case 2: DENY  create_invoice") ||
		!strings.Contains(out.String(), "2 case(s), 1 failed") {
		t.Fatalf("test exit %d, out %q, err %q", code, out.String(), errOut.String())
	}
	out.Reset()
	if code := gateCmd([]string{"test", unnamed, "--config", filepath.Join(dir, "tight.yaml"), "--compare", filepath.Join(dir, "old.yaml")}, &out, &errOut); code != 1 ||
		!strings.Contains(out.String(), "case 2: allow -> deny") {
		t.Fatalf("compare exit %d, out %q", code, out.String())
	}

	empty := filepath.Join(dir, "empty.yaml")
	writeFile(t, empty, "cases: []\n")
	unknown := filepath.Join(dir, "unknown.yaml")
	writeFile(t, unknown, "cases:\n  - tool: read_calendar\n    expect: allow\n    surprise: 1\n")
	badExpect := filepath.Join(dir, "bad-expect.yaml")
	writeFile(t, badExpect, "cases:\n  - tool: read_calendar\n    expect: maybe\n")
	for _, tc := range []struct {
		file string
		code int
		err  string
	}{
		{filepath.Join(dir, "absent.yaml"), 1, "gate: open "},
		{empty, 2, "gate: " + empty + " has no cases"},
		{unknown, 2, "gate: " + unknown + ": "},
		{badExpect, 2, `gate: case 1: expect must be allow, deny or ask (got "maybe")`},
	} {
		for _, compare := range []bool{false, true} {
			args := []string{"test", tc.file, "--config", filepath.Join(dir, "tight.yaml")}
			if compare {
				args = append(args, "--compare", filepath.Join(dir, "old.yaml"))
				if tc.file == badExpect {
					continue // expect is not checked under --compare
				}
			}
			out.Reset()
			errOut.Reset()
			if code := gateCmd(args, &out, &errOut); code != tc.code || !strings.HasPrefix(errOut.String(), tc.err) || out.Len() != 0 {
				t.Fatalf("%v: exit %d, out %q, err %q; want %d %q", args, code, out.String(), errOut.String(), tc.code, tc.err)
			}
		}
	}
}
