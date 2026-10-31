package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

type forwardedClaudeRequest struct {
	body  []byte
	betas string
}

// forwardClaudeMain sends one main Messages request through Execute against a
// direct api.anthropic.com OAuth credential and returns what went upstream.
func forwardClaudeMain(t *testing.T, cfg *config.Config, headers http.Header, model, payload string) forwardedClaudeRequest {
	t.Helper()
	var seen forwardedClaudeRequest
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			return nil, errRead
		}
		seen = forwardedClaudeRequest{body: body, betas: helps.HeaderValueCaseInsensitive(req.Header, "Anthropic-Beta")}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"msg_1","type":"message","model":"` + model + `","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)),
			Request:    req,
		}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	auth := &cliproxyauth.Auth{ID: "pixir-fallback-oauth", Metadata: claudeOAuthTestMetadata(), Attributes: map[string]string{
		"api_key": "sk-ant-oat-pixir-fallback", "base_url": "https://api.anthropic.com",
	}}
	if _, err := NewClaudeExecutor(cfg).Execute(ctx, auth, cliproxyexecutor.Request{Model: model, Payload: []byte(payload)}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		Headers:         headers,
		OriginalRequest: []byte(payload),
	}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(seen.body) == 0 {
		t.Fatal("no request reached the upstream")
	}
	return seen
}

// pixirHeaders is the header shape Pixir sends on its named Anthropic route:
// a plain Messages client that names its Session with X-Session-ID.
func pixirHeaders() http.Header {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Anthropic-Version", "2023-06-01")
	headers.Set("X-Session-ID", "pixir-session-0001")
	return headers
}

func pixirBody(model, extra string) string {
	return `{"model":"` + model + `","max_tokens":1024,"stream":false,` + extra +
		`"thinking":{"type":"adaptive","block_binding":{"prefix_mismatch_behavior":"drop_block"}},` +
		`"system":[{"type":"text","text":"You are Pixir."}],` +
		`"messages":[{"role":"user","content":"hello"}]}`
}

func assertNoServerFallback(t *testing.T, got forwardedClaudeRequest) {
	t.Helper()
	if fallbacks := gjson.GetBytes(got.body, "fallbacks"); fallbacks.Exists() {
		t.Errorf("forwarded body has fallbacks %s", fallbacks.Raw)
	}
	if gjson.GetBytes(got.body, "fallback_credit_token").Exists() {
		t.Error("forwarded body has fallback_credit_token")
	}
	for _, beta := range []string{claudeServerSideFallbackBeta, claudeFallbackCreditBeta} {
		if strings.Contains(got.betas, beta) {
			t.Errorf("Anthropic-Beta carries %s: %q", beta, got.betas)
		}
	}
}

func assertServerFallback(t *testing.T, got forwardedClaudeRequest, wantModel string) {
	t.Helper()
	fallbacks := gjson.GetBytes(got.body, "fallbacks").Array()
	if len(fallbacks) != 1 || fallbacks[0].Get("model").String() != wantModel {
		t.Errorf("fallbacks = %s, want [{model:%s}]", gjson.GetBytes(got.body, "fallbacks").Raw, wantModel)
	}
	if !strings.Contains(got.betas, claudeServerSideFallbackBeta) {
		t.Errorf("Anthropic-Beta lacks %s: %q", claudeServerSideFallbackBeta, got.betas)
	}
}

// A mid-response model fallback signs thinking under a model the caller did not
// ask for, and Pixir replays that thinking under the requested one, which the
// API then rejects. So a Pixir Session never gets a fallback the caller did
// not send, on any model the cloak would give one to.
func TestClaudeExecutor_PixirSessionGetsNoInjectedServerFallback(t *testing.T) {
	for _, model := range []string{"claude-opus-5-5", "claude-fable-5-1"} {
		t.Run(model, func(t *testing.T) {
			got := forwardClaudeMain(t, &config.Config{}, pixirHeaders(), model, pixirBody(model, ""))
			assertNoServerFallback(t, got)
		})
	}
}

func TestClaudeExecutor_PixirSessionKeepsCallerFallbacks(t *testing.T) {
	model := "claude-opus-5-5"
	got := forwardClaudeMain(t, &config.Config{}, pixirHeaders(), model, pixirBody(model, `"fallbacks":[{"model":"claude-sonnet-5"}],`))
	assertServerFallback(t, got, "claude-sonnet-5")
}

func TestClaudeExecutor_PixirServerFallbackSwitchOffRestoresInjection(t *testing.T) {
	off := false
	cfg := &config.Config{Claude: config.ClaudeConfig{PixirDisableServerFallback: &off}}
	model := "claude-opus-5-5"
	got := forwardClaudeMain(t, cfg, pixirHeaders(), model, pixirBody(model, ""))
	assertServerFallback(t, got, "claude-opus-4-8")
}

// Without X-Session-ID a cloaked caller is not Pixir and keeps the native
// Claude Code shape, fallback included.
func TestClaudeExecutor_CloakedCallerWithoutSessionKeepsInjectedFallback(t *testing.T) {
	headers := pixirHeaders()
	headers.Del("X-Session-ID")
	model := "claude-opus-5-5"
	got := forwardClaudeMain(t, &config.Config{}, headers, model, pixirBody(model, ""))
	assertServerFallback(t, got, "claude-opus-4-8")
}

// Real Claude Code is passed through: the fallback it sends goes upstream as
// sent, even if it also names a Session the way Pixir does.
func TestClaudeExecutor_NativeClaudeCodeKeepsItsFallbacks(t *testing.T) {
	const betas = claudeNativeHelperCoreBetas + ",effort-2025-11-24,server-side-fallback-2026-06-01,fallback-credit-2026-06-01"
	headers := claudeNativeHelperHeaders(betas, "gzip, deflate, br, zstd")
	headers.Set("X-Session-ID", "pixir-session-0001")
	payload := `{"model":"claude-opus-5-5","max_tokens":1024,"fallbacks":[{"model":"claude-opus-4-8"}],` +
		`"messages":[{"role":"user","content":"hello"}],` +
		`"metadata":{"user_id":"` + strings.ReplaceAll(claudeNativeHelperUserID, `"`, `\"`) + `"}}`
	got := forwardClaudeMain(t, &config.Config{}, headers, "claude-opus-5-5", payload)
	assertServerFallback(t, got, "claude-opus-4-8")
	if !strings.Contains(got.betas, claudeFallbackCreditBeta) {
		t.Errorf("Anthropic-Beta lacks %s: %q", claudeFallbackCreditBeta, got.betas)
	}
}
