package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// These tests pin the invariant behind kept thinking under the cloak: within
// one Session, named by the caller's X-Session-ID, every message the proxy
// forwarded before is forwarded again with the same bytes, across any number
// of compactions, while the caller sends it unchanged. Where the proxy cannot
// tell which earlier message a kept one is, it falls back to main's placement
// and counts the reason.

const linPolicy = "CALLER_POLICY_MUST_SURVIVE"

const linOK = `{"role":"assistant","content":"OK"}`

func linBlock(signature string) string {
	return fmt.Sprintf(`{"role":"assistant","content":[{"type":"compaction","content":"summary","signature":%q}]}`, signature)
}

func linUser(text string) string {
	return fmt.Sprintf(`{"role":"user","content":[{"type":"text","text":%q}]}`, text)
}

func linThinking(signature string) string {
	return fmt.Sprintf(`{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":%q},{"type":"text","text":"answer"}]}`, signature)
}

func linBody(messages ...string) []byte {
	return []byte(`{"model":"claude-haiku-5-5","system":"` + linPolicy + `","tools":[],"messages":[` + strings.Join(messages, ",") + `]}`)
}

func linCompacting(messages ...string) []byte {
	body, _ := sjson.SetRawBytes(linBody(messages...), "compaction", []byte(`{"type":"summarize"}`))
	return body
}

// linForwarded is one forwarded request seen per caller message: forms[i] is
// what the proxy forwarded for caller message i, with any system message it
// inserted after it, without cache_control, which the thinking check ignores.
type linForwarded struct {
	caller []string
	forms  []string
	body   []byte
}

func linSend(t *testing.T, session string, body []byte) linForwarded {
	t.Helper()
	ctx := context.Background()
	if session != "" {
		headers := make(http.Header)
		headers.Set("X-Session-ID", session)
		ctx = helps.WithIncomingHeaders(ctx, headers)
	}
	out, plan, cloaked, errCloak := applyCloakingInternal(ctx, &config.Config{}, directClaudeOAuthAuth(), body, "sk-ant-oat-direct-messages-test", false, true, true)
	if errCloak != nil || !cloaked {
		t.Fatalf("applyCloaking: cloaked=%v err=%v", cloaked, errCloak)
	}
	// The upstream accepts every request in these tests.
	plan.Commit()
	return linView(t, body, out)
}

func linView(t *testing.T, caller, forwarded []byte) linForwarded {
	t.Helper()
	view := linForwarded{body: forwarded}
	for _, message := range gjson.GetBytes(caller, "messages").Array() {
		view.caller = append(view.caller, message.Raw)
	}
	for _, message := range gjson.GetBytes(forwarded, "messages").Array() {
		form := canonicalWithoutCacheControl(t, message.Raw)
		if message.Get("role").String() == "system" && len(view.forms) > 0 {
			view.forms[len(view.forms)-1] += form
			continue
		}
		view.forms = append(view.forms, form)
	}
	if len(view.forms) != len(view.caller) {
		t.Fatalf("forwarded %d caller messages, sent %d", len(view.forms), len(view.caller))
	}
	return view
}

func linDated(form string) bool { return strings.Contains(form, "# currentDate") }

// linAssertKept checks that count caller messages, starting at from in prev and
// at at in next, were forwarded with the same bytes.
func linAssertKept(t *testing.T, what string, prev linForwarded, from int, next linForwarded, at, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		if prev.caller[from+i] != next.caller[at+i] {
			t.Fatalf("%s: test bug, caller message %d is not the kept one", what, at+i)
		}
		if prev.forms[from+i] != next.forms[at+i] {
			t.Fatalf("%s: kept message %d (was %d) forwarded differently:\nbefore %s\nafter  %s", what, at+i, from+i, prev.forms[from+i], next.forms[at+i])
		}
	}
}

// linPolicyAfterNewestBlock counts the caller's system prompt restatements the
// model sees: those after the newest compaction block.
func linPolicyAfterNewestBlock(view linForwarded) int {
	start := 0
	for idx, message := range view.caller {
		if strings.Contains(message, `"type":"compaction"`) {
			start = idx
		}
	}
	return strings.Count(strings.Join(view.forms[start:], ""), linPolicy)
}

// Repeated text in one Session: the earlier "continue" was never injected and
// stays plain when new thinking and a new turn follow. Adapted from review
// sol-2 (TestSol2RepeatedPredecessorInjectsPreviouslyPlainUser), which failed
// at 815ec975: the neighbour key matched both occurrences.
func TestClaudeCloakSessionRepeatedTextKeepsPlainKeptUserPlain(t *testing.T) {
	resetCloakLineage(t)
	const session = "lin-repeated-text"
	cont := linUser("continue")
	linSend(t, session, linBody(linUser("start"), linOK, cont))
	// The caller compacts [start], keeps [OK, continue], and appends a reply and
	// another continue.
	first := linSend(t, session, linBody(linBlock("lin-repeat-1"), linOK, cont, linOK, cont))
	if linDated(first.forms[2]) || !linDated(first.forms[4]) {
		t.Fatalf("only the new final continue should carry the date: kept %v, new %v", linDated(first.forms[2]), linDated(first.forms[4]))
	}
	second := linSend(t, session, linBody(linBlock("lin-repeat-1"), linOK, cont, linOK, cont, linThinking("lin-repeat-new"), linUser("next")))
	linAssertKept(t, "append after repeated text", first, 0, second, 0, 5)
	if got := linPolicyAfterNewestBlock(second); got != 1 {
		t.Fatalf("caller policy restated %d times, want 1", got)
	}
}

// A kept run that repeats with a period cannot be placed: keeping four
// messages or keeping two and appending two identical ones are the same
// request. The proxy falls back to main's placement, says so, and keeps what
// it forwarded from then on.
func TestClaudeCloakSessionAmbiguousRepeatFallsBackAndSaysSo(t *testing.T) {
	resetCloakLineage(t)
	const session = "lin-ambiguous-repeat"
	cont := linUser("continue")
	linSend(t, session, linBody(linUser("start"), linOK, cont, linOK, cont))
	before := cloakLineageFallbacks("ambiguous")
	first := linSend(t, session, linBody(linBlock("lin-ambiguous-1"), linOK, cont, linOK, cont))
	if got := cloakLineageFallbacks("ambiguous"); got != before+1 {
		t.Fatalf("ambiguous fallbacks %d, want %d", got, before+1)
	}
	if !linDated(first.forms[2]) || linDated(first.forms[4]) {
		t.Fatalf("fallback should use main's placement, the first user after the block")
	}
	second := linSend(t, session, linBody(linBlock("lin-ambiguous-1"), linOK, cont, linOK, cont, linThinking("lin-ambiguous-new"), linUser("next")))
	linAssertKept(t, "append after fallback", first, 0, second, 0, 5)
}

// A kept user whose text repeats an earlier one keeps its injection across a
// swap that keeps only it. Adapted from review sol-2
// (TestSol2AmbiguousContentLosesEarlierInjection), which failed at 815ec975:
// the ambiguous content key dropped the injection and moved it after the kept
// thinking.
func TestClaudeCloakSessionRepeatedKeptUserKeepsInjectionAcrossSwap(t *testing.T) {
	resetCloakLineage(t)
	const session = "lin-repeated-kept"
	cont := linUser("continue")
	linSend(t, session, linBody(linUser("start"), linThinking("lin-a"), cont))
	first := linSend(t, session, linBody(linBlock("lin-repeat-kept-1"), linThinking("lin-a"), cont, linThinking("lin-b"), cont))
	if linDated(first.forms[2]) || !linDated(first.forms[4]) {
		t.Fatalf("only the new final continue should carry the date")
	}
	// Generate after the final continue, then compact only the prefix before it.
	compacting := linSend(t, session, linCompacting(linBlock("lin-repeat-kept-1"), linThinking("lin-a"), cont, linThinking("lin-b")))
	linAssertKept(t, "compaction request", first, 0, compacting, 0, 4)
	next := linSend(t, session, linBody(linBlock("lin-repeat-kept-2"), cont, linThinking("lin-new-thinking"), linUser("fresh")))
	linAssertKept(t, "swap keeping the repeated user", first, 4, next, 1, 1)
	if linDated(next.forms[3]) {
		t.Fatal("the new user got a second date although a kept user carries one")
	}
	if got := linPolicyAfterNewestBlock(next); got != 1 {
		t.Fatalf("caller policy restated %d times, want 1", got)
	}
}

// Two Sessions with identical openings and identical kept text stay apart.
// Adapted from review sol-2 (TestSol2CrossConversationSuccessorCollision),
// which failed at 815ec975: A's recorded "continue, OK" pair injected B's
// plain kept continue.
func TestClaudeCloakSessionsWithIdenticalOpeningsStayApart(t *testing.T) {
	resetCloakLineage(t)
	cont := linUser("continue")
	opening := linUser("same opening")

	linSend(t, "lin-session-B", linBody(opening, linThinking("lin-b-old"), cont))
	b1 := linSend(t, "lin-session-B", linBody(linBlock("lin-B-1"), linThinking("lin-b-old"), cont, linOK, linUser("B fresh")))
	if linDated(b1.forms[2]) || !linDated(b1.forms[4]) {
		t.Fatalf("B: only the new user should carry the date")
	}

	linSend(t, "lin-session-A", linBody(opening, linThinking("lin-a-old"), linUser("A question")))
	a1 := linSend(t, "lin-session-A", linBody(linBlock("lin-A-1"), linThinking("lin-a-old"), linUser("A question"), linThinking("lin-a-2"), cont))
	if !linDated(a1.forms[4]) {
		t.Fatalf("A: the new continue should carry the date")
	}
	linSend(t, "lin-session-A", linBody(linBlock("lin-A-1"), linThinking("lin-a-old"), linUser("A question"), linThinking("lin-a-2"), cont, linOK, linUser("A fresh")))

	b2 := linSend(t, "lin-session-B", linBody(linBlock("lin-B-1"), linThinking("lin-b-old"), cont, linOK, linUser("B fresh"), linThinking("lin-B-new"), linUser("B next")))
	linAssertKept(t, "B after A's traffic", b1, 0, b2, 0, 5)
	if got := linPolicyAfterNewestBlock(b2); got != 1 {
		t.Fatalf("B: caller policy restated %d times, want 1", got)
	}
}

// Four compactions in a row with new thinking in every epoch. Each compaction
// summarizes the prefix before the injected user, so that user and the
// thinking after it are kept turns of the next block (review sol-1's
// recompaction shape), until the last one summarizes it away and a fresh turn
// takes the injection.
func TestClaudeCloakSessionSuccessiveCompactionsKeepEveryForwardedMessage(t *testing.T) {
	resetCloakLineage(t)
	const session = "lin-successive"
	prev := linSend(t, session, linBody(linUser("start"), linThinking("lin-e0"), linUser("q0")))
	keepFrom := 1
	for epoch := 1; epoch <= 4; epoch++ {
		compacting := linSend(t, session, linCompacting(prev.caller[:keepFrom]...))
		linAssertKept(t, fmt.Sprintf("epoch %d compaction request", epoch), prev, 0, compacting, 0, keepFrom)

		kept := append([]string{}, prev.caller[keepFrom:]...)
		swapped := append(append([]string{linBlock(fmt.Sprintf("lin-successive-%d", epoch))}, kept...),
			linThinking(fmt.Sprintf("lin-e%d-new", epoch)), linUser(fmt.Sprintf("new %d", epoch)))
		swap := linSend(t, session, linBody(swapped...))
		linAssertKept(t, fmt.Sprintf("epoch %d swap", epoch), prev, keepFrom, swap, 1, len(kept))
		if got := linPolicyAfterNewestBlock(swap); got != 1 {
			t.Fatalf("epoch %d: caller policy restated %d times, want 1", epoch, got)
		}

		more := append(append([]string{}, swapped...), linThinking(fmt.Sprintf("lin-e%d-more", epoch)), linUser(fmt.Sprintf("more %d", epoch)))
		appended := linSend(t, session, linBody(more...))
		linAssertKept(t, fmt.Sprintf("epoch %d append", epoch), swap, 0, appended, 0, len(swapped))

		// The next compaction keeps from the dated user, except the last one,
		// which summarizes it and keeps only what follows.
		prev = appended
		keepFrom = -1
		for idx := 1; idx < len(prev.forms); idx++ {
			if linDated(prev.forms[idx]) {
				keepFrom = idx
				break
			}
		}
		if keepFrom < 0 {
			t.Fatalf("epoch %d: no dated user after the block", epoch)
		}
		if epoch == 3 {
			keepFrom++
		}
	}
}

// count_tokens reads the lineage and never writes it: counting kept history
// before the next turn is appended must not move the next generation's
// injection, and must not touch the state on disk. Adapted from review sol-1
// (TestSolCountTokensMustNotEstablishKeptTurnAnchor).
func TestClaudeCloakSessionCountTokensNeverWrites(t *testing.T) {
	resetCloakLineage(t)
	dir := t.TempDir()
	restartCloakLineage(t, dir)
	const session = "lin-count-tokens"
	toolUse := `{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"lin-count-2"},{"type":"tool_use","id":"lin-tool-1","name":"read_file","input":{}}]}`
	toolResult := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"lin-tool-1","content":"ok"}]}`
	linSend(t, session, linBody(linUser("start"), linThinking("lin-count-1"), linUser("old kept question"), toolUse, toolResult))
	snapshot := linDirSnapshot(t, dir)

	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		_, _ = io.ReadAll(req.Body)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"input_tokens":12}`)), Request: req}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("X-Session-ID", session)
	kept := []string{linThinking("lin-count-1"), linUser("old kept question"), toolUse, toolResult}
	countBody := linBody(append([]string{linBlock("lin-count-block")}, kept...)...)
	for i := 0; i < 2; i++ {
		_, errCount := NewClaudeExecutor(&config.Config{}).CountTokens(ctx, directClaudeOAuthAuth(), cliproxyexecutor.Request{Model: "claude-haiku-5-5", Payload: countBody}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: headers, OriginalRequest: countBody})
		if errCount != nil {
			t.Fatalf("count_tokens: %v", errCount)
		}
	}
	if after := linDirSnapshot(t, dir); after != snapshot {
		t.Fatalf("count_tokens changed the cloak state on disk:\nbefore %s\nafter  %s", snapshot, after)
	}

	generation := linSend(t, session, linBody(append(append([]string{linBlock("lin-count-block")}, kept...), linThinking("lin-count-3"), linUser("new question"))...))
	for idx := 1; idx <= 4; idx++ {
		if linDated(generation.forms[idx]) {
			t.Fatalf("kept message %d carries the date after count_tokens", idx)
		}
	}
	if !linDated(generation.forms[6]) {
		t.Fatal("the new user turn did not get the date")
	}
}

func linDirSnapshot(t *testing.T, dir string) string {
	t.Helper()
	var parts []string
	errWalk := filepath.Walk(dir, func(path string, info os.FileInfo, errInfo error) error {
		if errInfo != nil || info.IsDir() {
			return errInfo
		}
		raw, errRead := os.ReadFile(path)
		if errRead != nil {
			return errRead
		}
		sum := sha256.Sum256(raw)
		parts = append(parts, fmt.Sprintf("%s:%s:%d", strings.TrimPrefix(path, dir), hex.EncodeToString(sum[:8]), info.ModTime().UnixNano()))
		return nil
	})
	if errWalk != nil {
		t.Fatal(errWalk)
	}
	return strings.Join(parts, ",")
}

// A restart that keeps the state directory keeps every placement.
func TestClaudeCloakSessionRestartWithPersistedState(t *testing.T) {
	resetCloakLineage(t)
	dir := t.TempDir()
	restartCloakLineage(t, dir)
	const session = "lin-restart-kept"
	linSend(t, session, linBody(linUser("start"), linThinking("lin-r0"), linUser("q0")))
	first := linSend(t, session, linBody(linBlock("lin-restart-1"), linThinking("lin-r0"), linUser("q0"), linThinking("lin-r1"), linUser("anchored")))
	if !linDated(first.forms[4]) {
		t.Fatal("the new user should carry the date")
	}
	restartCloakLineage(t, dir)
	next := linSend(t, session, linBody(linBlock("lin-restart-2"), linUser("anchored"), linThinking("lin-r2"), linUser("after restart")))
	linAssertKept(t, "swap after restart", first, 4, next, 1, 1)
	if linDated(next.forms[3]) {
		t.Fatal("the new user got a second date after the restart")
	}
}

// A restart that loses the state cannot know where the kept turns were
// injected. The proxy falls back to main's placement for that request, counts
// it, and keeps what it forwarded from then on.
func TestClaudeCloakSessionRestartWithLostStateFallsBackAndSaysSo(t *testing.T) {
	resetCloakLineage(t)
	restartCloakLineage(t, t.TempDir())
	const session = "lin-restart-lost"
	linSend(t, session, linBody(linUser("start"), linThinking("lin-l0"), linUser("q0")))
	linSend(t, session, linBody(linBlock("lin-lost-1"), linThinking("lin-l0"), linUser("q0"), linThinking("lin-l1"), linUser("anchored")))

	restartCloakLineage(t, t.TempDir())
	before := cloakLineageFallbacks("unknown_session")
	lost := linSend(t, session, linBody(linBlock("lin-lost-1"), linThinking("lin-l0"), linUser("q0"), linThinking("lin-l1"), linUser("anchored"), linThinking("lin-l2"), linUser("after restart")))
	if got := cloakLineageFallbacks("unknown_session"); got != before+1 {
		t.Fatalf("unknown_session fallbacks %d, want %d", got, before+1)
	}
	if !linDated(lost.forms[2]) {
		t.Fatal("fallback should use main's placement, the first user after the block")
	}
	next := linSend(t, session, linBody(linBlock("lin-lost-1"), linThinking("lin-l0"), linUser("q0"), linThinking("lin-l1"), linUser("anchored"), linThinking("lin-l2"), linUser("after restart"), linThinking("lin-l3"), linUser("later")))
	linAssertKept(t, "append after the fallback", lost, 0, next, 0, 7)
}

// Without X-Session-ID the proxy has no lineage and forwards exactly what main
// forwards, compaction or not, and counts the compaction requests it could
// not place.
func TestClaudeCloakWithoutSessionHeaderMatchesMain(t *testing.T) {
	resetCloakLineage(t)
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	digest := func(cases [][]byte) string {
		h := sha256.New()
		for _, body := range cases {
			h.Write(checkSystemInstructionsWithSigningModeAt(body, false, false, "2.1.280", "cli", "", at, false, "", ""))
			h.Write(relocateClaudeSystemPromptForCountTokens(body, false, ""))
		}
		return hex.EncodeToString(h.Sum(nil))
	}
	// Review sol-1's three no-compaction payloads; the digest is main's.
	noCompaction := [][]byte{
		[]byte(`{"model":"claude-haiku-5-5","system":"caller","messages":[{"role":"user","content":"hello"}]}`),
		[]byte(`{"model":"claude-sonnet-4-5","system":"caller","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]},{"role":"assistant","content":"yes"},{"role":"user","content":"next"}]}`),
		[]byte(`{"model":"claude-haiku-5-5","system":[{"type":"text","text":"caller"}],"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"1","content":"ok"}]}]}`),
	}
	if got := digest(noCompaction); got != "5627016d392ee9877f3ddd243814426b9b5c3ec5787195fab9868b0bf3a825c5" {
		t.Fatalf("no-compaction digest %s differs from main", got)
	}
	// Captured on main at 1ab1cf4b with the same payloads.
	withCompaction := [][]byte{
		[]byte(`{"model":"claude-haiku-5-5","system":"caller","messages":[{"role":"assistant","content":[{"type":"compaction","content":"summary","signature":"parity-1"}]},{"role":"user","content":"kept"},{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"parity-t"},{"type":"text","text":"ok"}]},{"role":"user","content":"new"}]}`),
		[]byte(`{"model":"claude-sonnet-4-5","system":[{"type":"text","text":"caller"}],"messages":[{"role":"assistant","content":[{"type":"compaction","content":"summary","signature":"parity-2"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"1","content":"ok"}]},{"role":"assistant","content":"yes"},{"role":"user","content":[{"type":"text","text":"next"}]}]}`),
		[]byte(`{"model":"claude-haiku-5-5","system":"caller","compaction":{"type":"summarize"},"messages":[{"role":"assistant","content":[{"type":"compaction","content":"summary","signature":"parity-3"}]},{"role":"user","content":"kept"}]}`),
	}
	if got := digest(withCompaction); got != "b5b06655bf3ba4908732061987a8a0df341b50b10bfd1877dc84c2a7f919608e" {
		t.Fatalf("compaction digest without a session header %s differs from main", got)
	}

	before := cloakLineageFallbacks("no_session")
	view := linSend(t, "", withCompaction[0])
	if got := cloakLineageFallbacks("no_session"); got != before+1 {
		t.Fatalf("no_session fallbacks %d, want %d", got, before+1)
	}
	if !linDated(view.forms[1]) || linDated(view.forms[3]) {
		t.Fatal("without a session header the first user after the block carries the date, as on main")
	}
}

// A swap whose kept turns are not a run ending where the lineage ends, here
// because the caller edited a kept turn, cannot be placed: main's placement,
// counted as unaligned.
func TestClaudeCloakSessionEditedKeptTurnFallsBackAndSaysSo(t *testing.T) {
	resetCloakLineage(t)
	const session = "lin-edited-kept"
	linSend(t, session, linBody(linUser("start"), linThinking("lin-x0"), linUser("q0")))
	linSend(t, session, linBody(linBlock("lin-edit-1"), linThinking("lin-x0"), linUser("q0"), linThinking("lin-x1"), linUser("anchored")))
	before := cloakLineageFallbacks("unaligned")
	edited := linSend(t, session, linBody(linBlock("lin-edit-2"), linUser("anchored, edited"), linThinking("lin-x2"), linUser("next")))
	if got := cloakLineageFallbacks("unaligned"); got != before+1 {
		t.Fatalf("unaligned fallbacks %d, want %d", got, before+1)
	}
	if !linDated(edited.forms[1]) || linDated(edited.forms[3]) {
		t.Fatal("fallback should use main's placement, the first user after the block")
	}
}

// Within an epoch the caller may resend its last turn edited, for example
// after a failed request. Everything before the edit keeps its bytes and the
// edited turn is new, so it takes the injection the old one had. No fallback.
func TestClaudeCloakSessionEditedLastTurnWithinAnEpochKeepsThePrefix(t *testing.T) {
	resetCloakLineage(t)
	const session = "lin-edited-last"
	linSend(t, session, linBody(linUser("start"), linThinking("lin-y0"), linUser("q0")))
	first := linSend(t, session, linBody(linBlock("lin-edit-last-1"), linThinking("lin-y0"), linUser("q0"), linThinking("lin-y1"), linUser("typo")))
	before := cloakLineageFallbacks("unaligned") + cloakLineageFallbacks("ambiguous")
	retried := linSend(t, session, linBody(linBlock("lin-edit-last-1"), linThinking("lin-y0"), linUser("q0"), linThinking("lin-y1"), linUser("fixed")))
	if got := cloakLineageFallbacks("unaligned") + cloakLineageFallbacks("ambiguous"); got != before {
		t.Fatalf("an edited last turn counted %d fallbacks", got-before)
	}
	linAssertKept(t, "resend with an edited last turn", first, 0, retried, 0, 4)
	if !linDated(retried.forms[4]) {
		t.Fatal("the edited last turn did not take the injection")
	}
}

// Unrelated Sessions do not move an active Session's injection. Adapted from
// review sol-1 (TestSolCapacityEvictionMustNotMoveExistingInjection), where a
// full cache cleared every live entry; the store now forgets only its least
// recently used Session past the bound.
func TestClaudeCloakSessionUnrelatedTrafficDoesNotMoveAnInjection(t *testing.T) {
	resetCloakLineage(t)
	const session = "lin-capacity-live"
	linSend(t, session, linBody(linUser("start"), linThinking("lin-c0"), linUser("q0")))
	first := linSend(t, session, linBody(linBlock("lin-capacity"), linThinking("lin-c0"), linUser("q0"), linThinking("lin-c1"), linUser("anchored")))
	for i := 0; i < 1000; i++ {
		other := fmt.Sprintf("lin-capacity-other-%d", i)
		linSend(t, other, linBody(linUser("start"), linThinking("lin-c0"), linUser("q0")))
	}
	next := linSend(t, session, linBody(linBlock("lin-capacity"), linThinking("lin-c0"), linUser("q0"), linThinking("lin-c1"), linUser("anchored"), linThinking("lin-c2"), linUser("next")))
	linAssertKept(t, "after 1000 unrelated Sessions", first, 0, next, 0, 5)
}
