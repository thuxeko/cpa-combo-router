package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// errorAction is what the router does when a target attempt fails with a status
// that matches an error policy rule.
type errorAction string

const (
	// errorActionFallback moves on to the next target and cools the failed one.
	errorActionFallback errorAction = "fallback"
	// errorActionStop returns the failure without trying another target and
	// without cooling anything. It is the right action for faults that belong
	// to the request itself (400/422) and for a caller that already hung up
	// (499): another target cannot fix either one, and cooling a healthy target
	// only removes capacity.
	errorActionStop errorAction = "stop"
	// errorActionFallbackNoPenalty moves on to the next target but leaves the
	// failed one immediately eligible. It is the safe action for a status whose
	// meaning is not yet known: try somewhere else without blaming anyone.
	errorActionFallbackNoPenalty errorAction = "fallback_no_penalty"
)

const (
	backoffExponential = "exponential"

	// defaultPolicyCooldownSeconds is the wait applied by the default rule when
	// the configuration names no number.
	defaultPolicyCooldownSeconds = 30
	// defaultMaxCooldownSeconds caps both an upstream Retry-After hint and an
	// exponential backoff when a rule does not set its own ceiling.
	defaultMaxCooldownSeconds = 900
	// hardMaxCooldownSeconds is the largest wait a rule may configure. It exists
	// to catch a typo such as 600000000 that would park a target for years.
	hardMaxCooldownSeconds = 86400
)

// errorRule describes the reaction to one class of failure.
type errorRule struct {
	// Statuses are the HTTP status codes this rule governs.
	Statuses []int
	// Action is what happens to the request and to the failed target.
	Action errorAction
	// CooldownSeconds is how long the failed target stays out of rotation.
	CooldownSeconds int
	// MaxCooldownSeconds caps an upstream Retry-After hint and an exponential
	// backoff. Zero means defaultMaxCooldownSeconds.
	MaxCooldownSeconds int
	// HonorRetryAfter lets the upstream's own Retry-After header override
	// CooldownSeconds, clamped to [1s, MaxCooldownSeconds].
	HonorRetryAfter bool
	// Backoff is "" or "exponential". Exponential doubles the wait on each
	// consecutive failure of the same target.
	Backoff string
}

// errorPolicy maps an HTTP status to the rule that governs it.
type errorPolicy struct {
	// Default applies to any status no rule lists, so an unrecognised code can
	// never wedge the router.
	Default errorRule
	// Rules are matched in written order; the first rule listing the status
	// wins, so a specific rule can be placed above a broader one.
	Rules []errorRule
}

// resolve returns the rule that governs status.
func (policy errorPolicy) resolve(status int) errorRule {
	rule, _ := policy.lookup(status)
	return rule
}

// lookup returns the rule that governs status and whether a rule named it
// explicitly. A status that no rule lists is answered by the default rule, and
// the caller then falls back to the legacy fallback_on_status lists instead.
func (policy errorPolicy) lookup(status int) (errorRule, bool) {
	for _, rule := range policy.Rules {
		for _, candidate := range rule.Statuses {
			if candidate == status {
				return rule, true
			}
		}
	}
	return policy.Default, false
}

// namesStatus reports whether any rule lists status explicitly.
func (policy errorPolicy) namesStatus(status int) bool {
	_, explicit := policy.lookup(status)
	return explicit
}

// maxCooldown is the ceiling this rule applies to an upstream hint or a backoff.
func (rule errorRule) maxCooldown() int {
	if rule.MaxCooldownSeconds > 0 {
		return rule.MaxCooldownSeconds
	}
	if rule.CooldownSeconds > defaultMaxCooldownSeconds {
		return rule.CooldownSeconds
	}
	return defaultMaxCooldownSeconds
}

// cooldownFor returns how long a failed target should stay out of rotation.
// An upstream Retry-After hint wins when the rule honours it; otherwise the
// rule's own number applies, or the route/target cooldown when the rule names
// none, doubled per consecutive failure when the rule asks for exponential
// backoff.
func (rule errorRule) cooldownFor(consecutiveFailures int, retryAfter time.Duration, inherit time.Duration) time.Duration {
	ceiling := time.Duration(rule.maxCooldown()) * time.Second

	if rule.HonorRetryAfter && retryAfter > 0 {
		if retryAfter > ceiling {
			return ceiling
		}
		if retryAfter < time.Second {
			return time.Second
		}
		return retryAfter
	}

	base := time.Duration(rule.CooldownSeconds) * time.Second
	if rule.CooldownSeconds <= 0 {
		// The rule names no number, so the route's own cooldown_seconds keeps
		// working exactly as it did before error_policy existed.
		base = inherit
	}
	if base <= 0 {
		return 0
	}
	if rule.Backoff == backoffExponential {
		for attempt := 1; attempt < consecutiveFailures; attempt++ {
			if base >= ceiling/2 {
				base = ceiling
				break
			}
			base *= 2
		}
	}
	if base > ceiling {
		base = ceiling
	}
	return base
}

// policyWarnings reports configurations that are valid but almost certainly not
// what the operator meant. They are returned next to a successful validation so
// the panel can ask for confirmation instead of refusing the save.
func policyWarnings(policy errorPolicy) []string {
	var warnings []string
	seen := make(map[int]errorAction)
	all := append([]errorRule{policy.Default}, policy.Rules...)
	for _, rule := range all {
		for _, status := range rule.Statuses {
			if previous, duplicate := seen[status]; duplicate && previous != rule.Action {
				warnings = append(warnings, fmt.Sprintf("HTTP %d bị khai báo hai lần với hai hành động khác nhau", status))
			}
			seen[status] = rule.Action
			switch {
			case status == http.StatusTooManyRequests && rule.Action == errorActionStop:
				warnings = append(warnings, "429 đang đặt là dừng lại: gặp giới hạn tốc độ đầu tiên là kết thúc request luôn, không thử đích khác")
			case status >= 500 && rule.Action == errorActionStop:
				warnings = append(warnings, fmt.Sprintf("%d đang đặt là dừng lại: nhà cung cấp có sự cố là kết thúc request luôn, không thử đích khác", status))
			case status == 0:
				warnings = append(warnings, "HTTP 0 không phải là một mã trạng thái")
			}
		}
	}
	if policy.Default.Action == errorActionStop {
		warnings = append(warnings, "hành động mặc định là dừng lại: mọi mã không có trong bảng trên sẽ kết thúc request mà không thử đích khác")
	}
	return warnings
}

// errorRuleYAML is the wire form of one rule.
type errorRuleYAML struct {
	Status             *[]int `yaml:"status,omitempty" json:"status,omitempty"`
	Action             string `yaml:"action,omitempty" json:"action,omitempty"`
	CooldownSeconds    *int   `yaml:"cooldown_seconds,omitempty" json:"cooldown_seconds,omitempty"`
	MaxCooldownSeconds *int   `yaml:"max_cooldown_seconds,omitempty" json:"max_cooldown_seconds,omitempty"`
	HonorRetryAfter    *bool  `yaml:"honor_retry_after,omitempty" json:"honor_retry_after,omitempty"`
	Backoff            string `yaml:"backoff,omitempty" json:"backoff,omitempty"`
}

// errorPolicyYAML is the wire form of the whole policy.
type errorPolicyYAML struct {
	Default *errorRuleYAML   `yaml:"default,omitempty" json:"default,omitempty"`
	Rules   *[]errorRuleYAML `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// defaultErrorPolicy reproduces the documented behaviour for a configuration
// that does not define error_policy, and is also the starting point the panel
// edits. Every status an operator is likely to meet is listed explicitly, so
// the panel shows a complete picture instead of an empty table plus a default.
func defaultErrorPolicy() errorPolicy {
	return errorPolicy{
		Default: errorRule{Action: errorActionFallback, CooldownSeconds: defaultPolicyCooldownSeconds},
		Rules: []errorRule{
			{
				Statuses:           []int{http.StatusTooManyRequests},
				Action:             errorActionFallback,
				CooldownSeconds:    20,
				HonorRetryAfter:    true,
				MaxCooldownSeconds: defaultMaxCooldownSeconds,
			},
			{
				Statuses:        []int{http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusRequestTimeout},
				Action:          errorActionFallback,
				CooldownSeconds: 600,
			},
			{
				Statuses:           []int{500, 501, 502, 503, 504, 505, 506, 507, 508, 510, 511, 520, 521, 522, 523, 524, 525, 526},
				Action:             errorActionFallback,
				CooldownSeconds:    5,
				Backoff:            backoffExponential,
				MaxCooldownSeconds: 300,
			},
			{
				Statuses: []int{http.StatusBadRequest, 405, 406, 409, 411, 413, 414, 415, http.StatusUnprocessableEntity},
				Action:   errorActionStop,
			},
			{
				Statuses: []int{statusClientClosedRequest},
				Action:   errorActionStop,
			},
		},
	}
}

// statusClientClosedRequest is the nginx-style code for a caller that hung up.
const statusClientClosedRequest = 499

// buildErrorPolicy turns the wire form into a policy. A nil or empty wire form
// falls back to defaultErrorPolicy, so an existing configuration keeps working
// and only gains the new defaults.
func buildErrorPolicy(wire *errorPolicyYAML) (errorPolicy, error) {
	if wire == nil || (wire.Default == nil && (wire.Rules == nil || len(*wire.Rules) == 0)) {
		return defaultErrorPolicy(), nil
	}
	// A configuration that writes its own rules replaces the built-in list
	// instead of adding to it, so a status the operator did not mention is
	// answered by fallback_on_status exactly as it was before error_policy
	// existed.
	policy := errorPolicy{}
	if wire.Default != nil {
		rule, err := decodeErrorRule(*wire.Default, "error_policy.default", false)
		if err != nil {
			return errorPolicy{}, err
		}
		policy.Default = rule
	} else {
		policy.Default = defaultErrorPolicy().Default
	}
	if wire.Rules == nil {
		return policy, nil
	}
	rules := make([]errorRule, 0, len(*wire.Rules))
	claimed := make(map[int]int, len(*wire.Rules))
	for index, item := range *wire.Rules {
		where := fmt.Sprintf("error_policy.rules[%d]", index)
		rule, err := decodeErrorRule(item, where, true)
		if err != nil {
			return errorPolicy{}, err
		}
		for _, status := range rule.Statuses {
			if previous, duplicate := claimed[status]; duplicate {
				return errorPolicy{}, fmt.Errorf("%s: mã HTTP %d đã bị rules[%d] chiếm; quy tắc đầu tiên thắng nên quy tắc trùng sẽ không bao giờ áp dụng", where, status, previous)
			}
			claimed[status] = index
		}
		rules = append(rules, rule)
	}
	policy.Rules = rules
	return policy, nil
}

func decodeErrorRule(wire errorRuleYAML, where string, requireStatus bool) (errorRule, error) {
	rule := errorRule{Action: errorActionFallback}
	if wire.Status == nil {
		if requireStatus {
			return errorRule{}, fmt.Errorf("%s: cần có status", where)
		}
	} else {
		if len(*wire.Status) == 0 {
			return errorRule{}, fmt.Errorf("%s: status phải liệt kê ít nhất một mã HTTP", where)
		}
		seen := make(map[int]struct{}, len(*wire.Status))
		for _, status := range *wire.Status {
			if status < 100 || status > 599 {
				return errorRule{}, fmt.Errorf("%s: mã HTTP không hợp lệ %d (phải trong khoảng 100-599)", where, status)
			}
			if _, duplicate := seen[status]; duplicate {
				return errorRule{}, fmt.Errorf("%s: mã HTTP %d bị liệt kê hai lần", where, status)
			}
			seen[status] = struct{}{}
		}
		rule.Statuses = append([]int(nil), (*wire.Status)...)
	}
	if action := strings.ToLower(strings.TrimSpace(wire.Action)); action != "" {
		switch errorAction(action) {
		case errorActionFallback, errorActionStop, errorActionFallbackNoPenalty:
			rule.Action = errorAction(action)
		default:
			return errorRule{}, fmt.Errorf("%s: action phải là %q, %q hoặc %q", where, errorActionFallback, errorActionStop, errorActionFallbackNoPenalty)
		}
	}
	if wire.CooldownSeconds != nil {
		if *wire.CooldownSeconds < 0 || *wire.CooldownSeconds > hardMaxCooldownSeconds {
			return errorRule{}, fmt.Errorf("%s: cooldown_seconds phải trong khoảng 0 đến %d", where, hardMaxCooldownSeconds)
		}
		rule.CooldownSeconds = *wire.CooldownSeconds
	}
	if wire.MaxCooldownSeconds != nil {
		if *wire.MaxCooldownSeconds < 0 || *wire.MaxCooldownSeconds > hardMaxCooldownSeconds {
			return errorRule{}, fmt.Errorf("%s: max_cooldown_seconds phải trong khoảng 0 đến %d", where, hardMaxCooldownSeconds)
		}
		rule.MaxCooldownSeconds = *wire.MaxCooldownSeconds
	}
	if rule.MaxCooldownSeconds > 0 && rule.CooldownSeconds > rule.MaxCooldownSeconds {
		return errorRule{}, fmt.Errorf("%s: cooldown_seconds (%d) không được vượt max_cooldown_seconds (%d)", where, rule.CooldownSeconds, rule.MaxCooldownSeconds)
	}
	if wire.HonorRetryAfter != nil {
		rule.HonorRetryAfter = *wire.HonorRetryAfter
	}
	if backoff := strings.ToLower(strings.TrimSpace(wire.Backoff)); backoff != "" {
		if backoff != backoffExponential {
			return errorRule{}, fmt.Errorf("%s: backoff phải để trống hoặc là %q", where, backoffExponential)
		}
		rule.Backoff = backoff
	}
	if rule.Action == errorActionStop && (rule.CooldownSeconds != 0 || rule.HonorRetryAfter) {
		return errorRule{}, fmt.Errorf("%s: action %q không bao giờ phạt đích, nên phải bỏ cooldown_seconds và honor_retry_after", where, errorActionStop)
	}
	if rule.Action == errorActionFallbackNoPenalty && (rule.CooldownSeconds != 0 || rule.HonorRetryAfter) {
		return errorRule{}, fmt.Errorf("%s: action %q không bao giờ phạt đích, nên phải bỏ cooldown_seconds và honor_retry_after", where, errorActionFallbackNoPenalty)
	}
	return rule, nil
}

// parseRetryAfter reads an upstream Retry-After hint. Both forms in RFC 9110 are
// accepted: a delay in seconds and an HTTP-date.
func parseRetryAfter(headers http.Header) time.Duration {
	if headers == nil {
		return 0
	}
	raw := strings.TrimSpace(headers.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		delay := time.Until(when)
		if delay <= 0 {
			return 0
		}
		return delay
	}
	return 0
}
