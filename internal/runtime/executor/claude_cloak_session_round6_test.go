package executor

import (
	"fmt"
	"testing"
)

func cloakRecoveries() int64 {
	var n int64
	for _, reason := range []string{"unknown_session", "unaligned", "ambiguous", "non_forward", "stale", "oversize"} {
		n += cloakLineageFallbacks(reason)
	}
	return n
}

// A retry from an epoch however old is an older retry, not a new swap: it is
// counted non_forward, is not committed, and the current epoch keeps its
// injection. Ported from review sol-6, which failed at f3c21d3c from 66 swaps
// on: the 64 retired hashes had forgotten epoch 0's block, so the retry was
// accepted as a fresh swap, aligned on a repeated suffix of the current epoch
// and moved its anchor without counting anything. Through 256 swaps every
// forward request also aligns; past that bound the Session takes main's
// placement for every new block, which the retry must survive as well.
func TestSol6RetryBeyondRetiredWindowMustNotMoveCurrentAnchor(t *testing.T) {
	for _, swaps := range []int{65, 66, 256, 1000} {
		t.Run(fmt.Sprintf("swaps-%d", swaps), func(t *testing.T) {
			resetCloakLineage(t)
			const sid = "sol6-retired-bound"
			initial := []string{linUser("start"), linThinking("initial"), linUser("old")}
			previous := linSend(t, sid, linBody(initial...))
			var oldBody []byte
			var current linForwarded
			for epoch := 0; epoch < swaps; epoch++ {
				before := cloakRecoveries()
				body := linBody(linBlock(fmt.Sprintf("epoch-%d", epoch)), previous.caller[len(previous.caller)-2], previous.caller[len(previous.caller)-1], linThinking(fmt.Sprintf("b-%d", epoch)), linUser(fmt.Sprintf("anchor-%d", epoch)))
				current = linSend(t, sid, body)
				if epoch == 0 {
					oldBody = body
				}
				// Keep a plain tail after the anchor so the next swap summarizes the
				// old anchor away and generates a fresh one at message four.
				current = linSend(t, sid, linBody(append(append([]string{}, current.caller...), linThinking(fmt.Sprintf("tail-%d", epoch)), linUser(fmt.Sprintf("plain-%d", epoch)))...))
				if got := cloakRecoveries() - before; epoch <= 256 && got != 0 {
					t.Fatalf("forward swap %d counted %d recoveries, want 0", epoch, got)
				}
				previous = current
			}
			// Repeated caller turns are valid. A suffix of the latest epoch can equal
			// the old request's retained prefix without making that request current.
			current = linSend(t, sid, linBody(append(append([]string{}, current.caller...), initial[1], initial[2])...))
			before := cloakLineageFallbacks("non_forward")
			linSend(t, sid, oldBody)
			if got := cloakLineageFallbacks("non_forward") - before; got != 1 {
				t.Errorf("retry of epoch 0 after %d swaps counted non_forward=%d, want 1", swaps, got)
			}
			next := linSend(t, sid, linBody(append(append([]string{}, current.caller...), linThinking("continue"), linUser("next"))...))
			linAssertKept(t, "current epoch after old retired retry", current, 0, next, 0, len(current.caller))
		})
	}
}
