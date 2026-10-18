package logging

import (
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

func TestLogFormatterKeepsRateLimitValuesQuotedOnce(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Message = "Anthropic 429 rate limit response headers"
	entry.Data = log.Fields{
		"rate_limit_headers": `anthropic-ratelimit-unified-status="rejected",anthropic-ratelimit-unified-reset="123"`,
		"retry_after":        `"30","31\nforged"`,
	}
	formatted, err := (&LogFormatter{}).Format(entry)
	if err != nil {
		t.Fatal(err)
	}
	want := ` rate_limit_headers=anthropic-ratelimit-unified-status="rejected",anthropic-ratelimit-unified-reset="123" retry_after="30","31\nforged"` + "\n"
	if !strings.HasSuffix(string(formatted), want) {
		t.Fatalf("expected prequoted rate-limit values once, got %s", formatted)
	}
}

func TestLogFormatterDoesNotPrintGenericStatus(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Message = "unrelated upstream record"
	entry.Data = log.Fields{"status": 429, "ratelimit_status": "fallback_exponential_backoff"}
	formatted, err := (&LogFormatter{}).Format(entry)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(formatted), " status=") {
		t.Fatalf("generic status widened global logging: %s", formatted)
	}
	if !strings.Contains(string(formatted), " ratelimit_status=fallback_exponential_backoff") {
		t.Fatalf("missing scoped rate-limit status: %s", formatted)
	}
}
