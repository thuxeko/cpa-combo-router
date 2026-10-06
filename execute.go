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
	attemptTimeout := time.Duration(cfg.AttemptTimeoutSeconds) * time.Second
	var lastErr error
	attempted := make(map[string]struct{}, len(route.Targets))
	for attempt := 0; attempt < len(route.Targets); attempt++ {
		selection := p.runtime.SelectExcluding(route, attempted)
		if selection.allCooling {
			return pluginapi.ExecutorResponse{}, cooldownRouteError(route, selection.retryAfter)
		}
		if selection.model == "" {
			break
		}
		attempted[routeKey(selection.model)] = struct{}{}
		target := targetModel(requestedModel, selection.model)
		response, err := executeWithAttemptTimeout(host, hostRequest(request, bodyInfo, target, false), attemptTimeout)
		status := response.StatusCode
		if status == 0 && err == nil {
			status = http.StatusOK
		}
		if status == 0 && err != nil {
			status = statusFromError(err)
		}
		if err == nil && status >= 200 && status < 300 {
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
		// The upstream's own Retry-After is more accurate than any configured
		// number, so it is read here and handed to the policy.
		retryAfter := parseRetryAfter(response.Headers)
		consecutive := p.runtime.FailureCount(route, selection.model) + 1
		outcome := decideAttempt(err, cfg, route, selection.model, retryAfter, consecutive)
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
	detail := ""
	if lastErr != nil {
		detail = lastErr.Error()
	}
	return pluginapi.ExecutorResponse{}, newRouteError(http.StatusServiceUnavailable, "model_route_unavailable", route.Alias, fmt.Sprintf("no available candidate for model route %q", route.Alias), detail)
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
