package auth

import (
	"context"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	soonestResetTieTolerance    = time.Minute
	soonestResetExploreInterval = 16
)

// SoonestResetFirstSelector prefers the next long-window reset while periodically
// exploring credentials whose reset is unknown. Its zero value is ready to use.
type SoonestResetFirstSelector struct {
	mu        sync.Mutex
	rotations map[string]*soonestResetRotation
	maxKeys   int
}

type soonestResetRotation struct {
	lastPicked   string
	lastExplored string
	untilExplore int
}

type quotaResetWindow struct {
	reset     time.Time
	exhausted bool
}

// Pick preserves availability, priority, and WebSocket eligibility before ordering resets.
func (s *SoonestResetFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	available, errAvailable := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if errAvailable != nil {
		return nil, errAvailable
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)

	var known, unknown, exhausted []*Auth
	var earliest time.Time
	for _, candidate := range available {
		window := authQuotaResetWindow(candidate, provider, now)
		switch {
		case window.exhausted:
			exhausted = append(exhausted, candidate)
		case window.reset.IsZero():
			unknown = append(unknown, candidate)
		default:
			if earliest.IsZero() || window.reset.Before(earliest) {
				earliest = window.reset
			}
			known = append(known, candidate)
		}
	}
	if len(known) > 0 {
		cutoff := earliest.Add(soonestResetTieTolerance)
		tied := known[:0]
		for _, candidate := range known {
			if !authQuotaResetWindow(candidate, provider, now).reset.After(cutoff) {
				tied = append(tied, candidate)
			}
		}
		known = tied
	}

	key := provider + ":" + canonicalModelKey(model)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rotations == nil {
		s.rotations = make(map[string]*soonestResetRotation)
	}
	limit := s.maxKeys
	if limit <= 0 {
		limit = 4096
	}
	if _, exists := s.rotations[key]; !exists && len(s.rotations) >= limit {
		clear(s.rotations)
	}
	rotation := s.rotations[key]
	if rotation == nil {
		rotation = &soonestResetRotation{}
		s.rotations[key] = rotation
	}

	var picked *Auth
	switch {
	case len(known) > 0 && len(unknown) > 0 && rotation.untilExplore == 0:
		picked = unknown[successorIndex(unknown, rotation.lastExplored)]
		rotation.lastExplored = picked.ID
		rotation.untilExplore = soonestResetExploreInterval - 1
	case len(known) > 0:
		picked = known[successorIndex(known, rotation.lastPicked)]
		if rotation.untilExplore > 0 {
			rotation.untilExplore--
		}
	case len(unknown) > 0:
		picked = unknown[successorIndex(unknown, rotation.lastPicked)]
	default:
		picked = exhausted[successorIndex(exhausted, rotation.lastPicked)]
	}
	rotation.lastPicked = picked.ID
	return picked, nil
}

func authQuotaResetWindow(auth *Auth, provider string, now time.Time) quotaResetWindow {
	quota := auth.Quota
	if quota.ObservedAt.IsZero() {
		return quotaResetWindow{}
	}
	if auth.Provider != "" {
		provider = auth.Provider
	}
	signals := quota.Signals
	var reset time.Time
	var usedRaw string
	threshold := 100.0
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		reset = quotaResetTimestamp(signals["Anthropic-Ratelimit-Unified-7d-Reset"])
		usedRaw = signals["Anthropic-Ratelimit-Unified-7d-Utilization"]
		threshold = 1.0
	case "codex":
		prefix := "X-Codex-Primary-"
		for name := range signals {
			if strings.HasPrefix(name, "X-Codex-Secondary-") {
				prefix = "X-Codex-Secondary-"
				break
			}
		}
		minutesRaw := strings.TrimSpace(signals[prefix+"Window-Minutes"])
		if minutesRaw != "" {
			minutes, errParse := strconv.ParseInt(minutesRaw, 10, 64)
			if errParse != nil || minutes <= 300 {
				return quotaResetWindow{}
			}
		} else if prefix == "X-Codex-Primary-" {
			// A primary window can be 5h or weekly. Without its length it cannot rank resets.
			return quotaResetWindow{}
		}
		usedRaw = signals[prefix+"Used-Percent"]
		if raw := strings.TrimSpace(signals[prefix+"Reset-At"]); raw != "" {
			reset = quotaResetTimestamp(raw)
		} else if seconds, errParse := strconv.ParseInt(strings.TrimSpace(signals[prefix+"Reset-After-Seconds"]), 10, 64); errParse == nil && seconds >= 0 && seconds <= math.MaxInt64/int64(time.Second) {
			// Relative snapshots age from observation, never from the current selection.
			reset = quota.ObservedAt.Add(time.Duration(seconds) * time.Second)
		}
	default:
		return quotaResetWindow{}
	}

	window := quotaResetWindow{}
	if reset.After(now) {
		window.reset = reset
	}
	if usedRaw = strings.TrimSpace(usedRaw); usedRaw != "" {
		used, errParse := strconv.ParseFloat(usedRaw, 64)
		if errParse != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 {
			return quotaResetWindow{}
		}
		window.exhausted = used >= threshold
	}
	return window
}

func quotaResetTimestamp(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if seconds, errParse := strconv.ParseInt(raw, 10, 64); errParse == nil && seconds > 0 {
		return time.Unix(seconds, 0)
	}
	if parsed, errParse := time.Parse(time.RFC3339, raw); errParse == nil {
		return parsed
	}
	return time.Time{}
}
