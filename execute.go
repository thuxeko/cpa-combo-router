package main

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type modelHost interface {
	Execute(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error)
	StartStream(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelStreamResponse, error)
	ReadStream(string) (pluginapi.HostModelStreamReadResponse, error)
	CloseStream(string) error
	Emit(string, []byte) error
	ClosePluginStream(string, string)
}

// executeWithAttemptTimeout runs one target attempt under the configured cap.
// The cap exists because the plugin otherwise waits indefinitely on a wedged
// target: the client gives up first, which surfaces as a 499 with no failover.
// The abandoned goroutine is not leaked — the result channel is buffered, so the
// host call always has somewhere to deliver its answer.
func executeWithAttemptTimeout(host modelHost, request pluginapi.HostModelExecutionRequest, timeout time.Duration) (pluginapi.HostModelExecutionResponse, error) {
	if timeout <= 0 {
		return host.Execute(request)
	}
	type outcome struct {
		response pluginapi.HostModelExecutionResponse
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		response, err := host.Execute(request)
		done <- outcome{response: response, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-done:
		return result.response, result.err
	case <-timer.C:
		return pluginapi.HostModelExecutionResponse{}, attemptTimeoutError{target: request.Model, limit: timeout}
	}
}

// attemptTimeoutFor resolves the first-byte timeout for one attempt at a
// target. A target that failed inside the health window gets the degraded
// timeout when one is configured, so a slow or sick target costs less of the
// caller's patience before the router moves on. The degraded value may not
// exceed the base attempt timeout: making a sick target wait LONGER than a
// healthy one defeats the point.
func (p *comboRouterPlugin) attemptTimeoutFor(route modelRoute, model string, budget retryBudget) time.Duration {
	base := time.Duration(p.config.AttemptTimeoutSeconds) * time.Second
	degraded := time.Duration(p.config.DegradedAttemptTimeoutSeconds) * time.Second
	if degraded > 0 && degraded < base && p.runtime.IsDegraded(route, model) {
		base = degraded
	}
	return budget.attemptLimit(base)
}

func (p *comboRouterPlugin) executeWithHost(request pluginapi.ExecutorRequest, host modelHost) (pluginapi.ExecutorResponse, error) {
	if p == nil || host == nil {
		return pluginapi.ExecutorResponse{}, statusError{status: http.StatusBadGateway, message: "model router host callback is unavailable"}
	}
	route, ok := p.matchingRoute(request.Model)
	if !ok {
		return pluginapi.ExecutorResponse{}, statusError{status: http.StatusBadGateway, message: "no model route matched executor request"}
	}
	requestedModel := strings.TrimSpace(request.Model)
	bodyInfo := bodyForExecution(request)
	cfg := p.config
	budget := newRetryBudget(cfg.RetryDeadlineSeconds)
	backoff := time.Duration(cfg.RetryBackoffSeconds) * time.Second
	maxCooldownWait := time.Duration(cfg.MaxCooldownWaitSeconds) * time.Second
	// cooldownDeadline bounds the TOTAL time one request may hold waiting for
	// a cooling target. Zero (feature disabled) keeps the historical
	// behaviour of failing the request at once. The retry budget still clamps
	// every individual wait, so the shorter of the two wins.
	cooldownDeadline := time.Time{}
	if maxCooldownWait > 0 {
		cooldownDeadline = time.Now().Add(maxCooldownWait)
	}
	// lastErr survives across passes: it carries the most recent failure
	// detail into the "no available candidate" error when the request gives
	// up.
	var lastErr error
	for {
		// The total cooldown ceiling bounds the WHOLE request. Reached at the
		// top of a pass, it means the previous pass failed and the request has
		// already held as long as it is allowed to: surface the route error
		// instead of looping again.
		if !cooldownDeadline.IsZero() && !time.Now().Before(cooldownDeadline) {
			detail := ""
			if lastErr != nil {
				detail = lastErr.Error()
			}
			return pluginapi.ExecutorResponse{}, newRouteError(http.StatusServiceUnavailable, "model_route_unavailable", route.Alias, fmt.Sprintf("no available candidate for model route %q", route.Alias), detail)
		}
		// One pass over the target pool. When every candidate is cooling the
		// request waits (bounded by max_cooldown_wait_seconds and the retry
		// budget) and re-enters the pass instead of dying with a 503, so a
		// transient burst of failures does not tear a client task apart.
		var coolingWait time.Duration
		attempted := make(map[string]struct{}, len(route.Targets))
		for attempt := 0; attempt < len(route.Targets); attempt++ {
			// The retry budget covers the whole request, so it can run out while
			// targets remain. Starting another attempt with a sliver of budget
			// would only produce a timeout, so stop and report what is known.
			if budget.exhaustedForAttempt() {
				break
			}
			selection := p.runtime.SelectExcluding(route, attempted)
			if selection.allCooling {
				// Feature off: keep the historical behaviour of failing the
				// request at once with the 429 cooldown error.
				if maxCooldownWait <= 0 {
					return pluginapi.ExecutorResponse{}, cooldownRouteError(route, selection.retryAfter)
				}
				coolingWait = selection.retryAfter
				break
			}
			if selection.model == "" {
				// A candidate that was passed over only because its provider was
				// full is not a sick target: report it as a capacity limit rather
				// than as a route with nothing left.
				if selection.providerBlocked {
					return pluginapi.ExecutorResponse{}, providerBusyError(route)
				}
				break
			}
			attempted[routeKey(selection.model)] = struct{}{}
			target := targetModel(requestedModel, selection.model)

			// Retry the SAME target before moving on. A provider that answered
			// "busy" a moment ago is usually ready a second later, and treating
			// that as a dead target is what cools a healthy route.
			//
			// The provider slot stays claimed for the whole retry sequence. That is
			// deliberate: the slot represents load we are sending upstream, and
			// retrying is still sending to that provider. Releasing between
			// attempts would let other requests take the freed capacity and push
			// the busy provider further over its limit.
			var (
				response pluginapi.HostModelExecutionResponse
				err      error
				outcome  policyOutcome
			)
			for retriesUsed := 0; ; retriesUsed++ {
				attemptStart := time.Now()
				response, err = executeWithAttemptTimeout(host, hostRequest(request, bodyInfo, target, false), p.attemptTimeoutFor(route, selection.model, budget))
				// TTFB for the buffered path is the whole host call: the first
				// byte and the last byte land together. It still measures how
				// long the target held the request open.
				ttfb := time.Since(attemptStart)
				timedOut := false
				if _, isTimeout := err.(attemptTimeoutError); isTimeout {
					timedOut = true
				}
				status := response.StatusCode
				if status == 0 && err == nil {
					status = http.StatusOK
				}
				if status == 0 && err != nil {
					status = statusFromError(err)
				}
				if err == nil && status >= 200 && status < 300 {
					p.runtime.RecordAttempt(route, selection.model, ttfb.Milliseconds(), false, false)
					p.runtime.Release(selection.model)
					p.runtime.MarkSuccess(route, selection.model)
					return pluginapi.ExecutorResponse{
						Payload: rewriteResponseModel(response.Body, requestedModel),
						Headers: cloneHeader(response.Headers),
						Metadata: map[string]any{
							"route_alias":    route.Alias,
							"selected_model": target,
							"selected_index": attempt,
						},
					}, nil
				}
				if err == nil {
					err = statusError{status: status, message: hostStatusMessage(target, status, response.Body)}
				}
				p.runtime.RecordAttempt(route, selection.model, ttfb.Milliseconds(), timedOut, true)
				// The upstream's own Retry-After is more accurate than any
				// configured number, so it is read here and handed to the policy.
				retryAfter := parseRetryAfter(response.Headers)
				consecutive := p.runtime.FailureCount(route, selection.model) + 1
				outcome = decideAttempt(err, cfg, route, selection.model, retryAfter, consecutive)
				if !shouldRetry(cfg, err, outcome, retriesUsed, budget) {
					break
				}
				if pause := budget.pauseFor(backoff, retryAfter); pause > 0 {
					time.Sleep(pause)
				}
			}
			// The retry sequence is over for this target, so its slot goes back.
			p.runtime.Release(selection.model)
			if !outcome.shouldFallback() {
				p.runtime.ReleaseProbe(route, selection.model)
				return pluginapi.ExecutorResponse{}, err
			}
			if outcome.penalises() {
				p.runtime.MarkFailure(route, selection.model, outcome.Cooldown)
			} else {
				p.runtime.ReleaseProbe(route, selection.model)
			}
			lastErr = err
		}
		// The pass exhausted every candidate. A wait may have been computed
		// mid-pass (every target cooling on entry), or it is discovered now:
		// after every target failed, re-query with an empty exclusion set — if
		// all of them are cooling, the request may hold until the earliest one
		// frees instead of dying with the 503 it used to get.
		if coolingWait <= 0 && lastErr != nil {
			selection := p.runtime.SelectExcluding(route, nil)
			switch {
			case selection.allCooling:
				coolingWait = selection.retryAfter
			case selection.providerBlocked:
				return pluginapi.ExecutorResponse{}, providerBusyError(route)
			}
		}
		// Hold only while the total cooldown ceiling still allows it. When the
		// feature is off (maxCooldownWaitSeconds == 0) or the deadline passed,
		// surface the route error exactly as before.
		if coolingWait <= 0 || cooldownDeadline.IsZero() || !time.Now().Before(cooldownDeadline) {
			detail := ""
			if lastErr != nil {
				detail = lastErr.Error()
			}
			return pluginapi.ExecutorResponse{}, newRouteError(http.StatusServiceUnavailable, "model_route_unavailable", route.Alias, fmt.Sprintf("no available candidate for model route %q", route.Alias), detail)
		}
		if left := time.Until(cooldownDeadline); coolingWait > left {
			coolingWait = left
		}
		if !budget.waitForCooldown(coolingWait) {
			detail := ""
			if lastErr != nil {
				detail = lastErr.Error()
			}
			return pluginapi.ExecutorResponse{}, newRouteError(http.StatusServiceUnavailable, "model_route_unavailable", route.Alias, fmt.Sprintf("no available candidate for model route %q", route.Alias), detail)
		}
	}
}

func hostRequest(request pluginapi.ExecutorRequest, bodyInfo executionBody, model string, stream bool) pluginapi.HostModelExecutionRequest {
	return pluginapi.HostModelExecutionRequest{
		EntryProtocol: bodyInfo.entryProtocol,
		ExitProtocol:  bodyInfo.responseProtocol,
		Model:         model,
		Stream:        stream,
		Body:          requestBodyForTarget(bodyInfo.body, model),
		Headers:       sanitizeNestedHeaders(request.Headers),
		Query:         cloneValues(request.Query),
		Alt:           request.Alt,
	}
}

func hostStatusMessage(model string, status int, body []byte) string {
	message := fmt.Sprintf("host model %s returned status %d", model, status)
	detail := strings.TrimSpace(string(body))
	if len(detail) > 512 {
		detail = detail[:512]
	}
	if detail != "" {
		message += ": " + detail
	}
	return message
}

func cooldownRouteError(route modelRoute, retryAfter time.Duration) error {
	seconds := int(math.Ceil(retryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	return newRouteError(http.StatusTooManyRequests, "model_route_cooldown", route.Alias, fmt.Sprintf("all candidates for model route %q are cooling down", route.Alias), fmt.Sprintf("retry after %d seconds", seconds))
}

// providerBusyError reports that every remaining candidate sits at a provider
// whose in-flight budget is spent. It is distinct from a cooldown: the targets
// are healthy, the upstream account is simply full, so the caller should retry
// shortly rather than treat the route as broken.
func providerBusyError(route modelRoute) error {
	return newRouteError(http.StatusTooManyRequests, "provider_inflight_limit", route.Alias, fmt.Sprintf("all candidates for model route %q are at a provider that reached its in-flight limit", route.Alias), "retry shortly")
}
