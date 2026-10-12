package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// retryPlugin builds a plugin through the real parser so attempt_retries and
// its friends are decoded exactly as production decodes them.
func retryPlugin(t *testing.T, raw string) *comboRouterPlugin {
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

const retryConfig = `
routes:
  - alias: smart
    cooldown_seconds: 30
    strategy: priority
    targets:
      - model: ca2/m1
        weight: 1
      - model: vsl/m1
        weight: 1
attempt_retries: 3
retry_backoff_seconds: 0
retry_deadline_seconds: 90
`

func TestDecodeRouterConfigReadsRetrySettings(t *testing.T) {
	cfg, err := decodeRouterConfig([]byte(retryConfig))
	if err != nil {
		t.Fatalf("decodeRouterConfig() error = %v", err)
	}
	if cfg.AttemptRetries != 3 {
		t.Fatalf("AttemptRetries = %d, want 3", cfg.AttemptRetries)
	}
	if cfg.RetryBackoffSeconds != 0 {
		t.Fatalf("RetryBackoffSeconds = %d, want 0", cfg.RetryBackoffSeconds)
	}
	if cfg.RetryDeadlineSeconds != 90 {
		t.Fatalf("RetryDeadlineSeconds = %d, want 90", cfg.RetryDeadlineSeconds)
	}
}

// TestDecodeRouterConfigRetryDefaultsKeepOldBehaviour is the compatibility
// guarantee: a configuration that never mentions retries must behave exactly as
// before, i.e. one attempt per target.
func TestDecodeRouterConfigRetryDefaultsKeepOldBehaviour(t *testing.T) {
	cfg, err := decodeRouterConfig([]byte(`
routes:
  - alias: smart
    cooldown_seconds: 30
    targets:
      - model: ca2/m1
`))
	if err != nil {
		t.Fatalf("decodeRouterConfig() error = %v", err)
	}
	if cfg.AttemptRetries != 0 {
		t.Fatalf("AttemptRetries = %d, want 0 (one attempt per target)", cfg.AttemptRetries)
	}
	if cfg.RetryBackoffSeconds != defaultRetryBackoffSeconds {
		t.Fatalf("RetryBackoffSeconds = %d, want the default %d", cfg.RetryBackoffSeconds, defaultRetryBackoffSeconds)
	}
	if cfg.RetryDeadlineSeconds != defaultRetryDeadlineSeconds {
		t.Fatalf("RetryDeadlineSeconds = %d, want the default %d", cfg.RetryDeadlineSeconds, defaultRetryDeadlineSeconds)
	}
}

func TestDecodeRouterConfigRejectsBadRetrySettings(t *testing.T) {
	for name, raw := range map[string]string{
		"retries_too_high":  "routes:\n  - alias: a\n    targets:\n      - model: ca2/m\nattempt_retries: 99\n",
		"retries_negative":  "routes:\n  - alias: a\n    targets:\n      - model: ca2/m\nattempt_retries: -1\n",
		"backoff_negative":  "routes:\n  - alias: a\n    targets:\n      - model: ca2/m\nretry_backoff_seconds: -1\n",
		"deadline_negative": "routes:\n  - alias: a\n    targets:\n      - model: ca2/m\nretry_deadline_seconds: -1\n",
	} {
		if _, err := decodeRouterConfig([]byte(raw)); err == nil {
			t.Fatalf("%s: decodeRouterConfig() accepted an invalid retry setting", name)
		}
	}
}

// TestRetryKeepsSameTargetWhenItRecovers is the heart of the change: the first
// target answers 502 (the provider's own "overloaded"), then recovers. The
// request must be served by ca2 — NOT handed to vsl — and ca2 must not be cooled,
// because a provider that answered on the second try was never broken.
func TestRetryKeepsSameTargetWhenItRecovers(t *testing.T) {
	plugin := retryPlugin(t, retryConfig)
	calls := 0
	host := &fakeModelHost{execute: func(request pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		calls++
		if calls == 1 {
			return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusBadGateway, Body: []byte(`{"error":"overloaded"}`)}, nil
		}
		return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"model":"smart"}`)}, nil
	}}

	response, err := plugin.executeWithHost(pluginapi.ExecutorRequest{Model: "smart"}, host)
	if err != nil {
		t.Fatalf("executeWithHost() error = %v", err)
	}
	if len(host.executeCalls) != 2 {
		t.Fatalf("host called %d times, want 2 (one failure, one retry)", len(host.executeCalls))
	}
	for index, call := range host.executeCalls {
		if call.Model != "ca2/m1" {
			t.Fatalf("call %d went to %q, want ca2/m1 (the same target must be retried)", index, call.Model)
		}
	}
	if len(response.Payload) == 0 {
		t.Fatal("executeWithHost() returned an empty payload")
	}
	// A target that answered must carry no failure history: cooling it here is
	// exactly the bug that took the whole route down.
	route := plugin.config.Routes[0]
	if failures := plugin.runtime.FailureCount(route, "ca2/m1"); failures != 0 {
		t.Fatalf("ca2/m1 failure count = %d after a successful retry, want 0", failures)
	}
}

// TestRetryExhaustedThenFallsOverToNextTarget: only after the retries at the
// first target are used up may the router move on.
func TestRetryExhaustedThenFallsOverToNextTarget(t *testing.T) {
	plugin := retryPlugin(t, retryConfig)
	host := &fakeModelHost{execute: func(request pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		if request.Model == "ca2/m1" {
			return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusBadGateway, Body: []byte(`{"error":"overloaded"}`)}, nil
		}
		return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"model":"smart"}`)}, nil
	}}

	if _, err := plugin.executeWithHost(pluginapi.ExecutorRequest{Model: "smart"}, host); err != nil {
		t.Fatalf("executeWithHost() error = %v", err)
	}
	// 1 initial attempt + 3 retries at ca2, then one attempt at vsl.
	if len(host.executeCalls) != 5 {
		t.Fatalf("host called %d times, want 5 (1+3 at ca2 then 1 at vsl)", len(host.executeCalls))
	}
	first, second := host.executeCalls[3], host.executeCalls[4]
	if first.Model != "ca2/m1" {
		t.Fatalf("call 4 went to %q, want the last retry at ca2/m1", first.Model)
	}
	if second.Model != "vsl/m1" {
		t.Fatalf("call 5 went to %q, want vsl/m1", second.Model)
	}
}

// TestRetryStopsOnTerminalFailure: a 400 belongs to the request, so retrying it
// only wastes the provider's time and the caller's budget.
func TestRetryStopsOnTerminalFailure(t *testing.T) {
	plugin := retryPlugin(t, retryConfig)
	host := &fakeModelHost{execute: func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusBadRequest, Body: []byte(`{"error":"bad request"}`)}, nil
	}}

	if _, err := plugin.executeWithHost(pluginapi.ExecutorRequest{Model: "smart"}, host); err == nil {
		t.Fatal("executeWithHost() returned no error for a terminal failure")
	}
	if len(host.executeCalls) != 1 {
		t.Fatalf("host called %d times, want 1 (a bad request must not be retried)", len(host.executeCalls))
	}
}

// TestRetryDisabledByDefaultKeepsOneAttemptPerTarget pins the compatibility
// contract for configurations that do not set attempt_retries.
func TestRetryDisabledByDefaultKeepsOneAttemptPerTarget(t *testing.T) {
	plugin := retryPlugin(t, `
routes:
  - alias: smart
    cooldown_seconds: 30
    strategy: priority
    targets:
      - model: ca2/m1
        weight: 1
      - model: vsl/m1
        weight: 1
`)
	host := &fakeModelHost{execute: func(request pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		if request.Model == "ca2/m1" {
			return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusBadGateway}, nil
		}
		return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"model":"smart"}`)}, nil
	}}
	if _, err := plugin.executeWithHost(pluginapi.ExecutorRequest{Model: "smart"}, host); err != nil {
		t.Fatalf("executeWithHost() error = %v", err)
	}
	if len(host.executeCalls) != 2 {
		t.Fatalf("host called %d times, want 2 (one attempt per target, no retries)", len(host.executeCalls))
	}
}

// TestRetryBudgetStopsTheLoop: a target that keeps answering "busy" must not be
// able to hold the request past the deadline. The retry count is deliberately
// high so only the budget can end the loop.
func TestRetryBudgetStopsTheLoop(t *testing.T) {
	plugin := retryPlugin(t, `
routes:
  - alias: smart
    cooldown_seconds: 30
    strategy: priority
    targets:
      - model: ca2/m1
        weight: 1
attempt_retries: 10
retry_backoff_seconds: 0
retry_deadline_seconds: 1
`)
	host := &fakeModelHost{execute: func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusBadGateway}, nil
	}}
	start := time.Now()
	if _, err := plugin.executeWithHost(pluginapi.ExecutorRequest{Model: "smart"}, host); err == nil {
		t.Fatal("executeWithHost() returned no error although every attempt failed")
	}
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("executeWithHost() took %v, want the 1s budget to stop the loop", elapsed)
	}
	if len(host.executeCalls) >= 11 {
		t.Fatalf("host called %d times, want fewer than the configured 11 attempts", len(host.executeCalls))
	}
}

// TestRetryBudgetArithmetic covers the boundary rules without a network call.
func TestRetryBudgetArithmetic(t *testing.T) {
	inactive := newRetryBudget(0)
	if inactive.expired() || inactive.exhaustedForAttempt() {
		t.Fatal("a disabled budget must never report itself exhausted")
	}

	active := newRetryBudget(90)
	if active.expired() {
		t.Fatal("a fresh 90s budget reported itself expired")
	}
	if got := active.attemptLimit(60 * time.Second); got != 60*time.Second {
		t.Fatalf("attemptLimit() = %v, want the 60s cap unchanged", got)
	}
	// A pause can never run past the deadline.
	if got := active.pauseFor(2*time.Second, 0); got != 2*time.Second {
		t.Fatalf("pauseFor() = %v, want 2s", got)
	}
	if got := active.pauseFor(2*time.Second, 30*time.Second); got != 30*time.Second {
		t.Fatalf("pauseFor() = %v, want the upstream 30s Retry-After to win", got)
	}

	// A budget shorter than the attempt cap must clip the cap.
	tight := newRetryBudget(5)
	if got := tight.attemptLimit(60 * time.Second); got > 5*time.Second {
		t.Fatalf("attemptLimit() = %v, want it clipped to the 5s budget", got)
	}
}

// TestRetryDoesNotRetryATimedOutTarget: a target that produced no first byte for
// the whole attempt window is the genuinely dead case. Retrying it would double
// the caller's wait before reaching a target that might actually answer, so the
// router must move on after the FIRST timeout.
func TestRetryDoesNotRetryATimedOutTarget(t *testing.T) {
	plugin := retryPlugin(t, `
routes:
  - alias: smart
    cooldown_seconds: 30
    strategy: priority
    targets:
      - model: ca2/m1
        weight: 1
      - model: vsl/m1
        weight: 1
attempt_retries: 3
retry_backoff_seconds: 0
retry_deadline_seconds: 90
`)
	// The fake host blocks past the attempt cap so the attempt times out.
	host := &fakeModelHost{execute: func(request pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		if request.Model == "ca2/m1" {
			// Longer than the 1s attempt cap, so this attempt times out.
			time.Sleep(1200 * time.Millisecond)
			return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"model":"smart"}`)}, nil
		}
		return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"model":"smart"}`)}, nil
	}}
	// A 1-second attempt cap makes the sleeping ca2 attempt time out while vsl
	// answers immediately.
	plugin.config.AttemptTimeoutSeconds = 1
	start := time.Now()
	response, err := plugin.executeWithHost(pluginapi.ExecutorRequest{Model: "smart"}, host)
	if err != nil {
		t.Fatalf("executeWithHost() error = %v", err)
	}
	if len(response.Payload) == 0 {
		t.Fatal("executeWithHost() returned an empty payload; vsl should have answered")
	}
	if len(host.executeCalls) != 2 {
		t.Fatalf("host called %d times, want 2 (one timeout at ca2, then vsl — no retry)", len(host.executeCalls))
	}
	if host.executeCalls[1].Model != "vsl/m1" {
		t.Fatalf("second call went to %q, want vsl/m1", host.executeCalls[1].Model)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("executeWithHost() took %v; a timeout must not be retried", elapsed)
	}
}

// TestShouldRetryRules pins the decision table.
func TestShouldRetryRules(t *testing.T) {
	cfg := routerConfig{AttemptRetries: 3}
	fallback := policyOutcome{Action: errorActionFallback}
	noPenalty := policyOutcome{Action: errorActionFallbackNoPenalty}
	stop := policyOutcome{Action: errorActionStop}
	budget := newRetryBudget(90)
	busy := statusError{status: http.StatusBadGateway, message: "overloaded"}
	timedOut := attemptTimeoutError{target: "ca2/m1", limit: time.Minute}

	if !shouldRetry(cfg, busy, fallback, 0, budget) {
		t.Fatal("a busy answer with retries left must be retried")
	}
	if !shouldRetry(cfg, busy, noPenalty, 2, budget) {
		t.Fatal("a no-penalty busy answer with retries left must be retried")
	}
	if shouldRetry(cfg, busy, stop, 0, budget) {
		t.Fatal("a stop decision must never be retried")
	}
	if shouldRetry(cfg, busy, fallback, 3, budget) {
		t.Fatal("a failure with no retries left must not be retried")
	}
	if shouldRetry(routerConfig{AttemptRetries: 0}, busy, fallback, 0, budget) {
		t.Fatal("attempt_retries=0 must disable retrying entirely")
	}
	if !shouldRetry(cfg, busy, fallback, 0, newRetryBudget(-1)) {
		t.Fatal("a disabled budget must not block retrying")
	}
	// A target that never produced a first byte is the genuinely dead case:
	// retrying it only doubles the caller's wait.
	if shouldRetry(cfg, timedOut, fallback, 0, budget) {
		t.Fatal("a first-byte timeout must not be retried at the same target")
	}
}
