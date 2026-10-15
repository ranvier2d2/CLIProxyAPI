package helps

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

// A lineage lives while its Session keeps sending requests. Past a bound the
// least recently used Session is forgotten alone; live ones are never cleared
// wholesale.
const (
	claudeCloakLineageTTL         = 30 * 24 * time.Hour
	claudeCloakLineageFileVersion = 2
	claudeCloakLineageDirName     = "claude-cloak-lineage"
	// claudeCloakLineagePruneEvery spaces out directory sweeps that no new
	// Session triggered, so a growing Session still meets the total budget.
	claudeCloakLineagePruneEvery = time.Minute
)

// claudeCloakLineageLimits bounds the store. A record is a 32-character hash,
// a flag and a date, under 64 encoded bytes, so for records the proxy writes
// the message cap binds before the byte cap; the byte cap bounds the file
// whatever it holds. A Session past either cap loses its lineage.
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
	Hash       string `json:"h"`
	Compaction bool   `json:"c,omitempty"`
	Date       string `json:"d,omitempty"`
}

// claudeCloakLineage is a Session's lineage: the transcript the caller holds,
// as the cloak forwarded it, and the cut of a pending compaction request, the
// position its next swap keeps the turns from (0 for none). Rev changes on
// every write and is 0 for a Session the store does not know; a write lands
// only on the revision it was planned against.
type claudeCloakLineage struct {
	Rev      uint64                      `json:"rev"`
	Cut      int                         `json:"cut,omitempty"`
	Messages []ClaudeCloakLineageMessage `json:"messages"`
}

type claudeCloakLineageFile struct {
	Version int   `json:"version"`
	Used    int64 `json:"used"`
	claudeCloakLineage
}

// claudeCloakLineageEntry is a Session as the store holds it in memory. With a
// directory it also records how the entry relates to the Session's file: base
// is the revision on disk the lineage descends from (0 for no file), writing
// the revision being written now, and seen the file as the store last read or
// wrote it (nil for none).
type claudeCloakLineageEntry struct {
	lineage claudeCloakLineage
	used    int64
	size    int
	// dropped marks a Session whose lineage the store dropped while its file
	// still has to be removed.
	dropped bool
	base    uint64
	writing uint64
	seen    os.FileInfo
}

func (e *claudeCloakLineageEntry) current(now time.Time) (claudeCloakLineage, bool) {
	if e == nil || e.dropped || claudeCloakLineageExpired(e.used, now) {
		return claudeCloakLineage{}, false
	}
	return e.lineage, true
}

type claudeCloakLineageOutcome int

const (
	claudeCloakLineageSwapped claudeCloakLineageOutcome = iota
	claudeCloakLineageStale
	claudeCloakLineageOversize
	claudeCloakLineageUnwritten
)

// ClaudeCloakLineageStore keeps one lineage per Session. Memory is
// authoritative: a request reads and compare-and-swaps the Session's lineage
// in memory and never waits for the disk.
//
// With a directory each Session is also one JSON file, written in the
// background: an accepted lineage queues its Session, and one writer per store
// writes the latest lineage of each queued Session, so a Session written many
// times while the writer is busy is written once. The queue holds at most one
// entry per Session in memory, so the memory bounds bound it too. Each file
// write holds an exclusive lock on one of 256 shard lock files in the sibling
// directory dir.locks and is itself a compare-and-swap: it lands only if the
// file still holds the revision the lineage descends from. Otherwise another
// process wrote the Session first; its lineage stays, this one is counted
// stale, and the next request reloads the file. Every read stats the file and
// reloads it when another process replaced it since the store last saw it, so
// a second process sees a write once it reaches the disk. A crash loses only
// the lineages still queued: the Session's next request plans against the
// lineage on disk, which costs at most one fallback for that Session. Without
// a directory the store is memory only.
type ClaudeCloakLineageStore struct {
	dir    string
	now    func() time.Time
	limits claudeCloakLineageLimits

	// mu guards the in-memory Sessions and the write queue; no disk I/O runs
	// under it.
	mu       sync.Mutex
	sessions map[string]*claudeCloakLineageEntry
	bytes    int
	queued   map[string]struct{}
	draining bool
	drained  *sync.Cond

	// pruning lets one goroutine at a time sweep the directory.
	pruning   sync.Mutex
	lastPrune atomic.Int64
}

// NewClaudeCloakLineageStore returns a store backed by dir, or held in memory
// when dir is empty.
func NewClaudeCloakLineageStore(dir string) *ClaudeCloakLineageStore {
	s := &ClaudeCloakLineageStore{
		dir:      dir,
		now:      time.Now,
		limits:   defaultClaudeCloakLineageLimits,
		sessions: make(map[string]*claudeCloakLineageEntry),
		queued:   make(map[string]struct{}),
	}
	s.drained = sync.NewCond(&s.mu)
	return s
}

var claudeCloakLineageStore atomic.Pointer[ClaudeCloakLineageStore]

func init() {
	claudeCloakLineageStore.Store(NewClaudeCloakLineageStore(""))
}

// ClaudeCloakLineages returns the process-wide store.
func ClaudeCloakLineages() *ClaudeCloakLineageStore {
	return claudeCloakLineageStore.Load()
}

// SetClaudeCloakLineages installs the process-wide store and returns the
// previous one, once the previous one has flushed its queued writes.
func SetClaudeCloakLineages(s *ClaudeCloakLineageStore) *ClaudeCloakLineageStore {
	previous := claudeCloakLineageStore.Swap(s)
	if previous != nil && previous != s {
		previous.Flush()
	}
	return previous
}

// Flush waits until every lineage the store accepted so far is on disk, or
// counted as stale or unwritten. Call it before the process exits.
func (s *ClaudeCloakLineageStore) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.draining {
		s.drained.Wait()
	}
}

// ClaudeCloakLineageDir returns where the server keeps lineages, following
// the discovery state directory's precedence: WRITABLE_PATH, then the user
// config directory. It returns "" when neither resolves.
func ClaudeCloakLineageDir() string {
	if writable := util.WritablePath(); writable != "" {
		return filepath.Join(writable, "state", claudeCloakLineageDirName)
	}
	if configDir, errConfig := os.UserConfigDir(); errConfig == nil && configDir != "" {
		return filepath.Join(configDir, "cpa", claudeCloakLineageDirName)
	}
	if homeDir, errHome := os.UserHomeDir(); errHome == nil && homeDir != "" {
		return filepath.Join(homeDir, ".config", "cpa", claudeCloakLineageDirName)
	}
	return ""
}

// claudeCloakLineageKey names a Session without storing the caller's value.
func claudeCloakLineageKey(sessionID string) string {
	sum := sha256.Sum256([]byte("cpa-claude-cloak-lineage\x00" + sessionID))
	return hex.EncodeToString(sum[:16])
}

func (s *ClaudeCloakLineageStore) load(key string) (claudeCloakLineage, bool) {
	now := s.now()
	s.refresh(key, now)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[key].current(now)
}

// swap makes next the Session's lineage if the Session is still at revision
// expect, and otherwise returns the lineage it is at. Only a request at the
// current revision decides anything else: a lineage past the per-Session caps,
// or one the store cannot encode, then drops the Session.
func (s *ClaudeCloakLineageStore) swap(key string, expect uint64, next claudeCloakLineage) (claudeCloakLineage, bool, claudeCloakLineageOutcome) {
	now := s.now()
	next.Rev = newClaudeCloakLineageRev()
	raw, errEncode := s.encode(next, now)
	s.refresh(key, now)

	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.sessions[key]
	if current, live := entry.current(now); current.Rev != expect {
		return current, live, claudeCloakLineageStale
	}
	switch {
	case errEncode != nil:
		log.Warnf("claude cloak lineage: encode: %v", errEncode)
		s.dropLocked(key)
		return claudeCloakLineage{}, false, claudeCloakLineageUnwritten
	case s.exceeds(next, raw):
		s.dropLocked(key)
		return claudeCloakLineage{}, false, claudeCloakLineageOversize
	}
	if entry == nil {
		entry = &claudeCloakLineageEntry{}
		s.sessions[key] = entry
	}
	s.bytes += len(raw) - entry.size
	entry.lineage, entry.used, entry.size, entry.dropped = next, now.Unix(), len(raw), false
	s.queueLocked(key)
	s.pruneMemoryLocked(now, key)
	return next, true, claudeCloakLineageSwapped
}

func (s *ClaudeCloakLineageStore) encode(lineage claudeCloakLineage, now time.Time) ([]byte, error) {
	return json.Marshal(claudeCloakLineageFile{Version: claudeCloakLineageFileVersion, Used: now.Unix(), claudeCloakLineage: lineage})
}

// exceeds reports whether lineage, encoded as raw, is past the per-Session
// caps.
func (s *ClaudeCloakLineageStore) exceeds(lineage claudeCloakLineage, raw []byte) bool {
	return len(lineage.Messages) > s.limits.sessionMessages || len(raw) > s.limits.sessionBytes
}

// dropLocked forgets key's lineage. A file the store wrote is removed by the
// writer, under the same lock and revision check as a write.
func (s *ClaudeCloakLineageStore) dropLocked(key string) {
	entry := s.sessions[key]
	if entry == nil {
		return
	}
	if s.dir == "" || (entry.base == 0 && entry.writing == 0) {
		s.forgetLocked(key)
		return
	}
	s.bytes -= entry.size
	entry.lineage, entry.size, entry.dropped = claudeCloakLineage{}, 0, true
	s.queueLocked(key)
}

// forgetLocked removes key from memory; a write still queued for it is lost.
func (s *ClaudeCloakLineageStore) forgetLocked(key string) {
	if entry, ok := s.sessions[key]; ok {
		s.bytes -= entry.size
		delete(s.sessions, key)
	}
	delete(s.queued, key)
}

func (s *ClaudeCloakLineageStore) queueLocked(key string) {
	if s.dir == "" {
		return
	}
	s.queued[key] = struct{}{}
	if !s.draining {
		s.draining = true
		go s.drain()
	}
}

// drain writes queued Sessions until none is left.
func (s *ClaudeCloakLineageStore) drain() {
	for {
		s.mu.Lock()
		if len(s.queued) == 0 {
			s.draining = false
			s.drained.Broadcast()
			s.mu.Unlock()
			return
		}
		keys := make([]string, 0, len(s.queued))
		for key := range s.queued {
			keys = append(keys, key)
		}
		clear(s.queued)
		s.mu.Unlock()

		// The sweep's clock is the newest use it wrote, so the writer never
		// reads the store's clock.
		created, latest := false, int64(0)
		for _, key := range keys {
			wrote, used := s.persist(key)
			created, latest = created || wrote, max(latest, used)
		}
		if now := time.Unix(latest, 0); latest > 0 && (created || now.Sub(time.Unix(0, s.lastPrune.Load())) >= claudeCloakLineagePruneEvery) {
			s.pruneDir(now)
		}
	}
}

// persist writes key's lineage, or removes its file after a drop, if the file
// still holds the revision the lineage descends from. It reports whether the
// write created the file, and the Session's last use.
func (s *ClaudeCloakLineageStore) persist(key string) (bool, int64) {
	s.mu.Lock()
	entry := s.sessions[key]
	if entry == nil {
		s.mu.Unlock()
		return false, 0
	}
	lineage, used, base, dropped := entry.lineage, entry.used, entry.base, entry.dropped
	entry.writing = lineage.Rev
	s.mu.Unlock()

	path := s.path(key)
	settle := func(onDisk uint64, seen os.FileInfo) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.sessions[key] != entry {
			return
		}
		entry.base, entry.writing, entry.seen = onDisk, 0, seen
		if _, again := s.queued[key]; entry.dropped && !again && onDisk == 0 {
			s.forgetLocked(key)
		}
	}
	unlock, errLock := lockClaudeCloakLineageShard(s.dir, key)
	if errLock != nil {
		log.Warnf("claude cloak lineage: lock %s: %v", s.dir, errLock)
		countClaudeCloakLineageFallback(ClaudeCloakRecoveryUnwritten)
		s.mu.Lock()
		if s.sessions[key] == entry {
			entry.writing = 0
		}
		s.mu.Unlock()
		return false, used
	}
	defer unlock()

	if file, _ := readClaudeCloakLineageFile(path); file.Rev != base {
		countClaudeCloakLineageFallback(ClaudeCloakRecoveryStale)
		log.Infof("claude cloak: another process wrote a Session's lineage first; its lineage stays")
		s.mu.Lock()
		if s.sessions[key] == entry {
			s.forgetLocked(key)
		}
		s.mu.Unlock()
		return false, used
	}
	_, errStat := os.Stat(path)
	created := errors.Is(errStat, os.ErrNotExist)
	if dropped {
		if errRemove := os.Remove(path); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			log.Warnf("claude cloak lineage: drop %s: %v", path, errRemove)
		}
		settle(s.onDisk(path))
		return false, used
	}
	raw, errEncode := json.Marshal(claudeCloakLineageFile{Version: claudeCloakLineageFileVersion, Used: used, claudeCloakLineage: lineage})
	if errEncode == nil {
		errEncode = writeFileAtomically(path, raw)
	}
	if errEncode != nil {
		log.Warnf("claude cloak lineage: write %s: %v", path, errEncode)
		countClaudeCloakLineageFallback(ClaudeCloakRecoveryUnwritten)
		_ = os.Remove(path)
		settle(s.onDisk(path))
		return false, used
	}
	// The modification time is the Session's last use; pruning reads only that.
	if errTimes := os.Chtimes(path, time.Unix(used, 0), time.Unix(used, 0)); errTimes != nil {
		log.Warnf("claude cloak lineage: touch %s: %v", path, errTimes)
	}
	seen, _ := os.Stat(path)
	settle(lineage.Rev, seen)
	return created, used
}

// onDisk returns the revision the file at path holds, 0 for none, and the
// file as it is now.
func (s *ClaudeCloakLineageStore) onDisk(path string) (uint64, os.FileInfo) {
	info, errStat := os.Stat(path)
	if errStat != nil {
		return 0, nil
	}
	file, _ := readClaudeCloakLineageFile(path)
	return file.Rev, info
}

// refresh makes memory hold the Session's file when the store does not hold
// the Session yet, or when another process replaced or removed the file since
// the store last saw it. A write of this store's still queued for the Session
// is then lost and counted stale.
func (s *ClaudeCloakLineageStore) refresh(key string, now time.Time) {
	if s.dir == "" {
		return
	}
	path := s.path(key)
	s.mu.Lock()
	entry := s.sessions[key]
	var seen os.FileInfo
	if entry != nil {
		seen = entry.seen
	}
	s.mu.Unlock()
	info, errStat := os.Stat(path)
	if errStat != nil {
		info = nil
	}
	if entry != nil && sameClaudeCloakLineageFile(seen, info) {
		return
	}
	var file claudeCloakLineageFile
	ok := false
	if info != nil {
		file, ok = readClaudeCloakLineageFile(path)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[key] != entry {
		return
	}
	if entry != nil {
		if file.Rev == entry.base || (file.Rev != 0 && file.Rev == entry.writing) {
			entry.seen = info
			return
		}
		if _, lost := s.queued[key]; lost {
			countClaudeCloakLineageFallback(ClaudeCloakRecoveryStale)
			log.Infof("claude cloak: another process wrote a Session's lineage first; its lineage stays")
		}
		s.forgetLocked(key)
	}
	if !ok || claudeCloakLineageExpired(file.Used, now) {
		return
	}
	size := int(info.Size())
	s.sessions[key] = &claudeCloakLineageEntry{lineage: file.claudeCloakLineage, used: file.Used, size: size, base: file.Rev, seen: info}
	s.bytes += size
	s.pruneMemoryLocked(now, key)
}

// sameClaudeCloakLineageFile reports whether a and b are the same version of
// a Session file. A write renames a new file over the old one, so a write by
// any process makes them differ.
func sameClaudeCloakLineageFile(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return os.SameFile(a, b) && a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size()
}

func (s *ClaudeCloakLineageStore) path(key string) string {
	return filepath.Join(s.dir, key+".json")
}

// readClaudeCloakLineageFile reads a Session file whether or not it expired.
func readClaudeCloakLineageFile(path string) (claudeCloakLineageFile, bool) {
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		if !errors.Is(errRead, os.ErrNotExist) {
			log.Warnf("claude cloak lineage: read %s: %v", path, errRead)
		}
		return claudeCloakLineageFile{}, false
	}
	var file claudeCloakLineageFile
	if errDecode := json.Unmarshal(raw, &file); errDecode != nil || file.Version != claudeCloakLineageFileVersion {
		log.Warnf("claude cloak lineage: ignoring unreadable %s (version %d, err %v); it is replaced on the next write", path, file.Version, errDecode)
		return claudeCloakLineageFile{}, false
	}
	return file, true
}

// openClaudeCloakLineageShard opens the lock file of key's shard, one of 256
// that are created once and never removed, so a lock never outlives its file.
// They live beside dir, which holds only Session files.
func openClaudeCloakLineageShard(dir, key string) (*os.File, error) {
	locks := filepath.Clean(dir) + ".locks"
	if errDir := os.MkdirAll(locks, 0o700); errDir != nil {
		return nil, errDir
	}
	sum := sha256.Sum256([]byte(key))
	return os.OpenFile(filepath.Join(locks, fmt.Sprintf("%02x", sum[0])), os.O_CREATE|os.O_RDWR, 0o600)
}

func newClaudeCloakLineageRev() uint64 {
	for {
		if rev := rand.Uint64(); rev != 0 {
			return rev
		}
	}
}

func claudeCloakLineageExpired(used int64, now time.Time) bool {
	return now.Sub(time.Unix(used, 0)) >= claudeCloakLineageTTL
}

// pruneMemoryLocked forgets expired Sessions, then the least recently used
// ones other than keep, until the store is within its Session and byte
// bounds. A forgotten Session's queued write is lost and counted unwritten.
func (s *ClaudeCloakLineageStore) pruneMemoryLocked(now time.Time, keep string) {
	if len(s.sessions) <= s.limits.sessions && s.bytes <= s.limits.totalBytes {
		return
	}
	forget := func(key string) {
		if _, lost := s.queued[key]; lost {
			countClaudeCloakLineageFallback(ClaudeCloakRecoveryUnwritten)
		}
		s.forgetLocked(key)
	}
	keys := make([]string, 0, len(s.sessions))
	for key, entry := range s.sessions {
		if key != keep && claudeCloakLineageExpired(entry.used, now) {
			forget(key)
			continue
		}
		if key != keep {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return s.sessions[keys[i]].used < s.sessions[keys[j]].used })
	for _, key := range keys {
		if len(s.sessions) <= s.limits.sessions && s.bytes <= s.limits.totalBytes {
			return
		}
		forget(key)
	}
}

// pruneDir sweeps the directory: expired files go, then the least recently
// used ones until the directory is within its Session and byte bounds. Each
// removal holds the Session's shard lock and skips a file written since the
// sweep listed it. A sweep already running elsewhere in the process is not
// waited for.
func (s *ClaudeCloakLineageStore) pruneDir(now time.Time) {
	if !s.pruning.TryLock() {
		return
	}
	defer s.pruning.Unlock()
	s.lastPrune.Store(now.UnixNano())
	entries, errList := os.ReadDir(s.dir)
	if errList != nil {
		log.Warnf("claude cloak lineage: list %s: %v", s.dir, errList)
		return
	}
	type session struct {
		key  string
		info os.FileInfo
	}
	remove := func(file session) {
		unlock, errLock := lockClaudeCloakLineageShard(s.dir, file.key)
		if errLock != nil {
			return
		}
		defer unlock()
		path := s.path(file.key)
		if info, errStat := os.Stat(path); errStat == nil && sameClaudeCloakLineageFile(file.info, info) {
			_ = os.Remove(path)
		}
	}
	var live []session
	var total int64
	for _, entry := range entries {
		key, isSession := strings.CutSuffix(entry.Name(), ".json")
		if entry.IsDir() || !isSession {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil {
			continue
		}
		if now.Sub(info.ModTime()) >= claudeCloakLineageTTL {
			remove(session{key: key, info: info})
			continue
		}
		live = append(live, session{key: key, info: info})
		total += info.Size()
	}
	sort.Slice(live, func(i, j int) bool { return live[i].info.ModTime().Before(live[j].info.ModTime()) })
	for len(live) > 1 && (len(live) > s.limits.sessions || total > int64(s.limits.totalBytes)) {
		remove(live[0])
		total -= live[0].info.Size()
		live = live[1:]
	}
}

func writeFileAtomically(path string, raw []byte) error {
	dir := filepath.Dir(path)
	if errDir := os.MkdirAll(dir, 0o700); errDir != nil {
		return errDir
	}
	tmp, errCreate := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if errCreate != nil {
		return errCreate
	}
	tmpPath := tmp.Name()
	_, errWrite := tmp.Write(raw)
	if errWrite == nil {
		errWrite = tmp.Sync()
	}
	if errClose := tmp.Close(); errWrite == nil {
		errWrite = errClose
	}
	if errWrite == nil {
		errWrite = os.Rename(tmpPath, path)
	}
	if errWrite != nil {
		_ = os.Remove(tmpPath)
	}
	return errWrite
}

var claudeCloakLineageFallbacks sync.Map

// ClaudeCloakLineageFallbacks returns how many accepted generation requests
// this process counted for reason: a compacted request forwarded with main's
// placement (the ClaudeCloakFallback reasons), or a placement that did not
// become its Session's lineage (the ClaudeCloakRecovery reasons).
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
