package helps

import (
	"fmt"
	"testing"
)

// A stale oversized plan leaves the newer lineage in place and counts stale:
// the revision is checked before the size. Ported from review sol-4, which
// failed at 5859bec5: the oversize drop ran first and erased
// the newer accepted lineage.
func TestSol4StaleOversizedCommitMustKeepNewerLineage(t *testing.T) {
	old := SetClaudeCloakLineages(NewClaudeCloakLineageStore())
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
}
