package helps

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// All header sets here are synthetic. The incident log did not retain them.
func TestClaudeCooldownSyncMatrix(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	const accountMessage = "This request would exceed your account's rate limit. Please try again later."
	const minuteMessage = "This request would exceed the requests per minute rate limit. Please try again later."
	for _, tc := range []struct {
		name, message               string
		status, overageReset, reset string
		sharedStatuses              bool
		want                        time.Duration
		wantNil                     bool
	}{
		{name: "account_message_quota_headers_remains_multiday", message: accountMessage, status: "rejected", reset: "70h", want: 70 * time.Hour},
		{name: "account_message_unclassified_reset_remains_multiday", message: accountMessage, reset: "70h", want: 70 * time.Hour},
		{name: "explicit_per_minute_unclassified_reset_is_short", message: minuteMessage, reset: "70h", want: 30 * time.Second},
		{name: "explicit_per_minute_with_quota_status_stays_conservative", message: minuteMessage, status: "rejected", reset: "70h", want: 70 * time.Hour},
		{name: "account_message_matching_overage_boundary_is_ignored", message: accountMessage, status: "rejected", reset: "70h", overageReset: "70h", want: 30 * time.Second},
		{name: "account_message_reset_beyond_maximum_window_is_ignored", message: accountMessage, status: "rejected", reset: "200h", want: 30 * time.Second},
		{name: "account_message_two_rejected_windows_chooses_latest", message: accountMessage, status: "rejected", reset: "70h", sharedStatuses: true, want: 70 * time.Hour},
		{name: "account_message_no_reset_falls_back", message: accountMessage, status: "rejected", wantNil: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := make(http.Header)
			if !tc.wantNil {
				headers.Set("Retry-After", "30")
			}
			if tc.status != "" {
				headers.Set("Anthropic-Ratelimit-Unified-Status", tc.status)
			}
			for name, duration := range map[string]string{
				"Anthropic-Ratelimit-Unified-Reset":         tc.reset,
				"Anthropic-Ratelimit-Unified-Overage-Reset": tc.overageReset,
			} {
				if duration == "" {
					continue
				}
				d, err := time.ParseDuration(duration)
				if err != nil {
					t.Fatal(err)
				}
				headers.Set(name, now.Add(d).Format(time.RFC3339))
			}
			if tc.overageReset != "" {
				headers.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "overage")
				headers.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
				headers.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.69")
				headers.Set("Anthropic-Ratelimit-Unified-Overage-Status", "rejected")
			}
			if tc.sharedStatuses {
				headers.Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
				headers.Set("Anthropic-Ratelimit-Unified-5h-Reset", now.Add(time.Hour).Format(time.RFC3339))
				headers.Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
				headers.Set("Anthropic-Ratelimit-Unified-7d-Reset", now.Add(70*time.Hour).Format(time.RFC3339))
			}
			body, err := json.Marshal(map[string]any{"error": map[string]string{"type": "rate_limit_error", "message": tc.message}})
			if err != nil {
				t.Fatal(err)
			}
			got := ParseClaudeRateLimitResetForError(headers, body, now)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("want nil for exponential fallback, got %s", *got)
				}
				t.Log("cooldown=nil (generic exponential fallback)")
				return
			}
			if got == nil {
				t.Fatal("missing cooldown")
			}
			t.Logf("cooldown=%s; base=%s", *got, tc.want)
			if *got < tc.want+time.Second || *got > tc.want+30*time.Second {
				t.Fatalf("got %s, want %s plus 1..30s fuzz", *got, tc.want)
			}
		})
	}
}
