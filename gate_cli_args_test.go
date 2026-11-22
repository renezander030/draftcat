package main

import (
	"bytes"
	"testing"
)

// The gate CLI's argument errors are its contract with scripts: exact stderr
// and exit code 2, checked before any config is read.
func TestGateArgErrorsExact(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"check", "x", "--config"}, "gate: --config requires a value\n"},
		{[]string{"check", "x", "-config"}, "gate: -config requires a value\n"},
		{[]string{"check", "--tool"}, "gate: --tool requires a value\n"},
		{[]string{"check", "x", "--bogus"}, "gate: unknown option \"--bogus\"\n"},
		{[]string{"check", "x", "--spent", "nope"}, "gate: --spent wants arg=N (got \"nope\")\n"},
		{[]string{"check", "x", "--spent", " =3"}, "gate: --spent wants arg=N (got \" =3\")\n"},
		{[]string{"check", "x", "--calls", "-1"}, "gate: --calls wants a count of 0 or more (got \"-1\")\n"},
		{[]string{"status", "--max-age", "soon"}, "gate: --max-age wants a duration like 5m (got \"soon\")\n"},
		{[]string{"unfreeze", "--agent", "  "}, "gate: --agent wants an agent name\n"},
		{[]string{"shadow", "x"}, "gate: shadow takes only --since, --json and --config\n"},
		{[]string{"check", "x", "--since", "1h"}, "gate: --since belongs to shadow\n"},
		{[]string{"status", "--reason", "r"}, "gate: --reason belongs to freeze\n"},
		{[]string{"freeze", "--json"}, "gate: --json belongs to status\n"},
		{[]string{"check", "x", "--agent", "a"}, "gate: --agent belongs to unfreeze\n"},
		{[]string{"test", "x", "--definition", "d.json"}, "gate: --definition belongs to check; test reads definition: from the file\n"},
		{[]string{"check", "x", "--compare", "o.yaml"}, "gate: --compare belongs to test\n"},
		{[]string{"diff-definition", "a"}, "gate: diff-definition wants two files: <pinned.json> <current.json>\n"},
		{[]string{"check", "x", "--json", "--calls", "2", "--bogus", "--calls"}, "gate: unknown option \"--bogus\"\n"},
	} {
		var out, errb bytes.Buffer
		if code := gateCmd(c.args, &out, &errb); code != 2 || errb.String() != c.want {
			t.Errorf("%q: code=%d stderr=%q, want 2 %q", c.args, code, errb.String(), c.want)
		}
	}
}
