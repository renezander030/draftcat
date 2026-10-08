package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	statestore "github.com/renezander030/draftcat/internal/state"
)

const doctorConfig = `telegram:
  token_env: DOC_TG_TOKEN
  chat_id: 42
  security:
    allowed_users: [42]
    max_input_length: 500
    rate_limit: 10
provider:
  type: openrouter
  api_key_env: DOC_PROVIDER_KEY
models:
  m:
    model: x/y
roles:
  drafter: m
state:
  path: STATE
pipelines:
  - name: morning
    schedule: "0 8 * * 1-5"
    timezone: Europe/Berlin
    steps:
      - name: fetch
        type: deterministic
`

func writeDoctorConfig(t *testing.T, statePath string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(strings.Replace(doctorConfig, "STATE", statePath, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func checkByName(rep doctorReport, name string) doctorCheck {
	for _, c := range rep.Checks {
		if c.Name == name {
			return c
		}
	}
	return doctorCheck{}
}

func TestDoctorReadyConfig(t *testing.T) {
	t.Setenv("DOC_TG_TOKEN", "123:abc")
	t.Setenv("DOC_PROVIDER_KEY", "sk-test")
	t.Setenv("DRAFTCAT_APPROVAL_SECRET", "approval-secret")
	t.Setenv("DRAFTCAT_STATE_PATH", "")
	statePath := filepath.Join(t.TempDir(), "state.db")
	cfgPath := writeDoctorConfig(t, statePath)
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC) // Friday
	rep := doctor(cfgPath, t.TempDir(), now)
	if !rep.OK {
		t.Fatalf("ready config reported failures: %+v", rep.Checks)
	}
	if c := checkByName(rep, "state store"); c.Status != "ok" || !strings.Contains(c.Detail, "will be created") {
		t.Errorf("state store: %+v", c)
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("doctor created the state store: %v", err)
	}
	if c := checkByName(rep, "schedule morning"); c.Status != "ok" || !strings.Contains(c.Detail, "Mon 12 Oct 2026 08:00 CEST") {
		t.Errorf("schedule: %+v", c)
	}
}

func TestDoctorReportsMissingCredentials(t *testing.T) {
	t.Setenv("DOC_TG_TOKEN", "")
	t.Setenv("DRAFTCAT_TG_TOKEN", "")
	t.Setenv("DOC_PROVIDER_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("DRAFTCAT_APPROVAL_SECRET", "")
	t.Setenv("DRAFTCAT_STATE_PATH", "")
	cfgPath := writeDoctorConfig(t, filepath.Join(t.TempDir(), "state.db"))
	rep := doctor(cfgPath, t.TempDir(), time.Now())
	if rep.OK {
		t.Fatal("missing credentials reported OK")
	}
	for name, want := range map[string]string{"telegram token": "fail", "provider key": "fail", "receipt signing": "warn"} {
		if c := checkByName(rep, name); c.Status != want || c.Hint == "" {
			t.Errorf("%s: %+v, want %s with a hint", name, c, want)
		}
	}
	var out bytes.Buffer
	if code := runDoctor([]string{"--config", cfgPath, "--skills", t.TempDir()}, &out); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "check(s) failed") {
		t.Errorf("summary missing:\n%s", out.String())
	}
}

func TestDoctorInspectsExistingStoreReadOnly(t *testing.T) {
	t.Setenv("DOC_TG_TOKEN", "123:abc")
	t.Setenv("DOC_PROVIDER_KEY", "sk-test")
	t.Setenv("DRAFTCAT_STATE_PATH", "")
	statePath := filepath.Join(t.TempDir(), "state.db")
	st, err := statestore.OpenStateStore(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddBudgetUsage(t.Context(), time.Now(), 120, 0.25, 0, 0); err != nil {
		t.Fatal(err)
	}
	st.Close()
	before, _ := os.Stat(statePath)
	rep := doctor(writeDoctorConfig(t, statePath), t.TempDir(), time.Now())
	if c := checkByName(rep, "state store"); c.Status != "ok" || !strings.Contains(c.Detail, "120 tokens") {
		t.Errorf("state store: %+v", c)
	}
	after, _ := os.Stat(statePath)
	if !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() {
		t.Error("doctor modified the state store")
	}
}

func TestDoctorJSONAndBadConfig(t *testing.T) {
	var out bytes.Buffer
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	if code := runDoctor([]string{"--config", missing, "--json"}, &out); code != 1 {
		t.Fatalf("exit = %d", code)
	}
	var rep doctorReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	if rep.OK || len(rep.Checks) != 1 || rep.Checks[0].Name != "config" || rep.Checks[0].Status != "fail" {
		t.Fatalf("report: %+v", rep)
	}
}

func TestDoctorFlagsBusyPort(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	rep := doctorReport{}
	checkListen(&rep, "webhook", ln.Addr().String())
	if rep.Checks[0].Status != "warn" {
		t.Errorf("busy port: %+v", rep.Checks[0])
	}
}
