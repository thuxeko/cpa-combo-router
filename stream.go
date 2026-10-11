package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func (p *comboRouterPlugin) startStream(request executorRPCRequest, host modelHost) ([]byte, error) {
	streamID := strings.TrimSpace(request.StreamID)
	if streamID == "" {
		return errorEnvelope("executor_error", "stream_id is required for executor.execute_stream", 0), nil
	}
	go func() {
		errorMessage := ""
		defer func() {
			if recovered := recover(); recovered != nil {
				errorMessage = fmt.Sprintf("stream orchestration panic: %v", recovered)
			}
			host.ClosePluginStream(streamID, errorMessage)
		}()
		if err := p.executeStreamWithHost(context.Background(), request.ExecutorRequest, streamID, host); err != nil {
			errorMessage = err.Error()
		}
	}()
	return okEnvelope(map[string]any{"headers": http.Header{"Content-Type": []string{"text/event-stream"}}})
}

func (p *comboRouterPlugin) executeStreamWithHost(_ context.Context, request pluginapi.ExecutorRequest, pluginStreamID string, host modelHost) error {
	if p == nil || host == nil {
		return statusError{status: http.StatusBadGateway, message: "model router host callback is unavailable"}
	}
	route, ok := p.matchingRoute(request.Model)
	if !ok {
		return statusError{status: http.StatusBadGateway, message: "no model route matched executor stream request"}
	}
	requestedModel := strings.TrimSpace(request.Model)
	bodyInfo := bodyForExecution(request)
	attemptTimeout := time.Duration(p.config.AttemptTimeoutSeconds) * time.Second
	var lastErr error
	attempted := make(map[string]struct{}, len(route.Targets))
	for attempt := 0; attempt < len(route.Targets); attempt++ {
		selection := p.runtime.SelectExcluding(route, attempted)
		if selection.allCooling {
			return cooldownRouteError(route, selection.retryAfter)
		}
		if selection.model == "" {
			if selection.providerBlocked {
				return providerBusyError(route)
			}
			break
		}
		attempted[routeKey(selection.model)] = struct{}{}
		target := targetModel(requestedModel, selection.model)
		outcome := forwardStreamAttempt(request, bodyInfo, target, requestedModel, pluginStreamID, host, attemptTimeout, p.config, route, selection.model)
		// The stream is fully consumed by the time this returns, so the
		// provider is no longer occupied and its slot goes back.
		p.runtime.Release(selection.model)
		if outcome.err == nil {
			p.runtime.MarkSuccess(route, selection.model)
			return nil
		}
		// A stream that already emitted a payload cannot be spliced onto another
		// target: the caller has seen the beginning of one answer.
		if outcome.receivedPayload || !outcome.decision.shouldFallback() {
			p.runtime.ReleaseProbe(route, selection.model)
			return outcome.err
		}
		if outcome.decision.penalises() {
			p.runtime.MarkFailure(route, selection.model, outcome.decision.Cooldown)
		} else {
			p.runtime.ReleaseProbe(route, selection.model)
		}
		lastErr = outcome.err
	}
	detail := ""
	if lastErr != nil {
		detail = lastErr.Error()
	}
	return newRouteError(http.StatusServiceUnavailable, "model_route_unavailable", route.Alias, fmt.Sprintf("no available candidate for model route %q", route.Alias), detail)
}

type streamAttemptOutcome struct {
	receivedPayload bool
	decision        policyOutcome
	err             error
}

// streamStartOutcome carries the result of an asynchronous StartStream call.
// streamID is captured separately from response so an abandoned attempt can
// still close the upstream stream once the call finally reports it.
type streamStartOutcome struct {
	response pluginapi.HostModelStreamResponse
	streamID string
	err      error
}

// abandonedStreamGracePeriod bounds how long we wait for an abandoned
// StartStream call to report its stream id before giving up on closing it.
const abandonedStreamGracePeriod = 2 * time.Minute

// closeAbandonedStream closes a host model stream whose attempt was already
// abandoned because the first-byte timeout fired before StartStream returned.
// Without this the upstream keeps generating into a stream nobody reads,
// burning provider tokens and, on a per-token provider, money.
func closeAbandonedStream(host modelHost, started <-chan streamStartOutcome) {
	go func() {
		select {
		case result := <-started:
			if streamID := strings.TrimSpace(result.streamID); streamID != "" {
				_ = host.CloseStream(streamID)
			}
		case <-time.After(abandonedStreamGracePeriod):
		}
	}()
}

// forwardStreamAttempt runs one streaming attempt. The attempt timeout covers
// only the wait for the FIRST byte: once the upstream has started answering,
// the router no longer cuts it off, because a partial answer cannot be handed to
// another target.
//
// A heartbeat runs for the whole attempt. CPA holds the response headers until
// the first chunk arrives, so without it the client sees dead air — no headers,
// no bytes — for the entire first-byte wait, which is what turns a slow target
// into a client-side abort (499). The heartbeat makes CPA commit the headers and
// start streaming immediately, so the caller's budget survives the wait.
func forwardStreamAttempt(request pluginapi.ExecutorRequest, bodyInfo executionBody, target, requestedModel, pluginStreamID string, host modelHost, attemptTimeout time.Duration, cfg routerConfig, route modelRoute, selected string) streamAttemptOutcome {
	heartbeat := startStreamHeartbeat(host, pluginStreamID, bodyInfo.responseProtocol)
	defer heartbeat.Stop()

	started := make(chan streamStartOutcome, 1)
	go func() {
		response, err := host.StartStream(hostRequest(request, bodyInfo, target, true))
		started <- streamStartOutcome{response: response, streamID: response.StreamID, err: err}
	}()

	var response pluginapi.HostModelStreamResponse
	var err error
	if attemptTimeout > 0 {
		timer := time.NewTimer(attemptTimeout)
		select {
		case result := <-started:
			timer.Stop()
			response, err = result.response, result.err
		case <-timer.C:
			// The attempt is abandoned here, but the host call is not: it may
			// still open a stream. Close that stream when it reports one,
			// otherwise the provider generates for nobody.
			closeAbandonedStream(host, started)
			err = attemptTimeoutError{target: target, limit: attemptTimeout}
		}
	} else {
		result := <-started
		response, err = result.response, result.err
	}

	// The upstream's Retry-After is only visible when it rejected before the
	// stream opened; a mid-stream failure has to fall back to the policy number.
	retryAfter := parseRetryAfter(response.Headers)
	consecutive := 1
	if err != nil {
		if status := response.StatusCode; status > 0 {
			err = statusError{status: status, message: err.Error()}
		}
		return streamAttemptOutcome{err: err, decision: decideAttempt(err, cfg, route, selected, retryAfter, consecutive)}
	}
	if response.StatusCode >= 400 {
		_ = host.CloseStream(response.StreamID)
		statusErr := statusError{status: response.StatusCode, message: fmt.Sprintf("host model %s stream returned status %d", target, response.StatusCode)}
		return streamAttemptOutcome{err: statusErr, decision: decideAttempt(statusErr, cfg, route, selected, retryAfter, consecutive)}
	}
	if strings.TrimSpace(response.StreamID) == "" {
		statusErr := statusError{status: http.StatusBadGateway, message: "host model stream returned an empty stream id"}
		return streamAttemptOutcome{err: statusErr, decision: decideAttempt(statusErr, cfg, route, selected, retryAfter, consecutive)}
	}
	defer func() { _ = host.CloseStream(response.StreamID) }()
	rewriter := newStreamModelRewriter(requestedModel)
	receivedPayload := false
	for {
		chunk, err := host.ReadStream(response.StreamID)
		if err != nil {
			return streamAttemptOutcome{receivedPayload: receivedPayload, err: err, decision: decideAttempt(err, cfg, route, selected, retryAfter, consecutive)}
		}
		if strings.TrimSpace(chunk.Error) != "" {
			chunkErr := statusError{status: statusFromError(fmt.Errorf("%s", chunk.Error)), message: chunk.Error}
			return streamAttemptOutcome{receivedPayload: receivedPayload, err: chunkErr, decision: decideAttempt(chunkErr, cfg, route, selected, retryAfter, consecutive)}
		}
		if len(chunk.Payload) > 0 {
			receivedPayload = true
			if rewritten := rewriter.Rewrite(chunk.Payload); len(rewritten) > 0 {
				if err := host.Emit(pluginStreamID, rewritten); err != nil {
					return streamAttemptOutcome{receivedPayload: true, err: err, decision: policyOutcome{Action: errorActionStop}}
				}
			}
		}
		if chunk.Done {
			if tail := rewriter.Finish(); len(tail) > 0 {
				if err := host.Emit(pluginStreamID, tail); err != nil {
					return streamAttemptOutcome{receivedPayload: receivedPayload, err: err, decision: policyOutcome{Action: errorActionStop}}
				}
			}
			return streamAttemptOutcome{receivedPayload: receivedPayload, err: nil}
		}
	}
}
