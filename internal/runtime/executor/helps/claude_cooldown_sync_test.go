package helps

import (
	"net/http"
	"testing"
	"time"
)

// This is a synthetic reproducer, not a captured response header set.
func TestClaudeShortRetryAfterDoesNotInheritSeventyHourReset(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	headers := make(http.Header)
	headers.Set("Retry-After", "30")
	headers.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	headers.Set("Anthropic-Ratelimit-Unified-Reset", now.Add(70*time.Hour).Format(time.RFC3339))
	got := parseClaudeRateLimitResetWithFuzz(headers, now, 0, 0)
	if got == nil {
		t.Fatal("expected a retry deadline")
	}
	t.Logf("synthetic short Retry-After with rejected unified reset: cooldown=%s", *got)
	if *got != 30*time.Second {
		t.Fatalf("30s Retry-After became %s; want 30s", *got)
	}
}
