package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	comboRouterDashboardPath  = "/v0/resource/plugins/" + pluginID + "/config"
	comboRouterValidationPath = "/v0/management/plugins/" + pluginID + "/validate"
	maxManagementRequestBytes = 1 << 20
)

type managementRPCRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}

// modelRouterValidationRequest mirrors the object the dashboard POSTs to
// /validate, which is the same object it later PATCHes to /config. Keeping the
// two shapes identical is the point of the endpoint: whatever the panel
// validates is exactly what it saves. Adding a field to the dashboard payload
// without adding it here turns every save into a 400 (DisallowUnknownFields).
type modelRouterValidationRequest struct {
	Enabled               *bool            `json:"enabled"`
	Priority              *int             `json:"priority,omitempty"`
	Routes                []modelRouteYAML `json:"routes"`
	LegacyRoutes          json.RawMessage  `json:"model-routes,omitempty"`
	ErrorPolicy           *errorPolicyYAML `json:"error_policy,omitempty"`
	AttemptTimeoutSeconds *int             `json:"attempt_timeout_seconds,omitempty"`
}

func modelRouterManagementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{Method: http.MethodPost, Path: "/plugins/" + pluginID + "/validate", Description: "Validate Combo Router configuration before saving it."},
		},
		Resources: []resourceRoute{{
			Path:        "/config",
			Menu:        "Combo Router",
			Description: "Configure logical aliases and ordered model target pools.",
		}},
	}
}

func handleModelRouterManagement(plugin *comboRouterPlugin, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	path := strings.TrimRight(strings.TrimSpace(request.Path), "/")
	switch path {
	case comboRouterDashboardPath:
		if !strings.EqualFold(request.Method, http.MethodGet) {
			return modelRouterJSONResponse(http.StatusMethodNotAllowed, map[string]any{
				"error":   "method_not_allowed",
				"message": "dashboard only supports GET",
			})
		}
		if configDashboardError != nil {
			return modelRouterJSONResponse(http.StatusInternalServerError, map[string]any{
				"error":   "dashboard_render_failed",
				"message": configDashboardError.Error(),
			})
		}
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":            {"text/html; charset=utf-8"},
				"Cache-Control":           {"no-store"},
				"Content-Security-Policy": {"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'"},
				"Referrer-Policy":         {"no-referrer"},
				"X-Content-Type-Options":  {"nosniff"},
			},
			Body: append([]byte(nil), configDashboardHTML...),
		}
	case comboRouterValidationPath:
		if !strings.EqualFold(request.Method, http.MethodPost) {
			return modelRouterJSONResponse(http.StatusMethodNotAllowed, map[string]any{
				"error":   "method_not_allowed",
				"message": "validation only supports POST",
			})
		}
		return validateModelRouterManagementConfig(request.Body)
	default:
		return modelRouterJSONResponse(http.StatusNotFound, map[string]any{
			"error":   "not_found",
			"message": "management resource not found",
		})
	}
}

func validateModelRouterManagementConfig(body []byte) pluginapi.ManagementResponse {
	if len(body) > maxManagementRequestBytes {
		return modelRouterJSONResponse(http.StatusRequestEntityTooLarge, map[string]any{
			"valid":   false,
			"error":   "request_too_large",
			"message": "configuration exceeds the 1 MiB validation limit",
		})
	}
	var request modelRouterValidationRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return modelRouterJSONResponse(http.StatusBadRequest, map[string]any{
			"valid":   false,
			"error":   "invalid_request",
			"message": err.Error(),
		})
	}
	if err := ensureModelRouterJSONEOF(decoder); err != nil {
		return modelRouterJSONResponse(http.StatusBadRequest, map[string]any{
			"valid":   false,
			"error":   "invalid_request",
			"message": err.Error(),
		})
	}
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	// The panel always sends model-routes as an explicit null, so only a real
	// array counts as "the caller used the legacy key". Passing it through means
	// a payload that sets both keys fails here with the same message the config
	// loader would give, instead of validating clean and breaking on PATCH.
	var legacyRoutes *[]modelRouteYAML
	if trimmed := bytes.TrimSpace(request.LegacyRoutes); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		var parsed []modelRouteYAML
		if err := json.Unmarshal(trimmed, &parsed); err != nil {
			return modelRouterJSONResponse(http.StatusBadRequest, map[string]any{
				"valid":   false,
				"error":   "invalid_request",
				"message": "model-routes: " + err.Error(),
			})
		}
		legacyRoutes = &parsed
	}
	priority := 0
	if request.Priority != nil {
		priority = *request.Priority
	}
	wire := struct {
		Enabled               bool              `yaml:"enabled"`
		Priority              int               `yaml:"priority,omitempty"`
		Routes                []modelRouteYAML  `yaml:"routes"`
		LegacyRoutes          *[]modelRouteYAML `yaml:"model-routes,omitempty"`
		ErrorPolicy           *errorPolicyYAML  `yaml:"error_policy,omitempty"`
		AttemptTimeoutSeconds *int              `yaml:"attempt_timeout_seconds,omitempty"`
	}{Enabled: enabled, Priority: priority, Routes: request.Routes, LegacyRoutes: legacyRoutes, ErrorPolicy: request.ErrorPolicy, AttemptTimeoutSeconds: request.AttemptTimeoutSeconds}
	raw, err := yaml.Marshal(wire)
	if err != nil {
		return modelRouterJSONResponse(http.StatusInternalServerError, map[string]any{
			"valid":   false,
			"error":   "validation_failed",
			"message": err.Error(),
		})
	}
	if _, err := decodeRouterConfig(raw); err != nil {
		return modelRouterJSONResponse(http.StatusBadRequest, map[string]any{
			"valid":   false,
			"error":   "invalid_config",
			"message": err.Error(),
		})
	}
	response := map[string]any{
		"valid":       true,
		"route_count": len(request.Routes),
	}
	// Warnings describe a configuration that is valid but probably not what was
	// meant. They are reported next to success so the panel can ask for
	// confirmation instead of refusing the save.
	policy, err := buildErrorPolicy(request.ErrorPolicy)
	if err != nil {
		return modelRouterJSONResponse(http.StatusBadRequest, map[string]any{
			"valid":   false,
			"error":   "invalid_config",
			"message": err.Error(),
		})
	}
	if warnings := policyWarnings(policy); len(warnings) > 0 {
		response["warnings"] = warnings
	}
	return modelRouterJSONResponse(http.StatusOK, response)
}

func ensureModelRouterJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("không hỗ trợ nhiều giá trị JSON")
		}
		return err
	}
	return nil
}

func modelRouterJSONResponse(status int, value any) pluginapi.ManagementResponse {
	body, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte(`{"error":"response_encode_failed"}`)
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":           {"application/json; charset=utf-8"},
			"Cache-Control":          {"no-store"},
			"X-Content-Type-Options": {"nosniff"},
		},
		Body: body,
	}
}
