package main

import "time"

// Retry policy. The retry loop exists because a provider that says "busy" for
// a second is not a broken provider: without retries the router treats one
// transient rejection as a dead target, cools it, and — on a two-target route —
// ends up with every candidate cooling down, which is reported to the caller as
// a route failure even though both providers are healthy.
//
// Retries default to ZERO so a configuration that does not ask for them keeps
// the original one-attempt-per-target behaviour exactly.
const (
	// defaultRetryBackoffSeconds is the pause between attempts at the same
	// target when the configuration names no value.
	defaultRetryBackoffSeconds = 2
	// defaultRetryDeadlineSeconds bounds the total time one request may spend
	// retrying. It must stay under the client's own patience: the calling app
	// aborts at 60 s (Claude Code) to 300 s (omp), and an abort while the
	// router is still retrying surfaces as a 499.
	defaultRetryDeadlineSeconds = 90
	// maxRetryCount caps configured retries. Retrying a genuinely overloaded
	// provider adds load to it, so a large number makes an overload worse.
	maxRetryCount = 10
	// maxRetryBackoffSeconds caps a single configured pause.
	maxRetryBackoffSeconds = 60
	// maxRetryDeadlineSeconds caps the whole-request retry budget.
	maxRetryDeadlineSeconds = 3600
	// maxHealthWindowSeconds caps how long a failure keeps a target labelled
	// degraded, so a typo cannot park a target as "unhealthy" beyond a day.
	maxHealthWindowSeconds = 86400
)

// retryBudget bounds the total time one request may spend retrying. A provider
// that answers "busy" forever must not be able to hold the caller past its own
// timeout: that is the failure mode the deadline exists to prevent.
type retryBudget struct {
	deadline time.Time
	active   bool
}

// newRetryBudget starts the clock for one request. A non-positive number of
// seconds disables the deadline, which is the same as "no bound".
func newRetryBudget(seconds int) retryBudget {
	if seconds <= 0 {
		return retryBudget{}
	}
	return retryBudget{
		deadline: time.Now().Add(time.Duration(seconds) * time.Second),
		active:   true,
	}
}

// expired reports whether no further attempt may be started.
func (budget retryBudget) expired() bool {
	return budget.active && !time.Now().Before(budget.deadline)
}

// exhaustedForAttempt reports whether the budget is too small to start another
// attempt. A sliver of time would only produce a timeout that is then reported
// as a provider fault, so the router stops instead of blaming a healthy target.
func (budget retryBudget) exhaustedForAttempt() bool {
	if !budget.active {
		return false
	}
	return budget.remaining() < time.Second
}

// remaining reports how much of the budget is left. It is meaningless when the
// budget is inactive, so callers check active first.
func (budget retryBudget) remaining() time.Duration {
	if !budget.active {
		return 0
	}
	return time.Until(budget.deadline)
}

// pauseFor returns how long to wait before the next attempt at the same target.
// The upstream's own Retry-After wins when it is longer, because the provider
// knows when it will be ready and a shorter local guess only earns another
// rejection. The wait is clipped to the budget so it cannot run past the
// deadline.
func (budget retryBudget) pauseFor(backoff, retryAfter time.Duration) time.Duration {
	pause := backoff
	if retryAfter > pause {
		pause = retryAfter
	}
	if pause < 0 {
		return 0
	}
	if budget.active {
		if left := budget.remaining(); pause > left {
			pause = left
		}
	}
	if pause < 0 {
		return 0
	}
	return pause
}

// attemptLimit clips a per-attempt first-byte cap to what is left of the retry
// budget, so an attempt that hangs cannot carry the request past the deadline.
// A cap of zero means "no cap" and is returned unchanged.
func (budget retryBudget) attemptLimit(attemptTimeout time.Duration) time.Duration {
	if !budget.active || attemptTimeout <= 0 {
		return attemptTimeout
	}
	left := budget.remaining()
	if left <= 0 {
		return time.Second
	}
	if left < attemptTimeout {
		return left
	}
	return attemptTimeout
}

// shouldRetry reports whether the SAME target deserves another attempt.
//
// Retrying is for a provider that answered quickly with "busy" (429, 502,
// 503): those clear in a second and the retry is what keeps a healthy target
// from being cooled. It is NOT for an attempt that timed out — a target that
// produced no first byte for the whole attempt window is the "genuinely dead"
// case, and retrying it only doubles the caller's wait before the router moves
// to a target that might actually answer.
//
// A failure the request itself caused (a `stop` decision) is never retried,
// because no number of retries fixes a malformed request.
func shouldRetry(cfg routerConfig, err error, decision policyOutcome, retriesUsed int, budget retryBudget) bool {
	if !decision.shouldFallback() {
		return false
	}
	if _, timedOut := err.(attemptTimeoutError); timedOut {
		return false
	}
	if retriesUsed >= cfg.AttemptRetries {
		return false
	}
	return !budget.expired()
}
