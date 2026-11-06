package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/renezander030/draftcat/internal/config"
	"github.com/renezander030/draftcat/internal/validate"
)

// countFindings is the verdict the doctor and a reload report: errors,
// warnings and the first error.
func countFindings(t *testing.T, body, skills string) (errs, warns int, first string) {
	t.Helper()
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}
	for _, f := range validate.CheckAtStartup(&cfg, skills) {
		if f.Level != "error" {
			warns++
			continue
		}
		if errs == 0 {
			first = f.Path + ": " + f.Message
		}
		errs++
	}
	return errs, warns, first
}

func TestDoctorAndReloadReportTheStartupVerdict(t *testing.T) {
	t.Setenv("DOC_TG_TOKEN", "123:abc")
	t.Setenv("DOC_PROVIDER_KEY", "sk-test")
	t.Setenv("DRAFTCAT_STATE_PATH", "")
	skills := t.TempDir()
	bad := strings.Replace(strings.Replace(doctorConfig, "STATE", filepath.Join(t.TempDir(), "state.db"), 1),
		"type: deterministic", "type: deterministic\n      - name: second\n        type: nonsense", 1)
	errs, warns, first := countFindings(t, bad, skills)
	if errs < 1 || warns < 1 {
		t.Fatalf("fixture gives %d error(s), %d warning(s)", errs, warns)
	}
	cfgPath := writeReloadConfig(t, t.TempDir(), bad)

	c := checkByName(doctor(cfgPath, skills, time.Now(), false), "validate")
	if want := fmt.Sprintf("%d error(s), %d warning(s); first: %s", errs, warns, first); c.Status != "fail" || c.Detail != want {
		t.Fatalf("doctor validate = %+v, want fail %q", c, want)
	}

	// A reload validates the running snapshot with the file's run sections, so
	// its warnings are its own; the errors here come from the file's pipelines.
	running, boot := bootFrom(t, reloadBase)
	_, _, res, err := buildReload(cfgPath, skills, running, boot)
	if want := fmt.Sprintf("%d error(s); first: %s", errs, first); err == nil || err.Error() != want || res.Warnings == 0 {
		t.Fatalf("reload err = %v, warnings %d; want %q and the warnings counted", err, res.Warnings, want)
	}

	good := strings.Replace(doctorConfig, "STATE", filepath.Join(t.TempDir(), "state.db"), 1)
	_, warns, _ = countFindings(t, good, skills)
	c = checkByName(doctor(writeReloadConfig(t, t.TempDir(), good), skills, time.Now(), false), "validate")
	if want := fmt.Sprintf("%d warning(s)", warns); warns == 0 || c.Status != "warn" || c.Detail != want {
		t.Fatalf("doctor validate = %+v, want warn %q", c, want)
	}
}
