package main

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type routeRuntime struct {
	mu     sync.Mutex
	now    func() time.Time
	states map[string]*routeState
	// persistPath is where cooldown state survives a restart. Empty disables it.
	persistPath string
	// jitter spreads expiry so a batch of targets does not return to rotation on
	// the same instant.
	jitter func(time.Duration) time.Duration
}

type routeState struct {
	signature string
	cursor    int64
	cooldowns map[string]time.Time
	// failures counts consecutive failures per target. It feeds the exponential
	// backoff and opens the circuit once the target has failed repeatedly.
	failures map[string]int
	// probing holds the targets whose cooldown expired after repeated failures
	// and that currently have one request out testing them. Only one request may
	// probe a target at a time: a recovered target should be discovered, not
	// stampeded.
	probing map[string]struct{}
}

type routeSelection struct {
	model      string
	allCooling bool
	retryAfter time.Duration
}

// circuitOpenAfter is how many consecutive failures make a target "open". Past
// this point the target is only re-entered by a single probe.
const circuitOpenAfter = 3

// statePersistVersion guards the on-disk shape so a future change can be
// recognised and discarded instead of misread.
const statePersistVersion = 1

type persistedRuntime struct {
	Version int                       `json:"version"`
	Routes  map[string]persistedRoute `json:"routes"`
}

type persistedRoute struct {
	Signature string            `json:"signature"`
	Cursor    int64             `json:"cursor"`
	Cooldowns map[string]string `json:"cooldowns,omitempty"`
	Failures  map[string]int    `json:"failures,omitempty"`
}

func newRouteRuntime(now func() time.Time) *routeRuntime {
	if now == nil {
		now = time.Now
	}
	// Jitter is off by default so the runtime behaves deterministically; the
	// plugin turns it on for live traffic (see EnableJitter).
	return &routeRuntime{now: now, states: make(map[string]*routeState)}
}

// EnableJitter spreads cooldown expiry so a batch of targets that failed
// together does not return to rotation on the same instant.
func (runtime *routeRuntime) EnableJitter() {
	if runtime == nil {
		return
	}
	runtime.mu.Lock()
	runtime.jitter = defaultJitter
	runtime.mu.Unlock()
}

// defaultJitter varies a cooldown by up to ±25%. Without it, every target that
// failed in the same burst returns to rotation at the same instant and the next
// request hits them all in sequence.
func defaultJitter(cooldown time.Duration) time.Duration {
	if cooldown <= 0 {
		return cooldown
	}
	spread := float64(cooldown) * 0.25
	delta := (rand.Float64()*2 - 1) * spread
	adjusted := float64(cooldown) + delta
	if adjusted < float64(time.Second) {
		return time.Second
	}
	return time.Duration(adjusted)
}

// Load restores persisted cooldowns so a restart does not forget which targets
// were failing.
func (runtime *routeRuntime) Load(path string) {
	if runtime == nil || strings.TrimSpace(path) == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var stored persistedRuntime
	if err := json.Unmarshal(raw, &stored); err != nil || stored.Version != statePersistVersion {
		return
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.persistPath = path
	now := runtime.now()
	for key, entry := range stored.Routes {
		state := &routeState{
			signature: entry.Signature,
			cursor:    entry.Cursor,
			cooldowns: make(map[string]time.Time, len(entry.Cooldowns)),
			failures:  make(map[string]int, len(entry.Failures)),
			probing:   make(map[string]struct{}),
		}
		for model, until := range entry.Cooldowns {
			parsed, err := time.Parse(time.RFC3339Nano, until)
			if err != nil || !now.Before(parsed) {
				continue
			}
			state.cooldowns[model] = parsed
		}
		for model, count := range entry.Failures {
			if count > 0 {
				state.failures[model] = count
			}
		}
		runtime.states[key] = state
	}
}

func (runtime *routeRuntime) SetPersistPath(path string) {
	if runtime == nil {
		return
	}
	runtime.mu.Lock()
	runtime.persistPath = path
	runtime.mu.Unlock()
}

// persistLocked writes the cooldown state. Callers hold runtime.mu.
func (runtime *routeRuntime) persistLocked() {
	if runtime.persistPath == "" {
		return
	}
	now := runtime.now()
	stored := persistedRuntime{Version: statePersistVersion, Routes: make(map[string]persistedRoute, len(runtime.states))}
	for key, state := range runtime.states {
		entry := persistedRoute{
			Signature: state.signature,
			Cursor:    state.cursor,
			Cooldowns: make(map[string]string, len(state.cooldowns)),
			Failures:  make(map[string]int, len(state.failures)),
		}
		for model, until := range state.cooldowns {
			if now.Before(until) {
				entry.Cooldowns[model] = until.Format(time.RFC3339Nano)
			}
		}
		for model, count := range state.failures {
			if count > 0 {
				entry.Failures[model] = count
			}
		}
		stored.Routes[key] = entry
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		return
	}
	// Write beside the target and rename, so a crash mid-write cannot leave a
	// truncated file that the next start would fail to parse.
	temp := runtime.persistPath + ".tmp"
	if err := os.WriteFile(temp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(temp, runtime.persistPath)
}

// defaultStatePath is where live cooldown state is kept. It is derived from the
// CPA plugins directory so the file lands next to the plugin, and it is a plain
// JSON file rather than a database: it is written rarely and must stay cheap on
// a small host.
func defaultStatePath(dataPath string) string {
	dir := filepath.Dir(strings.TrimSpace(dataPath))
	if dir == "" || dir == "." {
		return defaultStateFileName
	}
	return filepath.Join(dir, defaultStateFileName)
}

const defaultStateFileName = "combo-router-state.json"

func (runtime *routeRuntime) Sync(routes []modelRoute) {
	if runtime == nil {
		return
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	next := make(map[string]*routeState, len(routes))
	for _, route := range routes {
		key := routeKey(route.Alias)
		signature := routeSignature(route)
		state := runtime.states[key]
		if state == nil || state.signature != signature {
			state = newRouteState(signature)
		}
		next[key] = state
	}
	runtime.states = next
}

func newRouteState(signature string) *routeState {
	return &routeState{
		signature: signature,
		cooldowns: make(map[string]time.Time),
		failures:  make(map[string]int),
		probing:   make(map[string]struct{}),
	}
}

func (runtime *routeRuntime) Clone(routes []modelRoute) *routeRuntime {
	if runtime == nil {
		cloned := newRouteRuntime(nil)
		cloned.Sync(routes)
		return cloned
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	cloned := newRouteRuntime(runtime.now)
	cloned.persistPath = runtime.persistPath
	cloned.jitter = runtime.jitter
	for _, route := range routes {
		key := routeKey(route.Alias)
		signature := routeSignature(route)
		old := runtime.states[key]
		state := newRouteState(signature)
		if old != nil && old.signature == signature {
			state.cursor = old.cursor
			for model, until := range old.cooldowns {
				state.cooldowns[model] = until
			}
			for model, count := range old.failures {
				state.failures[model] = count
			}
		}
		cloned.states[key] = state
	}
	return cloned
}

func (runtime *routeRuntime) Select(route modelRoute) routeSelection {
	return runtime.SelectExcluding(route, nil)
}

// cooling reports whether a target may be used right now. A target whose
// circuit is open and whose cooldown has expired is only usable by a single
// probe, which this call claims.
func (state *routeState) cooling(model string, now time.Time, claimProbe bool) (bool, time.Time) {
	key := routeKey(model)
	until := state.cooldowns[key]
	if !until.IsZero() && now.Before(until) {
		return true, until
	}
	delete(state.cooldowns, key)
	if state.failures[key] < circuitOpenAfter {
		return false, time.Time{}
	}
	// The cooldown expired but the target failed repeatedly: let exactly one
	// request through to find out whether it recovered.
	if claimProbe {
		if _, busy := state.probing[key]; busy {
			// Another request is already testing it. Treat the target as
			// unavailable, but do not push its cooldown out.
			return true, now.Add(probeHoldSeconds * time.Second)
		}
		state.probing[key] = struct{}{}
	}
	return false, time.Time{}
}

// probeHoldSeconds is how long a request that lost the probe race treats the
// target as unavailable. It is short because the probe itself is expected to
// finish quickly.
const probeHoldSeconds = 5

func (runtime *routeRuntime) SelectExcluding(route modelRoute, excluded map[string]struct{}) routeSelection {
	if runtime == nil || len(route.Targets) == 0 {
		return routeSelection{}
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state := runtime.stateLocked(route)
	if state == nil {
		return routeSelection{}
	}
	now := runtime.now()
	var earliest time.Time
	if route.Strategy != routeStrategyRoundRobin {
		for _, target := range route.Targets {
			model := strings.TrimSpace(target.Model)
			if _, skip := excluded[routeKey(model)]; skip {
				continue
			}
			cooling, until := state.cooling(model, now, true)
			if !cooling {
				return routeSelection{model: model}
			}
			if earliest.IsZero() || until.Before(earliest) {
				earliest = until
			}
		}
	} else {
		totalWeight := routeTargetWeightTotal(route.Targets)
		if totalWeight <= 0 {
			return routeSelection{}
		}
		start := state.cursor % totalWeight
		if start < 0 {
			start += totalWeight
		}
		startIndex, startOffset := weightedTargetAt(route.Targets, start)
		slot := start
		for offset := 0; offset < len(route.Targets); offset++ {
			index := (startIndex + offset) % len(route.Targets)
			target := route.Targets[index]
			model := strings.TrimSpace(target.Model)
			if _, skip := excluded[routeKey(model)]; !skip {
				cooling, until := state.cooling(model, now, true)
				if !cooling {
					state.cursor = (slot + 1) % totalWeight
					return routeSelection{model: model}
				}
				if earliest.IsZero() || until.Before(earliest) {
					earliest = until
				}
			}
			weight := int64(target.Weight)
			if offset == 0 {
				weight -= startOffset
			}
			if weight > 0 {
				slot = (slot + weight) % totalWeight
			}
		}
	}
	if earliest.IsZero() {
		return routeSelection{}
	}
	return routeSelection{allCooling: true, retryAfter: earliest.Sub(now)}
}

// targetCooldownSeconds resolves the cooldown for one target: the target's own
// value wins, then the route's, then the default.
func targetCooldownSeconds(route modelRoute, model string) int {
	for _, target := range route.Targets {
		if routeKey(target.Model) == routeKey(model) {
			if target.CooldownSeconds > 0 {
				return target.CooldownSeconds
			}
			break
		}
	}
	if route.CooldownSeconds > 0 {
		return route.CooldownSeconds
	}
	return defaultCooldownSeconds
}

// MarkFailure records a failed attempt. The cooldown comes from the error
// policy; a zero cooldown means the policy chose not to penalise the target.
func (runtime *routeRuntime) MarkFailure(route modelRoute, model string, cooldown time.Duration) {
	if runtime == nil || strings.TrimSpace(model) == "" {
		return
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state := runtime.stateLocked(route)
	if state == nil {
		return
	}
	key := routeKey(model)
	state.failures[key]++
	delete(state.probing, key)
	if cooldown <= 0 {
		runtime.persistLocked()
		return
	}
	if runtime.jitter != nil {
		cooldown = runtime.jitter(cooldown)
	}
	state.cooldowns[key] = runtime.now().Add(cooldown)
	runtime.persistLocked()
}

// FailureCount reports the target's consecutive failure count. It feeds the
// exponential backoff so the first failure doubles correctly.
func (runtime *routeRuntime) FailureCount(route modelRoute, model string) int {
	if runtime == nil || strings.TrimSpace(model) == "" {
		return 0
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state := runtime.stateLocked(route)
	if state == nil {
		return 0
	}
	return state.failures[routeKey(model)]
}

// MarkSuccess clears the failure history of a target that just answered.
func (runtime *routeRuntime) MarkSuccess(route modelRoute, model string) {
	if runtime == nil || strings.TrimSpace(model) == "" {
		return
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state := runtime.stateLocked(route)
	if state == nil {
		return
	}
	key := routeKey(model)
	_, hadFailures := state.failures[key]
	_, wasProbing := state.probing[key]
	if !hadFailures && !wasProbing {
		return
	}
	delete(state.failures, key)
	delete(state.probing, key)
	delete(state.cooldowns, key)
	runtime.persistLocked()
}

// ReleaseProbe returns a probe slot that never produced a verdict, so a request
// cancelled mid-probe does not block the target forever.
func (runtime *routeRuntime) ReleaseProbe(route modelRoute, model string) {
	if runtime == nil || strings.TrimSpace(model) == "" {
		return
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state := runtime.stateLocked(route)
	if state == nil {
		return
	}
	delete(state.probing, routeKey(model))
}

func (runtime *routeRuntime) stateLocked(route modelRoute) *routeState {
	key := routeKey(route.Alias)
	if key == "" {
		return nil
	}
	if runtime.states == nil {
		runtime.states = make(map[string]*routeState)
	}
	signature := routeSignature(route)
	state := runtime.states[key]
	if state == nil || state.signature != signature {
		state = newRouteState(signature)
		runtime.states[key] = state
	}
	return state
}

func routeTargetWeightTotal(targets []modelTarget) int64 {
	var total int64
	for _, target := range targets {
		weight := int64(target.Weight)
		if weight < 1 || total > (1<<63-1)-weight {
			return 0
		}
		total += weight
	}
	return total
}

func weightedTargetAt(targets []modelTarget, slot int64) (int, int64) {
	var offset int64
	for index, target := range targets {
		weight := int64(target.Weight)
		if weight < 1 {
			continue
		}
		if slot < offset+weight {
			return index, slot - offset
		}
		offset += weight
	}
	return 0, 0
}
