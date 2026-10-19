package config

import "testing"

func TestCodexWebsocketsDefaultLayoutCompatibility(t *testing.T) {
	for _, raw := range []string{
		"codex:\n  websockets: true\n",
		"config-version: 8\noauth:\n  providers:\n    codex:\n      websockets: true\n",
		"config-version: 8\nupstream:\n  codex:\n    websockets: true\n",
	} {
		cfg, err := ParseConfigBytes([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.Codex.Websockets {
			t.Fatal("Codex WebSocket default was lost during layout translation")
		}
	}
	cfg, err := ParseConfigBytes([]byte("config-version: 8\ncodex:\n  websockets: true\nupstream:\n  codex:\n    websockets: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Codex.Websockets {
		t.Fatal("canonical explicit false must win over the legacy default")
	}
}
