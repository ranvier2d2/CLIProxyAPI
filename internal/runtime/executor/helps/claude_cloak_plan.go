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

// Reasons an accepted request's placement did not become its Session's
// lineage. The Session's next compacted request either aligns against the
// lineage that stayed or, after a drop, falls back as an unknown Session.
const (
	// ClaudeCloakRecoveryStale: the request was planned against a lineage
	// another request of the Session has since replaced, and planning it again
	// against the newer one gives a different placement; or another process
	// sharing the state directory wrote the Session's file first. The newer
	// lineage stays.
	ClaudeCloakRecoveryStale = "stale"
	// ClaudeCloakRecoveryOversize: the request is past the per-Session cap, so
	// the Session's lineage is dropped.
	ClaudeCloakRecoveryOversize = "oversize"
	// ClaudeCloakRecoveryUnwritten: the store could not write the lineage to
	// disk, or forgot it before writing it. The process keeps using what it
	// holds in memory; a restart sees the Session as the disk last held it.
	ClaudeCloakRecoveryUnwritten = "unwritten"
)

// claudeCloakCommitAttempts bounds how often Commit plans again after another
// request of the same Session committed first.
const claudeCloakCommitAttempts = 3

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
	request  claudeCloakRequest
	// expect is the revision the plan was made against; next is what Commit
	// writes in its place.
	expect uint64
	next   claudeCloakLineage
}

// PlanClaudeCloak decides which caller user messages carry the cloak's
// injections.
//
// Anthropic keeps a replayed thinking block only while every message before it
// arrives as it did when the block was produced; after a compaction the check
// starts at the newest compaction block. So within a Session every message the
// proxy forwarded before must be forwarded again with the same bytes. The
// Session's lineage is the transcript the caller holds, as forwarded, plus the
// cut of a pending compaction. A request is one of these transitions:
//
//   - No compaction block: the anchor is the first user message, dated today,
//     exactly as on main. That position never moves.
//   - Append: the request repeats the lineage through its newest compaction
//     block. The common prefix aligns; what follows is new.
//   - Compaction request: a request with a top-level compaction field, or one
//     that repeats only a prefix of what the caller holds. It summarizes that
//     prefix, so it leaves the rest of the lineage in place and records the
//     cut: the next swap keeps the turns from the cut on. Only that swap
//     consumes the cut and only another compaction request replaces it; any
//     other request that repeats the summarized prefix (an unchanged resend,
//     a retry, an appended or edited later turn) keeps it. A request that
//     changes the summarized prefix replaces the history the cut points into
//     and clears it.
//   - Swap after a compaction request: [new block, kept turns, new turns]. The
//     kept turns align, by position, with the lineage from the cut. When that
//     request is itself a compaction request carrying the new block, before
//     any ordinary one, the lineage becomes [new block] plus the whole tail
//     from the cut, and its own length is the next cut; the older retained
//     tail is never dropped.
//   - Swap without a pending cut, such as one the proxy saw no compaction
//     request for: the kept turns are a run ending where the lineage ends.
//
// Every aligned message keeps the anchor and date it had; a new last user turn
// gets a fresh one, dated today, only when no aligned anchor follows the
// newest block. Without a Session, an unknown one, a request that does not
// align, or one that aligns two ways with different results, the request gets
// main's placement and the plan names the reason. Nothing is guessed, and no
// message is matched by content anywhere but at its position.
//
// The plan reads the lineage; only Commit writes it.
func PlanClaudeCloak(payload []byte, sessionID, today string) ClaudeCloakPlan {
	plan := ClaudeCloakPlan{request: parseClaudeCloakRequest(payload, today)}
	var read claudeCloakLineage
	known := false
	if sessionID != "" {
		plan.key = claudeCloakLineageKey(sessionID)
		read, known = ClaudeCloakLineages().load(plan.key)
	}
	plan.expect = read.Rev
	plan.Anchors, plan.Fallback, plan.next = plan.request.place(plan.key != "", known, read)
	return plan
}

// Commit records the plan's request as its Session's lineage and counts a
// fallback. Call it only once the upstream accepted the request for
// generation; count_tokens and rejected requests never commit.
//
// The write is a compare-and-swap on the Session's revision. When another
// request of the Session committed after this plan read the lineage, the
// request is planned again against the newer lineage: if that gives the same
// placement and a lineage within the per-Session caps, the bytes forwarded are
// the ones the newer lineage implies and the new plan commits; if not, the
// newer lineage stays and the conflict is counted as stale. Only a request
// planned at the current revision can drop its Session for its size.
func (p ClaudeCloakPlan) Commit() {
	if p.Fallback != "" {
		countClaudeCloakLineageFallback(p.Fallback)
		if p.Fallback == ClaudeCloakFallbackNoSession {
			log.Debugf("claude cloak: compacted request without X-Session-ID uses main's placement")
		} else {
			log.Infof("claude cloak: compacted request uses main's placement (%s); kept thinking after its first user turn may be dropped", p.Fallback)
		}
	}
	if p.key == "" {
		return
	}
	store := ClaudeCloakLineages()
	expect, next := p.expect, p.next
	for attempt := 1; ; attempt++ {
		current, known, outcome := store.swap(p.key, expect, next)
		switch outcome {
		case claudeCloakLineageSwapped:
			return
		case claudeCloakLineageOversize:
			countClaudeCloakLineageFallback(ClaudeCloakRecoveryOversize)
			log.Infof("claude cloak: a Session's request is past the lineage cap of %d messages or %d bytes; its lineage is dropped and its next compacted request uses main's placement", store.limits.sessionMessages, store.limits.sessionBytes)
			return
		case claudeCloakLineageUnwritten:
			countClaudeCloakLineageFallback(ClaudeCloakRecoveryUnwritten)
			return
		}
		anchors, _, replanned := p.request.place(true, known, current)
		raw, errEncode := store.encode(replanned, store.now())
		if attempt == claudeCloakCommitAttempts || !equalClaudeCloakAnchors(anchors, p.Anchors) || errEncode != nil || store.exceeds(replanned, raw) {
			countClaudeCloakLineageFallback(ClaudeCloakRecoveryStale)
			log.Infof("claude cloak: a request planned against an older lineage of its Session conflicts with a newer one; the newer lineage stays")
			return
		}
		expect, next = current.Rev, replanned
	}
}

type claudeCloakMessage struct {
	ClaudeCloakLineageMessage
	user bool
}

// claudeCloakRequest is what planning needs of one request.
type claudeCloakRequest struct {
	messages []claudeCloakMessage
	// head is the newest compaction block, first the first user message; -1
	// when there is none.
	head, first int
	// compaction is a top-level compaction field: the request asks for a
	// summary of its messages.
	compaction bool
	today      string
}

func parseClaudeCloakRequest(payload []byte, today string) claudeCloakRequest {
	raw := gjson.GetBytes(payload, "messages").Array()
	r := claudeCloakRequest{
		messages:   make([]claudeCloakMessage, len(raw)),
		head:       -1,
		first:      -1,
		compaction: gjson.GetBytes(payload, "compaction").Exists(),
		today:      today,
	}
	for idx, message := range raw {
		r.messages[idx] = claudeCloakMessage{
			ClaudeCloakLineageMessage: ClaudeCloakLineageMessage{Hash: claudeCloakMessageHash(message), Compaction: claudeMessageHasCompaction(message)},
			user:                      message.Get("role").String() == "user",
		}
		if r.messages[idx].Compaction {
			r.head = idx
		}
		if r.first < 0 && r.messages[idx].user {
			r.first = idx
		}
	}
	return r
}

// place plans the request against previous and returns the anchors, the
// fallback reason, and the lineage the request leaves behind.
func (r claudeCloakRequest) place(sessioned, known bool, previous claudeCloakLineage) ([]ClaudeCloakAnchor, string, claudeCloakLineage) {
	view, aligned, fallback := r.align(previous)
	switch {
	case r.head < 0:
		fallback = ""
	case !sessioned:
		fallback = ClaudeCloakFallbackNoSession
	case !known:
		fallback = ClaudeCloakFallbackUnknownSession
	}
	var anchors []ClaudeCloakAnchor
	if r.head < 0 || fallback != "" {
		if r.first >= 0 {
			anchors = []ClaudeCloakAnchor{{Index: r.first, Date: r.today}}
		}
		if fallback != "" {
			view, aligned = nil, nil
		}
	} else {
		anchors = r.placeAnchors(view, aligned)
	}
	if !sessioned {
		return anchors, fallback, claudeCloakLineage{}
	}

	// A request that repeats only a prefix of the transcript the caller holds
	// leaves the rest of it in place.
	prefix := len(aligned) == len(r.messages) && len(r.messages) < len(view)
	for idx := range aligned {
		prefix = prefix && aligned[idx]
	}
	next := claudeCloakLineage{}
	if prefix {
		next.Messages = append([]ClaudeCloakLineageMessage(nil), view...)
	} else {
		next.Messages = make([]ClaudeCloakLineageMessage, len(r.messages))
		for idx, message := range r.messages {
			next.Messages[idx] = message.ClaudeCloakLineageMessage
		}
	}
	for _, anchor := range anchors {
		next.Messages[anchor.Index].Date = anchor.Date
	}
	switch {
	case prefix || r.compaction:
		next.Cut = len(r.messages)
	case fallback == "" && r.keepsCut(previous):
		next.Cut = previous.Cut
	}
	return anchors, fallback, next
}

// keepsCut reports whether the request leaves previous's pending cut in
// place: it stays within the lineage's epoch and repeats the summarized
// prefix, so the swap the cut waits for is still to come.
func (r claudeCloakRequest) keepsCut(previous claudeCloakLineage) bool {
	if previous.Cut == 0 {
		return false
	}
	common := claudeCloakCommonPrefix(r.messages, previous.Messages)
	return (r.head < 0 || common > r.head) && common >= previous.Cut
}

// align lines the request up against the transcript the caller holds, view,
// and reports which of its messages are positions of view. It returns a
// fallback reason when a request with a compaction block cannot be placed.
func (r claudeCloakRequest) align(previous claudeCloakLineage) ([]ClaudeCloakLineageMessage, []bool, string) {
	messages, head := r.messages, r.head
	aligned := make([]bool, len(messages))

	// Append, or a compaction request within the epoch.
	if common := claudeCloakCommonPrefix(messages, previous.Messages); head < 0 || common > head {
		for idx := 0; idx < common; idx++ {
			aligned[idx] = true
		}
		return previous.Messages, aligned, ""
	}

	// A swap holds [request through its newest block, kept turns...].
	swapped := func(start int) []ClaudeCloakLineageMessage {
		view := make([]ClaudeCloakLineageMessage, 0, head+1+len(previous.Messages)-start)
		for _, message := range messages[:head+1] {
			view = append(view, message.ClaudeCloakLineageMessage)
		}
		return append(view, previous.Messages[start:]...)
	}

	// After a compaction request the kept turns start at its cut.
	if cut := previous.Cut; cut > 0 && cut <= len(previous.Messages) {
		view := swapped(cut)
		kept := 0
		for head+1+kept < len(messages) && cut+kept < len(previous.Messages) && messages[head+1+kept].Hash == previous.Messages[cut+kept].Hash {
			kept++
		}
		if kept == 0 && cut < len(previous.Messages) && head+1 < len(messages) {
			return nil, nil, ClaudeCloakFallbackUnaligned
		}
		for idx := 0; idx <= head+kept; idx++ {
			aligned[idx] = true
		}
		return view, aligned, ""
	}

	// Otherwise the kept turns are a run ending where the lineage ends.
	previousHead := -1
	for idx, message := range previous.Messages {
		if message.Compaction {
			previousHead = idx
		}
	}
	var view []ClaudeCloakLineageMessage
	var chosen []ClaudeCloakAnchor
	var chosenAligned []bool
	for kept := min(len(previous.Messages)-previousHead-1, len(messages)-head-1); kept >= 1; kept-- {
		start := len(previous.Messages) - kept
		matches := true
		for i := 0; i < kept && matches; i++ {
			matches = messages[head+1+i].Hash == previous.Messages[start+i].Hash
		}
		if !matches {
			continue
		}
		candidate := make([]bool, len(messages))
		for idx := 0; idx <= head+kept; idx++ {
			candidate[idx] = true
		}
		candidateView := swapped(start)
		anchors := r.placeAnchors(candidateView, candidate)
		if chosenAligned == nil {
			view, chosen, chosenAligned = candidateView, anchors, candidate
		} else if !equalClaudeCloakAnchors(chosen, anchors) {
			return nil, nil, ClaudeCloakFallbackAmbiguous
		}
	}
	if chosenAligned == nil {
		return nil, nil, ClaudeCloakFallbackUnaligned
	}
	return view, chosenAligned, ""
}

// placeAnchors keeps every aligned message's anchor after the newest block and
// gives a new last user turn a fresh one when none of them carries one.
func (r claudeCloakRequest) placeAnchors(view []ClaudeCloakLineageMessage, aligned []bool) []ClaudeCloakAnchor {
	var anchors []ClaudeCloakAnchor
	afterHead := false
	for idx := range r.messages {
		if aligned[idx] && view[idx].Date != "" {
			anchors = append(anchors, ClaudeCloakAnchor{Index: idx, Date: view[idx].Date})
			afterHead = afterHead || idx > r.head
		}
	}
	if afterHead {
		return anchors
	}
	for idx := len(r.messages) - 1; idx > r.head; idx-- {
		if r.messages[idx].user {
			if !aligned[idx] {
				anchors = append(anchors, ClaudeCloakAnchor{Index: idx, Date: r.today})
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
