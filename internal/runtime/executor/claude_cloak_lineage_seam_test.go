package executor

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
)

// The session lineage tests reach the cloak's state only through this seam.

func resetCloakLineage(t *testing.T) {
	t.Helper()
	previous := helps.SetClaudeCloakLineages(helps.NewClaudeCloakLineageStore(""))
	t.Cleanup(func() { helps.SetClaudeCloakLineages(previous) })
}

// restartCloakLineage models a proxy process starting over the state in dir.
func restartCloakLineage(t *testing.T, dir string) {
	t.Helper()
	helps.SetClaudeCloakLineages(helps.NewClaudeCloakLineageStore(dir))
}

func cloakLineageFallbacks(reason string) int64 {
	return helps.ClaudeCloakLineageFallbacks(reason)
}

// mainCloakAnchors is main's placement: the first user message, dated now.
func mainCloakAnchors(payload []byte, now time.Time) []helps.ClaudeCloakAnchor {
	return helps.PlanClaudeCloak(payload, "", claudeCodeLocalDate(now)).Anchors
}
