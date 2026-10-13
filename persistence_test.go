package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A freshly configured plugin must write its state file on the first failure.
// Regression: SetPersistPath existed but was never called, and Load only adopts
// the path when the file ALREADY exists, so on a first run persistPath stayed
// empty and cooldown state was never written at all — the restart-amnesia the
// fork was built to fix.
func TestNewComboRouterPluginAdoptsStatePathWithoutAnExistingFile(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "combo-router-state.json")
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("precondition failed: %s already exists", statePath)
	}

	config := []byte("state_path: " + filepath.ToSlash(statePath) + `
routes:
  - alias: fast
    strategy: priority
    targets:
      - model: provider-a/model-x
      - model: provider-b/model-x
`)
	plugin, _, err := newComboRouterPlugin(config, nil)
	if err != nil {
		t.Fatalf("newComboRouterPlugin: %v", err)
	}
	if plugin.runtime.persistPath != statePath {
		t.Fatalf("persistPath = %q, want %q — state would never be written", plugin.runtime.persistPath, statePath)
	}

	// Drive a failure through the public path and prove the file appears.
	route := plugin.config.Routes[0]
	plugin.runtime.MarkFailure(route, "provider-a/model-x", 30*time.Second)
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state file was not written after a failure: %v", err)
	}
}

// TestLoadRestoredStateSurvivesFirstAttempt is the regression for the restart
// panic. Load() rebuilds route state from the persisted file; before the fix it
// left the metrics and lastFailures maps nil, so the first RecordAttempt (or
// MarkFailure) after a restart panicked with "assignment to entry in nil map"
// and took the whole plugin down.
func TestLoadRestoredStateSurvivesFirstAttempt(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "combo-router-state.json")
	route := testModelRoute("fast", routeStrategyPriority, 30, "provider-a/model-x", "provider-b/model-x")

	original := newRouteRuntime(nil)
	original.Sync([]modelRoute{route})
	original.SetPersistPath(statePath)
	original.MarkFailure(route, "provider-a/model-x", 30*time.Second)
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state file was not written: %v", err)
	}

	loaded := newRouteRuntime(nil)
	loaded.Load(statePath)

	// The persisted failure must survive the restart.
	if failures := loaded.FailureCount(route, "provider-a/model-x"); failures != 1 {
		t.Fatalf("FailureCount after Load = %d, want 1", failures)
	}

	// Each of these wrote to a nil map before the fix and panicked.
	loaded.RecordAttempt(route, "provider-a/model-x", 120, false, true)
	loaded.MarkFailure(route, "provider-a/model-x", 30*time.Second)
	loaded.MarkSuccess(route, "provider-a/model-x")
	loaded.RecordAttempt(route, "provider-a/model-x", 80, false, false)
}
