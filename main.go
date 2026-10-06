package main

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	pluginID   = "combo-router"
	pluginName = "Combo Router"
)

// pluginVersion is what the metadata reports. Note that CPA derives the plugin
// version it logs from the .so FILE NAME, so the file must be named
// combo-router-<version>.so to match.
var pluginVersion = "1.0.0"

// comboRouterPlugin keeps only what routing needs: the parsed configuration and
// the live cooldown/circuit state. The usage, pricing and dashboard stores of
// 0.5.4 were removed on purpose — the separate usage-statistics plugin already
// records alias traffic, and a second SQLite writer on a two-core host costs
// more than it explains.
type comboRouterPlugin struct {
	config  routerConfig
	runtime *routeRuntime
}

var (
	_ pluginapi.ModelRouter      = (*comboRouterPlugin)(nil)
	_ pluginapi.ModelRegistrar   = (*comboRouterPlugin)(nil)
	_ pluginapi.ProviderExecutor = (*comboRouterPlugin)(nil)
)

func newComboRouterPlugin(configYAML []byte, previous *comboRouterPlugin) (*comboRouterPlugin, pluginapi.Metadata, error) {
	cfg, err := decodeRouterConfig(configYAML)
	if err != nil {
		return nil, pluginapi.Metadata{}, err
	}
	var runtime *routeRuntime
	if previous != nil {
		// A reconfigure keeps the cooldowns of every route whose shape did not
		// change, so editing one route does not clear another's circuit.
		runtime = previous.runtime.Clone(cfg.Routes)
	} else {
		runtime = newRouteRuntime(nil)
		runtime.Sync(cfg.Routes)
		// Restore cooldowns written by the previous process, so a restart does
		// not immediately send traffic back to a target that was failing.
		runtime.Load(cfg.StatePath)
	}
	// Load only adopts the path when the file already exists, so on a first run
	// persistPath would stay empty and no cooldown would ever be written. Set it
	// explicitly; an empty StatePath still means "do not persist".
	runtime.SetPersistPath(cfg.StatePath)
	// Live traffic gets jitter; tests build the runtime directly and stay
	// deterministic.
	runtime.EnableJitter()
	plugin := &comboRouterPlugin{config: cfg, runtime: runtime}
	metadata := pluginapi.Metadata{
		Name:             pluginName,
		Version:          pluginVersion,
		Author:           "thuxeko",
		GitHubRepository: "https://github.com/thuxeko/cpa-combo-router",
		ConfigFields: []pluginapi.ConfigField{
			{Name: "routes", Type: pluginapi.ConfigFieldTypeArray, Description: "Logical model aliases backed by priority or weighted round-robin target pools."},
			{Name: "error_policy", Type: pluginapi.ConfigFieldTypeObject, Description: "What to do for each failure status: action (fallback, stop, fallback_no_penalty), cooldown_seconds, max_cooldown_seconds, honor_retry_after, backoff."},
			{Name: "attempt_timeout_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "How long one target attempt may wait for its first byte before the router tries the next target (0 disables the cap)."},
			{Name: "fallback", Type: pluginapi.ConfigFieldTypeObject, Description: "Legacy status lists kept for compatibility: fallback_on_status, no_fallback_on_status."},
			{Name: "state_path", Type: pluginapi.ConfigFieldTypeString, Description: "Where live cooldown state is persisted across restarts; defaults to combo-router-state.json in the CPA plugins directory."},
		},
	}
	return plugin, metadata, nil
}

func (p *comboRouterPlugin) Identifier() string { return pluginID }

func (p *comboRouterPlugin) Execute(_ context.Context, _ pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	return pluginapi.ExecutorResponse{}, fmt.Errorf("%s execution requires the native host callback context", pluginID)
}

func (p *comboRouterPlugin) ExecuteStream(_ context.Context, _ pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	return pluginapi.ExecutorStreamResponse{}, fmt.Errorf("%s streaming requires the native stream bridge", pluginID)
}

func (p *comboRouterPlugin) CountTokens(_ context.Context, _ pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	return pluginapi.ExecutorResponse{}, newRouteError(501, "model_route_count_tokens_unsupported", "", "token counting for routed aliases is not supported by the current CLIProxyAPI plugin ABI", "")
}

func (p *comboRouterPlugin) HttpRequest(_ context.Context, _ pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	return pluginapi.ExecutorHTTPResponse{}, fmt.Errorf("%s does not implement executor.http_request", pluginID)
}

func main() {}
