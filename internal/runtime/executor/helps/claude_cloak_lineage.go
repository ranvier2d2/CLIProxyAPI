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

type claudeCloakLineageEntry struct {
	lineage claudeCloakLineage
	used    int64
	size    int
}

type claudeCloakLineageOutcome int

const (
	claudeCloakLineageSwapped claudeCloakLineageOutcome = iota
	claudeCloakLineageStale
	claudeCloakLineageOversize
	claudeCloakLineageUnwritten
)

// ClaudeCloakLineageStore keeps one lineage per Session. With a directory each
// Session is one JSON file, written whole to a temporary file and renamed over
// the old one, and read again on every request, so a restart or a second
// process sharing the directory sees the last write. A write holds an
// exclusive file lock on one of 256 shard lock files in the sibling directory
// dir.locks, which serializes writers of a Session across processes without
// making other Sessions wait. Without a
// directory it is held in memory only.
type ClaudeCloakLineageStore struct {
	dir    string
	now    func() time.Time
	limits claudeCloakLineageLimits

	// mu guards the in-memory Sessions; no disk I/O runs under it.
	mu       sync.Mutex
	sessions map[string]*claudeCloakLineageEntry
	bytes    int

	// pruning lets one goroutine at a time sweep the directory.
	pruning   sync.Mutex
	lastPrune atomic.Int64
}

// NewClaudeCloakLineageStore returns a store backed by dir, or held in memory
// when dir is empty.
func NewClaudeCloakLineageStore(dir string) *ClaudeCloakLineageStore {
	return &ClaudeCloakLineageStore{dir: dir, sessions: make(map[string]*claudeCloakLineageEntry), now: time.Now, limits: defaultClaudeCloakLineageLimits}
}

var claudeCloakLineageStore atomic.Pointer[ClaudeCloakLineageStore]

func init() {
	claudeCloakLineageStore.Store(NewClaudeCloakLineageStore(""))
}

// ClaudeCloakLineages returns the process-wide store.
func ClaudeCloakLineages() *ClaudeCloakLineageStore {
	return claudeCloakLineageStore.Load()
}

// SetClaudeCloakLineages installs the process-wide store and returns the previous one.
func SetClaudeCloakLineages(s *ClaudeCloakLineageStore) *ClaudeCloakLineageStore {
	return claudeCloakLineageStore.Swap(s)
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
	if s.dir == "" {
		s.mu.Lock()
		defer s.mu.Unlock()
		entry, ok := s.sessions[key]
		if !ok || claudeCloakLineageExpired(entry.used, now) {
			return claudeCloakLineage{}, false
		}
		return entry.lineage, true
	}
	return s.readFile(s.path(key), now)
}

// swap writes next as the Session's lineage if the Session is still at
// revision expect, and otherwise returns the lineage it is at. A lineage past
// the per-Session caps, or one the store cannot write, drops the Session.
func (s *ClaudeCloakLineageStore) swap(key string, expect uint64, next claudeCloakLineage) (claudeCloakLineage, bool, claudeCloakLineageOutcome) {
	now := s.now()
	next.Rev = newClaudeCloakLineageRev()
	raw, errEncode := json.Marshal(claudeCloakLineageFile{Version: claudeCloakLineageFileVersion, Used: now.Unix(), claudeCloakLineage: next})
	if errEncode != nil {
		log.Warnf("claude cloak lineage: encode: %v", errEncode)
		s.drop(key)
		return claudeCloakLineage{}, false, claudeCloakLineageUnwritten
	}
	if len(next.Messages) > s.limits.sessionMessages || len(raw) > s.limits.sessionBytes {
		s.drop(key)
		return claudeCloakLineage{}, false, claudeCloakLineageOversize
	}
	if s.dir == "" {
		return s.swapMemory(key, expect, next, len(raw), now)
	}
	current, known, outcome, created := s.swapFile(key, expect, raw, now)
	if outcome == claudeCloakLineageSwapped && (created || now.Sub(time.Unix(0, s.lastPrune.Load())) >= claudeCloakLineagePruneEvery) {
		s.pruneDir(now)
	}
	if outcome == claudeCloakLineageSwapped {
		return next, true, outcome
	}
	return current, known, outcome
}

func (s *ClaudeCloakLineageStore) swapMemory(key string, expect uint64, next claudeCloakLineage, size int, now time.Time) (claudeCloakLineage, bool, claudeCloakLineageOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.sessions[key]
	live := ok && !claudeCloakLineageExpired(entry.used, now)
	var current claudeCloakLineage
	if live {
		current = entry.lineage
	}
	if current.Rev != expect {
		return current, live, claudeCloakLineageStale
	}
	if ok {
		s.bytes -= entry.size
	}
	s.sessions[key] = &claudeCloakLineageEntry{lineage: next, used: now.Unix(), size: size}
	s.bytes += size
	s.pruneMemoryLocked(now, key)
	return next, true, claudeCloakLineageSwapped
}

// swapFile compares and writes under the Session's shard lock and reports
// whether the write created the Session's file.
func (s *ClaudeCloakLineageStore) swapFile(key string, expect uint64, raw []byte, now time.Time) (claudeCloakLineage, bool, claudeCloakLineageOutcome, bool) {
	path := s.path(key)
	unlock, errLock := lockClaudeCloakLineageShard(s.dir, key)
	if errLock != nil {
		log.Warnf("claude cloak lineage: lock %s: %v", s.dir, errLock)
		_ = os.Remove(path)
		return claudeCloakLineage{}, false, claudeCloakLineageUnwritten, false
	}
	defer unlock()
	current, known := s.readFile(path, now)
	if current.Rev != expect {
		return current, known, claudeCloakLineageStale, false
	}
	_, errStat := os.Stat(path)
	if errWrite := writeFileAtomically(path, raw); errWrite != nil {
		log.Warnf("claude cloak lineage: write %s: %v", path, errWrite)
		_ = os.Remove(path)
		return claudeCloakLineage{}, false, claudeCloakLineageUnwritten, false
	}
	// The modification time is the Session's last use; pruning reads only that.
	if errTimes := os.Chtimes(path, now, now); errTimes != nil {
		log.Warnf("claude cloak lineage: touch %s: %v", path, errTimes)
	}
	return claudeCloakLineage{}, true, claudeCloakLineageSwapped, errors.Is(errStat, os.ErrNotExist)
}

func (s *ClaudeCloakLineageStore) drop(key string) {
	if s.dir == "" {
		s.mu.Lock()
		defer s.mu.Unlock()
		if entry, ok := s.sessions[key]; ok {
			s.bytes -= entry.size
			delete(s.sessions, key)
		}
		return
	}
	if errRemove := os.Remove(s.path(key)); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
		log.Warnf("claude cloak lineage: drop %s: %v", s.path(key), errRemove)
	}
}

func (s *ClaudeCloakLineageStore) path(key string) string {
	return filepath.Join(s.dir, key+".json")
}

func (s *ClaudeCloakLineageStore) readFile(path string, now time.Time) (claudeCloakLineage, bool) {
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		if !errors.Is(errRead, os.ErrNotExist) {
			log.Warnf("claude cloak lineage: read %s: %v", path, errRead)
		}
		return claudeCloakLineage{}, false
	}
	var file claudeCloakLineageFile
	if errDecode := json.Unmarshal(raw, &file); errDecode != nil || file.Version != claudeCloakLineageFileVersion {
		log.Warnf("claude cloak lineage: ignoring unreadable %s (version %d, err %v); it is replaced on the next write", path, file.Version, errDecode)
		return claudeCloakLineage{}, false
	}
	if claudeCloakLineageExpired(file.Used, now) {
		return claudeCloakLineage{}, false
	}
	return file.claudeCloakLineage, true
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
// bounds.
func (s *ClaudeCloakLineageStore) pruneMemoryLocked(now time.Time, keep string) {
	if len(s.sessions) <= s.limits.sessions && s.bytes <= s.limits.totalBytes {
		return
	}
	keys := make([]string, 0, len(s.sessions))
	for key, entry := range s.sessions {
		if key != keep && claudeCloakLineageExpired(entry.used, now) {
			s.bytes -= entry.size
			delete(s.sessions, key)
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
		s.bytes -= s.sessions[key].size
		delete(s.sessions, key)
	}
}

// pruneDir sweeps the directory: expired files go, then the least recently
// used ones until the directory is within its Session and byte bounds. A
// sweep already running elsewhere in the process is not waited for.
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
		path string
		used time.Time
		size int64
	}
	var live []session
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil {
			continue
		}
		path := filepath.Join(s.dir, entry.Name())
		if now.Sub(info.ModTime()) >= claudeCloakLineageTTL {
			_ = os.Remove(path)
			continue
		}
		live = append(live, session{path: path, used: info.ModTime(), size: info.Size()})
		total += info.Size()
	}
	sort.Slice(live, func(i, j int) bool { return live[i].used.Before(live[j].used) })
	for len(live) > 1 && (len(live) > s.limits.sessions || total > int64(s.limits.totalBytes)) {
		_ = os.Remove(live[0].path)
		total -= live[0].size
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
