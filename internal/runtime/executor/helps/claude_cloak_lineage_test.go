package helps

import (
	"fmt"
	"sync"
	"testing"
)

func newTestClaudeCloakLineageStore(maxSessions int) *ClaudeCloakLineageStore {
	s := NewClaudeCloakLineageStore()
	s.limits.sessions = maxSessions
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

func assertLineage(t *testing.T, s *ClaudeCloakLineageStore, key string, want bool) {
	t.Helper()
	got, ok := s.load(key)
	if ok != want {
		t.Fatalf("session %s found=%v, want %v", key, ok, want)
	}
	if ok && (len(got.Messages) != len(testLineage) || got.Messages[1] != testLineage[1]) {
		t.Fatalf("session %s lineage %+v, want %+v", key, got, testLineage)
	}
}

// Past the bound only the least recently written Session is forgotten; the
// others stay, however much unrelated traffic arrives.
func TestClaudeCloakLineageEvictsOnlyTheLeastRecentlyUsed(t *testing.T) {
	s := newTestClaudeCloakLineageStore(4)
	for i := 0; i < 4; i++ {
		testSave(t, s, fmt.Sprintf("s%d", i), testLineage)
	}
	testSave(t, s, "s0", testLineage) // s1 is now the least recently written
	testSave(t, s, "s4", testLineage)
	assertLineage(t, s, "s1", false)
	for _, key := range []string{"s0", "s2", "s3", "s4"} {
		assertLineage(t, s, key, true)
	}
}

// Writers of one Session that read the same revision race to write the next
// one: exactly one wins and the others see its lineage.
func TestClaudeCloakLineageConcurrentWritersOneRevisionWins(t *testing.T) {
	s := newTestClaudeCloakLineageStore(64)
	for round := 0; round < 20; round++ {
		key := fmt.Sprintf("w%d", round%4)
		read, _ := s.load(key)
		var wg sync.WaitGroup
		var mu sync.Mutex
		won := 0
		for worker := 0; worker < 8; worker++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
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
		if won != 1 {
			t.Fatalf("round %d: %d writers won the same revision", round, won)
		}
	}
}

// Past the total byte budget the least recently written Sessions go until the
// store fits; the Session just written stays.
func TestClaudeCloakLineageTotalBudgetEvictsLeastRecentlyUsed(t *testing.T) {
	s := newTestClaudeCloakLineageStore(64)
	messages := make([]ClaudeCloakLineageMessage, 100)
	for i := range messages {
		messages[i] = ClaudeCloakLineageMessage{Hash: fmt.Sprintf("%032x", i)}
	}
	size := claudeCloakLineage{Messages: messages}.size()
	s.limits.totalBytes = 3*size + size/2
	for i := 0; i < 5; i++ {
		testSave(t, s, fmt.Sprintf("s%d", i), messages)
	}
	for i := 0; i < 5; i++ {
		_, ok := s.load(fmt.Sprintf("s%d", i))
		if ok != (i >= 2) {
			t.Fatalf("session s%d kept=%v; want only the three most recent within the budget", i, ok)
		}
	}
	if s.bytes != 3*size {
		t.Fatalf("store accounts %d bytes, want %d", s.bytes, 3*size)
	}
}
