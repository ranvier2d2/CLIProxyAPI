package executor

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// These tests pin the invariant behind kept thinking under the cloak: every
// message the proxy has already forwarded is forwarded again with the same
// bytes, across any number of compactions, while the caller sends it unchanged.

var lineageNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func lineageBody(signature string, tail ...string) []byte {
	messages := append([]string{fmt.Sprintf(`{"role":"assistant","content":[{"type":"compaction","content":"summary","signature":%q}]}`, signature)}, tail...)
	return []byte(`{"model":"claude-haiku-5-5","system":"CALLER_POLICY_MUST_SURVIVE","tools":[],"messages":[` + strings.Join(messages, ",") + `]}`)
}

func lineageCloak(body []byte) []byte {
	return checkSystemInstructionsWithSigningModeAt(body, false, false, "2.1.280", "cli", "", lineageNow, false, "", "")
}

var lineageKept = []string{
	`{"role":"assistant","content":[{"type":"thinking","thinking":"kept","signature":"thinking-1"},{"type":"text","text":"kept answer"}]}`,
	`{"role":"user","content":[{"type":"text","text":"old kept question"}]}`,
	`{"role":"assistant","content":[{"type":"thinking","thinking":"kept2","signature":"thinking-2"},{"type":"tool_use","id":"tool-1","name":"read_file","input":{}}]}`,
}

const lineageNewUser = `{"role":"user","content":[{"type":"text","text":"new question"}]}`

func lineageAssertPrefix(t *testing.T, input, output []byte, count int) {
	t.Helper()
	a, b := gjson.GetBytes(input, "messages").Array(), gjson.GetBytes(output, "messages").Array()
	for i := 0; i < count; i++ {
		if a[i].Raw != b[i].Raw {
			t.Fatalf("message %d before the anchor changed", i)
		}
	}
}

// A later compaction can keep a user turn that carried an earlier epoch's
// injections, with thinking produced after it. That turn has to keep them.
// Ported from review sol-1, where it failed at 2edda956.
func TestClaudeCloakKeepsEarlierEpochInjectionAcrossRecompaction(t *testing.T) {
	tail := []string{lineageKept[0], lineageNewUser}
	for epoch := 1; epoch <= 3; epoch++ {
		sig := fmt.Sprintf("lineage-successive-%d", epoch)
		_ = lineageCloak(lineageBody(sig, tail...))
		anchor := len(tail)
		tail = append(tail,
			fmt.Sprintf(`{"role":"assistant","content":[{"type":"thinking","thinking":"epoch %d","signature":"lineage-new-thinking-%d"},{"type":"text","text":"answer"}]}`, epoch, epoch),
			lineageNewUser)
		beforeSwapRaw := lineageBody(sig, tail...)
		beforeSwap := lineageCloak(beforeSwapRaw)
		// Compact only the prefix before this epoch's last-but-one user turn, so
		// that turn and the thinking after it are kept turns of the next block.
		messages := gjson.GetBytes(beforeSwapRaw, "messages").Array()
		prefixRaw := make([]string, anchor)
		for i := range prefixRaw {
			prefixRaw[i] = messages[i].Raw
		}
		compacting, _ := sjson.SetRawBytes(beforeSwapRaw, "messages", []byte("["+strings.Join(prefixRaw, ",")+"]"))
		compacting, _ = sjson.SetRawBytes(compacting, "compaction", []byte(`{"type":"summarize"}`))
		lineageAssertPrefix(t, beforeSwap, lineageCloak(compacting), anchor)
		if epoch == 3 {
			break
		}
		tail = nil
		for _, msg := range messages[anchor:] {
			tail = append(tail, msg.Raw)
		}
		afterSwap := lineageCloak(lineageBody(fmt.Sprintf("lineage-successive-%d", epoch+1), tail...))
		// Forwarded beforeSwap has one inserted system message after messages[2]
		// at most, so locate the kept user by its position among user turns.
		previousKeptUser := forwardedUserMessages(beforeSwap)[len(forwardedUserMessages(beforeSwap))-2]
		newKeptUser := gjson.GetBytes(afterSwap, "messages.1").Raw
		if previousKeptUser != newKeptUser {
			t.Errorf("swap %d -> %d changed the kept user turn that precedes new thinking:\nbefore %s\nafter  %s", epoch, epoch+1, previousKeptUser, newKeptUser)
		}
		if strings.Count(string(afterSwap), "CALLER_POLICY_MUST_SURVIVE") != 1 {
			t.Errorf("swap %d -> %d: caller policy restated %d times after the block, want 1", epoch, epoch+1, strings.Count(string(afterSwap), "CALLER_POLICY_MUST_SURVIVE"))
		}
	}
}

func forwardedUserMessages(body []byte) []string {
	var users []string
	for _, message := range gjson.GetBytes(body, "messages").Array() {
		if message.Get("role").String() == "user" {
			users = append(users, message.Raw)
		}
	}
	return users
}

// Counting tokens for kept history before the next user turn is appended must
// not decide where the next generation request injects. Ported from review
// sol-1, where count_tokens recorded the kept user as the anchor.
func TestClaudeCloakCountTokensDoesNotPlaceTheAnchor(t *testing.T) {
	const sig = "lineage-count-before-generation"
	countBody := lineageBody(sig, lineageKept...)
	_ = relocateClaudeSystemPromptForCountTokens(countBody, false)
	out := lineageCloak(lineageBody(sig, append(append([]string{}, lineageKept...), lineageNewUser)...))
	if kept := gjson.GetBytes(out, "messages.2").Raw; kept != gjson.GetBytes(countBody, "messages.2").Raw {
		t.Fatalf("count_tokens on kept history made the next request edit kept message 2:\n%s", kept)
	}
	if !strings.Contains(gjson.GetBytes(out, "messages.4").Raw, "# currentDate") {
		t.Fatalf("the new user turn did not get the date reminder: %s", gjson.GetBytes(out, "messages.4").Raw)
	}
}

// Traffic from unrelated conversations must not move an active conversation's
// injections. Ported from review sol-1, where 4,097 distinct blocks cleared
// every live entry.
func TestClaudeCloakUnrelatedTrafficDoesNotMoveAnInjection(t *testing.T) {
	const sig = "lineage-capacity-live"
	tail := append(append([]string{}, lineageKept...), lineageNewUser)
	first := lineageCloak(lineageBody(sig, tail...))
	for i := 0; i <= 4096; i++ {
		unrelated := fmt.Sprintf(`{"role":"user","content":[{"type":"text","text":"unrelated %d"}]}`, i)
		_ = lineageCloak(lineageBody(fmt.Sprintf("lineage-capacity-%d", i), lineageKept[0], unrelated))
	}
	tail = append(tail, `{"role":"assistant","content":"new response"}`, lineageNewUser)
	next := lineageCloak(lineageBody(sig, tail...))
	if before, after := gjson.GetBytes(first, "messages.4").Raw, gjson.GetBytes(next, "messages.4").Raw; before != after {
		t.Fatalf("unrelated traffic moved the injection off messages.4:\nbefore %s\nafter  %s", before, after)
	}
	if strings.Count(string(next), "# currentDate") != 1 {
		t.Fatalf("date reminder appears %d times, want 1", strings.Count(string(next), "# currentDate"))
	}
}

// The caller can compact right after the injected turn's response, before any
// request carries that turn with its successor. The kept turn then follows the
// new block directly and is found by its bytes alone.
func TestClaudeCloakKeepsInjectionWhenCompactedRightAfterItsTurn(t *testing.T) {
	user := `{"role":"user","content":[{"type":"text","text":"compacted right after"}]}`
	first := lineageCloak(lineageBody("lineage-immediate-1", lineageKept[0], user))
	compacting, _ := sjson.SetRawBytes(lineageBody("lineage-immediate-1", lineageKept[0]), "compaction", []byte(`{"type":"summarize"}`))
	_ = lineageCloak(compacting)
	after := lineageCloak(lineageBody("lineage-immediate-2",
		user,
		`{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"lineage-immediate-thinking"},{"type":"text","text":"answer"}]}`,
		lineageNewUser))
	if before, kept := gjson.GetBytes(first, "messages.2").Raw, gjson.GetBytes(after, "messages.1").Raw; before != kept {
		t.Fatalf("the kept turn changed after the swap:\nbefore %s\nafter  %s", before, kept)
	}
	if got := strings.Count(string(after), "# currentDate"); got != 1 {
		t.Fatalf("date reminder appears %d times, want 1 (on the kept turn only)", got)
	}
}
