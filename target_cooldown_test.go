package main

import (
	"net/http"
	"testing"
	"time"
)

// The "Chờ riêng" box on a target only means something if the fallback chain
// behind it is exactly the documented one. Pin it down so a future edit to
// targetCooldownSeconds cannot silently change what operators configured.
func TestTargetCooldownFallsBackFromTargetToRouteToDefault(t *testing.T) {
	route := modelRoute{
		Alias:           "fast",
		CooldownSeconds: 30,
		Targets: []modelTarget{
			{Model: "a6/deepseek-v4.1-flash", CooldownSeconds: 5},
			{Model: "lbd/deepseek-v4.1-flash"},
		},
	}

	// A target that names its own number uses it.
	if got := targetCooldownSeconds(route, "a6/deepseek-v4.1-flash"); got != 5 {
		t.Errorf("target with its own cooldown: got %d, want 5", got)
	}
	// A target that names none inherits the route's.
	if got := targetCooldownSeconds(route, "lbd/deepseek-v4.1-flash"); got != 30 {
		t.Errorf("target without its own cooldown: got %d, want the route's 30", got)
	}
	// A route with no number either falls through to the built-in default.
	bare := modelRoute{Alias: "bare", Targets: []modelTarget{{Model: "m"}}}
	if got := targetCooldownSeconds(bare, "m"); got != defaultCooldownSeconds {
		t.Errorf("unconfigured route: got %d, want the default %d", got, defaultCooldownSeconds)
	}
	// The lookup is case/space insensitive, matching how targets are matched.
	if got := targetCooldownSeconds(route, "  A6/DeepSeek-V4.1-Flash "); got != 5 {
		t.Errorf("case-insensitive lookup: got %d, want 5", got)
	}
}

// When an error_policy rule names no cooldown of its own, the wait must be the
// target's "Chờ riêng" value — not a fresh default. This is the link that makes
// the box meaningful at all.
func TestPolicyRuleWithoutCooldownInheritsTheTargetValue(t *testing.T) {
	route := modelRoute{
		Alias:           "fast",
		CooldownSeconds: 30,
		Targets:         []modelTarget{{Model: "a6/x", CooldownSeconds: 5}},
	}
	rule := errorRule{Action: errorActionFallback} // names no cooldown_seconds

	inherit := time.Duration(targetCooldownSeconds(route, "a6/x")) * time.Second
	if got := rule.cooldownFor(1, 0, inherit); got != 5*time.Second {
		t.Errorf("rule with no cooldown: got %v, want the target's 5s", got)
	}

	// A rule that does name a number overrides the target's box.
	explicit := errorRule{Action: errorActionFallback, CooldownSeconds: 20}
	if got := explicit.cooldownFor(1, 0, inherit); got != 20*time.Second {
		t.Errorf("rule with its own cooldown: got %v, want 20s", got)
	}

	// A rule honouring Retry-After lets the upstream decide, capped by the rule.
	honouring := errorRule{Action: errorActionFallback, CooldownSeconds: 20, HonorRetryAfter: true, MaxCooldownSeconds: 60}
	if got := honouring.cooldownFor(1, 45*time.Second, inherit); got != 45*time.Second {
		t.Errorf("Retry-After honoured: got %v, want 45s", got)
	}
	if got := honouring.cooldownFor(1, 600*time.Second, inherit); got != 60*time.Second {
		t.Errorf("Retry-After above the ceiling: got %v, want the 60s cap", got)
	}
}

// The end-to-end shape: a 429 with a rule that names no cooldown parks the
// failing target for its own "Chờ riêng" value, while the healthy target keeps
// serving.
func TestFailoverUsesTheTargetCooldownOn429(t *testing.T) {
	cfg := routerConfig{
		Enabled: true,
		Routes: []modelRoute{{
			Alias:           "fast",
			Strategy:        routeStrategyPriority,
			CooldownSeconds: 30,
			Targets: []modelTarget{
				{Model: "a6/x", CooldownSeconds: 5},
				{Model: "lbd/x"},
			},
		}},
		ErrorPolicy: errorPolicy{
			Default: errorRule{Action: errorActionFallback, CooldownSeconds: 30},
			Rules: []errorRule{
				{Statuses: []int{http.StatusTooManyRequests}, Action: errorActionFallback},
			},
		},
		ErrorPolicyConfigured: true,
	}

	outcome := decideAttempt(
		newRouteError(http.StatusTooManyRequests, "rate_limited", "fast", "upstream said 429", ""),
		cfg, cfg.Routes[0], "a6/x", 0, 1,
	)
	if outcome.Action != errorActionFallback {
		t.Fatalf("action = %q, want fallback", outcome.Action)
	}
	if outcome.Cooldown != 5*time.Second {
		t.Errorf("cooldown = %v, want the target's own 5s", outcome.Cooldown)
	}
}
