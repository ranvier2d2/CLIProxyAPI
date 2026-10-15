package helps

import (
	"fmt"
	"strings"
	"testing"
)

func sol3Body(messages ...string) []byte {
	return []byte(`{"messages":[` + strings.Join(messages, ",") + `]}`)
}
func sol3User(s string) string      { return fmt.Sprintf(`{"role":"user","content":%q}`, s) }
func sol3Assistant(s string) string { return fmt.Sprintf(`{"role":"assistant","content":%q}`, s) }
func sol3Block(s string) string {
	return fmt.Sprintf(`{"role":"assistant","content":[{"type":"compaction","signature":%q}]}`, s)
}

// A duplicate request planned before B's compaction is delayed until after
// the swap commits. The store must detect the stale commit rather than silently moving B's historical date. Ported from
// review sol-3, which failed at 9c63cb98.
func TestSol3StalePlanMustNotSilentlyMoveAnchor(t *testing.T) {
	old := SetClaudeCloakLineages(NewClaudeCloakLineageStore())
	t.Cleanup(func() { SetClaudeCloakLineages(old) })
	const sid, today = "sol3-stale", "2026-10-15"
	initial := []string{sol3User("start"), sol3Assistant("a0"), sol3User("q0")}
	PlanClaudeCloak(sol3Body(initial...), sid, today).Commit()
	epoch1 := []string{sol3Block("c1"), initial[1], initial[2], sol3Assistant("a1"), sol3User("anchor1")}
	PlanClaudeCloak(sol3Body(epoch1...), sid, today).Commit()
	appended := append(append([]string{}, epoch1...), sol3Assistant("a2"), sol3User("q2"))
	stale := PlanClaudeCloak(sol3Body(appended...), sid, today)
	PlanClaudeCloak(sol3Body(appended...), sid, today).Commit()
	// Summarize through anchor1; keep [a2,q2], then generate on q3.
	PlanClaudeCloak(sol3Body(epoch1...), sid, today).Commit()
	epoch2 := []string{sol3Block("c2"), appended[5], appended[6], sol3Assistant("a3"), sol3User("anchor2")}
	first := PlanClaudeCloak(sol3Body(epoch2...), sid, today)
	if first.Fallback != "" || len(first.Anchors) != 1 || first.Anchors[0].Index != 4 {
		t.Fatalf("bad setup: %+v", first)
	}
	first.Commit()
	stale.Commit()
	continuation := append(append([]string{}, epoch2...), sol3Assistant("a4"), sol3User("next"))
	next := PlanClaudeCloak(sol3Body(continuation...), sid, today)
	t.Logf("stale commit: before anchor=%d after anchors=%+v fallback=%q", first.Anchors[0].Index, next.Anchors, next.Fallback)
	if next.Fallback == "" && (len(next.Anchors) != 1 || next.Anchors[0].Index != 4) {
		t.Fatal("stale concurrent plan silently removed historical anchor2")
	}
}

// A Session whose request is longer than the per-Session cap loses its
// lineage, and says so: the drop is counted, and the Session's next compacted
// request takes main's placement as an unknown Session instead of aligning
// against a truncated record.
func TestClaudeCloakLineageOversizedSessionIsDroppedAndCounted(t *testing.T) {
	old := SetClaudeCloakLineages(NewClaudeCloakLineageStore())
	t.Cleanup(func() { SetClaudeCloakLineages(old) })
	const sid, today = "oversize", "2026-10-15"
	PlanClaudeCloak(sol3Body(sol3User("start")), sid, today).Commit()
	// Well past the per-Session cap.
	huge := make([]string, 0, 10001)
	for i := 0; i < 5000; i++ {
		huge = append(huge, sol3User(fmt.Sprintf("u%d", i)), sol3Assistant(fmt.Sprintf("a%d", i)))
	}
	huge = append(huge, sol3User("last"))
	before := ClaudeCloakLineageFallbacks("oversize")
	PlanClaudeCloak(sol3Body(huge...), sid, today).Commit()
	if got := ClaudeCloakLineageFallbacks("oversize"); got != before+1 {
		t.Fatalf("oversize recoveries %d, want %d", got, before+1)
	}
	if _, ok := ClaudeCloakLineages().load(claudeCloakLineageKey(sid)); ok {
		t.Fatal("a Session over its cap kept a lineage")
	}
	next := PlanClaudeCloak(sol3Body(sol3Block("c"), huge[len(huge)-2], huge[len(huge)-1]), sid, today)
	if next.Fallback != ClaudeCloakFallbackUnknownSession {
		t.Fatalf("next compacted request fallback %q, want %q", next.Fallback, ClaudeCloakFallbackUnknownSession)
	}
}

// Two identical requests planned against the same lineage both commit: the
// later one plans again against the earlier one's lineage, gets the same
// placement, and writes it. Nothing is counted stale.
func TestClaudeCloakLineageStalePlanWithTheSamePlacementCommits(t *testing.T) {
	old := SetClaudeCloakLineages(NewClaudeCloakLineageStore())
	t.Cleanup(func() { SetClaudeCloakLineages(old) })
	const sid, today = "same-placement", "2026-10-15"
	PlanClaudeCloak(sol3Body(sol3User("start"), sol3Assistant("a0"), sol3User("q0")), sid, today).Commit()
	swap := sol3Body(sol3Block("c1"), sol3Assistant("a0"), sol3User("q0"), sol3Assistant("a1"), sol3User("new"))
	first, second := PlanClaudeCloak(swap, sid, today), PlanClaudeCloak(swap, sid, today)
	before := ClaudeCloakLineageFallbacks(ClaudeCloakRecoveryStale)
	first.Commit()
	second.Commit()
	if got := ClaudeCloakLineageFallbacks(ClaudeCloakRecoveryStale); got != before {
		t.Fatalf("an agreeing duplicate counted %d stale", got-before)
	}
	current, _ := ClaudeCloakLineages().load(claudeCloakLineageKey(sid))
	if current.Messages[4].Date != today || current.Rev == first.expect {
		t.Fatalf("lineage after both commits: %+v", current)
	}
}
