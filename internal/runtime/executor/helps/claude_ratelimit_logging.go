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
	"golang.org/x/net/http/httpguts"
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

// LogClaudeRateLimitResponseHeaders records the unified rate-limit namespace,
// known non-unified rate-limit headers and Retry-After. Auth-like extensions and
// invalid header names are excluded even within the unified namespace.
func LogClaudeRateLimitResponseHeaders(entry *log.Entry, headers http.Header) {
	if entry == nil {
		entry = log.NewEntry(log.StandardLogger())
	}
	var rateHeaders, retryAfter []string
	for name, values := range headers {
		key := strings.ToLower(name)
		if !safeClaudeRateLimitHeader(key) {
			continue
		}
		for _, value := range values {
			// Quote upstream-controlled text so it cannot inject log lines.
			if key == "retry-after" {
				retryAfter = append(retryAfter, quoteClaudeRateLimitHeaderValue(value))
			} else {
				rateHeaders = append(rateHeaders, key+"="+quoteClaudeRateLimitHeaderValue(value))
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

// quoteClaudeRateLimitHeaderValue caps the value at 512 bytes, including the
// truncation marker, before escaping it. Quoting each item prevents line and
// field injection, even when values contain commas, quotes or control bytes.
func quoteClaudeRateLimitHeaderValue(value string) string {
	const maxBytes = 512
	if len(value) <= maxBytes {
		return strconv.Quote(value)
	}
	omitted := len(value) - maxBytes
	for {
		marker := "<truncated " + strconv.Itoa(omitted) + " bytes>"
		keep := maxBytes - len(marker)
		if nextOmitted := len(value) - keep; nextOmitted != omitted {
			omitted = nextOmitted
			continue
		}
		return strconv.Quote(value[:keep] + marker)
	}
}

func safeClaudeRateLimitHeader(name string) bool {
	if !httpguts.ValidHeaderFieldName(name) {
		return false
	}
	const unifiedPrefix = "anthropic-ratelimit-unified-"
	if strings.HasPrefix(name, unifiedPrefix) {
		suffix := strings.TrimPrefix(name, unifiedPrefix)
		// Do not let the extensible rate-limit namespace become a route for
		// authentication headers. Token counters remain allowed only under
		// the exact, known non-unified names below.
		for _, sensitive := range []string{"auth", "cookie", "token", "key", "secret", "credential", "session", "password", "passwd", "bearer", "jwt", "signature"} {
			if strings.Contains(suffix, sensitive) {
				return false
			}
		}
		return suffix != ""
	}
	switch name {
	case "retry-after",
		"anthropic-ratelimit-requests-limit", "anthropic-ratelimit-requests-remaining", "anthropic-ratelimit-requests-reset",
		"anthropic-ratelimit-tokens-limit", "anthropic-ratelimit-tokens-remaining", "anthropic-ratelimit-tokens-reset",
		"anthropic-ratelimit-input-tokens-limit", "anthropic-ratelimit-input-tokens-remaining", "anthropic-ratelimit-input-tokens-reset",
		"anthropic-ratelimit-output-tokens-limit", "anthropic-ratelimit-output-tokens-remaining", "anthropic-ratelimit-output-tokens-reset":
		return true
	default:
		return false
	}
}
