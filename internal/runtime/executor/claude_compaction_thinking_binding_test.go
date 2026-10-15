package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// pixirCompactionSequence is a recorded Pixir Session (Haiku 5.5, adaptive
// thinking with block_binding drop_block) through two on-demand compactions
// with keep-tail and a cold resume. Every string is a hash placeholder, so equal
// values stay equal across requests. The two 400s the live run got (the cloak's
// context_management beside compaction) are left out; their retries are in.
type pixirCompactionRequest struct {
	Tee           int             `json:"tee"`
	AnthropicBeta string          `json:"anthropic_beta"`
	Body          json.RawMessage `json:"body"`
}

func loadPixirCompactionSequence(t *testing.T) []pixirCompactionRequest {
	t.Helper()
	raw, errRead := os.ReadFile("testdata/pixir_compaction_keep_tail_sequence.json")
	if errRead != nil {
		t.Fatal(errRead)
	}
	var sequence []pixirCompactionRequest
	if errUnmarshal := json.Unmarshal(raw, &sequence); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	return sequence
}

type forwardedPixirRequest struct {
	tee     int
	headers http.Header
	caller  []byte
	body    []byte
}

// forwardPixirCompactionSequence replays the recording through ExecuteStream
// with a fresh in-memory anchor memo. beforeEach, when set, runs before every
// request, for example to model a proxy restart.
func forwardPixirCompactionSequence(t *testing.T, sessionHeader string, beforeEach func()) []forwardedPixirRequest {
	t.Helper()
	previous := helps.SetClaudeCloakAnchors(helps.NewClaudeCloakAnchorMemo(""))
	t.Cleanup(func() { helps.SetClaudeCloakAnchors(previous) })
	var captured []byte
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			return nil, errRead
		}
		captured = body
		sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"model\":\"claude-haiku-5-5\",\"role\":\"assistant\",\"content\":[]}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		header := make(http.Header)
		header.Set("Content-Type", "text/event-stream")
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(sse)), Request: req}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	auth := &cliproxyauth.Auth{
		ID:         "test-pixir-compaction-oauth",
		Attributes: map[string]string{"api_key": "sk-ant-oat-test-pixir-compaction"},
		Metadata: map[string]any{
			"account_uuid": "11111111-2222-4333-8444-555555555555",
			claudeauth.ClaudeDeviceIDsMetadataKey: []string{
				"0000000000000000000000000000000000000000000000000000000000000001",
			},
		},
	}
	exec := NewClaudeExecutor(&config.Config{})

	var forwarded []forwardedPixirRequest
	for _, request := range loadPixirCompactionSequence(t) {
		if beforeEach != nil {
			beforeEach()
		}
		headers := make(http.Header)
		headers.Set("Content-Type", "application/json")
		headers.Set("Anthropic-Version", "2023-06-01")
		headers.Set("Anthropic-Beta", request.AnthropicBeta)
		if sessionHeader != "" {
			headers.Set("X-Session-ID", sessionHeader)
		}
		opts := cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatClaude,
			Stream:          true,
			Headers:         headers,
			OriginalRequest: request.Body,
		}
		result, errExec := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "claude-haiku-5-5", Payload: request.Body}, opts)
		if errExec != nil {
			t.Fatalf("tee #%d: %v", request.Tee, errExec)
		}
		for range result.Chunks {
		}
		forwarded = append(forwarded, forwardedPixirRequest{tee: request.Tee, headers: headers, caller: request.Body, body: captured})
	}
	return forwarded
}

// canonicalWithoutCacheControl renders a JSON value with sorted keys and no
// cache_control, which the API leaves out of the thinking prefix check.
func canonicalWithoutCacheControl(t *testing.T, raw string) string {
	t.Helper()
	var value any
	if errUnmarshal := json.Unmarshal([]byte(raw), &value); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	var strip func(any) any
	strip = func(v any) any {
		switch typed := v.(type) {
		case map[string]any:
			out := make(map[string]any, len(typed))
			for key, inner := range typed {
				if key != "cache_control" {
					out[key] = strip(inner)
				}
			}
			return out
		case []any:
			for i := range typed {
				typed[i] = strip(typed[i])
			}
			return typed
		default:
			return v
		}
	}
	out, errMarshal := json.Marshal(strip(value))
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return string(out)
}

type thinkingBinding struct {
	head        string
	prefix      []string
	systemTools string
}

// TestClaudeCloakKeepsCompactionKeptThinkingBound replays the recorded Pixir
// Session and applies Anthropic's documented check to the forwarded bodies: a
// replayed thinking block stays valid while system, tools and every message
// before it are unchanged since it was produced; after an on-demand compaction
// the checked prefix starts at the newest compaction block, and the swap is
// accepted when that compaction request's messages followed by the kept
// messages equal the prefix the block was produced under. Before this fix the
// cloak re-anchored its date reminder and relocated system message on the
// first kept user turn after each swap, which the live API reported as 7 of 8
// and 7 of 7 kept blocks dropped with prefix_binding_mismatch.
func TestClaudeCloakKeepsCompactionKeptThinkingBound(t *testing.T) {
	assertPixirKeptThinkingBound(t, forwardPixirCompactionSequence(t, "", nil))
}

// TestClaudeCloakKeepsCompactionKeptThinkingBoundAcrossRestarts replays the
// same recording with a proxy restart before every request: each request sees
// a new memo that only has what the previous ones wrote to its file. A second
// process sharing the file is the same case.
func TestClaudeCloakKeepsCompactionKeptThinkingBoundAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude-cloak-anchors.json")
	forwarded := forwardPixirCompactionSequence(t, "", func() {
		helps.SetClaudeCloakAnchors(helps.NewClaudeCloakAnchorMemo(path))
	})
	assertPixirKeptThinkingBound(t, forwarded)
	// The billing header and, on requests without a session key, the random
	// metadata.user_id differ per process anyway; both are outside the check.
	want := forwardPixirCompactionSequence(t, "", nil)
	for i := range want {
		if gjson.GetBytes(forwarded[i].body, "messages").Raw != gjson.GetBytes(want[i].body, "messages").Raw {
			t.Fatalf("tee #%d: forwarded messages differ between a restarting proxy and one that never restarts", want[i].tee)
		}
	}
}

func assertPixirKeptThinkingBound(t *testing.T, forwarded []forwardedPixirRequest) {
	t.Helper()
	bindings := make(map[string]thinkingBinding)
	compactionCovered := make(map[string][]string)
	var lastCompactionRequest []string
	replayed := 0
	var drops []string
	for _, request := range forwarded {
		messages := gjson.GetBytes(request.body, "messages").Array()
		// The billing header block is per-request metadata outside the binding: the
		// live run kept messages.1 at the first swap although this block changed.
		var system []string
		for _, block := range gjson.GetBytes(request.body, "system").Array() {
			if !strings.HasPrefix(block.Get("text").String(), "x-anthropic-billing-header") {
				system = append(system, block.Raw)
			}
		}
		systemTools := canonicalWithoutCacheControl(t, "["+strings.Join(system, ",")+"]") + canonicalWithoutCacheControl(t, gjson.GetBytes(request.body, "tools").Raw)

		start, head := 0, ""
		for idx, message := range messages {
			for _, block := range message.Get("content").Array() {
				if block.Get("type").String() == "compaction" {
					start, head = idx, block.Get("signature").String()
				}
			}
		}
		if head != "" {
			if _, ok := compactionCovered[head]; !ok {
				compactionCovered[head] = lastCompactionRequest
			}
		}
		// The caller's system prompt must stay in force after each swap: every
		// ordinary request restates it as one role=system message.
		if !gjson.GetBytes(request.body, "compaction").Exists() {
			restated := 0
			for _, message := range messages[start:] {
				if message.Get("role").String() == "system" {
					restated++
				}
			}
			if restated != 1 {
				t.Fatalf("tee #%d: %d relocated system messages after the newest compaction block, want 1", request.tee, restated)
			}
		}
		canonical := make([]string, len(messages))
		for idx, message := range messages {
			canonical[idx] = canonicalWithoutCacheControl(t, message.Raw)
		}

		for idx, message := range messages {
			if message.Get("role").String() != "assistant" {
				continue
			}
			for blockIdx, block := range message.Get("content").Array() {
				if block.Get("type").String() != "thinking" {
					continue
				}
				signature := block.Get("signature").String()
				prefix := append([]string(nil), canonical[start:idx]...)
				previous, seen := bindings[signature]
				if seen {
					replayed++
					valid := previous.systemTools == systemTools
					if valid && previous.head == head {
						valid = equalStrings(previous.prefix, prefix)
					} else if valid {
						covered, ok := compactionCovered[head]
						valid = ok && equalStrings(append(append([]string(nil), covered...), prefix[1:]...), previous.prefix)
					}
					if !valid {
						drops = append(drops, fmt.Sprintf("tee #%d messages.%d.content.%d", request.tee, idx, blockIdx))
						continue
					}
				}
				bindings[signature] = thinkingBinding{head: head, prefix: prefix, systemTools: systemTools}
			}
		}
		if gjson.GetBytes(request.body, "compaction").Exists() {
			lastCompactionRequest = append([]string(nil), canonical[start:]...)
		}
	}

	if replayed < 70 {
		t.Fatalf("replayed %d thinking blocks, want the recorded Session's 70+ (placeholder signatures must survive the sanitizer)", replayed)
	}
	if len(drops) > 0 {
		t.Fatalf("%d of %d replayed thinking blocks would fail Anthropic's prefix check:\n%s", len(drops), replayed, strings.Join(drops, "\n"))
	}
}

// TestClaudeCloakSessionHeaderKeepsAffinityAndUserIDAcrossCompaction pins what a
// caller must send for one upstream account and one metadata.user_id across a
// compaction swap. Without a session header the affinity key falls back to a
// hash of the first user and assistant texts, which the swap replaces, and a
// compaction request whose kept messages carry no text has no key at all.
func TestClaudeCloakSessionHeaderKeepsAffinityAndUserIDAcrossCompaction(t *testing.T) {
	forwarded := forwardPixirCompactionSequence(t, "pixir-session-0123456789abcdef", nil)

	wantKey := cliproxyauth.CanonicalSessionID(forwarded[0].headers, forwarded[0].caller, nil)
	wantUserID := gjson.GetBytes(forwarded[0].body, "metadata.user_id").String()
	if wantKey == "" || wantUserID == "" {
		t.Fatalf("first request: affinity key %q, user id present %v", wantKey, wantUserID != "")
	}
	for _, request := range forwarded[1:] {
		if key := cliproxyauth.CanonicalSessionID(request.headers, request.caller, nil); key != wantKey {
			t.Fatalf("tee #%d: affinity key %q, want %q", request.tee, key, wantKey)
		}
		if userID := gjson.GetBytes(request.body, "metadata.user_id").String(); userID != wantUserID {
			t.Fatalf("tee #%d: metadata.user_id changed", request.tee)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
