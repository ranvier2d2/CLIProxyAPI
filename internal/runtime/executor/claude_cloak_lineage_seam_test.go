package executor

import (
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
)

// The session lineage tests reach the cloak's state only through this seam.
// At 815ec975 the state is the content-keyed anchor memo, which has no
// fallback counter.

func resetCloakLineage(t *testing.T) {
	t.Helper()
	previous := helps.SetClaudeCloakAnchors(helps.NewClaudeCloakAnchorMemo(""))
	t.Cleanup(func() { helps.SetClaudeCloakAnchors(previous) })
}

// restartCloakLineage models a proxy process starting over the state in dir.
func restartCloakLineage(t *testing.T, dir string) {
	t.Helper()
	helps.SetClaudeCloakAnchors(helps.NewClaudeCloakAnchorMemo(filepath.Join(dir, "claude-cloak-anchors.json")))
}

func cloakLineageFallbacks(reason string) int64 {
	return -1
}
