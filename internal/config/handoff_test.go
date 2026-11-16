package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHandoffConfigRoundTripAndClone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "port: 8317\nrouting:\n  handoff:\n    enabled: true\n    caller-scopes: [scope-a]\n    state-file: state/handoff.db\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.Routing.Handoff
	if !h.Enabled || len(h.CallerScopes) != 1 || h.StateFile != "state/handoff.db" {
		t.Fatalf("%+v", h)
	}
	clone := cfg.CloneForRuntime()
	clone.Routing.Handoff.CallerScopes[0] = "other"
	if cfg.Routing.Handoff.CallerScopes[0] != "scope-a" {
		t.Fatal("shared allowlist")
	}
	if err = SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(path)
	if err != nil || !cfg.Routing.Handoff.Enabled {
		t.Fatalf("%+v %v", cfg, err)
	}
}
