package management

import "testing"

func TestNormalizeRoutingStrategySoonestResetFirst(t *testing.T) {
	for _, input := range []string{"soonest-reset-first", " SOONEST-RESET-FIRST "} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "soonest-reset-first" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v", input, got, ok)
		}
	}
	for _, input := range []string{"srf", "soonestresetfirst", "soonest-reset"} {
		if _, ok := normalizeRoutingStrategy(input); ok {
			t.Fatalf("unexpected alias %q accepted", input)
		}
	}
}
