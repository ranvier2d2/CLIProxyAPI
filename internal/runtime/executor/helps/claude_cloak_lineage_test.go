package helps

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestClaudeCloakLineageStore returns a store on a test clock whose
// queued writes finish before the test's directory is removed.
func newTestClaudeCloakLineageStore(t *testing.T, dir string, now *time.Time, maxSessions int) *ClaudeCloakLineageStore {
	s := NewClaudeCloakLineageStore(dir)
	s.now = func() time.Time { return *now }
	s.limits.sessions = maxSessions
	t.Cleanup(s.Flush)
	return s
}

// testSave writes messages as key's lineage over whatever revision it holds.
func testSave(t *testing.T, s *ClaudeCloakLineageStore, key string, messages []ClaudeCloakLineageMessage) {
	t.Helper()
	current, _ := s.load(key)
	if _, _, outcome := s.swap(key, current.Rev, claudeCloakLineage{Messages: messages}); outcome != claudeCloakLineageSwapped {
		t.Fatalf("save %s: outcome %d", key, outcome)
	}
}

var testLineage = []ClaudeCloakLineageMessage{{Hash: "h0", Compaction: true}, {Hash: "h1", Date: "2026-10-15"}}

// assertLineage checks what a request of key would read once the store's
// queued writes reached the disk.
func assertLineage(t *testing.T, s *ClaudeCloakLineageStore, key string, want bool) {
	t.Helper()
	s.Flush()
	got, ok := s.load(key)
	if ok != want {
		t.Fatalf("session %s found=%v, want %v", key, ok, want)
	}
	if ok && (len(got.Messages) != len(testLineage) || got.Messages[1] != testLineage[1]) {
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
		s := newTestClaudeCloakLineageStore(t, dir, &now, 8)
		testSave(t, s, "live", testLineage)
		for day := 1; day <= 60; day++ {
			now = now.Add(24 * time.Hour)
			assertLineage(t, s, "live", true)
			testSave(t, s, "live", testLineage)
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
		s := newTestClaudeCloakLineageStore(t, dir, &now, 4)
		for i := 0; i < 4; i++ {
			now = now.Add(time.Minute)
			testSave(t, s, fmt.Sprintf("s%d", i), testLineage)
		}
		now = now.Add(time.Minute)
		testSave(t, s, "s0", testLineage) // s1 is now the least recently used
		now = now.Add(time.Minute)
		testSave(t, s, "s4", testLineage)
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
			s := newTestClaudeCloakLineageStore(t, dir, &now, 8)
			if errWrite := os.WriteFile(filepath.Join(dir, "k.json"), []byte(raw), 0o600); errWrite != nil {
				t.Fatal(errWrite)
			}
			assertLineage(t, s, "k", false)
			testSave(t, s, "k", testLineage)
			assertLineage(t, s, "k", true)
		})
	}
}

// A restart, or a second process over the same directory, reads what the
// other wrote last, once it reached the disk. The files hold hashes and dates,
// never the caller's Session id or message content.
func TestClaudeCloakLineageSharedDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lineage")
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	a := newTestClaudeCloakLineageStore(t, dir, &now, 8)
	b := newTestClaudeCloakLineageStore(t, dir, &now, 8)
	key := claudeCloakLineageKey("pixir-session-secret-name")
	testSave(t, a, key, testLineage)
	a.Flush()
	assertLineage(t, b, key, true)
	testSave(t, b, key, testLineage[:1])
	b.Flush()
	if got, _ := a.load(key); len(got.Messages) != 1 {
		t.Fatalf("process A reads %d messages, want process B's 1", len(got.Messages))
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

// Writers of one Session that read the same revision race to write the next
// one: exactly one wins and the others see its lineage, within one process
// and across stores sharing a directory. Within a store the race is decided in
// memory. Across stores each may admit one writer in memory, and the disk
// decides: exactly one of those writes lands, every other one is counted
// stale, and both stores then read the one that landed.
func TestClaudeCloakLineageConcurrentWritersOneRevisionWins(t *testing.T) {
	forEachLineageBacking(t, func(t *testing.T, dir string) {
		now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
		stores := []*ClaudeCloakLineageStore{newTestClaudeCloakLineageStore(t, dir, &now, 64)}
		if dir != "" {
			stores = append(stores, newTestClaudeCloakLineageStore(t, dir, &now, 64))
		}
		for round := 0; round < 20; round++ {
			key := fmt.Sprintf("w%d", round%4)
			read, _ := stores[0].load(key)
			staleBefore := ClaudeCloakLineageFallbacks(ClaudeCloakRecoveryStale)
			var wg sync.WaitGroup
			var mu sync.Mutex
			won := 0
			for worker := 0; worker < 8; worker++ {
				wg.Add(1)
				go func(worker int) {
					defer wg.Done()
					s := stores[worker%len(stores)]
					messages := []ClaudeCloakLineageMessage{{Hash: fmt.Sprintf("r%d-w%d", round, worker)}}
					written, _, outcome := s.swap(key, read.Rev, claudeCloakLineage{Messages: messages})
					if outcome == claudeCloakLineageSwapped {
						mu.Lock()
						won++
						mu.Unlock()
						return
					}
					if outcome != claudeCloakLineageStale || written.Rev == read.Rev {
						t.Errorf("round %d worker %d: outcome %d at revision %d", round, worker, outcome, written.Rev)
					}
				}(worker)
			}
			wg.Wait()
			for _, s := range stores {
				s.Flush()
			}
			if landed := int64(won) - (ClaudeCloakLineageFallbacks(ClaudeCloakRecoveryStale) - staleBefore); won < 1 || won > len(stores) || landed != 1 {
				t.Fatalf("round %d: %d writers won the same revision and %d landed", round, won, landed)
			}
			first, _ := stores[0].load(key)
			last, _ := stores[len(stores)-1].load(key)
			if first.Rev != last.Rev {
				t.Fatalf("round %d: the stores read different revisions", round)
			}
		}
	})
}

// Past the total byte budget the least recently used Sessions go until the
// store fits; the Session just written stays.
func TestClaudeCloakLineageTotalBudgetEvictsLeastRecentlyUsed(t *testing.T) {
	forEachLineageBacking(t, func(t *testing.T, dir string) {
		now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
		s := newTestClaudeCloakLineageStore(t, dir, &now, 64)
		messages := make([]ClaudeCloakLineageMessage, 100)
		for i := range messages {
			messages[i] = ClaudeCloakLineageMessage{Hash: fmt.Sprintf("%032x", i)}
		}
		raw, _ := json.Marshal(claudeCloakLineageFile{Version: claudeCloakLineageFileVersion, Used: now.Unix(), claudeCloakLineage: claudeCloakLineage{Rev: 1 << 63, Messages: messages}})
		s.limits.totalBytes = 3*len(raw) + len(raw)/2
		for i := 0; i < 5; i++ {
			now = now.Add(time.Minute)
			testSave(t, s, fmt.Sprintf("s%d", i), messages)
		}
		s.Flush()
		for i := 0; i < 5; i++ {
			_, ok := s.load(fmt.Sprintf("s%d", i))
			if ok != (i >= 2) {
				t.Fatalf("session s%d kept=%v; want only the three most recent within the budget", i, ok)
			}
		}
	})
}
