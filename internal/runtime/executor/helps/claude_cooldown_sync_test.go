package helps

import (
	"net/http"
	"testing"
	"time"
)

// This is a synthetic reproducer, not a captured response header set.
// The original short-cooldown expectation remains RED after the upstream sync.
// Characterize that unresolved behavior without inventing an unobserved-header fix.
func TestClaudeShortRetryAfterStillInheritsSeventyHourReset(t *testing.T) {
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
	if *got != 70*time.Hour {
		t.Fatalf("conservative latest-deadline behavior changed: got %s, want 70h", *got)
	}
}
