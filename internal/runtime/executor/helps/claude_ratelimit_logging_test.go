package helps

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	log "github.com/sirupsen/logrus"
)

func TestClaudeRateLimitLogsUnifiedExtensionsAndKnownHeaders(t *testing.T) {
	for _, name := range []string{
		"Anthropic-Ratelimit-Unified-30d-Status",
		"aNtHrOpIc-RaTeLiMiT-UnIfIeD-7d-Sonnet-Reset",
		"Anthropic-Ratelimit-Unified-New-Signal",
		"Anthropic-Ratelimit-Unified-Representative-Claim",
		"Anthropic-Ratelimit-Unified-Overage-Disabled-Reason",
		"Anthropic-Ratelimit-Requests-Limit",
		"Anthropic-Ratelimit-Tokens-Remaining",
		"Anthropic-Ratelimit-Input-Tokens-Reset",
		"Anthropic-Ratelimit-Output-Tokens-Limit",
		"Retry-After",
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			logger := log.New()
			logger.SetOutput(&output)
			logger.SetFormatter(&logging.LogFormatter{})
			LogClaudeRateLimitResponseHeaders(log.NewEntry(logger), http.Header{name: {"SAFE_HEADER_VALUE"}})
			if !strings.Contains(output.String(), "SAFE_HEADER_VALUE") {
				t.Fatalf("missing safe header %s: %s", name, output.String())
			}
		})
	}
}

func TestClaudeRateLimitNeverLogsAuthLikeHeaders(t *testing.T) {
	names := []string{
		"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie", "X-Api-Key", "X-Access-Token", "Request-Id",
		"Anthropic-Ratelimit-Api-Key", "Anthropic-Ratelimit-Token", "Retry-After-Token",
		"X-Anthropic-Ratelimit-Unified-Status", "Anthropic-Ratelimit-Unified-Reset ",
		"Anthropic-Ratelimit-Unified-Status\r\nX-Injected", "Anthropic-Ratelimit-Unified-Status\x1b",
	}
	for _, suffix := range []string{
		"Authorization", "Proxy-Authorization", "Status-Token", "Api-Key", "APIKEY", "Api_Key", "Access-Key",
		"Cookie", "Set-Cookie", "Secret", "Client-Secret", "Credential", "Credentials", "Password", "Passwd",
		"Bearer", "Jwt", "Signature", "5h-Session", "7d-Auth", "AUTHentication",
	} {
		names = append(names, "Anthropic-Ratelimit-Unified-"+suffix)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			logger := log.New()
			logger.SetOutput(&output)
			logger.SetFormatter(&logging.LogFormatter{})
			LogClaudeRateLimitResponseHeaders(log.NewEntry(logger), http.Header{name: {"FORBIDDEN_HEADER_VALUE"}})
			if strings.Contains(output.String(), "FORBIDDEN_HEADER_VALUE") {
				t.Fatalf("logged auth-like or invalid header %q", name)
			}
		})
	}
}
