package auth

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func resetTestAuth(id, provider string, observed time.Time, signals map[string]string) *Auth {
	headers := make(http.Header)
	for name, value := range signals {
		headers.Set(name, value)
	}
	auth := &Auth{ID: id, Provider: provider}
	auth.Quota.ObserveResponseHeadersForProvider(provider, headers, observed)
	return auth
}

func claudeResetTestAuth(id string, now time.Time, reset time.Duration, used string) *Auth {
	return resetTestAuth(id, "claude", now, map[string]string{
		"anthropic-ratelimit-unified-7d-reset":       strconv.FormatInt(now.Add(reset).Unix(), 10),
		"anthropic-ratelimit-unified-7d-utilization": used,
		"anthropic-ratelimit-unified-5h-reset":       strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
	})
}

func pickResetTestAuth(t *testing.T, selector Selector, provider, model string, opts cliproxyexecutor.Options, auths []*Auth, want string) {
	t.Helper()
	got, errPick := selector.Pick(context.Background(), provider, model, opts, auths)
	if errPick != nil || got == nil {
		t.Fatalf("Pick() = %v, %v; want %s", got, errPick, want)
	}
	if got.ID != want {
		t.Fatalf("Pick().ID = %q; want %q", got.ID, want)
	}
}

func TestSoonestResetFirstClaude(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, tc := range []struct {
		name   string
		mutate func([]*Auth)
		want   []string
	}{
		{name: "soonest repeatedly", want: []string{"b", "b", "b", "b"}},
		{name: "cooldown", mutate: func(auths []*Auth) {
			auths[1].Unavailable = true
			auths[1].NextRetryAfter = now.Add(time.Hour)
		}, want: []string{"c", "c"}},
		{name: "full utilization", mutate: func(auths []*Auth) {
			auths[1].Quota.Signals["Anthropic-Ratelimit-Unified-7d-Utilization"] = "1.00"
		}, want: []string{"c", "c"}},
		{name: "overfull utilization", mutate: func(auths []*Auth) {
			auths[1].Quota.Signals["Anthropic-Ratelimit-Unified-7d-Utilization"] = "1.05"
		}, want: []string{"c", "c"}},
		{name: "priority wins", mutate: func(auths []*Auth) {
			auths[0].Attributes = map[string]string{"priority": "10"}
		}, want: []string{"a", "a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auths := []*Auth{
				claudeResetTestAuth("a", now, 6*24*time.Hour, "0.2"),
				claudeResetTestAuth("b", now, 24*time.Hour, "0.7"),
				claudeResetTestAuth("c", now, 3*24*time.Hour, "0.1"),
			}
			if tc.mutate != nil {
				tc.mutate(auths)
			}
			selector := &SoonestResetFirstSelector{}
			for _, want := range tc.want {
				pickResetTestAuth(t, selector, "claude", "", cliproxyexecutor.Options{}, auths, want)
			}
		})
	}
}

func TestSoonestResetFirstUnknownExploration(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, tc := range []struct {
		name    string
		signals map[string]string
	}{
		{name: "never observed"},
		{name: "past reset", signals: map[string]string{"anthropic-ratelimit-unified-7d-reset": strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)}},
		{name: "malformed reset", signals: map[string]string{"anthropic-ratelimit-unified-7d-reset": "invalid"}},
		{name: "5h only", signals: map[string]string{"anthropic-ratelimit-unified-5h-reset": strconv.FormatInt(now.Add(time.Hour).Unix(), 10)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unknown := resetTestAuth("z", "claude", now, tc.signals)
			auths := []*Auth{claudeResetTestAuth("a", now, 24*time.Hour, "0.1"), unknown}
			selector := &SoonestResetFirstSelector{}
			pickResetTestAuth(t, selector, "claude", "", cliproxyexecutor.Options{}, auths, "z")
			for i := 0; i < 15; i++ {
				pickResetTestAuth(t, selector, "claude", "", cliproxyexecutor.Options{}, auths, "a")
			}
			pickResetTestAuth(t, selector, "claude", "", cliproxyexecutor.Options{}, auths, "z")
			unknown.Quota = claudeResetTestAuth("z", now, 6*24*time.Hour, "0.1").Quota
			for i := 0; i < 20; i++ {
				pickResetTestAuth(t, selector, "claude", "", cliproxyexecutor.Options{}, auths, "a")
			}
		})
	}
}

func TestSoonestResetFirstUnknownsRotate(t *testing.T) {
	now := time.Now()
	selector := &SoonestResetFirstSelector{}
	auths := []*Auth{claudeResetTestAuth("a", now, 24*time.Hour, "0.1"), {ID: "y"}, {ID: "z"}}
	for _, unknown := range []string{"y", "z", "y"} {
		pickResetTestAuth(t, selector, "claude", "", cliproxyexecutor.Options{}, auths, unknown)
		for i := 0; i < 15; i++ {
			pickResetTestAuth(t, selector, "claude", "", cliproxyexecutor.Options{}, auths, "a")
		}
	}
}

func TestSoonestResetFirstTies(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	selector := &SoonestResetFirstSelector{}
	a := claudeResetTestAuth("a", now, 24*time.Hour+60*time.Second, "0.1")
	b := claudeResetTestAuth("b", now, 24*time.Hour, "0.1")
	c := claudeResetTestAuth("c", now, 24*time.Hour+61*time.Second, "0.1")
	for _, want := range []string{"a", "b", "a", "b"} {
		pickResetTestAuth(t, selector, "claude", "m(high)", cliproxyexecutor.Options{}, []*Auth{c, b, a}, want)
	}
	pickResetTestAuth(t, selector, "claude", "m", cliproxyexecutor.Options{}, []*Auth{b, c}, "b")
	pickResetTestAuth(t, selector, "claude", "m", cliproxyexecutor.Options{}, []*Auth{a, b, c}, "a")
	pickResetTestAuth(t, selector, "codex", "m", cliproxyexecutor.Options{}, []*Auth{a, b, c}, "a")
	pickResetTestAuth(t, selector, "claude", "other", cliproxyexecutor.Options{}, []*Auth{a, b, c}, "a")
}

func TestSoonestResetFirstAffinity(t *testing.T) {
	now := time.Now()
	selector := NewSessionAffinitySelector(&SoonestResetFirstSelector{})
	defer selector.Stop()
	a := claudeResetTestAuth("a", now, 24*time.Hour, "0.2")
	b := claudeResetTestAuth("b", now, 3*24*time.Hour, "0.2")
	auths := []*Auth{a, b}
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"existing-session"}}}
	pickResetTestAuth(t, selector, "claude", "", opts, auths, "a")
	b.Quota = claudeResetTestAuth("b", now, 12*time.Hour, "0.2").Quota
	pickResetTestAuth(t, selector, "claude", "", opts, auths, "a")
	pickResetTestAuth(t, selector, "claude", "", cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"new-session"}}}, auths, "b")
	a.Unavailable = true
	a.NextRetryAfter = now.Add(time.Hour)
	pickResetTestAuth(t, selector, "claude", "", opts, auths, "b")
}

func TestSoonestResetFirstCodex(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, tc := range []struct {
		name, prefix string
		relative     bool
	}{
		{name: "secondary absolute", prefix: "secondary"},
		{name: "secondary relative", prefix: "secondary", relative: true},
		{name: "primary weekly", prefix: "primary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var auths []*Auth
			for i, days := range []int{6, 1, 3} {
				signals := map[string]string{
					"x-codex-" + tc.prefix + "-used-percent":   "20",
					"x-codex-" + tc.prefix + "-window-minutes": "10080",
				}
				if tc.relative {
					signals["x-codex-"+tc.prefix+"-reset-after-seconds"] = strconv.Itoa(days*86400 + 3600)
				} else {
					signals["x-codex-"+tc.prefix+"-reset-at"] = strconv.FormatInt(now.Add(time.Duration(days)*24*time.Hour).Unix(), 10)
				}
				if tc.prefix == "secondary" {
					signals["x-codex-primary-reset-after-seconds"] = "60"
					signals["x-codex-primary-window-minutes"] = "300"
				}
				auths = append(auths, resetTestAuth(string(rune('a'+i)), "codex", now.Add(-time.Hour), signals))
			}
			selector := &SoonestResetFirstSelector{}
			for i := 0; i < 3; i++ {
				pickResetTestAuth(t, selector, "codex", "", cliproxyexecutor.Options{}, auths, "b")
			}
			auths[1].Quota.Signals[http.CanonicalHeaderKey("x-codex-"+tc.prefix+"-used-percent")] = "100"
			pickResetTestAuth(t, selector, "codex", "", cliproxyexecutor.Options{}, auths, "c")
		})
	}
}

func TestSoonestResetFirstUnavailable(t *testing.T) {
	selector := &SoonestResetFirstSelector{}
	if _, errPick := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, nil); errPick == nil {
		t.Fatal("empty candidates must return an error")
	}
	if _, errPick := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, []*Auth{{ID: "a", Disabled: true}}); errPick == nil {
		t.Fatal("disabled candidate must return an error")
	}
	for _, want := range []string{"a", "b", "a"} {
		pickResetTestAuth(t, selector, "gemini", "", cliproxyexecutor.Options{}, []*Auth{{ID: "b"}, {ID: "a"}}, want)
	}
}

func TestSoonestResetFirstConcurrent(t *testing.T) {
	selector := &SoonestResetFirstSelector{}
	now := time.Now()
	auths := []*Auth{claudeResetTestAuth("a", now, 3*24*time.Hour, "0.1"), claudeResetTestAuth("b", now, 24*time.Hour, "0.1")}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				got, errPick := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
				if errPick != nil || got == nil || got.ID != "b" {
					t.Errorf("concurrent Pick() = %v, %v; want b", got, errPick)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestSoonestResetFirstManager(t *testing.T) {
	now := time.Now()
	for _, plugin := range []bool{false, true} {
		t.Run(fmt.Sprint("plugin=", plugin), func(t *testing.T) {
			var selector Selector = &SoonestResetFirstSelector{}
			if plugin {
				selector = &RoundRobinSelector{}
			}
			manager := NewManager(nil, selector, nil)
			manager.executors["claude"] = schedulerTestExecutor{}
			if plugin {
				manager.SetPluginScheduler(&fakePluginScheduler{handled: true, resp: pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinSoonestResetFirst}})
			}
			for _, auth := range []*Auth{claudeResetTestAuth("a", now, 3*24*time.Hour, "0.1"), claudeResetTestAuth("b", now, 24*time.Hour, "0.1")} {
				if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
					t.Fatal(errRegister)
				}
			}
			for i := 0; i < 3; i++ {
				got, _, errPick := manager.pickNext(context.Background(), "claude", "", cliproxyexecutor.Options{}, nil)
				if errPick != nil || got == nil || got.ID != "b" {
					t.Fatalf("manager pick = %v, %v; want b", got, errPick)
				}
			}
		})
	}
}

func TestSoonestResetFirstCodexUnknownWindows(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, tc := range []struct {
		name     string
		signals  map[string]string
		observed time.Time
	}{
		{name: "5h primary", signals: map[string]string{"x-codex-primary-window-minutes": "300", "x-codex-primary-reset-after-seconds": "3600"}, observed: now},
		{name: "primary length missing", signals: map[string]string{"x-codex-primary-reset-after-seconds": "3600"}, observed: now},
		{name: "relative reset expired", signals: map[string]string{"x-codex-secondary-window-minutes": "10080", "x-codex-secondary-reset-after-seconds": "60"}, observed: now.Add(-time.Hour)},
		{name: "secondary stale ignores primary", signals: map[string]string{"x-codex-secondary-reset-at": strconv.FormatInt(now.Add(-time.Hour).Unix(), 10), "x-codex-primary-window-minutes": "10080", "x-codex-primary-reset-after-seconds": "3600"}, observed: now},
		{name: "relative overflow", signals: map[string]string{"x-codex-secondary-reset-after-seconds": "9223372036854775807"}, observed: now},
		{name: "invalid utilization", signals: map[string]string{"x-codex-secondary-reset-after-seconds": "3600", "x-codex-secondary-used-percent": "NaN"}, observed: now},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unknown := resetTestAuth("z", "codex", tc.observed, tc.signals)
			known := resetTestAuth("a", "codex", now, map[string]string{"x-codex-secondary-reset-after-seconds": "86400", "x-codex-secondary-used-percent": "20"})
			selector := &SoonestResetFirstSelector{}
			pickResetTestAuth(t, selector, "codex", "", cliproxyexecutor.Options{}, []*Auth{known, unknown}, "z")
			pickResetTestAuth(t, selector, "codex", "", cliproxyexecutor.Options{}, []*Auth{known, unknown}, "a")
		})
	}
}

func TestSoonestResetFirstExhaustedFallback(t *testing.T) {
	now := time.Now()
	selector := &SoonestResetFirstSelector{}
	a := claudeResetTestAuth("a", now, time.Hour, "1.0")
	b := claudeResetTestAuth("b", now, 24*time.Hour, "1.2")
	for _, want := range []string{"a", "b", "a"} {
		pickResetTestAuth(t, selector, "claude", "", cliproxyexecutor.Options{}, []*Auth{a, b}, want)
	}
	pickResetTestAuth(t, selector, "claude", "", cliproxyexecutor.Options{}, []*Auth{a, b, {ID: "z"}}, "z")
}

func TestSoonestResetFirstBoundedRotation(t *testing.T) {
	selector := &SoonestResetFirstSelector{maxKeys: 2}
	auths := []*Auth{{ID: "a"}, {ID: "b"}}
	pickResetTestAuth(t, selector, "claude", "m1", cliproxyexecutor.Options{}, auths, "a")
	pickResetTestAuth(t, selector, "claude", "m2", cliproxyexecutor.Options{}, auths, "a")
	pickResetTestAuth(t, selector, "claude", "m1(high)", cliproxyexecutor.Options{}, auths, "b")
	pickResetTestAuth(t, selector, "claude", "m3", cliproxyexecutor.Options{}, auths, "a")
	if len(selector.rotations) > 2 {
		t.Fatalf("rotation entries = %d; want <= 2", len(selector.rotations))
	}
	pickResetTestAuth(t, selector, "claude", "m1", cliproxyexecutor.Options{}, auths, "a")
}

func TestSoonestResetFirstCodexWebsocketPreference(t *testing.T) {
	now := time.Now()
	a := resetTestAuth("a", "codex", now, map[string]string{"x-codex-secondary-reset-after-seconds": "86400"})
	b := resetTestAuth("b", "codex", now, map[string]string{"x-codex-secondary-reset-after-seconds": "259200"})
	b.Attributes = map[string]string{"websockets": "true"}
	selector := &SoonestResetFirstSelector{}
	got, errPick := selector.Pick(cliproxyexecutor.WithDownstreamWebsocket(context.Background()), "codex", "", cliproxyexecutor.Options{}, []*Auth{a, b})
	if errPick != nil || got == nil {
		t.Fatalf("websocket Pick() error = %v", errPick)
	}
	if got.ID != "b" {
		t.Fatalf("websocket Pick().ID = %q; want b", got.ID)
	}
	pickResetTestAuth(t, selector, "codex", "", cliproxyexecutor.Options{}, []*Auth{a, b}, "a")
}
