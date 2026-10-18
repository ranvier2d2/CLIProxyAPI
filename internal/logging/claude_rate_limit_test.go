package logging

import (
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

func TestLogFormatterPrintsClaudeRateLimitDecisionFields(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Message = "parsed Anthropic rate limit reset headers"
	entry.Data = log.Fields{
		"rejected_windows":   "unified,retry-after",
		"deadline":           "2026-10-21T10:00:00Z",
		"base_cooldown":      "70h0m0s",
		"effective_cooldown": "70h0m1s",
		"fuzz":               "1s",
		"status":             "fallback_exponential_backoff",
	}
	formatted, err := (&LogFormatter{}).Format(entry)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range entry.Data {
		if !strings.Contains(string(formatted), key+"="+value.(string)) {
			t.Errorf("missing %s=%s in %s", key, value, formatted)
		}
	}
}
