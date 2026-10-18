package helps

import (
	"context"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// ClaudeRateLimitLogger correlates header and cooldown records with the request
// and the same auth-file basename used by the auth manager. It never logs auth
// metadata, tokens, account details or host paths.
func ClaudeRateLimitLogger(ctx context.Context, auth *cliproxyauth.Auth) *log.Entry {
	entry := LogWithRequestID(ctx)
	if auth == nil {
		return entry
	}
	authFile := strings.TrimSpace(auth.FileName)
	if authFile == "" {
		authFile = strings.TrimSpace(auth.ID)
	}
	if authFile != "" {
		entry = entry.WithField("auth_file", filepath.Base(authFile))
	}
	return entry
}

// LogClaudeRateLimitResponseHeaders records only known, non-secret rate-limit
// response headers. Do not replace this allowlist with a prefix match or dump the
// response headers: authentication material and unknown extensions must stay out.
func LogClaudeRateLimitResponseHeaders(entry *log.Entry, headers http.Header) {
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
	entry.WithFields(log.Fields{
		"rate_limit_headers": strings.Join(rateHeaders, ","),
		"retry_after":        strings.Join(retryAfter, ","),
	}).Info("Anthropic 429 rate limit response headers")
}
