package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
)

func TestClaude429LogsRequestAndAuthIdentity(t *testing.T) {
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

	for _, path := range []struct {
		name                              string
		stream, fast, tokens, decodeError bool
	}{
		{name: "execute"},
		{name: "stream", stream: true},
		{name: "count_tokens", tokens: true},
		{name: "fast_execute", fast: true},
		{name: "fast_stream", fast: true, stream: true},
		{name: "execute_decode_error", decodeError: true},
		{name: "stream_decode_error", stream: true, decodeError: true},
		{name: "count_tokens_decode_error", tokens: true, decodeError: true},
	} {
		for _, fallback := range []bool{false, true} {
			for _, idOnly := range []bool{false, true} {
				name := path.name + "/deadline"
				if fallback {
					name = path.name + "/fallback"
				}
				if idOnly {
					name += "/id_only"
				} else {
					name += "/file_name"
				}
				t.Run(name, func(t *testing.T) {
					output.Reset()
					headers := http.Header{"Anthropic-Ratelimit-Unified-Status": {"rejected"}}
					decision := "parsed Anthropic rate limit reset headers"
					if fallback {
						decision = "Anthropic rate limit window rejected; falling back to generic exponential backoff"
					} else {
						headers.Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(time.Now().Add(70*time.Hour).Unix(), 10))
					}
					if path.decodeError {
						headers.Set("Content-Encoding", "gzip")
					}
					transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						return &http.Response{StatusCode: http.StatusTooManyRequests, Header: headers, Request: req,
							Body: io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."}}`))}, nil
					})
					ctx := logging.WithRequestID(t.Context(), "018f3a5b-1234-7abc-def0-12345678abcd")
					ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(transport))
					auth := &cliproxyauth.Auth{ID: "/private/synthetic/account-id.json", FileName: "/private/synthetic/account-file.json", Provider: "claude",
						Attributes: map[string]string{"api_key": "FORBIDDEN_API_KEY"}}
					wantFile := "account-file.json"
					if idOnly {
						auth.FileName = ""
						wantFile = "account-id.json"
					}
					payload := []byte(`{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"reply OK"}]}`)
					if path.fast {
						payload = []byte(`{"model":"claude-opus-5","max_tokens":16,"speed":"fast","messages":[{"role":"user","content":"reply OK"}]}`)
					}
					request := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: payload}
					options := cliproxyexecutor.Options{Stream: path.stream, SourceFormat: sdktranslator.FormatClaude, ResponseFormat: sdktranslator.FormatClaude}
					executor := NewClaudeExecutor(&config.Config{})
					var err error
					switch {
					case path.tokens:
						_, err = executor.countTokensUpstream(ctx, auth, request, options)
					case path.stream:
						_, err = executor.ExecuteStream(ctx, auth, request, options)
					default:
						_, err = executor.Execute(ctx, auth, request, options)
					}
					if err == nil {
						t.Fatal("expected synthetic 429 error")
					}
					text := output.String()
					for _, message := range []string{"Anthropic 429 rate limit response headers", decision} {
						found := 0
						for _, line := range strings.Split(text, "\n") {
							if !strings.Contains(line, message) {
								continue
							}
							found++
							if !strings.Contains(line, "[5678abcd]") || !strings.Contains(line, "auth_file="+strconv.Quote(wantFile)) {
								t.Errorf("missing request/account attribution: %s", line)
							}
						}
						if found != 1 {
							t.Errorf("got %d %q records, want 1: %s", found, message, text)
						}
					}
					if strings.Contains(text, "FORBIDDEN_") || strings.Contains(text, "/private/synthetic/") {
						t.Error("logged credential material or host path")
					}
				})
			}
		}
	}
}
