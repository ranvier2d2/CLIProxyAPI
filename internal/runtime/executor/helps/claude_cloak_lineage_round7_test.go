package helps

import (
	"fmt"
	"math"
	"math/bits"
	"testing"

	"github.com/tidwall/gjson"
)

// At the most blocks a Session's filter is ever read with, 255, a block it
// does not hold reads as held at a rate under 1e-6. The sample is 40,000,000
// absent blocks; the test bounds the rate by the one-sided 99% Poisson upper
// limit on the count it sees, so it fails if that limit reaches 1e-6.
func TestClaudeCloakRetiredFalsePositiveRateAtOperationalOccupancy(t *testing.T) {
	var f claudeCloakRetired
	for i := 0; i < claudeCloakRetiredBlocks-1; i++ {
		f.add(claudeCloakMessageHash(gjson.Parse(sol3Block(fmt.Sprintf("sol7-held-%d", i)))))
	}
	const queries = 40_000_000
	positives := 0
	for i := 0; i < queries; i++ {
		if f.holds(fmt.Sprintf("%064x", i)) {
			positives++
		}
	}
	set := 0
	for _, word := range f.bits {
		set += bits.OnesCount64(word)
	}
	upper := poissonUpperLimit(positives, 0.01) / queries
	t.Logf("%d of %d absent blocks read as held at %d blocks; 99%% upper limit on the rate %.3g; %d of %d bits set, so independent probes miss at %.3g", positives, queries, f.blocks, upper, set, claudeCloakRetiredBits, math.Pow(float64(set)/claudeCloakRetiredBits, claudeCloakRetiredProbes))
	if upper >= 1e-6 {
		t.Fatalf("%d of %d absent blocks read as held: the rate's 99%% upper limit %.3g is not under 1e-6", positives, queries, upper)
	}
}

// poissonUpperLimit is the mean whose chance of a count at most x is alpha.
func poissonUpperLimit(x int, alpha float64) float64 {
	cdf := func(mean float64) float64 {
		term, sum := math.Exp(-mean), 0.0
		for k := 0; k <= x; k++ {
			sum += term
			term *= mean / float64(k+1)
		}
		return sum
	}
	low, high := float64(x), float64(x)+100
	for i := 0; i < 200; i++ {
		mid := (low + high) / 2
		if cdf(mid) > alpha {
			low = mid
		} else {
			high = mid
		}
	}
	return high
}

// Review sol-7 found this through ordinary Plan and Commit at f5f84c68: after
// sol7-held-0 through sol7-held-254 retire, the new block sol7-absent-21577
// read as retired, so a forward swap lost its anchor to main's placement.
func TestSol7NaturalFalsePositive(t *testing.T) {
	old := SetClaudeCloakLineages(NewClaudeCloakLineageStore())
	t.Cleanup(func() { SetClaudeCloakLineages(old) })
	const sid, day = "sol7-natural-collision", "2026-10-15"
	PlanClaudeCloak(sol3Body(sol3User("start"), sol3Assistant("a"), sol3User("kept")), sid, day).Commit()
	for i := 0; i <= 255; i++ {
		plan := PlanClaudeCloak(sol3Body(sol3Block(fmt.Sprintf("sol7-held-%d", i)), sol3Assistant("a"), sol3User("kept")), sid, day)
		if plan.Fallback != "" {
			t.Fatalf("epoch %d fallback %q", i, plan.Fallback)
		}
		plan.Commit()
	}
	store, key := ClaudeCloakLineages(), claudeCloakLineageKey(sid)
	before, _ := store.load(key)
	if before.Retired.blocks != 255 {
		t.Fatalf("retired %d blocks, want 255", before.Retired.blocks)
	}

	body := sol3Body(sol3Block("sol7-absent-21577"), sol3Assistant("a"), sol3User("kept"), sol3Assistant("fresh"), sol3User("new-anchor"))
	plan := PlanClaudeCloak(body, sid, day)
	counted := ClaudeCloakLineageFallbacks(ClaudeCloakFallbackNonForward)
	plan.Commit()
	after, _ := store.load(key)
	counted = ClaudeCloakLineageFallbacks(ClaudeCloakFallbackNonForward) - counted
	if plan.Fallback != "" || !equalClaudeCloakAnchors(plan.Anchors, []ClaudeCloakAnchor{{Index: 4, Date: day}}) || counted != 0 || after.Rev == before.Rev {
		t.Fatalf("new block: fallback %q anchors %+v non_forward %d committed %t", plan.Fallback, plan.Anchors, counted, after.Rev != before.Rev)
	}
}
