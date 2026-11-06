package validate

import (
	"os"
	"strings"

	"github.com/renezander030/draftcat/internal/config"
	"github.com/renezander030/draftcat/internal/httpaction"
)

// checkHTTPRequestStep validates one http_request step at index si. The
// action is the outbound side of an approval: an approval step must come
// before it, after any earlier http_request, or the engine never sends it.
func checkHTTPRequestStep(steps []config.StepConfig, si int, spath string, rep *validateReport) {
	st := steps[si]
	spec, errs := httpaction.Parse(st.Vars)
	for _, e := range errs {
		rep.errf(spath+".vars."+e.Var, "%s", e.Message)
	}

	released := false
	for j := si - 1; j >= 0; j-- {
		if steps[j].Type == "approval" {
			released = true
			break
		}
		if steps[j].Type == "deterministic" && steps[j].Action == "http_request" {
			break
		}
	}
	if !released {
		rep.errf(spath, "http_request needs an approval step before it (and after any earlier http_request); the engine sends only a request the operator was shown and released")
	}

	if spec.AuthEnv != "" && os.Getenv(spec.AuthEnv) == "" {
		rep.warnf(spath+".vars.auth_env", "env var %s is empty (the step will refuse to send)", spec.AuthEnv)
	}
	if spec.AuthEnv == "" {
		for _, k := range []string{"auth_scheme", "auth_header"} {
			if strings.TrimSpace(st.Vars[k]) != "" {
				rep.warnf(spath+".vars."+k, "has no effect without auth_env")
			}
		}
	}
	if spec.PlainRemote() {
		rep.warnf(spath+".vars.url", "plain http to a host other than loopback; the payload and any credential cross the network unencrypted")
	}
	for _, k := range httpaction.UnknownVars(st.Vars) {
		rep.warnf(spath+".vars."+k, "http_request does not read %q%s", k, didYouMean(k, httpaction.KnownVars))
	}
}
