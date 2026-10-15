package helps

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

// The memo is sized for live conversations, not for history: an entry lives
// while its conversation keeps sending it, and is evicted alone, least
// recently used first, only when the bound is reached.
const (
	claudeCloakAnchorTTL           = 30 * 24 * time.Hour
	claudeCloakAnchorMaxEntries    = 16384
	claudeCloakAnchorTouchInterval = time.Hour
	claudeCloakAnchorFileVersion   = 1
	claudeCloakAnchorFileName      = "claude-cloak-anchors.json"
)

// ClaudeCloakAnchorMark is what the cloak recorded about one caller message
// key: the date reminder it forwarded there, or that the key is ambiguous and
// must not be trusted on its own.
type ClaudeCloakAnchorMark struct {
	Date      string `json:"d,omitempty"`
	Ambiguous bool   `json:"a,omitempty"`
}

type claudeCloakAnchorEntry struct {
	ClaudeCloakAnchorMark
	Used  int64 `json:"u"`
	saved int64
}

type claudeCloakAnchorFile struct {
	Version int                                `json:"version"`
	Entries map[string]*claudeCloakAnchorEntry `json:"entries"`
}

// ClaudeCloakAnchorMemo remembers which caller user messages carried the
// cloak's injections. Thinking produced after such a message stays valid only
// while the proxy forwards it with the same bytes, so the memo has to outlive
// compactions and restarts. With a path it persists to one JSON file, written
// whole to a temporary file and renamed over the old one. Keys are hashes and
// marks are dates; no message content is stored.
type ClaudeCloakAnchorMemo struct {
	mu       sync.Mutex
	path     string
	entries  map[string]*claudeCloakAnchorEntry
	diskMod  time.Time
	diskSize int64
	dirty    bool
	now      func() time.Time
}

// NewClaudeCloakAnchorMemo returns a memo backed by path, or held in memory
// only when path is empty. A missing file starts empty; an unreadable or
// corrupt one is logged, starts empty and is replaced on the next write.
func NewClaudeCloakAnchorMemo(path string) *ClaudeCloakAnchorMemo {
	m := &ClaudeCloakAnchorMemo{path: path, entries: make(map[string]*claudeCloakAnchorEntry), now: time.Now}
	m.mu.Lock()
	m.mergeDiskLocked()
	m.mu.Unlock()
	return m
}

var claudeCloakAnchorMemo atomic.Pointer[ClaudeCloakAnchorMemo]

func init() {
	claudeCloakAnchorMemo.Store(NewClaudeCloakAnchorMemo(""))
}

// ClaudeCloakAnchors returns the process-wide memo.
func ClaudeCloakAnchors() *ClaudeCloakAnchorMemo {
	return claudeCloakAnchorMemo.Load()
}

// SetClaudeCloakAnchors installs the process-wide memo and returns the previous one.
func SetClaudeCloakAnchors(m *ClaudeCloakAnchorMemo) *ClaudeCloakAnchorMemo {
	return claudeCloakAnchorMemo.Swap(m)
}

// ClaudeCloakAnchorMemoPath returns where the server keeps the memo, following
// the discovery state directory's precedence: WRITABLE_PATH, then the user
// config directory. It returns "" when neither resolves.
func ClaudeCloakAnchorMemoPath() string {
	if writable := util.WritablePath(); writable != "" {
		return filepath.Join(writable, "state", claudeCloakAnchorFileName)
	}
	if configDir, errConfig := os.UserConfigDir(); errConfig == nil && configDir != "" {
		return filepath.Join(configDir, "cpa", claudeCloakAnchorFileName)
	}
	if homeDir, errHome := os.UserHomeDir(); errHome == nil && homeDir != "" {
		return filepath.Join(homeDir, ".config", "cpa", claudeCloakAnchorFileName)
	}
	return ""
}

// Reload merges entries another process wrote to the file since this one last
// read or wrote it.
func (m *ClaudeCloakAnchorMemo) Reload() {
	if m.path == "" {
		return
	}
	info, errStat := os.Stat(m.path)
	m.mu.Lock()
	defer m.mu.Unlock()
	if errStat == nil && (!info.ModTime().Equal(m.diskMod) || info.Size() != m.diskSize) {
		m.mergeDiskLocked()
	}
}

// Lookup returns the mark for key. With touch, the lookup counts as use and
// extends the entry's lifetime.
func (m *ClaudeCloakAnchorMemo) Lookup(key string, touch bool) (ClaudeCloakAnchorMark, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().Unix()
	entry, ok := m.entries[key]
	if !ok || m.expired(entry, now) {
		return ClaudeCloakAnchorMark{}, false
	}
	if touch {
		entry.Used = now
		if time.Duration(now-entry.saved)*time.Second >= claudeCloakAnchorTouchInterval {
			m.dirty = true
		}
	}
	return entry.ClaudeCloakAnchorMark, true
}

// Remember records marks as used now and persists when an entry changed or
// a lifetime extension is due to reach the file.
func (m *ClaudeCloakAnchorMemo) Remember(marks map[string]ClaudeCloakAnchorMark) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().Unix()
	changed := false
	for key, mark := range marks {
		entry, ok := m.entries[key]
		if ok && !m.expired(entry, now) && entry.ClaudeCloakAnchorMark == mark {
			entry.Used = now
			continue
		}
		m.entries[key] = &claudeCloakAnchorEntry{ClaudeCloakAnchorMark: mark, Used: now}
		changed = true
	}
	if changed {
		m.pruneLocked(now)
	}
	if changed || m.dirty {
		m.saveLocked()
	}
}

func (m *ClaudeCloakAnchorMemo) expired(entry *claudeCloakAnchorEntry, now int64) bool {
	return time.Duration(now-entry.Used)*time.Second >= claudeCloakAnchorTTL
}

// pruneLocked keeps the memo within its bound: past it, expired entries go
// first, then the least recently used ones. It never clears live entries
// wholesale. Below the bound expired entries are only invisible; saveLocked
// leaves them out of the file.
func (m *ClaudeCloakAnchorMemo) pruneLocked(now int64) {
	if len(m.entries) <= claudeCloakAnchorMaxEntries {
		return
	}
	m.dropExpiredLocked(now)
	excess := len(m.entries) - claudeCloakAnchorMaxEntries
	if excess <= 0 {
		return
	}
	keys := make([]string, 0, len(m.entries))
	for key := range m.entries {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return m.entries[keys[i]].Used < m.entries[keys[j]].Used })
	for _, key := range keys[:excess] {
		delete(m.entries, key)
	}
}

// mergeDiskLocked adds file entries this process lacks or used less recently.
func (m *ClaudeCloakAnchorMemo) mergeDiskLocked() {
	if m.path == "" {
		return
	}
	info, errStat := os.Stat(m.path)
	if errStat != nil {
		if !errors.Is(errStat, os.ErrNotExist) {
			log.Warnf("claude cloak anchors: stat %s: %v", m.path, errStat)
		}
		return
	}
	m.diskMod, m.diskSize = info.ModTime(), info.Size()
	raw, errRead := os.ReadFile(m.path)
	if errRead != nil {
		log.Warnf("claude cloak anchors: read %s: %v", m.path, errRead)
		return
	}
	var file claudeCloakAnchorFile
	if errDecode := json.Unmarshal(raw, &file); errDecode != nil || file.Version != claudeCloakAnchorFileVersion {
		log.Warnf("claude cloak anchors: ignoring unreadable %s (version %d, err %v); it is replaced on the next write", m.path, file.Version, errDecode)
		return
	}
	now := m.now().Unix()
	for key, entry := range file.Entries {
		if entry == nil || m.expired(entry, now) {
			continue
		}
		if current, ok := m.entries[key]; ok && current.Used >= entry.Used {
			continue
		}
		entry.saved = entry.Used
		m.entries[key] = entry
	}
	m.pruneLocked(now)
}

func (m *ClaudeCloakAnchorMemo) dropExpiredLocked(now int64) {
	for key, entry := range m.entries {
		if m.expired(entry, now) {
			delete(m.entries, key)
		}
	}
}

func (m *ClaudeCloakAnchorMemo) saveLocked() {
	m.dirty = false
	if m.path == "" {
		return
	}
	m.mergeDiskLocked()
	m.dropExpiredLocked(m.now().Unix())
	raw, errEncode := json.Marshal(claudeCloakAnchorFile{Version: claudeCloakAnchorFileVersion, Entries: m.entries})
	if errEncode != nil {
		log.Warnf("claude cloak anchors: encode: %v", errEncode)
		return
	}
	if errWrite := writeFileAtomically(m.path, raw); errWrite != nil {
		log.Warnf("claude cloak anchors: write %s: %v", m.path, errWrite)
		return
	}
	if info, errStat := os.Stat(m.path); errStat == nil {
		m.diskMod, m.diskSize = info.ModTime(), info.Size()
	}
	for _, entry := range m.entries {
		entry.saved = entry.Used
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
