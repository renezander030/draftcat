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
