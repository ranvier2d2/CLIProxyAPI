package helps

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// claudeCompactionAnchorTTL bounds how long a compaction block's cloak anchor is
// remembered. A conversation that outlives it gets a fresh anchor, which moves
// the cloak's injections once.
const (
	claudeCompactionAnchorTTL        = 24 * time.Hour
	claudeCompactionAnchorMaxEntries = 4096
)

type claudeCompactionAnchorEntry struct {
	offset int
	expire time.Time
}

var (
	claudeCompactionAnchors   = make(map[string]claudeCompactionAnchorEntry)
	claudeCompactionAnchorsMu sync.Mutex
	claudeCompactionAnchorNow = time.Now
)

func claudeCompactionAnchorKey(signature string) string {
	sum := sha256.Sum256([]byte(signature))
	return hex.EncodeToString(sum[:])
}

// RememberClaudeCompactionAnchorOffset returns the message offset, relative to
// the compaction block's message, where cloaking anchored its injections the
// first time it saw the block with that signature. The first sighting records
// candidate; later sightings return the recorded offset unchanged, so every
// request of one compaction epoch places the injections at the same message.
func RememberClaudeCompactionAnchorOffset(signature string, candidate int) int {
	key := claudeCompactionAnchorKey(signature)
	now := claudeCompactionAnchorNow()
	claudeCompactionAnchorsMu.Lock()
	defer claudeCompactionAnchorsMu.Unlock()
	if entry, ok := claudeCompactionAnchors[key]; ok && entry.expire.After(now) {
		entry.expire = now.Add(claudeCompactionAnchorTTL)
		claudeCompactionAnchors[key] = entry
		return entry.offset
	}
	if len(claudeCompactionAnchors) >= claudeCompactionAnchorMaxEntries {
		for k, entry := range claudeCompactionAnchors {
			if !entry.expire.After(now) {
				delete(claudeCompactionAnchors, k)
			}
		}
		if len(claudeCompactionAnchors) >= claudeCompactionAnchorMaxEntries {
			claudeCompactionAnchors = make(map[string]claudeCompactionAnchorEntry)
		}
	}
	claudeCompactionAnchors[key] = claudeCompactionAnchorEntry{offset: candidate, expire: now.Add(claudeCompactionAnchorTTL)}
	return candidate
}

// LookupClaudeCompactionAnchorOffset returns a recorded offset without recording one.
func LookupClaudeCompactionAnchorOffset(signature string) (int, bool) {
	key := claudeCompactionAnchorKey(signature)
	now := claudeCompactionAnchorNow()
	claudeCompactionAnchorsMu.Lock()
	defer claudeCompactionAnchorsMu.Unlock()
	entry, ok := claudeCompactionAnchors[key]
	if !ok || !entry.expire.After(now) {
		return 0, false
	}
	return entry.offset, true
}
