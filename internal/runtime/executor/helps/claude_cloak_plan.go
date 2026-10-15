package helps

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Reasons a compacted request falls back to main's placement.
const (
	ClaudeCloakFallbackNoSession      = "no_session"
	ClaudeCloakFallbackUnknownSession = "unknown_session"
	ClaudeCloakFallbackUnaligned      = "unaligned"
	ClaudeCloakFallbackAmbiguous      = "ambiguous"
)

// ClaudeCloakAnchor is a caller user message that carries the cloak's date
// reminder and after which the relocated caller system prompt goes.
type ClaudeCloakAnchor struct {
	Index int
	Date  string
}

// ClaudeCloakPlan says where the cloak injects into one request.
type ClaudeCloakPlan struct {
	Anchors []ClaudeCloakAnchor
	// Fallback names why a request with a compaction block got main's
	// placement; it is empty otherwise.
	Fallback string
	key      string
	lineage  []ClaudeCloakLineageMessage
}

// PlanClaudeCloak decides which caller user messages carry the cloak's
// injections.
//
// Anthropic keeps a replayed thinking block only while every message before it
// arrives as it did when the block was produced; after a compaction the check
// starts at the newest compaction block. So within a Session every message the
// proxy forwarded before must be forwarded again with the same bytes:
//
//   - Without a compaction block the anchor is the first user message, dated
//     today, exactly as on main. That position never moves.
//   - With one, the request is aligned by position against the Session's
//     lineage, the previous request's messages. Within an epoch the request
//     extends it, so the common prefix aligns. Across a swap the request is
//     [block, kept turns, new turns], and the kept turns are a run ending where
//     the lineage ends. Every aligned message keeps the anchor and date it had;
//     a new last user turn gets a fresh one, dated today, only when no aligned
//     anchor follows the newest block.
//   - Without a Session, an unknown one, a request that does not align, or one
//     that aligns two ways with different results, the request gets main's
//     placement and the plan names the reason. Nothing is guessed.
//
// The plan reads the lineage; only Commit writes it.
func PlanClaudeCloak(payload []byte, sessionID, today string) ClaudeCloakPlan {
	raw := gjson.GetBytes(payload, "messages").Array()
	messages := make([]claudeCloakMessage, len(raw))
	head, first := -1, -1
	for idx, message := range raw {
		messages[idx] = claudeCloakMessage{
			ClaudeCloakLineageMessage: ClaudeCloakLineageMessage{Hash: claudeCloakMessageHash(message), Compaction: claudeMessageHasCompaction(message)},
			user:                      message.Get("role").String() == "user",
		}
		if messages[idx].Compaction {
			head = idx
		}
		if first < 0 && messages[idx].user {
			first = idx
		}
	}

	var plan ClaudeCloakPlan
	var previous []ClaudeCloakLineageMessage
	known := false
	if sessionID != "" {
		plan.key = claudeCloakLineageKey(sessionID)
		previous, known = ClaudeCloakLineages().load(plan.key)
	}
	switch {
	case head < 0:
	case plan.key == "":
		plan.Fallback = ClaudeCloakFallbackNoSession
	case !known:
		plan.Fallback = ClaudeCloakFallbackUnknownSession
	default:
		plan.Anchors, plan.Fallback = alignClaudeCloak(messages, previous, head, today)
	}
	if (head < 0 || plan.Fallback != "") && first >= 0 {
		plan.Anchors = []ClaudeCloakAnchor{{Index: first, Date: today}}
	}
	if plan.key == "" {
		return plan
	}

	// A request that only repeats a prefix of the lineage, such as the
	// compaction request that summarizes it, leaves the lineage as it was: the
	// next swap keeps a run that ends where the lineage ends.
	common := claudeCloakCommonPrefix(messages, previous)
	if common == len(messages) && common < len(previous) {
		plan.lineage = previous
		return plan
	}
	plan.lineage = make([]ClaudeCloakLineageMessage, len(messages))
	for idx, message := range messages {
		plan.lineage[idx] = message.ClaudeCloakLineageMessage
	}
	for _, anchor := range plan.Anchors {
		plan.lineage[anchor.Index].Date = anchor.Date
	}
	return plan
}

// Commit records the plan's request as its Session's lineage and counts a
// fallback. Only requests forwarded for generation commit; count_tokens never
// does.
func (p ClaudeCloakPlan) Commit() {
	if p.Fallback != "" {
		countClaudeCloakLineageFallback(p.Fallback)
		if p.Fallback == ClaudeCloakFallbackNoSession {
			log.Debugf("claude cloak: compacted request without X-Session-ID uses main's placement")
		} else {
			log.Infof("claude cloak: compacted request uses main's placement (%s); kept thinking after its first user turn may be dropped once", p.Fallback)
		}
	}
	if p.key != "" {
		ClaudeCloakLineages().save(p.key, p.lineage)
	}
}

type claudeCloakMessage struct {
	ClaudeCloakLineageMessage
	user bool
}

// alignClaudeCloak places a compacted request against its Session's lineage.
// It returns the anchors, or the reason it cannot.
func alignClaudeCloak(messages []claudeCloakMessage, previous []ClaudeCloakLineageMessage, head int, today string) ([]ClaudeCloakAnchor, string) {
	aligned := make([]int, len(messages))
	if common := claudeCloakCommonPrefix(messages, previous); common > head {
		for idx := range aligned {
			aligned[idx] = -1
			if idx < common {
				aligned[idx] = idx
			}
		}
		return placeClaudeCloakAnchors(messages, previous, aligned, head, today), ""
	}

	previousHead := -1
	for idx, message := range previous {
		if message.Compaction {
			previousHead = idx
		}
	}
	var chosen []ClaudeCloakAnchor
	found := false
	for kept := min(len(previous)-previousHead-1, len(messages)-head-1); kept >= 1; kept-- {
		start := len(previous) - kept
		matches := true
		for i := 0; i < kept && matches; i++ {
			matches = messages[head+1+i].Hash == previous[start+i].Hash
		}
		if !matches {
			continue
		}
		for idx := range aligned {
			aligned[idx] = -1
			if idx > head && idx <= head+kept {
				aligned[idx] = start + idx - head - 1
			}
		}
		anchors := placeClaudeCloakAnchors(messages, previous, aligned, head, today)
		if !found {
			chosen, found = anchors, true
		} else if !equalClaudeCloakAnchors(chosen, anchors) {
			return nil, ClaudeCloakFallbackAmbiguous
		}
	}
	if !found {
		return nil, ClaudeCloakFallbackUnaligned
	}
	return chosen, ""
}

// placeClaudeCloakAnchors keeps every aligned message's anchor and gives a new
// last user turn a fresh one when no aligned anchor follows the newest block.
func placeClaudeCloakAnchors(messages []claudeCloakMessage, previous []ClaudeCloakLineageMessage, aligned []int, head int, today string) []ClaudeCloakAnchor {
	var anchors []ClaudeCloakAnchor
	afterHead := false
	for idx, from := range aligned {
		if from >= 0 && previous[from].Date != "" {
			anchors = append(anchors, ClaudeCloakAnchor{Index: idx, Date: previous[from].Date})
			afterHead = afterHead || idx > head
		}
	}
	if afterHead {
		return anchors
	}
	for idx := len(messages) - 1; idx > head; idx-- {
		if messages[idx].user {
			if aligned[idx] < 0 {
				anchors = append(anchors, ClaudeCloakAnchor{Index: idx, Date: today})
			}
			break
		}
	}
	return anchors
}

func claudeCloakCommonPrefix(messages []claudeCloakMessage, previous []ClaudeCloakLineageMessage) int {
	common := 0
	for common < len(messages) && common < len(previous) && messages[common].Hash == previous[common].Hash {
		common++
	}
	return common
}

func equalClaudeCloakAnchors(a, b []ClaudeCloakAnchor) bool {
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

func claudeMessageHasCompaction(message gjson.Result) bool {
	for _, block := range message.Get("content").Array() {
		if block.Get("type").String() == "compaction" {
			return true
		}
	}
	return false
}

// claudeCloakMessageHash identifies a caller message by its bytes without
// cache_control, which callers move between requests and Anthropic leaves out
// of the thinking check.
func claudeCloakMessageHash(message gjson.Result) string {
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
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:16])
}
