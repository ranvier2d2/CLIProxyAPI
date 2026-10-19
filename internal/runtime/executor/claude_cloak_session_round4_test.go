package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// linRejected sends body through Execute or ExecuteStream against an upstream
// that rejects it.
func linRejected(t *testing.T, session string, body []byte, stream bool) {
	t.Helper()
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		_, _ = io.ReadAll(req.Body)
		return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"synthetic"}}`)), Request: req}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: http.Header{"X-Session-Id": {session}}, OriginalRequest: body}
	req := cliproxyexecutor.Request{Model: "claude-haiku-5-5", Payload: body}
	var errExec error
	if stream {
		_, errExec = NewClaudeExecutor(&config.Config{}).ExecuteStream(ctx, directClaudeOAuthAuth(), req, opts)
	} else {
		_, errExec = NewClaudeExecutor(&config.Config{}).Execute(ctx, directClaudeOAuthAuth(), req, opts)
	}
	if errExec == nil {
		t.Fatal("synthetic rejection did not surface")
	}
}

// A pending cut is consumed only by the swap that follows it and replaced only
// by another compaction request. Every other request between a compaction
// request and its swap keeps it: retries of either compaction request, an
// unchanged resend of the full transcript, and a rejected request. Ported
// from review sol-4, whose resend-original case failed at 5859bec5: the
// resend cleared the cut, and the second compaction injected into plain u2.
func TestSol4TwoCompactionsThenRecompactionAndRetry(t *testing.T) {
	for _, extra := range []string{"none", "retry-first", "retry-second", "recompact-second", "resend-original", "failed-between"} {
		t.Run(extra, func(t *testing.T) {
			resetCloakLineage(t)
			const sid = "sol4-transitions"
			u0, u1, u2, u3 := linUser("zero"), linUser("one"), linUser("two"), linUser("three")
			a0, a1, a2 := linThinking("s4a0"), linThinking("s4a1"), linThinking("s4a2")
			originalBody := linBody(u0, a0, u1, a1, u2, a2, u3)
			original := linSend(t, sid, originalBody)
			firstBody := linCompacting(u0, a0, u1)
			first := linSend(t, sid, firstBody)
			linAssertKept(t, "first compact", original, 0, first, 0, 3)
			if extra == "retry-first" {
				retry := linSend(t, sid, firstBody)
				linAssertKept(t, "first retry", first, 0, retry, 0, 3)
			}
			if extra == "resend-original" {
				retry := linSend(t, sid, originalBody)
				linAssertKept(t, "previous request unchanged", original, 0, retry, 0, 7)
			}
			if extra == "failed-between" {
				linRejected(t, sid, linBody(linUser("not accepted")), false)
			}
			secondBody := linCompacting(linBlock("s4c1"), a1, u2)
			second := linSend(t, sid, secondBody)
			linAssertKept(t, "second compact", original, 3, second, 1, 2)
			if extra == "retry-second" || extra == "recompact-second" {
				retry := linSend(t, sid, secondBody)
				linAssertKept(t, "second retry and recompaction", second, 0, retry, 0, 3)
			}
			if extra == "recompact-second" {
				third := linSend(t, sid, linCompacting(linBlock("s4c2"), a2, u3))
				linAssertKept(t, "third compact", original, 5, third, 1, 2)
				linSend(t, sid, linBody(linBlock("s4c3"), linThinking("s4new"), linUser("fresh")))
				return
			}
			next := linSend(t, sid, linBody(linBlock("s4c2"), a2, u3, linThinking("s4new"), linUser("fresh")))
			linAssertKept(t, "last swap", original, 5, next, 1, 2)
			retry := linSend(t, sid, linBody(next.caller...))
			linAssertKept(t, "unchanged request", next, 0, retry, 0, len(next.caller))
		})
	}
}

// The same rule for requests that change the transcript after the cut: an
// ordinary turn appended, or the last turn edited, leaves the summarized
// prefix as it was, so the cut stays. A request that changes the summarized
// prefix itself replaces the history the cut points into; the next swap
// falls back and says so instead of guessing.
func TestClaudeCloakSessionPendingCutOutlivesRequestsThatKeepTheSummarizedPrefix(t *testing.T) {
	for _, between := range []string{"append", "edit-after-cut", "edit-before-cut"} {
		t.Run(between, func(t *testing.T) {
			resetCloakLineage(t)
			const sid = "lin-pending-cut"
			u0, u1, u2, u3 := linUser("zero"), linUser("one"), linUser("two"), linUser("three")
			a0, a1, a2 := linThinking("lin-pc-a0"), linThinking("lin-pc-a1"), linThinking("lin-pc-a2")
			original := linSend(t, sid, linBody(u0, a0, u1, a1, u2, a2, u3))
			linSend(t, sid, linCompacting(u0, a0, u1))
			switch between {
			case "append":
				linSend(t, sid, linBody(u0, a0, u1, a1, u2, a2, u3, linThinking("lin-pc-a3"), linUser("four")))
			case "edit-after-cut":
				linSend(t, sid, linBody(u0, a0, u1, a1, u2, a2, linUser("three, edited")))
			case "edit-before-cut":
				linSend(t, sid, linBody(u0, a0, linUser("one, edited"), a1, u2, a2, u3))
			}
			before := cloakLineageFallbacks("unaligned")
			second := linSend(t, sid, linCompacting(linBlock("lin-pc-c1"), a1, u2))
			if between == "edit-before-cut" {
				if got := cloakLineageFallbacks("unaligned"); got != before+1 {
					t.Fatalf("a swap after the summarized prefix changed counted %d unaligned, want 1", got-before)
				}
				return
			}
			linAssertKept(t, "compaction after "+between, original, 3, second, 1, 2)
		})
	}
}

// An oversized request planned before a newer epoch committed must not erase
// that epoch. Ported from review sol-4, which failed at 5859bec5: the stale
// oversized commit dropped the lineage before checking its revision, and the
// next request fell back as an unknown Session and dated plain message 2.
func TestSol4DelayedOversizeChangesForwardedBytes(t *testing.T) {
	resetCloakLineage(t)
	const sid = "sol4-oversize-bytes"
	initial := []string{linUser("initial"), linThinking("old"), linUser("kept")}
	linSend(t, sid, linBody(initial...))
	huge := append([]string{}, initial...)
	for i := 0; i < 4098; i++ {
		huge = append(huge, linUser(fmt.Sprintf("oversized-%d", i)))
	}
	delayed := helps.PlanClaudeCloak(linBody(huge...), sid, "2026-10-15")
	epoch := []string{linBlock("new-epoch"), initial[1], initial[2], linThinking("fresh"), linUser("anchor")}
	first := linSend(t, sid, linBody(epoch...))
	before := cloakLineageFallbacks(helps.ClaudeCloakRecoveryStale)
	delayed.Commit()
	if got := cloakLineageFallbacks(helps.ClaudeCloakRecoveryStale); got != before+1 {
		t.Fatalf("the delayed oversized commit counted %d stale, want 1", got-before)
	}
	next := linSend(t, sid, linBody(append(epoch, linThinking("latest"), linUser("next"))...))
	linAssertKept(t, "after stale oversized commit", first, 0, next, 0, len(epoch))
}
