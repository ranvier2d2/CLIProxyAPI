package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// Anthropic rejects claude-fable-5-2 from any client older than 2.1.290, so a
// cloaked Fable 5.2 request must leave the proxy as the 2.1.321 CLI.
func TestClaudeExecutor_CloakedFable52LeavesAsClaudeCode321(t *testing.T) {
	var seenBody []byte
	var seenHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenBody, _ = io.ReadAll(r.Body)
		seenHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-fable-5-2","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":    "key-fable-52",
		"base_url":   server.URL,
		"cloak_mode": "always",
	}}
	payload := []byte(`{"model":"claude-fable-5-2","thinking":{"type":"adaptive"},"messages":[{"role":"user","content":[{"type":"text","text":"say ok"}]}]}`)

	_, errExecute := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-fable-5-2",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}

	assertClaudeFingerprint(t, seenHeaders, "claude-cli/2.1.321 (external, cli)", "0.128.0", "v26.12.1", "MacOS", "arm64")
	// Native 2.1.321 sent cc_version=2.1.321.20d for the first user text "say ok".
	if got := gjson.GetBytes(seenBody, "system.0.text").String(); got != "x-anthropic-billing-header: cc_version=2.1.321.20d; cc_entrypoint=cli;" {
		t.Fatalf("billing header = %q, want the native 2.1.321 fingerprint", got)
	}
	if got := gjson.GetBytes(seenBody, "thinking.display").String(); got != "updates" {
		t.Fatalf("thinking.display = %q, want updates like native 2.1.321 sends for Fable 5.2", got)
	}
	betas := seenHeaders.Get("Anthropic-Beta")
	for _, want := range []string{claudePerTurnControlBeta, claudeMidConvToolChangesBeta, claudeThinkingBindingBeta, claudeThinkingDisplayUpdatesBeta} {
		if !strings.Contains(betas, want) {
			t.Fatalf("Anthropic-Beta = %q, missing %s that native 2.1.321 sends for Fable 5.2", betas, want)
		}
	}
	if strings.Contains(betas, claudeMidConvSystemBeta) {
		t.Fatalf("Anthropic-Beta = %q, 2.1.321 no longer sends %s", betas, claudeMidConvSystemBeta)
	}
}

// The want strings are the Anthropic-Beta headers native Claude Code 2.1.321
// sent from the interactive cli entrypoint with OAuth credentials, captured
// 2026-11-09 against a local endpoint with first-party base-URL behavior. Each
// body carries the fields that capture sent: adaptive thinking with progress
// display, per-turn effort, cache diagnostics, a deferred tool, and a
// mid-conversation system message.
func TestClaudeCodeCLIBetas_Matches321Captures(t *testing.T) {
	body := func(model string) []byte {
		return []byte(`{"model":"` + model + `","thinking":{"type":"adaptive","display":"updates"},"output_config":{"effort":"medium"},"diagnostics":{"previous_message_id":null},"tools":[{"name":"ToolSearch"},{"name":"Later","defer_loading":true}],"messages":[{"role":"user","content":"say ok"},{"role":"system","content":"ctx"}]}`)
	}
	const head = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,prompt-caching-scope-2026-01-05,"
	const tail = "tool-provenance-2026-10-26,thinking-token-count-2026-05-13,advanced-tool-use-2025-11-20,effort-2025-11-24,thinking-binding-controls-2026-08-01,thinking-display-updates-2026-08-18,extended-cache-ttl-2025-04-11,cache-diagnosis-2026-04-07"
	const perTurn = "per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,"

	tests := []struct {
		model string
		want  string
	}{
		{"claude-fable-5-2", head + perTurn + tail},
		{"claude-fable-5-1", head + perTurn + tail},
		{"claude-opus-5-5", head + perTurn + tail},
		{"claude-sonnet-5-5", head + perTurn + tail},
		{"claude-sonnet-5", head + tail},
		{"claude-opus-4-8", head + "mid-conversation-tool-changes-2026-07-01," + tail},
		{"claude-haiku-5-5", "oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,prompt-caching-scope-2026-01-05,claude-code-20250219," + perTurn + tail},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := claudeCodeCLIBetas(body(tt.model), nil, true); got != tt.want {
				t.Fatalf("claudeCodeCLIBetas(%s)\n got %s\nwant %s", tt.model, got, tt.want)
			}
		})
	}
}
