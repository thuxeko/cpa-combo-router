package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	routeStrategyPriority   = "priority"
	routeStrategyRoundRobin = "round-robin"
	defaultCooldownSeconds  = 60
	defaultTargetWeight     = 1
	maxTargetWeight         = 1_000_000
	// defaultAttemptTimeoutSeconds cuts one target attempt that produced no
	// first byte yet. It exists because the plugin otherwise waits forever on a
	// wedged target: the client gives up first, which surfaces as a 499 and no
	// failover. Set attempt_timeout_seconds to 0 to wait indefinitely.
	defaultAttemptTimeoutSeconds = 45
	maxAttemptTimeoutSeconds     = 3600
)

// defaultFallbackOnStatus is the status set used when fallback_on_status is not
// configured, and it matches the 0.5.0 behaviour: target availability and quota
// errors (401, 402, 403, 404, 408, 429) and server-side faults (500, 502, 503,
// 504). Statuses that describe the rejected request rather than the target stay
// out of the set, so an invalid request does not cool a healthy route.
var defaultFallbackOnStatus = []int{
	401, 402, 403, 404, 408, 429, 500, 502, 503, 504,
}

type modelTarget struct {
	Model  string
	Weight int
	// CooldownSeconds overrides the route cooldown for this target alone. Zero
	// means "use the route value".
	CooldownSeconds int
}

type modelRoute struct {
	Alias           string
	Strategy        string
	CooldownSeconds int
	Targets         []modelTarget
}

type modelTargetYAML struct {
	Model           string `yaml:"model" json:"model"`
	Weight          *int   `yaml:"weight,omitempty" json:"weight,omitempty"`
	CooldownSeconds *int   `yaml:"cooldown_seconds,omitempty" json:"cooldown_seconds,omitempty"`
}

type modelRouteYAML struct {
	Alias                 string             `yaml:"alias" json:"alias"`
	Strategy              string             `yaml:"strategy,omitempty" json:"strategy,omitempty"`
	CooldownSeconds       *int               `yaml:"cooldown_seconds,omitempty" json:"cooldown_seconds,omitempty"`
	LegacyCooldownSeconds *int               `yaml:"cooldown-seconds,omitempty" json:"cooldown-seconds,omitempty"`
	Models                *[]string          `yaml:"models,omitempty" json:"models,omitempty"`
	Targets               *[]modelTargetYAML `yaml:"targets,omitempty" json:"targets,omitempty"`
}

type routerConfig struct {
	Enabled            bool
	StatePath          string
	Routes             []modelRoute
	FallbackOnStatus   []int
	NoFallbackOnStatus []int
	// ErrorPolicy decides the action and the cooldown for each failure status.
	ErrorPolicy errorPolicy
	// ErrorPolicyConfigured reports whether the configuration wrote its own
	// error_policy. When false the plugin keeps the pre-0.5.4 behaviour driven
	// by fallback_on_status, so an untouched configuration cannot change
	// meaning under an upgrade.
	ErrorPolicyConfigured bool
	// AttemptTimeoutSeconds caps one target attempt that has not produced a
	// first byte yet. Zero disables the cap.
	AttemptTimeoutSeconds int
}

// fallbackPolicy resolves the configured status lists into lookup sets. An
// omitted or empty fallback_on_status inherits defaultFallbackOnStatus, and
// no_fallback_on_status then removes statuses from whichever set applies.
func (c routerConfig) fallbackPolicy() fallbackPolicy {
	onStatus := c.FallbackOnStatus
	if len(onStatus) == 0 {
		onStatus = defaultFallbackOnStatus
	}
	return newFallbackPolicy(onStatus, c.NoFallbackOnStatus)
}

type fallbackConfigYAML struct {
	FallbackOnStatus                   *[]int `yaml:"fallback_on_status,omitempty"`
	NoFallbackOnStatus                 *[]int `yaml:"no_fallback_on_status,omitempty"`
	StreamFallbackBeforeFirstChunkOnly *bool  `yaml:"stream_fallback_before_first_chunk_only,omitempty"`
}

type routerConfigYAML struct {
	Enabled        *bool               `yaml:"enabled,omitempty"`
	Priority       int                 `yaml:"priority,omitempty"`
	Store          yaml.Node           `yaml:"store,omitempty"`
	StatePath      string              `yaml:"state_path,omitempty"`
	Routes         *[]modelRouteYAML   `yaml:"routes,omitempty"`
	LegacyRoutes   *[]modelRouteYAML   `yaml:"model-routes,omitempty"`
	Fallback       *fallbackConfigYAML `yaml:"fallback,omitempty"`
	ErrorPolicy    *errorPolicyYAML    `yaml:"error_policy,omitempty"`
	AttemptTimeout *int                `yaml:"attempt_timeout_seconds,omitempty"`
}

func decodeRouterConfig(raw []byte) (routerConfig, error) {
	wire := routerConfigYAML{}
	if len(bytes.TrimSpace(raw)) > 0 {
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		decoder.KnownFields(true)
		if err := decoder.Decode(&wire); err != nil {
			return routerConfig{}, fmt.Errorf("giải mã cấu hình %s: %w", pluginID, err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			if err == nil {
				return routerConfig{}, fmt.Errorf("giải mã cấu hình %s: không hỗ trợ nhiều tài liệu YAML", pluginID)
			}
			return routerConfig{}, fmt.Errorf("giải mã cấu hình %s: %w", pluginID, err)
		}
	}
	if wire.Routes != nil && wire.LegacyRoutes != nil {
		return routerConfig{}, errors.New("cấu hình combo-router không được chứa đồng thời routes và model-routes")
	}
	statePath := strings.TrimSpace(wire.StatePath)
	if statePath == "" {
		statePath = defaultStatePath(defaultDataPathResolver())
	}
	absoluteStatePath, err := filepath.Abs(filepath.Clean(statePath))
	if err != nil {
		return routerConfig{}, fmt.Errorf("phân giải state_path: %w", err)
	}
	enabled := true
	if wire.Enabled != nil {
		enabled = *wire.Enabled
	}
	var routeWires []modelRouteYAML
	if wire.Routes != nil {
		routeWires = *wire.Routes
	} else if wire.LegacyRoutes != nil {
		routeWires = *wire.LegacyRoutes
	}
	routes := make([]modelRoute, 0, len(routeWires))
	aliases := make(map[string]int, len(routeWires))
	for index, item := range routeWires {
		if item.CooldownSeconds != nil && item.LegacyCooldownSeconds != nil {
			return routerConfig{}, fmt.Errorf("routes[%d] %q: không được chứa đồng thời cooldown_seconds và cooldown-seconds", index, strings.TrimSpace(item.Alias))
		}
		cooldown := 0
		if item.CooldownSeconds != nil {
			cooldown = *item.CooldownSeconds
		} else if item.LegacyCooldownSeconds != nil {
			cooldown = *item.LegacyCooldownSeconds
		}
		if item.Models != nil && item.Targets != nil {
			return routerConfig{}, fmt.Errorf("routes[%d] %q: không được chứa đồng thời models và targets", index, strings.TrimSpace(item.Alias))
		}
		targets := []modelTarget(nil)
		if item.Targets != nil {
			targets = normalizeTargetList(*item.Targets)
		} else if item.Models != nil {
			targets = normalizeLegacyTargetList(*item.Models)
		}
		route := modelRoute{
			Alias:           strings.TrimSpace(item.Alias),
			Strategy:        strings.ToLower(strings.TrimSpace(item.Strategy)),
			CooldownSeconds: cooldown,
			Targets:         targets,
		}
		if route.Strategy == "" {
			route.Strategy = routeStrategyPriority
		}
		if route.CooldownSeconds == 0 {
			route.CooldownSeconds = defaultCooldownSeconds
		}
		if err := validateRoute(route, index, aliases); err != nil {
			return routerConfig{}, err
		}
		aliases[routeKey(route.Alias)] = index
		routes = append(routes, route)
	}
	for routeIndex, route := range routes {
		for targetIndex, target := range route.Targets {
			base, _, _ := splitThinkingSuffix(target.Model)
			if aliasIndex, exists := aliases[routeKey(base)]; exists {
				return routerConfig{}, fmt.Errorf("routes[%d] %q targets[%d] %q: đích không được trỏ vào alias của tuyến ở vị trí %d", routeIndex, route.Alias, targetIndex, target.Model, aliasIndex)
			}
		}
	}
	var fallbackOnStatus []int
	var noFallbackOnStatus []int
	if wire.Fallback != nil {
		if wire.Fallback.StreamFallbackBeforeFirstChunkOnly != nil && !*wire.Fallback.StreamFallbackBeforeFirstChunkOnly {
			return routerConfig{}, errors.New("fallback.stream_fallback_before_first_chunk_only phải là true: luồng đã phát dữ liệu thì không thể nối sang đích khác")
		}
		if wire.Fallback.FallbackOnStatus != nil {
			fallbackOnStatus = append([]int(nil), (*wire.Fallback.FallbackOnStatus)...)
		}
		if wire.Fallback.NoFallbackOnStatus != nil {
			noFallbackOnStatus = append([]int(nil), (*wire.Fallback.NoFallbackOnStatus)...)
		}
	}
	// Only the lists the configuration supplies are validated. The default set is
	// expanded when the policy is built, so an exclusion may name a status it does
	// not list, and an omitted or empty fallback_on_status keeps the defaults.
	if err := validateFallbackStatuses(fallbackOnStatus, noFallbackOnStatus); err != nil {
		return routerConfig{}, err
	}
	if !enabled {
		routes = nil
	}
	policy, err := buildErrorPolicy(wire.ErrorPolicy)
	if err != nil {
		return routerConfig{}, err
	}
	errorPolicyConfigured := wire.ErrorPolicy != nil && (wire.ErrorPolicy.Default != nil || (wire.ErrorPolicy.Rules != nil && len(*wire.ErrorPolicy.Rules) > 0))
	attemptTimeout := defaultAttemptTimeoutSeconds
	if wire.AttemptTimeout != nil {
		attemptTimeout = *wire.AttemptTimeout
	}
	if attemptTimeout < 0 || attemptTimeout > maxAttemptTimeoutSeconds {
		return routerConfig{}, fmt.Errorf("attempt_timeout_seconds phải nằm trong khoảng 0 đến %d", maxAttemptTimeoutSeconds)
	}
	return routerConfig{
		Enabled:               enabled,
		StatePath:             absoluteStatePath,
		Routes:                routes,
		FallbackOnStatus:      fallbackOnStatus,
		NoFallbackOnStatus:    noFallbackOnStatus,
		ErrorPolicy:           policy,
		ErrorPolicyConfigured: errorPolicyConfigured,
		AttemptTimeoutSeconds: attemptTimeout,
	}, nil
}

func validateFallbackStatuses(fallbackOnStatus, noFallbackOnStatus []int) error {
	listed := make(map[int]struct{}, len(fallbackOnStatus))
	for _, status := range fallbackOnStatus {
		if status < 100 || status > 599 {
			return fmt.Errorf("fallback_on_status chứa mã HTTP không hợp lệ %d (phải trong khoảng 100-599)", status)
		}
		listed[status] = struct{}{}
	}
	for _, status := range noFallbackOnStatus {
		if status < 100 || status > 599 {
			return fmt.Errorf("no_fallback_on_status chứa mã HTTP không hợp lệ %d (phải trong khoảng 100-599)", status)
		}
		if _, duplicate := listed[status]; duplicate {
			return fmt.Errorf("mã HTTP %d xuất hiện ở cả fallback_on_status và no_fallback_on_status", status)
		}
	}
	return nil
}

func validateRoute(route modelRoute, index int, aliases map[string]int) error {
	if route.Alias == "" {
		return fmt.Errorf("routes[%d]: cần có alias", index)
	}
	if _, _, hasSuffix := splitThinkingSuffix(route.Alias); hasSuffix {
		return fmt.Errorf("routes[%d] %q: alias không được chứa hậu tố thinking", index, route.Alias)
	}
	if previous, duplicate := aliases[routeKey(route.Alias)]; duplicate {
		return fmt.Errorf("routes[%d] %q: alias bị trùng (cũng ở vị trí %d)", index, route.Alias, previous)
	}
	if route.Strategy != routeStrategyPriority && route.Strategy != routeStrategyRoundRobin {
		return fmt.Errorf("routes[%d] %q: cách chọn đích phải là %q hoặc %q", index, route.Alias, routeStrategyPriority, routeStrategyRoundRobin)
	}
	if route.CooldownSeconds < 0 {
		return fmt.Errorf("routes[%d] %q: cooldown_seconds phải >= 0", index, route.Alias)
	}
	if len(route.Targets) == 0 {
		return fmt.Errorf("routes[%d] %q: cần ít nhất một model", index, route.Alias)
	}
	seen := make(map[string]int, len(route.Targets))
	for targetIndex, target := range route.Targets {
		model := strings.TrimSpace(target.Model)
		if model == "" {
			return fmt.Errorf("routes[%d] %q targets[%d]: cần có model", index, route.Alias, targetIndex)
		}
		if target.Weight < 1 || target.Weight > maxTargetWeight {
			return fmt.Errorf("routes[%d] %q targets[%d] %q: trọng số phải trong khoảng 1 đến %d", index, route.Alias, targetIndex, model, maxTargetWeight)
		}
		if target.CooldownSeconds < 0 || target.CooldownSeconds > hardMaxCooldownSeconds {
			return fmt.Errorf("routes[%d] %q targets[%d] %q: cooldown_seconds phải trong khoảng 0 đến %d", index, route.Alias, targetIndex, model, hardMaxCooldownSeconds)
		}
		key := routeKey(model)
		if previous, duplicate := seen[key]; duplicate {
			return fmt.Errorf("routes[%d] %q targets[%d] %q: model bị trùng (cũng ở vị trí %d)", index, route.Alias, targetIndex, model, previous)
		}
		seen[key] = targetIndex
	}
	return nil
}

func normalizeModelList(models []string) []string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		if model = strings.TrimSpace(model); model != "" {
			out = append(out, model)
		}
	}
	return out
}

func normalizeLegacyTargetList(models []string) []modelTarget {
	normalized := normalizeModelList(models)
	targets := make([]modelTarget, 0, len(normalized))
	for _, model := range normalized {
		targets = append(targets, modelTarget{Model: model, Weight: defaultTargetWeight})
	}
	return targets
}

func normalizeTargetList(targets []modelTargetYAML) []modelTarget {
	out := make([]modelTarget, 0, len(targets))
	for _, target := range targets {
		weight := defaultTargetWeight
		if target.Weight != nil {
			weight = *target.Weight
		}
		cooldown := 0
		if target.CooldownSeconds != nil {
			cooldown = *target.CooldownSeconds
		}
		out = append(out, modelTarget{Model: strings.TrimSpace(target.Model), Weight: weight, CooldownSeconds: cooldown})
	}
	return out
}

func routeKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func routeSignature(route modelRoute) string {
	var builder strings.Builder
	builder.WriteString(routeKey(route.Alias))
	builder.WriteByte('\n')
	builder.WriteString(route.Strategy)
	builder.WriteByte('\n')
	builder.WriteString(strconv.Itoa(route.CooldownSeconds))
	for _, target := range route.Targets {
		builder.WriteByte('\n')
		builder.WriteString(strings.TrimSpace(target.Model))
		builder.WriteByte('\n')
		builder.WriteString(strconv.Itoa(target.Weight))
		builder.WriteByte('\n')
		builder.WriteString(strconv.Itoa(target.CooldownSeconds))
	}
	return builder.String()
}

func splitThinkingSuffix(model string) (string, string, bool) {
	model = strings.TrimSpace(model)
	open := strings.LastIndex(model, "(")
	if open <= 0 || !strings.HasSuffix(model, ")") {
		return model, "", false
	}
	base := strings.TrimSpace(model[:open])
	suffix := strings.TrimSpace(model[open+1 : len(model)-1])
	if base == "" || suffix == "" {
		return model, "", false
	}
	return base, suffix, true
}

func targetModel(requested, target string) string {
	target = strings.TrimSpace(target)
	if _, _, hasSuffix := splitThinkingSuffix(target); hasSuffix {
		return target
	}
	_, suffix, hasSuffix := splitThinkingSuffix(requested)
	if !hasSuffix {
		return target
	}
	return fmt.Sprintf("%s(%s)", target, suffix)
}
