package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The dashboard's save payload is the contract between dashboard/configuration/tab.js
// and the management validate route. Keep this test in sync with the payload
// literal in saveConfiguration(); a field added on one side and not the other
// turns every save into a 400 with no obvious cause.
func TestModelRouterManagementAcceptsTheDashboardSavePayload(t *testing.T) {
	// Byte-for-byte the object saveConfiguration() sends: enabled, priority,
	// attempt_timeout_seconds, error_policy, routes, and the explicit null for
	// the legacy key.
	payload := `{
		"enabled": true,
		"priority": 100,
		"attempt_timeout_seconds": 45,
		"error_policy": {
			"default": {"action": "fallback", "cooldown_seconds": 30},
			"rules": [{"status": [429], "action": "fallback", "cooldown_seconds": 20}]
		},
		"routes": [
			{
				"alias": "fast",
				"strategy": "priority",
				"cooldown_seconds": 30,
				"targets": [
					{"model": "provider-a/model-x", "weight": 1},
					{"model": "provider-b/model-y", "weight": 1}
				]
			}
		],
		"model-routes": null
	}`

	response := validateModelRouterManagementConfig([]byte(payload))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("dashboard payload rejected: status=%d body=%s", response.StatusCode, response.Body)
	}
	var decoded map[string]any
	if err := json.Unmarshal(response.Body, &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if decoded["valid"] != true {
		t.Fatalf("valid = %v, body = %s", decoded["valid"], response.Body)
	}
}
