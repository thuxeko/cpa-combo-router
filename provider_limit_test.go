package main

import (
	"errors"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// providerLimitPlugin builds a plugin through the real configuration parser so
// provider_limits is decoded exactly as production decodes it.
func providerLimitPlugin(t *testing.T, raw string) *comboRouterPlugin {
	t.Helper()
	cfg, err := decodeRouterConfig([]byte(raw))
	if err != nil {
		t.Fatalf("decodeRouterConfig() error = %v", err)
	}
	plugin := testRouterPlugin(cfg.Routes...)
	plugin.config = cfg
	plugin.runtime.SetProviderLimits(cfg.ProviderLimits)
	return plugin
}

const providerLimitConfig = `
routes:
  - alias: smart
    cooldown_seconds: 30
    strategy: priority
    targets:
      - model: ca2/m1
        weight: 1
      - model: vsl/m1
        weight: 1
provider_limits:
  ca2: 2
`

func TestDecodeRouterConfigReadsProviderLimits(t *testing.T) {
	cfg, err := decodeRouterConfig([]byte(providerLimitConfig))
	if err != nil {
		t.Fatalf("decodeRouterConfig() error = %v", err)
	}
	if got := cfg.ProviderLimits["ca2"]; got != 2 {
		t.Fatalf("provider_limits[ca2] = %d, want 2", got)
	}
	if len(cfg.ProviderLimits) != 1 {
		t.Fatalf("provider_limits = %#v", cfg.ProviderLimits)
	}
}

func TestDecodeRouterConfigRejectsBadProviderLimits(t *testing.T) {
	for name, raw := range map[string]string{
		"zero":     "routes:\n  - alias: a\n    targets:\n      - model: ca2/m\nprovider_limits:\n  ca2: 0\n",
		"negative": "routes:\n  - alias: a\n    targets:\n      - model: ca2/m\nprovider_limits:\n  ca2: -3\n",
		"empty":    "routes:\n  - alias: a\n    targets:\n      - model: ca2/m\nprovider_limits:\n  \"\": 4\n",
	} {
		if _, err := decodeRouterConfig([]byte(raw)); err == nil {
			t.Fatalf("%s: decodeRouterConfig() accepted an invalid provider_limits", name)
		}
	}
}

// TestProviderLimitFallsThroughWhenFull is the heart of the change: with ca2
// full, the request must be handed to the next target instead of being sent
// upstream, and no upstream call may be made to the capped provider.
func TestProviderLimitFallsThroughWhenFull(t *testing.T) {
	plugin := providerLimitPlugin(t, providerLimitConfig)
	route := plugin.config.Routes[0]

	// Occupy both ca2 slots the way a live request does: through selection,
	// which is where the reservation happens.
	for slot := 0; slot < 2; slot++ {
		selection := plugin.runtime.SelectExcluding(route, nil)
		if selection.model != "ca2/m1" {
			t.Fatalf("slot %d: selection = %#v, want the ca2 target", slot, selection)
		}
	}
	if inFlight := plugin.runtime.ProviderInFlight("ca2"); inFlight != 2 {
		t.Fatalf("ca2 in flight = %d, want 2", inFlight)
	}

	// The third request must skip ca2 and take vsl.
	third := plugin.runtime.SelectExcluding(route, nil)
	if third.model != "vsl/m1" {
		t.Fatalf("third selection = %#v, want vsl/m1 (ca2 full)", third)
	}
	if third.providerBlocked {
		t.Fatalf("third selection reported providerBlocked although vsl was free")
	}

	// Releasing one ca2 slot puts it back in rotation.
	plugin.runtime.Release("ca2/m1")
	if inFlight := plugin.runtime.ProviderInFlight("ca2"); inFlight != 1 {
		t.Fatalf("ca2 in flight after release = %d, want 1", inFlight)
	}
}

// TestProviderLimitReportsProviderBusyWhenEveryCandidateIsFull covers a route
// whose only target sits at the capped provider: the caller must get the
// capacity error, not a cooldown error.
func TestProviderLimitReportsProviderBusyWhenEveryCandidateIsFull(t *testing.T) {
	plugin := providerLimitPlugin(t, `
routes:
  - alias: solo
    cooldown_seconds: 30
    strategy: priority
    targets:
      - model: ca2/only
        weight: 1
provider_limits:
  ca2: 1
`)
	route := plugin.config.Routes[0]
	if selection := plugin.runtime.SelectExcluding(route, nil); selection.model != "ca2/only" {
		t.Fatalf("first selection = %#v", selection)
	}
	blocked := plugin.runtime.SelectExcluding(route, nil)
	if blocked.model != "" || !blocked.providerBlocked || blocked.allCooling {
		t.Fatalf("blocked selection = %#v, want providerBlocked", blocked)
	}

	_, err := plugin.executeWithHost(pluginapi.ExecutorRequest{Model: "solo"}, &fakeModelHost{
		execute: func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
			t.Fatalf("the host must not be called while the provider budget is spent")
			return pluginapi.HostModelExecutionResponse{}, nil
		},
	})
	if err == nil {
		t.Fatalf("executeWithHost() returned no error although the only target was full")
	}
	var carrier interface{ ErrorCode() string }
	if !errors.As(err, &carrier) || carrier.ErrorCode() != "provider_inflight_limit" {
		t.Fatalf("error = %v, want code provider_inflight_limit", err)
	}
	if status := statusFromError(err); status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
}

// TestProviderLimitSharesOneBudgetAcrossRoutes proves the budget belongs to the
// provider, not to a route: two different aliases that both reach ca2 must draw
// from the same allowance.
func TestProviderLimitSharesOneBudgetAcrossRoutes(t *testing.T) {
	plugin := providerLimitPlugin(t, `
routes:
  - alias: one
    cooldown_seconds: 30
    strategy: priority
    targets:
      - model: ca2/shared
        weight: 1
  - alias: two
    cooldown_seconds: 30
    strategy: priority
    targets:
      - model: ca2/shared
        weight: 1
provider_limits:
  ca2: 1
`)
	first := plugin.config.Routes[0]
	second := plugin.config.Routes[1]
	if selection := plugin.runtime.SelectExcluding(first, nil); selection.model != "ca2/shared" {
		t.Fatalf("route one selection = %#v", selection)
	}
	// Route two reaches the same provider through a different alias: it must
	// see the slot as taken.
	if selection := plugin.runtime.SelectExcluding(second, nil); selection.providerBlocked != true {
		t.Fatalf("route two selection = %#v, want providerBlocked", selection)
	}
}

// TestProviderLimitReleasedAfterAttempt checks the slot is handed back once the
// attempt finished, so a successful call does not leak a lease.
func TestProviderLimitReleasedAfterAttempt(t *testing.T) {
	plugin := providerLimitPlugin(t, providerLimitConfig)
	host := &fakeModelHost{execute: func(request pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"model":"smart"}`)}, nil
	}}
	for attempt := 0; attempt < 5; attempt++ {
		if _, err := plugin.executeWithHost(pluginapi.ExecutorRequest{Model: "smart"}, host); err != nil {
			t.Fatalf("attempt %d: executeWithHost() error = %v", attempt, err)
		}
		if inFlight := plugin.runtime.ProviderInFlight("ca2"); inFlight != 0 {
			t.Fatalf("attempt %d: ca2 in flight = %d after a finished attempt, want 0", attempt, inFlight)
		}
	}
}

// TestProviderLimitDoesNotApplyToUnprefixedTargets keeps the feature opt-in:
// a target without a provider prefix is never budgeted.
func TestProviderLimitDoesNotApplyToUnprefixedTargets(t *testing.T) {
	plugin := providerLimitPlugin(t, `
routes:
  - alias: plain
    cooldown_seconds: 30
    strategy: priority
    targets:
      - model: gemini-3.8-flash-high
        weight: 1
provider_limits:
  ca2: 1
`)
	route := plugin.config.Routes[0]
	for attempt := 0; attempt < 4; attempt++ {
		if selection := plugin.runtime.SelectExcluding(route, nil); selection.model != "gemini-3.8-flash-high" {
			t.Fatalf("attempt %d: selection = %#v, want the unprefixed target", attempt, selection)
		}
	}
}
