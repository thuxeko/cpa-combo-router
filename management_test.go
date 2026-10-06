package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestModelRouterABIAdvertisesConfigurationResource(t *testing.T) {
	resetModelRouterABIState(t)
	lifecycle, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte("routes: []\n")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handleModelRouterABIMethod(t.Context(), pluginabi.MethodPluginRegister, lifecycle); err != nil {
		t.Fatalf("plugin.register error = %v", err)
	}
	raw, err := handleModelRouterABIMethod(t.Context(), pluginabi.MethodManagementRegister, nil)
	if err != nil {
		t.Fatalf("management.register error = %v", err)
	}
	var envelope abiEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode management envelope: %v", err)
	}
	var management managementRegistrationResponse
	if err := json.Unmarshal(envelope.Result, &management); err != nil {
		t.Fatalf("decode management result: %v", err)
	}
	if len(management.Resources) != 1 || management.Resources[0].Path != "/config" || management.Resources[0].Menu != "Combo Router" {
		t.Fatalf("resources = %#v", management.Resources)
	}
	wantedRoutes := map[string]bool{
		http.MethodPost + " /plugins/combo-router/validate": false,
	}
	for _, route := range management.Routes {
		key := route.Method + " " + route.Path
		if _, wanted := wantedRoutes[key]; wanted {
			wantedRoutes[key] = true
		}
	}
	for route, found := range wantedRoutes {
		if !found {
			t.Fatalf("management routes missing %s: %#v", route, management.Routes)
		}
	}
	if len(management.Routes) != 1 {
		t.Fatalf("the fork advertises no usage routes, got %#v", management.Routes)
	}
}

func TestModelRouterManagementDashboardReusesCPAMCSessionAndTheme(t *testing.T) {
	resetModelRouterABIState(t)
	lifecycle, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte("routes: []\n")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handleModelRouterABIMethod(t.Context(), pluginabi.MethodPluginRegister, lifecycle); err != nil {
		t.Fatalf("plugin.register error = %v", err)
	}
	request, err := json.Marshal(managementRPCRequest{ManagementRequest: pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   comboRouterDashboardPath,
	}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := handleModelRouterABIMethod(t.Context(), pluginabi.MethodManagementHandle, request)
	if err != nil {
		t.Fatalf("management.handle error = %v", err)
	}
	var envelope abiEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	var response pluginapi.ManagementResponse
	if err := json.Unmarshal(envelope.Result, &response); err != nil {
		t.Fatalf("decode management response: %v", err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Headers.Get("Content-Type"), "text/html") {
		t.Fatalf("response status=%d headers=%v", response.StatusCode, response.Headers)
	}
	page := string(response.Body)
	for _, required := range []string{
		"<title>Combo Router</title>",
		"/v0/management/plugins/combo-router/config",
		"/v0/management/plugins/combo-router/validate",
		"/v0/management/api-keys",
		"/v1/models",
		"headers.Authorization='Bearer '+key",
		"'cli-proxy-auth'",
		"CPA_STORAGE_PREFIX='enc::v1::'",
		":root[data-host-theme=\"dark\"]",
		"new MutationObserver(refresh)",
		"class=\"auth-dock\" aria-labelledby=\"auth-title\" hidden",
		"if(managementKey())loadConfiguration()",
		"<select data-target-field=\"model\"",
		"data-target-field=\"weight\"",
		"data-target-field=\"cooldown_seconds\"",
		"data-target-help",
		"model+' (không khả dụng)'",
		"data-action=\"add-target\"",
		"role=\"tablist\" aria-label=\"Các mục của Combo Router\"",
		"id=\"configuration-tab\" class=\"page-tab\" type=\"button\" role=\"tab\" aria-selected=\"true\"",
		"id=\"policy-tab\" class=\"page-tab\" type=\"button\" role=\"tab\" aria-selected=\"false\"",
		"id=\"configuration-panel\" class=\"tab-panel\" role=\"tabpanel\"",
		"id=\"policy-panel\" class=\"tab-panel\" role=\"tabpanel\"",
		"id=\"configuration-actions\" class=\"configuration-actions-dock\" aria-hidden=\"true\" hidden inert",
		"#configuration-panel { padding-bottom: var(--configuration-actions-clearance); }",
		"bottom: calc(20px + var(--configuration-actions-clearance));",
		"baselineSnapshot:null",
		"function configurationSnapshot()",
		"return /^-?\\d+$/.test(trimmed)?{value:Number.parseInt(trimmed,10)}:{draft:raw}",
		"function refreshDirty()",
		"captureConfigurationBaseline();",
		"captureConfigurationBaseline(submittedSnapshot);",
		"if(state.busy||!state.dirty)return",
		"if(state.dirty)saveConfiguration()",
		"configurationActionsEl.inert=!value",
		"button.tabIndex=-1",
		"duration:value?280:220",
		"const hiddenTransform=reducedMotion?'none':'translateY(18px)'",
		".configuration-actions-dock { transform: none !important; }",
		"configurationActionsResizeObserver=new ResizeObserver(updateConfigurationActionsClearance)",
		"id=\"policy-rule-template\"",
		"id=\"policy-default-action\"",
		"id=\"policy-default-cooldown\"",
		"id=\"policy-add-rule\"",
		"id=\"attempt-timeout\"",
		"const POLICY_ACTIONS=[",
		"function builtinErrorPolicy()",
		"function normalizeErrorPolicy(config)",
		"function parseStatusList(text)",
		"function compactStatusList(statuses)",
		"function renderPolicy()",
		"function updatePolicyDefaultSummary()",
		"function serializeErrorPolicy()",
		"function validatePolicy()",
		"function initializePolicyEvents()",
		"@media (prefers-reduced-motion: reduce) {",
	} {
		if !strings.Contains(page, required) {
			t.Fatalf("dashboard missing %q", required)
		}
	}
	for _, required := range []string{
		"function updatePolicyFromInput(target)",
		"function policyRowFromRule(rule,row)",
		"function policyRuleIndexFrom(target)",
		"function updatePolicyWarnings()",
		"honor_retry_after",
		"max_cooldown_seconds",
		"backoff",
	} {
		if !strings.Contains(page, required) {
			t.Fatalf("dashboard missing policy editor piece %q", required)
		}
	}
	for _, forbidden := range []string{"http://", "https://", "innerHTML", "<input type=\"text\" data-target-field=\"model\"", "setDirty(true)"} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("dashboard contains forbidden text %q", forbidden)
		}
	}
	if csp := response.Headers.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "connect-src 'self'") {
		t.Fatalf("Content-Security-Policy = %q", csp)
	}
}

func TestModelRouterManagementValidationUsesPluginParser(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantText   string
	}{
		{
			name:       "valid",
			body:       `{"enabled":true,"routes":[{"alias":"auto","strategy":"priority","cooldown_seconds":60,"models":["provider-a/model","provider-b/model"]}]}`,
			wantStatus: http.StatusOK,
			wantText:   `"valid":true`,
		},
		{
			name:       "canonical weighted targets",
			body:       `{"enabled":true,"routes":[{"alias":"auto","strategy":"round-robin","cooldown_seconds":60,"targets":[{"model":"provider-a/model","weight":3},{"model":"provider-b/model"}]}]}`,
			wantStatus: http.StatusOK,
			wantText:   `"valid":true`,
		},
		{
			name:       "recursive target",
			body:       `{"enabled":true,"routes":[{"alias":"auto","strategy":"priority","cooldown_seconds":60,"models":["auto(high)"]}]}`,
			wantStatus: http.StatusBadRequest,
			wantText:   "đích không được trỏ vào alias của tuyến",
		},
		{
			name:       "empty target pool",
			body:       `{"enabled":true,"routes":[{"alias":"auto","strategy":"priority","cooldown_seconds":60,"models":[]}]}`,
			wantStatus: http.StatusBadRequest,
			wantText:   "cần ít nhất một model",
		},
		{
			name:       "unknown route field",
			body:       `{"enabled":true,"routes":[{"alias":"auto","models":["provider/model"],"unknown":true}]}`,
			wantStatus: http.StatusBadRequest,
			wantText:   "unknown field",
		},
		{
			name:       "both model schemas",
			body:       `{"enabled":true,"routes":[{"alias":"auto","models":["provider-a/model"],"targets":[{"model":"provider-b/model"}]}]}`,
			wantStatus: http.StatusBadRequest,
			wantText:   "đồng thời models và targets",
		},
		{
			name:       "multiple documents",
			body:       `{"enabled":true,"routes":[]} {"enabled":true,"routes":[]}`,
			wantStatus: http.StatusBadRequest,
			wantText:   "không hỗ trợ nhiều giá trị JSON",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := handleModelRouterManagement(nil, pluginapi.ManagementRequest{
				Method: http.MethodPost,
				Path:   comboRouterValidationPath,
				Body:   []byte(test.body),
			})
			if response.StatusCode != test.wantStatus || !strings.Contains(string(response.Body), test.wantText) {
				t.Fatalf("status=%d body=%s, want status=%d containing %q", response.StatusCode, response.Body, test.wantStatus, test.wantText)
			}
		})
	}
}

func TestModelRouterManagementRejectsUnsupportedMethod(t *testing.T) {
	response := handleModelRouterManagement(nil, pluginapi.ManagementRequest{Method: http.MethodDelete, Path: comboRouterDashboardPath})
	if response.StatusCode != http.StatusMethodNotAllowed || !strings.Contains(string(response.Body), "method_not_allowed") {
		t.Fatalf("response = %#v, body=%s", response, response.Body)
	}
}
