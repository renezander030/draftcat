//go:build voice

package validate

import "testing"

func TestValidateVoiceCallCapsEnabled(t *testing.T) {
	if rep := callCapsReport(t, 10, 60, "enabled: true"); len(rep.Findings) != 0 {
		t.Fatalf("caps with voice enabled: %+v", rep.Findings)
	}
}
