package helps

import (
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

// The retired filter holds every block added to it.
func TestClaudeCloakRetiredHoldsEveryBlock(t *testing.T) {
	var f claudeCloakRetired
	for i := 0; i < claudeCloakRetiredBlocks; i++ {
		f.add(fmt.Sprintf("held-%d", i))
	}
	f.blocks = 0 // read the bits, not the bound
	for i := 0; i < claudeCloakRetiredBlocks; i++ {
		if !f.holds(fmt.Sprintf("held-%d", i)) {
			t.Fatalf("filter forgot block %d", i)
		}
	}
}

// A swap whose new block the filter reads as held, whether by a collision or
// because the Session is past the bound, takes main's placement, counts
// non_forward and leaves the lineage as it was. Without either, the same swap
// keeps the epoch's anchor.
func TestClaudeCloakRetiredFalsePositiveTakesMainsPlacement(t *testing.T) {
	for _, filter := range []string{"control", "collision", "full"} {
		t.Run(filter, func(t *testing.T) {
			old := SetClaudeCloakLineages(NewClaudeCloakLineageStore())
			t.Cleanup(func() { SetClaudeCloakLineages(old) })
			const sid, day = "retired-false-positive", "2026-10-15"
			initial := []string{sol3User("start"), sol3Assistant("a0"), sol3User("kept")}
			PlanClaudeCloak(sol3Body(initial...), sid, day).Commit()
			epoch := []string{sol3Block("c1"), initial[1], initial[2], sol3Assistant("a1"), sol3User("anchor")}
			PlanClaudeCloak(sol3Body(epoch...), sid, day).Commit()
			block := sol3Block("c2")

			store, key := ClaudeCloakLineages(), claudeCloakLineageKey(sid)
			lineage, _ := store.load(key)
			switch filter {
			case "collision": // every probe of c2 is set, though no epoch had c2
				for _, bit := range claudeCloakRetiredBitsOf(claudeCloakMessageHash(gjson.Parse(block))) {
					lineage.Retired.bits[bit/64] |= 1 << (bit % 64)
				}
			case "full":
				lineage.Retired.blocks = claudeCloakRetiredBlocks
			}
			if _, _, outcome := store.swap(key, lineage.Rev, lineage); outcome != claudeCloakLineageSwapped {
				t.Fatalf("inject filter: outcome %d", outcome)
			}
			before, _ := store.load(key)

			swap := sol3Body(block, epoch[2], epoch[3], epoch[4], sol3Assistant("a2"), sol3User("next"))
			plan := PlanClaudeCloak(swap, sid, day)
			main := PlanClaudeCloak(swap, "", day)
			counted := ClaudeCloakLineageFallbacks(ClaudeCloakFallbackNonForward)
			plan.Commit()
			after, _ := store.load(key)
			counted = ClaudeCloakLineageFallbacks(ClaudeCloakFallbackNonForward) - counted

			if filter == "control" {
				if plan.Fallback != "" || !equalClaudeCloakAnchors(plan.Anchors, []ClaudeCloakAnchor{{Index: 3, Date: day}}) || counted != 0 || after.Rev == before.Rev {
					t.Fatalf("control swap: fallback %q anchors %+v non_forward %d committed %t", plan.Fallback, plan.Anchors, counted, after.Rev != before.Rev)
				}
				return
			}
			if plan.Fallback != ClaudeCloakFallbackNonForward || !equalClaudeCloakAnchors(plan.Anchors, main.Anchors) {
				t.Fatalf("fallback %q anchors %+v, want non_forward with main's %+v", plan.Fallback, plan.Anchors, main.Anchors)
			}
			if counted != 1 {
				t.Fatalf("counted non_forward %d, want 1", counted)
			}
			if after.Rev != before.Rev {
				t.Fatal("a swap read as an earlier epoch was committed")
			}
		})
	}
}
