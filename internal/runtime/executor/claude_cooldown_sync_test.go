package executor

import (
	"net/http"
	"testing"
	"time"
)

func TestClaudeAccountRateLimitMessageDoesNotInheritSeventyHourReset(t *testing.T) {
	headers := make(http.Header)
	headers.Set("Retry-After", "30")
	headers.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	headers.Set("Anthropic-Ratelimit-Unified-Reset", time.Now().Add(70*time.Hour).UTC().Format(time.RFC3339))
	body := []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."}}`)
	err := classifyClaudeUpstreamError(http.StatusTooManyRequests, headers, body)
	retry, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok || retry.RetryAfter() == nil {
		t.Fatalf("expected a retry deadline, got %T", err)
	}
	got := *retry.RetryAfter()
	t.Logf("exact incident message with synthetic quota headers: cooldown=%s", got)
	if got < 30*time.Second || got > time.Minute {
		t.Fatalf("30s Retry-After became %s; want 30s plus at most 30s fuzz", got)
	}
}
