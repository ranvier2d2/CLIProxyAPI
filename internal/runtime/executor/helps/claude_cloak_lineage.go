package helps

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"sync"
	"sync/atomic"
)

// claudeCloakLineageLimits bounds the store. A Session past a per-Session cap
// loses its lineage; past the store's bounds the least recently written
// Sessions are forgotten one at a time, never live ones wholesale.
type claudeCloakLineageLimits struct {
	sessions        int
	sessionMessages int
	sessionBytes    int
	totalBytes      int
}

var defaultClaudeCloakLineageLimits = claudeCloakLineageLimits{
	sessions:        1024,
	sessionMessages: 4096,
	sessionBytes:    512 << 10,
	totalBytes:      64 << 20,
}

// ClaudeCloakLineageMessage is what the cloak forwarded for one caller message
// of a Session: the message's hash, whether it holds a compaction block, and
// the date reminder the cloak anchored on it, if any. The caller's message
// content is never stored.
type ClaudeCloakLineageMessage struct {
	Hash       string
	Compaction bool
	Date       string
}

// claudeCloakLineage is a Session's lineage: the transcript the caller holds,
// as the cloak forwarded it; the cut of a pending compaction request, the
// position its next swap keeps the turns from (0 for none); and the
// compaction blocks of the Session's earlier epochs. Rev changes on every
// write and is 0 for a Session the store does not know; a write lands only on
// the revision it was planned against.
type claudeCloakLineage struct {
	Rev      uint64
	Cut      int
	Messages []ClaudeCloakLineageMessage
	Retired  claudeCloakRetired
}

// A Session's retired blocks are a Bloom filter of 8,192 bits probed 22 times
// per block, each probe 13 bits of its own from a SHA-512 of the block. With n
// blocks in it, a block it does not hold reads as held with probability
// (1 - e^(-22n/8192))^22: 2.0e-7 at the 255 blocks it is ever read with, 2.1e-7
// at 256. At 256 blocks the filter reads every block as held, so the Session's
// later swaps get main's placement and the bound holds for the Session's life.
const (
	claudeCloakRetiredBlocks = 256
	claudeCloakRetiredBits   = 8192
	claudeCloakRetiredProbes = 22
)

// claudeCloakRetired holds every compaction block a Session's earlier epochs
// had. It never forgets one, so a retry from any earlier epoch reads as one;
// it may read a block it does not hold as held, which only ever costs that
// request main's placement.
type claudeCloakRetired struct {
	bits   [claudeCloakRetiredBits / 64]uint64
	blocks int
}

func (f *claudeCloakRetired) add(hash string) {
	f.blocks++
	for _, bit := range claudeCloakRetiredBitsOf(hash) {
		f.bits[bit/64] |= 1 << (bit % 64)
	}
}

func (f *claudeCloakRetired) holds(hash string) bool {
	if f.blocks >= claudeCloakRetiredBlocks {
		return true
	}
	for _, bit := range claudeCloakRetiredBitsOf(hash) {
		if f.bits[bit/64]&(1<<(bit%64)) == 0 {
			return false
		}
	}
	return true
}

// claudeCloakRetiredBitsOf derives a block's probes from disjoint 13-bit
// fields of one SHA-512, so each probe is independent of the others.
func claudeCloakRetiredBitsOf(hash string) [claudeCloakRetiredProbes]uint64 {
	sum := sha512.Sum512([]byte("cpa-claude-cloak-retired\x00" + hash))
	var bits [claudeCloakRetiredProbes]uint64
	for i := range bits {
		at := 13 * i
		field := uint64(sum[at/8]) | uint64(sum[at/8+1])<<8 | uint64(sum[at/8+2])<<16
		bits[i] = (field >> (at % 8)) % claudeCloakRetiredBits
	}
	return bits
}

// size is what the lineage counts against the byte bounds.
func (l claudeCloakLineage) size() int {
	n := 64 + claudeCloakRetiredBits/8
	for _, message := range l.Messages {
		n += len(message.Hash) + len(message.Date) + 16
	}
	return n
}

type claudeCloakLineageOutcome int

const (
	claudeCloakLineageSwapped claudeCloakLineageOutcome = iota
	claudeCloakLineageStale
	claudeCloakLineageOversize
)

// ClaudeCloakLineageStore keeps one lineage per Session in memory. A request
// reads its Session's lineage and compare-and-swaps it on the revision it
// read. Nothing survives a restart: every Session is then unknown, and its
// next compacted request takes main's placement.
type ClaudeCloakLineageStore struct {
	limits claudeCloakLineageLimits

	mu       sync.Mutex
	sessions map[string]claudeCloakLineage
	bytes    int
	// rev is the last revision written; revisions grow, so a lower one is a
	// less recently written Session.
	rev uint64
}

func NewClaudeCloakLineageStore() *ClaudeCloakLineageStore {
	return &ClaudeCloakLineageStore{limits: defaultClaudeCloakLineageLimits, sessions: make(map[string]claudeCloakLineage)}
}

var claudeCloakLineageStore atomic.Pointer[ClaudeCloakLineageStore]

func init() {
	claudeCloakLineageStore.Store(NewClaudeCloakLineageStore())
}

// ClaudeCloakLineages returns the process-wide store.
func ClaudeCloakLineages() *ClaudeCloakLineageStore {
	return claudeCloakLineageStore.Load()
}

// SetClaudeCloakLineages installs the process-wide store and returns the
// previous one.
func SetClaudeCloakLineages(s *ClaudeCloakLineageStore) *ClaudeCloakLineageStore {
	return claudeCloakLineageStore.Swap(s)
}

// claudeCloakLineageKey names a Session without storing the caller's value.
func claudeCloakLineageKey(sessionID string) string {
	sum := sha256.Sum256([]byte("cpa-claude-cloak-lineage\x00" + sessionID))
	return hex.EncodeToString(sum[:16])
}

func (s *ClaudeCloakLineageStore) load(key string) (claudeCloakLineage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lineage, ok := s.sessions[key]
	return lineage, ok
}

// swap makes next the Session's lineage if the Session is still at revision
// expect, and otherwise returns the lineage it is at. Only a request at the
// current revision decides anything else: a lineage past the per-Session caps
// then drops the Session.
func (s *ClaudeCloakLineageStore) swap(key string, expect uint64, next claudeCloakLineage) (claudeCloakLineage, bool, claudeCloakLineageOutcome) {
	size := next.size()
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, known := s.sessions[key]; current.Rev != expect {
		return current, known, claudeCloakLineageStale
	}
	if s.exceeds(next) {
		s.forgetLocked(key)
		return claudeCloakLineage{}, false, claudeCloakLineageOversize
	}
	s.forgetLocked(key)
	s.rev++
	next.Rev = s.rev
	s.bytes += size
	s.sessions[key] = next
	s.pruneLocked(key)
	return next, true, claudeCloakLineageSwapped
}

// exceeds reports whether lineage is past the per-Session caps.
func (s *ClaudeCloakLineageStore) exceeds(lineage claudeCloakLineage) bool {
	return len(lineage.Messages) > s.limits.sessionMessages || lineage.size() > s.limits.sessionBytes
}

func (s *ClaudeCloakLineageStore) forgetLocked(key string) {
	if lineage, ok := s.sessions[key]; ok {
		s.bytes -= lineage.size()
		delete(s.sessions, key)
	}
}

// pruneLocked forgets the least recently written Sessions other than keep
// until the store is within its Session and byte bounds.
func (s *ClaudeCloakLineageStore) pruneLocked(keep string) {
	for len(s.sessions) > s.limits.sessions || s.bytes > s.limits.totalBytes {
		oldest, oldestRev := "", uint64(0)
		for key, lineage := range s.sessions {
			if key != keep && (oldest == "" || lineage.Rev < oldestRev) {
				oldest, oldestRev = key, lineage.Rev
			}
		}
		if oldest == "" {
			return
		}
		s.forgetLocked(oldest)
	}
}

var claudeCloakLineageFallbacks sync.Map

// ClaudeCloakLineageFallbacks returns how many accepted generation requests
// this process counted for reason: a request forwarded with main's placement
// (the ClaudeCloakFallback reasons), or a placement that did not become its
// Session's lineage (the ClaudeCloakRecovery reasons).
func ClaudeCloakLineageFallbacks(reason string) int64 {
	if counter, ok := claudeCloakLineageFallbacks.Load(reason); ok {
		return counter.(*atomic.Int64).Load()
	}
	return 0
}

func countClaudeCloakLineageFallback(reason string) {
	counter, _ := claudeCloakLineageFallbacks.LoadOrStore(reason, new(atomic.Int64))
	counter.(*atomic.Int64).Add(1)
}
