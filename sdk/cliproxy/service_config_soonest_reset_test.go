package cliproxy

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestSoonestResetFirstRoutingConfig(t *testing.T) {
	for _, input := range []string{"soonest-reset-first", " SOONEST-RESET-FIRST "} {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: input}})
		if state.strategy != "soonest-reset-first" {
			t.Fatalf("strategy = %q; want soonest-reset-first", state.strategy)
		}
		if _, ok := newRoutingSelector(state).(*coreauth.SoonestResetFirstSelector); !ok {
			t.Fatalf("selector = %T; want SoonestResetFirstSelector", newRoutingSelector(state))
		}
		state.sessionAffinity = true
		selector := newRoutingSelector(state)
		affinity, ok := selector.(*coreauth.SessionAffinitySelector)
		if !ok {
			t.Fatalf("affinity selector = %T", selector)
		}
		affinity.Stop()
	}
	for _, input := range []string{"", "soonestresetfirst", "srf", "soonest-reset"} {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: input}})
		if state.strategy != "round-robin" {
			t.Fatalf("strategy(%q) = %q; want round-robin", input, state.strategy)
		}
	}
}
