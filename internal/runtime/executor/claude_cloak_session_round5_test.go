package executor

import (
	"fmt"
	"strings"
	"testing"
)

// An older request retried after a swap is not a transition of the current
// lineage. It gets main's placement, is counted non_forward and is not
// committed, so the current epoch keeps its injection. Ported from review
// sol-5, which failed at 50dc6850: the retry replaced the lineage, and the
// continuation aligned on its partial suffix and moved anchor U3's date and
// caller policy to the fresh tail without counting anything.
func TestSol5SequentialOldRetryMustNotMoveCurrentAnchor(t *testing.T) {
	resetCloakLineage(t)
	const sid = "sol5-sequential-old-retry"
	initial := []string{linUser("start"), linThinking("a0"), linUser("u1"), linThinking("a1"), linUser("u2")}
	linSend(t, sid, linBody(initial...))
	epoch := []string{linBlock("c1"), initial[3], initial[4], linThinking("a2"), linUser("anchor")}
	first := linSend(t, sid, linBody(epoch...))
	// An older request is retried, after the swap has already succeeded.
	before := cloakLineageFallbacks("non_forward")
	linSend(t, sid, linBody(initial...))
	if got := cloakLineageFallbacks("non_forward"); got != before+1 {
		t.Errorf("old retry counted %d non_forward, want 1", got-before)
	}
	next := linSend(t, sid, linBody(append(epoch, linThinking("a3"), linUser("next"))...))
	linAssertKept(t, "current epoch after old retry", first, 0, next, 0, len(epoch))
}

// Requests that are not transitions of the current lineage leave it alone:
// a retry of the request that carried the previous compaction block, an
// unrelated history without a compaction block sent under the Session's
// header, and a rollback to before the swap. Each is counted non_forward, and the epoch's next request
// keeps every byte it was forwarded with.
func TestClaudeCloakSessionNonForwardHistoriesAreNotCommitted(t *testing.T) {
	for _, other := range []string{"previous-block", "unrelated", "rollback"} {
		t.Run(other, func(t *testing.T) {
			resetCloakLineage(t)
			const sid = "lin-non-forward"
			u0, u1, u2 := linUser("zero"), linUser("one"), linUser("two")
			a0, a1 := linThinking("lin-nf-a0"), linThinking("lin-nf-a1")
			linSend(t, sid, linBody(u0, a0, u1, a1, u2))
			epoch1 := []string{linBlock("lin-nf-c1"), a1, u2, linThinking("lin-nf-a2"), linUser("anchor1")}
			epoch1Body := linBody(epoch1...)
			linSend(t, sid, epoch1Body)
			linSend(t, sid, linCompacting(epoch1[:3]...))
			epoch2 := []string{linBlock("lin-nf-c2"), epoch1[3], epoch1[4], linThinking("lin-nf-a3"), linUser("anchor2")}
			current := linSend(t, sid, linBody(epoch2...))

			var body []byte
			switch other {
			case "previous-block":
				body = epoch1Body
			case "unrelated":
				body = linBody(linUser("another conversation"), linThinking("lin-nf-elsewhere"), linUser("more"))
			case "rollback":
				body = linBody(u0, a0, u1, a1, u2, linThinking("lin-nf-rolled"), linUser("instead"))
			}
			before := cloakLineageFallbacks("non_forward")
			linSend(t, sid, body)
			if got := cloakLineageFallbacks("non_forward"); got != before+1 {
				t.Errorf("%s counted %d non_forward, want 1", other, got-before)
			}
			next := linSend(t, sid, linBody(append(epoch2, linThinking("lin-nf-a4"), linUser("next"))...))
			linAssertKept(t, "current epoch after "+other, current, 0, next, 0, len(epoch2))
		})
	}
}

// Every three-operation history over append, compaction request, swap, swap
// that is itself a compaction request, retry of the request before the last
// transition, unchanged resend and a rejected request. Every message a
// committed request forwarded keeps its bytes in every later committed
// request. A request counted non_forward is exempt from that for its own
// bytes: it takes main's placement, which is all the proxy can honestly do
// for a history it no longer holds. Ported from review sol-5's 343-history
// matrix, whose swap/swap/retry-old, swap/retry-old/append,
// swap-compact/swap/retry-old and swap-compact/swap-compact/retry-old failed
// at 50dc6850.
func TestSol5TransitionTriples(t *testing.T) {
	ops := []string{"append", "compact", "swap", "swap-compact", "retry-old", "resend", "reject"}
	for _, a := range ops {
		for _, b := range ops {
			for _, c := range ops {
				sequence := []string{a, b, c}
				t.Run(strings.Join(sequence, "/"), func(t *testing.T) {
					resetCloakLineage(t)
					const sid = "sol5-model"
					messages := []string{linUser("initial")}
					for n := 0; n < 4; n++ {
						messages = append(messages, linThinking(fmt.Sprintf("initial-%d", n)), linUser(fmt.Sprintf("question-%d", n)))
					}
					forms := map[string]string{}
					remember := func(view linForwarded) {
						for i, message := range view.caller {
							if want, ok := forms[message]; ok && want != view.forms[i] {
								t.Fatalf("kept occurrence changed under operations %v at message %d", sequence, i)
							}
							forms[message] = view.forms[i]
						}
					}
					send := func(body []byte) {
						before := cloakLineageFallbacks("non_forward")
						view := linSend(t, sid, body)
						if cloakLineageFallbacks("non_forward") == before {
							remember(view)
						}
					}
					send(linBody(messages...))
					lastBody := linBody(messages...)
					cut := 0
					olderBody := lastBody
					for step, op := range sequence {
						priorBody := lastBody
						switch op {
						case "append":
							messages = append(messages, linThinking(fmt.Sprintf("append-%d", step)), linUser(fmt.Sprintf("append-user-%d", step)))
							lastBody = linBody(messages...)
							send(lastBody)
						case "compact":
							if len(messages) < 3 {
								return
							}
							cut = len(messages) - 2
							lastBody = linCompacting(messages[:cut]...)
							send(lastBody)
						case "swap", "swap-compact":
							hadCut := cut != 0
							if cut == 0 { // unseen compaction: keep a positional suffix
								cut = len(messages) - 2
							}
							messages = append([]string{linBlock(fmt.Sprintf("model-c-%d", step))}, messages[cut:]...)
							cut = 0
							if op == "swap" {
								messages = append(messages, linThinking(fmt.Sprintf("swap-%d", step)), linUser(fmt.Sprintf("swap-user-%d", step)))
								lastBody = linBody(messages...)
							} else {
								cut = 1
								// If the proxy never saw the compaction request, the
								// visible suffix is the only alignment evidence.
								if !hadCut {
									cut = len(messages)
								}
								lastBody = linCompacting(messages[:cut]...)
							}
							send(lastBody)
						case "resend":
							send(lastBody)
						case "retry-old":
							send(olderBody)
						case "reject":
							linRejected(t, sid, linBody(linUser(fmt.Sprintf("rejected-%d", step))), false)
						}
						if op != "resend" && op != "retry-old" && op != "reject" {
							olderBody = priorBody
						}
					}
				})
			}
		}
	}
}
