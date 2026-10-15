package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// claudeCloakAnchor is a caller user message that carries the cloak's date
// reminder and after which the relocated caller system prompt goes.
type claudeCloakAnchor struct {
	index int
	date  string
}

// planClaudeCloakAnchors decides which caller user messages carry the cloak's
// injections, before any of them is applied.
//
// Anthropic keeps a replayed thinking block only while every message before it
// arrives as it did when the block was produced. After an on-demand compaction
// the check starts at the newest compaction block, and the kept turns after it
// must arrive exactly as the proxy forwarded them before the swap, possibly
// several compactions ago. So:
//
//   - Without a compaction block the anchor is the first user message, dated
//     today. That position never moves, and nothing is recorded.
//   - With one, every user message the cloak injected before is injected again
//     with its recorded date, whatever its position or epoch. The memo finds it
//     by its own bytes and a neighbour's.
//   - Only when none of them follows the newest block does the last user
//     message get a fresh injection, dated today. It is the turn the request is
//     about, never a kept one. A compaction request summarizes history the
//     proxy already forwarded, so it gets none.
//
// With record the decision goes to the memo. count_tokens plans without it, so
// counting kept history never decides where the next generation injects.
func planClaudeCloakAnchors(payload []byte, now time.Time, record bool) []claudeCloakAnchor {
	today := claudeCodeLocalDate(now)
	messages := gjson.GetBytes(payload, "messages").Array()
	head := -1
	for idx, message := range messages {
		if claudeMessageHasCompaction(message) {
			head = idx
		}
	}
	if head < 0 {
		first := firstClaudeUserMessageIndex(payload)
		if first < 0 {
			return nil
		}
		return []claudeCloakAnchor{{index: first, date: today}}
	}

	memo := helps.ClaudeCloakAnchors()
	memo.Reload()
	keys := claudeCloakAnchorKeys{messages: messages, hashes: make([]string, len(messages))}
	var anchors []claudeCloakAnchor
	fresh := !gjson.GetBytes(payload, "compaction").Exists()
	for idx, message := range messages {
		if message.Get("role").String() != "user" {
			continue
		}
		if date, ok := keys.lookup(memo, idx, record); ok {
			anchors = append(anchors, claudeCloakAnchor{index: idx, date: date})
			if idx > head {
				fresh = false
			}
		}
	}
	if fresh {
		for idx := len(messages) - 1; idx > head; idx-- {
			if messages[idx].Get("role").String() == "user" {
				anchors = append(anchors, claudeCloakAnchor{index: idx, date: today})
				break
			}
		}
	}
	if record {
		keys.remember(memo, anchors)
	}
	return anchors
}

func claudeMessageHasCompaction(message gjson.Result) bool {
	for _, block := range message.Get("content").Array() {
		if block.Get("type").String() == "compaction" {
			return true
		}
	}
	return false
}

// claudeCloakAnchorKeys derives memo keys from the caller's messages. Each
// injected message is recorded under three keys:
//
//   - "s": itself and its successor. Successors carry signatures and tool ids,
//     so this key is unique, and it survives the swap that summarizes the
//     message's predecessor.
//   - "p": its predecessor and itself, for the requests where it is still the
//     last message.
//   - "c": itself alone, consulted only right after a compaction block, where
//     the predecessor is gone and no successor was seen yet. A caller can send
//     equal bytes twice, so a "c" key seen on a message the cloak did not
//     inject, or with another date, turns ambiguous and stops matching.
type claudeCloakAnchorKeys struct {
	messages []gjson.Result
	hashes   []string
}

// hash identifies a message by its bytes without cache_control, which callers
// move between requests and Anthropic leaves out of the thinking check.
func (k *claudeCloakAnchorKeys) hash(idx int) string {
	if idx < 0 || idx >= len(k.messages) {
		return "none"
	}
	if k.hashes[idx] == "" {
		sum := sha256.Sum256([]byte(claudeMessageWithoutCacheControl(k.messages[idx])))
		k.hashes[idx] = hex.EncodeToString(sum[:16])
	}
	return k.hashes[idx]
}

func (k *claudeCloakAnchorKeys) successorKey(idx int) string {
	if idx+1 >= len(k.messages) {
		return ""
	}
	return "s" + k.hash(idx) + k.hash(idx+1)
}

func (k *claudeCloakAnchorKeys) predecessorKey(idx int) string {
	return "p" + k.hash(idx-1) + k.hash(idx)
}

func (k *claudeCloakAnchorKeys) contentKey(idx int) string {
	return "c" + k.hash(idx)
}

func (k *claudeCloakAnchorKeys) lookup(memo *helps.ClaudeCloakAnchorMemo, idx int, touch bool) (string, bool) {
	candidates := []string{k.successorKey(idx), k.predecessorKey(idx)}
	if idx > 0 && claudeMessageHasCompaction(k.messages[idx-1]) {
		candidates = append(candidates, k.contentKey(idx))
	}
	for _, key := range candidates {
		if key == "" {
			continue
		}
		if mark, ok := memo.Lookup(key, touch); ok && !mark.Ambiguous {
			return mark.Date, true
		}
	}
	return "", false
}

func (k *claudeCloakAnchorKeys) remember(memo *helps.ClaudeCloakAnchorMemo, anchors []claudeCloakAnchor) {
	marks := make(map[string]helps.ClaudeCloakAnchorMark)
	anchored := make(map[int]bool, len(anchors))
	for _, anchor := range anchors {
		anchored[anchor.index] = true
		mark := helps.ClaudeCloakAnchorMark{Date: anchor.date}
		marks[k.predecessorKey(anchor.index)] = mark
		if key := k.successorKey(anchor.index); key != "" {
			marks[key] = mark
		}
		content := k.contentKey(anchor.index)
		if previous, ok := marks[content]; ok && previous != mark {
			mark = helps.ClaudeCloakAnchorMark{Ambiguous: true}
		} else if previous, ok := memo.Lookup(content, false); ok && previous != mark {
			mark = helps.ClaudeCloakAnchorMark{Ambiguous: true}
		}
		marks[content] = mark
	}
	for idx, message := range k.messages {
		if anchored[idx] || message.Get("role").String() != "user" {
			continue
		}
		content := k.contentKey(idx)
		if _, ok := marks[content]; ok {
			marks[content] = helps.ClaudeCloakAnchorMark{Ambiguous: true}
		} else if _, ok := memo.Lookup(content, false); ok {
			marks[content] = helps.ClaudeCloakAnchorMark{Ambiguous: true}
		}
	}
	memo.Remember(marks)
}

func claudeMessageWithoutCacheControl(message gjson.Result) string {
	raw := message.Raw
	if message.Get("cache_control").Exists() {
		raw, _ = sjson.Delete(raw, "cache_control")
	}
	for i, block := range message.Get("content").Array() {
		if block.Get("cache_control").Exists() {
			raw, _ = sjson.Delete(raw, fmt.Sprintf("content.%d.cache_control", i))
		}
		for j, inner := range block.Get("content").Array() {
			if inner.Get("cache_control").Exists() {
				raw, _ = sjson.Delete(raw, fmt.Sprintf("content.%d.content.%d.cache_control", i, j))
			}
		}
	}
	return raw
}
