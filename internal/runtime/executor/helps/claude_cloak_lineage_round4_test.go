package helps

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// A stale oversized plan leaves the newer lineage in place and counts stale:
// the revision is checked before the size. Ported from review sol-4, which
// failed at 5859bec5 in both backings: the oversize drop ran first and erased
// the newer accepted lineage.
func TestSol4StaleOversizedCommitMustKeepNewerLineage(t *testing.T) {
	forEachLineageBacking(t, func(t *testing.T, dir string) {
		old := SetClaudeCloakLineages(NewClaudeCloakLineageStore(dir))
		t.Cleanup(func() { SetClaudeCloakLineages(old) })
		const sid, day = "sol4-stale-oversize", "2026-10-15"
		initial := []string{sol3User("start"), sol3Assistant("old"), sol3User("kept")}
		PlanClaudeCloak(sol3Body(initial...), sid, day).Commit()
		huge := append([]string{}, initial...)
		for i := 0; i < 4098; i++ {
			huge = append(huge, sol3User(fmt.Sprintf("long%d", i)))
		}
		delayed := PlanClaudeCloak(sol3Body(huge...), sid, day)
		epoch := sol3Body(sol3Block("new"), initial[1], initial[2], sol3Assistant("fresh"), sol3User("anchor"))
		PlanClaudeCloak(epoch, sid, day).Commit()
		before, known := ClaudeCloakLineages().load(claudeCloakLineageKey(sid))
		if !known {
			t.Fatal("setup lost accepted lineage")
		}
		delayed.Commit()
		after, known := ClaudeCloakLineages().load(claudeCloakLineageKey(sid))
		if !known || after.Rev != before.Rev {
			t.Fatalf("stale oversized request erased the newer accepted lineage (known=%t, revision kept=%t)", known, after.Rev == before.Rev)
		}
		if dir != "" {
			restarted := NewClaudeCloakLineageStore(dir)
			// Whatever reached the disk last is the newer lineage, never a drop.
			deadline := time.Now().Add(5 * time.Second)
			for {
				onDisk, ok := restarted.load(claudeCloakLineageKey(sid))
				if ok && onDisk.Rev == before.Rev {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the disk does not hold the newer lineage (known=%t)", ok)
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	})
}

// A request never waits for the disk. While another holder keeps the
// Session's shard lock, an accepted request still commits at once, and its
// lineage reaches the disk once the lock is free.
func TestClaudeCloakLineageCommitNeverWaitsForTheDisk(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lineage")
	old := SetClaudeCloakLineages(NewClaudeCloakLineageStore(dir))
	t.Cleanup(func() { SetClaudeCloakLineages(old) })
	const sid, day = "disk-off-path", "2026-10-15"
	key := claudeCloakLineageKey(sid)
	unlock, errLock := lockClaudeCloakLineageShard(dir, key)
	if errLock != nil {
		t.Fatal(errLock)
	}
	held := true
	defer func() {
		if held {
			unlock()
		}
	}()

	committed := make(chan struct{})
	go func() {
		PlanClaudeCloak(sol3Body(sol3User("start"), sol3Assistant("a0"), sol3User("q0")), sid, day).Commit()
		close(committed)
	}()
	select {
	case <-committed:
	case <-time.After(2 * time.Second):
		t.Fatal("an accepted request waited for the disk lock")
	}
	if _, ok := ClaudeCloakLineages().load(key); !ok {
		t.Fatal("the committed lineage is not visible to the next request of the process")
	}

	unlock()
	held = false
	deadline := time.Now().Add(5 * time.Second)
	for {
		if onDisk, ok := NewClaudeCloakLineageStore(dir).load(key); ok && len(onDisk.Messages) == 3 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the lineage never reached the disk after the lock was released")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
