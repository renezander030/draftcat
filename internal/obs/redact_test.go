package obs

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/renezander030/draftcat/internal/redact"
)

func TestSpansAreRedacted(t *testing.T) {
	redact.Reset()
	defer redact.Reset()
	redact.Register("OPENROUTER_API_KEY", "sk-or-v1-spanleak0001")
	var buf bytes.Buffer
	Enable(&buf)
	defer reset()
	Pipeline("p").End("error", map[string]interface{}{"error": "401 for key sk-or-v1-spanleak0001"})
	EmitStep("p", "s", "ai", time.Now(), "error", map[string]interface{}{"error": "sk-or-v1-spanleak0001"})
	if strings.Contains(buf.String(), "sk-or-v1-spanleak0001") {
		t.Fatalf("span leaked a secret: %s", buf.String())
	}
	if strings.Count(buf.String(), "[REDACTED:OPENROUTER_API_KEY]") != 2 {
		t.Fatalf("spans not labelled: %s", buf.String())
	}
}
