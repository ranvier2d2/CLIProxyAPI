package executor

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
)

// Tests drive the system cloak through these adapters; production plans the
// anchors itself and calls checkSystemInstructionsWithAnchors.

func checkSystemInstructionsWithMode(payload []byte, strictMode bool) []byte {
	return checkSystemInstructionsWithSigningMode(payload, strictMode, false, "2.1.280", "cli", "")
}

// checkSystemInstructionsWithSigningMode keeps the top-level system in Claude
// Code's minimal CLI shape. Each caller system block is preserved as a separate
// mid-conversation system message after the first user turn, where supported
// Claude models give it operator-level authority without changing the cached
// top-level prefix.
func checkSystemInstructionsWithSigningMode(payload []byte, strictMode bool, cchSigning bool, version, entrypoint, workload string) []byte {
	return checkSystemInstructionsWithSigningModeAt(payload, strictMode, cchSigning, version, entrypoint, workload, time.Now(), false, "", "")
}

func checkSystemInstructionsWithSigningModeAt(
	payload []byte,
	strictMode bool,
	cchSigning bool,
	version, entrypoint, workload string,
	now time.Time,
	isSubagent bool,
	prevReq, promptID string,
	turnOrigin ...string,
) []byte {
	anchors := helps.PlanClaudeCloak(payload, "", claudeCodeLocalDate(now)).Anchors
	return checkSystemInstructionsWithAnchors(payload, strictMode, cchSigning, version, entrypoint, workload, anchors, isSubagent, prevReq, promptID, turnOrigin...)
}

func claudeCodeCurrentDateReminder(now time.Time) string {
	return claudeCodeDateReminder(claudeCodeLocalDate(now))
}
