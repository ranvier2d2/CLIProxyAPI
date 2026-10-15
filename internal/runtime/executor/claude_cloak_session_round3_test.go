package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// Two compactions without a generation request between them are ordinary
// prefix replacements. The second compaction carries C1 plus more old turns,
// while the rest of the old tail remains outside that compaction request.
// Ported from review sol-3, which failed at 9c63cb98: the second compaction
// fell back and replaced the lineage, losing the older retained tail.
func TestSol3BackToBackCompactionsKeepOlderTail(t *testing.T) {
	resetCloakLineage(t)
	const sid = "sol3-back-to-back"
	u0, u1, u2, u3 := linUser("start"), linUser("one"), linUser("two"), linUser("three")
	a0, a1, a2 := linThinking("sol3-a0"), linThinking("sol3-a1"), linThinking("sol3-a2")
	original := linSend(t, sid, linBody(u0, a0, u1, a1, u2, a2, u3))
	linSend(t, sid, linCompacting(u0, a0, u1))
	before := cloakLineageFallbacks("unaligned")
	secondCompact := linSend(t, sid, linCompacting(linBlock("sol3-c1"), a1, u2))
	t.Logf("second compact fallback delta=%d; previously plain u2 now dated=%t", cloakLineageFallbacks("unaligned")-before, linDated(secondCompact.forms[2]))
	if original.forms[4] != secondCompact.forms[2] {
		t.Error("second compaction changed the previously plain u2")
	}
	next := linSend(t, sid, linBody(linBlock("sol3-c2"), a2, u3, linThinking("sol3-a3"), linUser("fresh")))
	t.Logf("final swap cumulative unaligned fallbacks=%d; previously plain u3 now dated=%t", cloakLineageFallbacks("unaligned")-before, linDated(next.forms[2]))
	linAssertKept(t, "tail retained through two compactions", original, 5, next, 1, 2)
}

// A request planned before a swap and committed after it must not overwrite
// the newer lineage. Ported from review sol-3, which failed at 9c63cb98: the
// delayed commit won, and the continuation silently moved anchor2's date.
func TestSol3DelayedDuplicateChangesActualForwardedBytesWithoutFallback(t *testing.T) {
	resetCloakLineage(t)
	const sid = "sol3-delayed-duplicate"
	linSend(t, sid, linBody(linUser("start"), linThinking("sol3-r0"), linUser("q0")))
	epoch1 := []string{linBlock("sol3-r-c1"), linThinking("sol3-r0"), linUser("q0"), linThinking("sol3-r1"), linUser("anchor1")}
	linSend(t, sid, linBody(epoch1...))
	appended := append(append([]string{}, epoch1...), linThinking("sol3-r2"), linUser("q2"))
	stale := helps.PlanClaudeCloak(linBody(appended...), sid, "2026-10-15")
	linSend(t, sid, linBody(appended...))
	linSend(t, sid, linCompacting(epoch1...))
	epoch2 := []string{linBlock("sol3-r-c2"), appended[5], appended[6], linThinking("sol3-r3"), linUser("anchor2")}
	first := linSend(t, sid, linBody(epoch2...))
	stale.Commit()
	before := cloakLineageFallbacks("unaligned") + cloakLineageFallbacks("ambiguous")
	next := linSend(t, sid, linBody(append(epoch2, linThinking("sol3-r4"), linUser("next"))...))
	after := cloakLineageFallbacks("unaligned") + cloakLineageFallbacks("ambiguous")
	t.Logf("actual cloak: anchor2 date before=%t after=%t; next user dated=%t; fallback delta=%d", linDated(first.forms[4]), linDated(next.forms[4]), linDated(next.forms[6]), after-before)
	linAssertKept(t, "after delayed duplicate commit", first, 0, next, 0, len(epoch2))
}

// A request the upstream rejects never becomes lineage: the Session's next
// request aligns against the last accepted one.
func TestClaudeCloakSessionRejectedRequestNeverBecomesLineage(t *testing.T) {
	resetCloakLineage(t)
	const session = "lin-rejected"
	linSend(t, session, linBody(linUser("start"), linThinking("lin-j0"), linUser("q0")))
	first := linSend(t, session, linBody(linBlock("lin-rejected-1"), linThinking("lin-j0"), linUser("q0"), linThinking("lin-j1"), linUser("anchored")))

	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		_, _ = io.ReadAll(req.Body)
		return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"invalid_request_error","message":"rejected"}}`)), Request: req}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("X-Session-ID", session)
	rejected := linBody(linUser("an unrelated history the upstream rejects"))
	if _, errExec := NewClaudeExecutor(&config.Config{}).Execute(ctx, directClaudeOAuthAuth(), cliproxyexecutor.Request{Model: "claude-haiku-5-5", Payload: rejected}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: headers, OriginalRequest: rejected}); errExec == nil {
		t.Fatal("the upstream rejection did not surface")
	}

	before := cloakLineageFallbacks("unaligned")
	next := linSend(t, session, linBody(linBlock("lin-rejected-1"), linThinking("lin-j0"), linUser("q0"), linThinking("lin-j1"), linUser("anchored"), linThinking("lin-j2"), linUser("next")))
	if got := cloakLineageFallbacks("unaligned"); got != before {
		t.Fatalf("the rejected request became lineage: %d unaligned fallbacks", got-before)
	}
	linAssertKept(t, "after a rejected request", first, 0, next, 0, 5)
}
