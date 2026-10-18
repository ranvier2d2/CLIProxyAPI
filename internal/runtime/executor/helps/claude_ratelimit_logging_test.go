package helps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
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

func TestClaudeRateLimitHeaderValuesAreBoundedAndQuotedOnce(t *testing.T) {
	for _, name := range []string{"Anthropic-Ratelimit-Unified-New-Signal", "Retry-After"} {
		for _, size := range []int{511, 512, 513, 64 * 1024} {
			t.Run(fmt.Sprintf("%s/%d", name, size), func(t *testing.T) {
				var output bytes.Buffer
				logger := log.New()
				logger.SetOutput(&output)
				logger.SetFormatter(&log.JSONFormatter{})
				value := strings.Repeat("A", size)
				LogClaudeRateLimitResponseHeaders(log.NewEntry(logger), http.Header{name: {value}})
				var entry map[string]any
				if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
					t.Fatal(err)
				}
				field := "rate_limit_headers"
				if name == "Retry-After" {
					field = "retry_after"
				}
				quoted := entry[field].(string)
				if field == "rate_limit_headers" {
					quoted = strings.TrimPrefix(quoted, strings.ToLower(name)+"=")
				}
				logged, err := strconv.Unquote(quoted)
				if err != nil {
					t.Fatalf("value must be quoted once: %v", err)
				}
				if len(logged) > 512 {
					t.Fatalf("logged %d bytes, want at most 512", len(logged))
				}
				if size <= 512 {
					if logged != value {
						t.Fatal("truncated a value within the limit")
					}
				} else {
					marker := strings.Index(logged, "<truncated ")
					if marker < 0 {
						t.Fatal("missing truncation marker")
					}
					want := fmt.Sprintf("<truncated %d bytes>", size-marker)
					if logged[marker:] != want {
						t.Fatalf("marker = %q, want %q", logged[marker:], want)
					}
				}
			})
		}
	}
}

func TestClaudeRateLimitHeaderValuesCannotForgeLogFields(t *testing.T) {
	var output bytes.Buffer
	logger := log.New()
	logger.SetOutput(&output)
	logger.SetFormatter(&logging.LogFormatter{})
	LogClaudeRateLimitResponseHeaders(log.NewEntry(logger), http.Header{
		"Anthropic-Ratelimit-Unified-New-Signal": {"rejected\r\nforged\x1b\u2028", "\"quoted\",auth_file=FAKE"},
		"anthropic-ratelimit-unified-new-signal": {"allowed"},
		"Retry-After":                            {"30", "31\nforged"},
	})
	text := output.String()
	want := `rate_limit_headers=anthropic-ratelimit-unified-new-signal="\"quoted\",auth_file=FAKE",anthropic-ratelimit-unified-new-signal="allowed",anthropic-ratelimit-unified-new-signal="rejected\r\nforged\x1b\u2028" retry_after="30","31\nforged"`
	if !strings.Contains(text, want) {
		t.Fatalf("values must be individually quoted and sorted: %s", text)
	}
	if strings.Count(text, "\n") != 1 || strings.ContainsAny(text, "\r\x1b\u2028") {
		t.Fatal("upstream text forged a log record")
	}
}
