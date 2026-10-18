package helps

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
)

// LogClaudeRateLimitResponseHeaders records only known, non-secret rate-limit
// response headers. Do not replace this allowlist with a prefix match or dump the
// response headers: authentication material and unknown extensions must stay out.
func LogClaudeRateLimitResponseHeaders(headers http.Header) {
	allowed := map[string]bool{
		"anthropic-ratelimit-unified-status":                  true,
		"anthropic-ratelimit-unified-reset":                   true,
		"anthropic-ratelimit-unified-utilization":             true,
		"anthropic-ratelimit-unified-representative-claim":    true,
		"anthropic-ratelimit-unified-overage-status":          true,
		"anthropic-ratelimit-unified-overage-reset":           true,
		"anthropic-ratelimit-unified-overage-utilization":     true,
		"anthropic-ratelimit-unified-overage-disabled-reason": true,
	}
	for _, window := range []string{"5h", "7d", "7d_oi"} {
		for _, field := range []string{"status", "reset", "utilization"} {
			allowed["anthropic-ratelimit-unified-"+window+"-"+field] = true
		}
	}
	for _, window := range []string{"requests", "tokens", "input-tokens", "output-tokens"} {
		for _, field := range []string{"limit", "remaining", "reset"} {
			allowed["anthropic-ratelimit-"+window+"-"+field] = true
		}
	}
	var rateHeaders, retryAfter []string
	for name, values := range headers {
		key := strings.ToLower(name)
		if !allowed[key] && key != "retry-after" {
			continue
		}
		for _, value := range values {
			// Quote upstream-controlled text so it cannot inject log lines.
			if key == "retry-after" {
				retryAfter = append(retryAfter, strconv.Quote(value))
			} else {
				rateHeaders = append(rateHeaders, key+"="+strconv.Quote(value))
			}
		}
	}
	sort.Strings(rateHeaders)
	sort.Strings(retryAfter)
	log.WithFields(log.Fields{
		"rate_limit_headers": strings.Join(rateHeaders, ","),
		"retry_after":        strings.Join(retryAfter, ","),
	}).Info("Anthropic 429 rate limit response headers")
}
