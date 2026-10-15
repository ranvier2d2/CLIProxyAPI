package helps

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// A lineage lives while its Session keeps sending requests. Past the bound the
// least recently used Session is forgotten alone; live ones are never cleared
// wholesale.
const (
	claudeCloakLineageTTL         = 30 * 24 * time.Hour
	claudeCloakLineageMaxSessions = 1024
	claudeCloakLineageFileVersion = 1
	claudeCloakLineageDirName     = "claude-cloak-lineage"
)

// ClaudeCloakLineageMessage is what the cloak forwarded for one caller message
// of a Session's last request: the message's hash, whether it holds a
// compaction block, and the date reminder the cloak anchored on it, if any.
// The caller's message content is never stored.
type ClaudeCloakLineageMessage struct {
	Hash       string `json:"h"`
	Compaction bool   `json:"c,omitempty"`
	Date       string `json:"d,omitempty"`
}

type claudeCloakLineageFile struct {
	Version  int                         `json:"version"`
	Used     int64                       `json:"used"`
	Messages []ClaudeCloakLineageMessage `json:"messages"`
}

// ClaudeCloakLineageStore keeps one lineage per Session. With a directory each
// Session is one JSON file, written whole to a temporary file and renamed over
// the old one, and read again on every request, so a restart or a second
// process sharing the directory sees the last write. Without one it is held in
// memory only.
type ClaudeCloakLineageStore struct {
	mu          sync.Mutex
	dir         string
	sessions    map[string]*claudeCloakLineageFile
	now         func() time.Time
	maxSessions int
}

// NewClaudeCloakLineageStore returns a store backed by dir, or held in memory
// when dir is empty.
func NewClaudeCloakLineageStore(dir string) *ClaudeCloakLineageStore {
	return &ClaudeCloakLineageStore{dir: dir, sessions: make(map[string]*claudeCloakLineageFile), now: time.Now, maxSessions: claudeCloakLineageMaxSessions}
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

func (s *ClaudeCloakLineageStore) load(key string) ([]ClaudeCloakLineageMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.dir == "" {
		file, ok := s.sessions[key]
		if !ok || claudeCloakLineageExpired(file.Used, now) {
			return nil, false
		}
		return file.Messages, true
	}
	path := filepath.Join(s.dir, key+".json")
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		if !errors.Is(errRead, os.ErrNotExist) {
			log.Warnf("claude cloak lineage: read %s: %v", path, errRead)
		}
		return nil, false
	}
	var file claudeCloakLineageFile
	if errDecode := json.Unmarshal(raw, &file); errDecode != nil || file.Version != claudeCloakLineageFileVersion {
		log.Warnf("claude cloak lineage: ignoring unreadable %s (version %d, err %v); it is replaced on the next write", path, file.Version, errDecode)
		return nil, false
	}
	if claudeCloakLineageExpired(file.Used, now) {
		return nil, false
	}
	return file.Messages, true
}

func (s *ClaudeCloakLineageStore) save(key string, messages []ClaudeCloakLineageMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	file := &claudeCloakLineageFile{Version: claudeCloakLineageFileVersion, Used: now.Unix(), Messages: messages}
	if s.dir == "" {
		_, existed := s.sessions[key]
		s.sessions[key] = file
		if !existed {
			s.pruneMemoryLocked(now)
		}
		return
	}
	path := filepath.Join(s.dir, key+".json")
	_, errStat := os.Stat(path)
	raw, errEncode := json.Marshal(file)
	if errEncode != nil {
		log.Warnf("claude cloak lineage: encode: %v", errEncode)
		return
	}
	if errWrite := writeFileAtomically(path, raw); errWrite != nil {
		log.Warnf("claude cloak lineage: write %s: %v", path, errWrite)
		return
	}
	// The modification time is the Session's last use; pruning reads only that.
	if errTimes := os.Chtimes(path, now, now); errTimes != nil {
		log.Warnf("claude cloak lineage: touch %s: %v", path, errTimes)
	}
	if errors.Is(errStat, os.ErrNotExist) {
		s.pruneDirLocked(now)
	}
}

func claudeCloakLineageExpired(used int64, now time.Time) bool {
	return now.Sub(time.Unix(used, 0)) >= claudeCloakLineageTTL
}

func (s *ClaudeCloakLineageStore) pruneMemoryLocked(now time.Time) {
	if len(s.sessions) <= s.maxSessions {
		return
	}
	keys := make([]string, 0, len(s.sessions))
	for key, file := range s.sessions {
		if claudeCloakLineageExpired(file.Used, now) {
			delete(s.sessions, key)
			continue
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return s.sessions[keys[i]].Used < s.sessions[keys[j]].Used })
	for _, key := range keys[:max(0, len(keys)-s.maxSessions)] {
		delete(s.sessions, key)
	}
}

// pruneDirLocked runs when a new Session file appears: expired files go, then
// the least recently used ones past the bound.
func (s *ClaudeCloakLineageStore) pruneDirLocked(now time.Time) {
	entries, errList := os.ReadDir(s.dir)
	if errList != nil {
		log.Warnf("claude cloak lineage: list %s: %v", s.dir, errList)
		return
	}
	type session struct {
		path string
		used time.Time
	}
	var live []session
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
		live = append(live, session{path: path, used: info.ModTime()})
	}
	sort.Slice(live, func(i, j int) bool { return live[i].used.Before(live[j].used) })
	for _, stale := range live[:max(0, len(live)-s.maxSessions)] {
		_ = os.Remove(stale.path)
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

// ClaudeCloakLineageFallbacks returns how many generation requests with a
// compaction block this process forwarded with main's placement for reason.
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
