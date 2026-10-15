package helps

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestClaudeCloakAnchorMemo(path string, now *time.Time) *ClaudeCloakAnchorMemo {
	m := NewClaudeCloakAnchorMemo("")
	m.path = path
	m.now = func() time.Time { return *now }
	m.mu.Lock()
	m.mergeDiskLocked()
	m.mu.Unlock()
	return m
}

var memoMark = ClaudeCloakAnchorMark{Date: "2026-10-15"}

// A conversation that keeps sending its anchor keeps it for as long as it
// runs; only an idle one expires.
func TestClaudeCloakAnchorMemoLifetimeSlidesWithUse(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	m := newTestClaudeCloakAnchorMemo("", &now)
	m.Remember(map[string]ClaudeCloakAnchorMark{"k": memoMark})
	for day := 1; day <= 60; day++ {
		now = now.Add(24 * time.Hour)
		if mark, ok := m.Lookup("k", true); !ok || mark != memoMark {
			t.Fatalf("an anchor used daily expired on day %d", day)
		}
	}
	now = now.Add(claudeCloakAnchorTTL)
	if _, ok := m.Lookup("k", true); ok {
		t.Fatal("an anchor idle for the whole lifetime is still found")
	}
}

// A lookup without touch, as count_tokens makes, does not extend a lifetime.
func TestClaudeCloakAnchorMemoReadOnlyLookupDoesNotExtend(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	m := newTestClaudeCloakAnchorMemo("", &now)
	m.Remember(map[string]ClaudeCloakAnchorMark{"k": memoMark})
	now = now.Add(claudeCloakAnchorTTL - time.Hour)
	if _, ok := m.Lookup("k", false); !ok {
		t.Fatal("anchor missing before its lifetime ends")
	}
	now = now.Add(time.Hour)
	if _, ok := m.Lookup("k", false); ok {
		t.Fatal("a read-only lookup extended the lifetime")
	}
}

// At the bound only the least recently used entry goes; live ones stay.
func TestClaudeCloakAnchorMemoEvictsOnlyTheLeastRecentlyUsed(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	m := newTestClaudeCloakAnchorMemo("", &now)
	m.Remember(map[string]ClaudeCloakAnchorMark{"oldest": memoMark})
	now = now.Add(time.Second)
	m.Remember(map[string]ClaudeCloakAnchorMark{"live": memoMark})
	for i := 2; i < claudeCloakAnchorMaxEntries; i++ {
		now = now.Add(time.Second)
		m.Remember(map[string]ClaudeCloakAnchorMark{fmt.Sprintf("fill-%d", i): memoMark})
	}
	now = now.Add(time.Second)
	if _, ok := m.Lookup("live", true); !ok {
		t.Fatal("live entry missing at the bound")
	}
	now = now.Add(time.Second)
	m.Remember(map[string]ClaudeCloakAnchorMark{"one-more": memoMark})
	if _, ok := m.Lookup("oldest", false); ok {
		t.Fatal("the least recently used entry survived past the bound")
	}
	for _, key := range []string{"live", "fill-2", "one-more"} {
		if _, ok := m.Lookup(key, false); !ok {
			t.Fatalf("%s was evicted with the least recently used entry", key)
		}
	}
	if got := len(m.entries); got != claudeCloakAnchorMaxEntries {
		t.Fatalf("%d entries after eviction, want %d", got, claudeCloakAnchorMaxEntries)
	}
}

// A restarted process reads what the previous one wrote.
func TestClaudeCloakAnchorMemoSurvivesRestart(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state", claudeCloakAnchorFileName)
	first := newTestClaudeCloakAnchorMemo(path, &now)
	first.Remember(map[string]ClaudeCloakAnchorMark{"k": memoMark, "amb": {Ambiguous: true}})
	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatal(errStat)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("memo file mode %o, want 600", perm)
	}

	restarted := newTestClaudeCloakAnchorMemo(path, &now)
	if mark, ok := restarted.Lookup("k", true); !ok || mark != memoMark {
		t.Fatalf("restarted memo lost the anchor: %+v %v", mark, ok)
	}
	if mark, ok := restarted.Lookup("amb", true); !ok || !mark.Ambiguous {
		t.Fatalf("restarted memo lost the ambiguous mark: %+v %v", mark, ok)
	}
}

// A missing file starts empty; a corrupt one starts empty and is replaced
// with a readable file on the next write.
func TestClaudeCloakAnchorMemoMissingOrCorruptFile(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	missing := newTestClaudeCloakAnchorMemo(filepath.Join(dir, "absent", claudeCloakAnchorFileName), &now)
	if len(missing.entries) != 0 {
		t.Fatal("a missing file produced entries")
	}

	path := filepath.Join(dir, claudeCloakAnchorFileName)
	if errWrite := os.WriteFile(path, []byte(`{"version":1,"entries":{"k":`), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	corrupt := newTestClaudeCloakAnchorMemo(path, &now)
	if len(corrupt.entries) != 0 {
		t.Fatal("a corrupt file produced entries")
	}
	corrupt.Remember(map[string]ClaudeCloakAnchorMark{"k": memoMark})
	if mark, ok := newTestClaudeCloakAnchorMemo(path, &now).Lookup("k", false); !ok || mark != memoMark {
		t.Fatal("the next write did not replace the corrupt file")
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".*.tmp-*"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}

// Two processes sharing the file see each other's anchors, and neither write
// erases the other's.
func TestClaudeCloakAnchorMemoTwoProcessesShareTheFile(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), claudeCloakAnchorFileName)
	a := newTestClaudeCloakAnchorMemo(path, &now)
	b := newTestClaudeCloakAnchorMemo(path, &now)
	a.Remember(map[string]ClaudeCloakAnchorMark{"from-a": memoMark})
	now = now.Add(time.Second)
	b.Remember(map[string]ClaudeCloakAnchorMark{"from-b": memoMark})
	a.Reload()
	if _, ok := a.Lookup("from-b", false); !ok {
		t.Fatal("process a does not see process b's anchor after reload")
	}
	if _, ok := newTestClaudeCloakAnchorMemo(path, &now).Lookup("from-a", false); !ok {
		t.Fatal("process b's write erased process a's anchor")
	}
}

func TestClaudeCloakAnchorMemoConcurrentUse(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	m := newTestClaudeCloakAnchorMemo(filepath.Join(t.TempDir(), claudeCloakAnchorFileName), &now)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("k-%d", i%4)
			m.Remember(map[string]ClaudeCloakAnchorMark{key: memoMark})
			m.Reload()
			if _, ok := m.Lookup(key, true); !ok {
				t.Errorf("%s missing right after it was remembered", key)
			}
		}(i)
	}
	wg.Wait()
}
