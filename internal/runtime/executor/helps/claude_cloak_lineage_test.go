package helps

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestClaudeCloakLineageStore(dir string, now *time.Time, maxSessions int) *ClaudeCloakLineageStore {
	s := NewClaudeCloakLineageStore(dir)
	s.now = func() time.Time { return *now }
	s.maxSessions = maxSessions
	return s
}

var testLineage = []ClaudeCloakLineageMessage{{Hash: "h0", Compaction: true}, {Hash: "h1", Date: "2026-10-15"}}

func assertLineage(t *testing.T, s *ClaudeCloakLineageStore, key string, want bool) {
	t.Helper()
	got, ok := s.load(key)
	if ok != want {
		t.Fatalf("session %s found=%v, want %v", key, ok, want)
	}
	if ok && (len(got) != len(testLineage) || got[1] != testLineage[1]) {
		t.Fatalf("session %s lineage %+v, want %+v", key, got, testLineage)
	}
}

func forEachLineageBacking(t *testing.T, run func(t *testing.T, dir string)) {
	t.Run("memory", func(t *testing.T) { run(t, "") })
	t.Run("disk", func(t *testing.T) { run(t, filepath.Join(t.TempDir(), "lineage")) })
}

// A Session that keeps sending requests keeps its lineage for as long as it
// runs; only an idle one expires, and reading does not count as use.
func TestClaudeCloakLineageLifetimeSlidesWithUse(t *testing.T) {
	forEachLineageBacking(t, func(t *testing.T, dir string) {
		now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
		s := newTestClaudeCloakLineageStore(dir, &now, 8)
		s.save("live", testLineage)
		for day := 1; day <= 60; day++ {
			now = now.Add(24 * time.Hour)
			assertLineage(t, s, "live", true)
			s.save("live", testLineage)
		}
		now = now.Add(claudeCloakLineageTTL - time.Hour)
		assertLineage(t, s, "live", true)
		now = now.Add(time.Hour)
		assertLineage(t, s, "live", false)
	})
}

// Past the bound only the least recently used Session is forgotten; the
// others stay, however much unrelated traffic arrives.
func TestClaudeCloakLineageEvictsOnlyTheLeastRecentlyUsed(t *testing.T) {
	forEachLineageBacking(t, func(t *testing.T, dir string) {
		now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
		s := newTestClaudeCloakLineageStore(dir, &now, 4)
		for i := 0; i < 4; i++ {
			now = now.Add(time.Minute)
			s.save(fmt.Sprintf("s%d", i), testLineage)
		}
		now = now.Add(time.Minute)
		s.save("s0", testLineage) // s1 is now the least recently used
		now = now.Add(time.Minute)
		s.save("s4", testLineage)
		assertLineage(t, s, "s1", false)
		for _, key := range []string{"s0", "s2", "s3", "s4"} {
			assertLineage(t, s, key, true)
		}
	})
}

// A file that cannot be read as a lineage is an unknown Session, and the next
// request's lineage replaces it.
func TestClaudeCloakLineageUnreadableFileIsUnknown(t *testing.T) {
	for name, raw := range map[string]string{
		"truncated":       `{"version":1,"used":`,
		"unknown version": `{"version":999,"used":1,"messages":[]}`,
		"not json":        `lineage`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
			s := newTestClaudeCloakLineageStore(dir, &now, 8)
			if errWrite := os.WriteFile(filepath.Join(dir, "k.json"), []byte(raw), 0o600); errWrite != nil {
				t.Fatal(errWrite)
			}
			assertLineage(t, s, "k", false)
			s.save("k", testLineage)
			assertLineage(t, s, "k", true)
		})
	}
}

// A restart, or a second process over the same directory, reads what the
// other wrote last. The files hold hashes and dates, never the caller's
// Session id or message content.
func TestClaudeCloakLineageSharedDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lineage")
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	a := newTestClaudeCloakLineageStore(dir, &now, 8)
	b := newTestClaudeCloakLineageStore(dir, &now, 8)
	key := claudeCloakLineageKey("pixir-session-secret-name")
	a.save(key, testLineage)
	assertLineage(t, b, key, true)
	b.save(key, testLineage[:1])
	if got, _ := a.load(key); len(got) != 1 {
		t.Fatalf("process A reads %d messages, want process B's 1", len(got))
	}

	info, errStat := os.Stat(dir)
	if errStat != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode %v, err %v; want 0700", info.Mode().Perm(), errStat)
	}
	path := filepath.Join(dir, key+".json")
	fileInfo, errFile := os.Stat(path)
	if errFile != nil || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v, err %v; want 0600", fileInfo.Mode().Perm(), errFile)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "pixir-session-secret-name") || strings.Contains(filepath.Base(path), "pixir") {
		t.Fatal("the caller's Session id reached the disk")
	}
}

func TestClaudeCloakLineageConcurrentSessions(t *testing.T) {
	forEachLineageBacking(t, func(t *testing.T, dir string) {
		now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
		s := newTestClaudeCloakLineageStore(dir, &now, 64)
		var wg sync.WaitGroup
		for worker := 0; worker < 8; worker++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				for i := 0; i < 20; i++ {
					key := fmt.Sprintf("w%d", worker%4)
					s.save(key, testLineage)
					if _, ok := s.load(key); !ok {
						t.Errorf("worker %d lost its own session", worker)
						return
					}
				}
			}(worker)
		}
		wg.Wait()
	})
}
