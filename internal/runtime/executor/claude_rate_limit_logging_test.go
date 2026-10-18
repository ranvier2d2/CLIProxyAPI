package executor

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	log "github.com/sirupsen/logrus"
)

func TestClaude429LogsOnlySafeRateLimitHeaders(t *testing.T) {
	logger := log.StandardLogger()
	previousOutput, previousFormatter, previousLevel := logger.Out, logger.Formatter, logger.GetLevel()
	t.Cleanup(func() {
		logger.SetOutput(previousOutput)
		logger.SetFormatter(previousFormatter)
		logger.SetLevel(previousLevel)
	})
	var output bytes.Buffer
	logger.SetOutput(&output)
	logger.SetFormatter(&logging.LogFormatter{})
	logger.SetLevel(log.InfoLevel)
	headers := http.Header{
		"aNtHrOpIc-RaTeLiMiT-UnIfIeD-StAtUs":               {"rejected"},
		"Anthropic-Ratelimit-Unified-Reset":                {"1792576800"},
		"Anthropic-Ratelimit-Unified-5h-Utilization":       {"0.99"},
		"Anthropic-Ratelimit-Unified-Representative-Claim": {"five_hour"},
		"Anthropic-Ratelimit-Unified-Overage-Status":       {"allowed"},
		"Retry-After":               {"30", "31\nforged log line"},
		"Authorization":             {"FORBIDDEN_AUTHORIZATION"},
		"Proxy-Authorization":       {"FORBIDDEN_PROXY_AUTHORIZATION"},
		"Cookie":                    {"FORBIDDEN_COOKIE"},
		"Set-Cookie":                {"FORBIDDEN_SET_COOKIE"},
		"X-Api-Key":                 {"FORBIDDEN_API_KEY"},
		"X-Access-Token":            {"FORBIDDEN_ACCESS_TOKEN"},
		"Anthropic-Ratelimit-Token": {"FORBIDDEN_PREFIX_TOKEN"},
		"Anthropic-Ratelimit-Unified-Authorization": {"FORBIDDEN_PREFIX_AUTH"},
	}
	body := []byte(`{"error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."}}`)
	for _, fast := range []bool{false, true} {
		output.Reset()
		if fast {
			_ = newClaudeFastDirectResponseError(&http.Response{StatusCode: http.StatusTooManyRequests, Header: headers}, body)
		} else {
			_ = classifyClaudeUpstreamError(http.StatusTooManyRequests, headers, body)
		}
		line := output.String()
		for _, want := range []string{"Anthropic 429 rate limit response headers", "anthropic-ratelimit-unified-status", "rejected", "1792576800", "0.99", "five_hour", "anthropic-ratelimit-unified-overage-status", "retry_after=", "30", "31"} {
			if !strings.Contains(line, want) {
				t.Errorf("fast=%v: missing %q in %s", fast, want, line)
			}
		}
		if strings.Contains(line, "FORBIDDEN_") {
			t.Errorf("fast=%v: sensitive or unknown response header leaked", fast)
		}
		if strings.Contains(line, "\nforged log line") {
			t.Errorf("fast=%v: header injected a log line", fast)
		}
		if strings.Count(line, "Anthropic 429 rate limit response headers") != 1 {
			t.Errorf("fast=%v: expected one response-header record", fast)
		}
	}
	output.Reset()
	_ = classifyClaudeUpstreamError(http.StatusBadRequest, headers, body)
	if strings.Contains(output.String(), "Anthropic 429 rate limit response headers") {
		t.Fatal("logged a non-429 response as a rate limit")
	}
}
