# CPA Combo Router

[![test](https://github.com/thuxeko/cpa-combo-router/actions/workflows/test.yml/badge.svg)](https://github.com/thuxeko/cpa-combo-router/actions/workflows/test.yml)
[![release](https://github.com/thuxeko/cpa-combo-router/actions/workflows/release.yml/badge.svg)](https://github.com/thuxeko/cpa-combo-router/actions/workflows/release.yml)
[![license](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

CPA Combo Router is a native [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) model-router and executor plugin. It exposes logical model aliases that fan out to an ordered pool of physical models, choosing each attempt by priority or weighted round-robin, cooling down targets that fail, and failing over to the next candidate.

The plugin does not call providers directly. For each selected physical model it calls CPA's host model executor, so CPA keeps control of provider credentials, protocol translation, proxy policy, request logging, and usage capture.

This repository is a fork of [markhuangai/cpa-plugin-model-router](https://github.com/markhuangai/cpa-plugin-model-router) at `v0.5.4`, reduced to the routing core and extended with a configurable error policy. It is an independent plugin with its own identifier, configuration key, management routes, and state file: it is meant to **replace** Model Router, not to run alongside it.

## What this fork changes

Removed, relative to `v0.5.4`:

- the plugin-local usage store (SQLite/bbolt), its schema, migration, and retention
- the estimated-cost/pricing workflow and the models.dev price synchronization
- the Usage tracking dashboard tab and every usage management route
- the `data_path` and `retention_days` configuration fields

The plugin therefore declares neither `usage_plugin` nor `request_interceptor`. It writes no request history. What remains is routing: configuration, selection, cooldown, circuit breaking, and failover.

Added:

- a configurable `error_policy` that maps HTTP status to an action and a cooldown
- per-target `cooldown_seconds` that overrides the route value
- an `attempt_timeout_seconds` cap that applies before the first byte
- per-target circuit breaking with a single half-open probe, cooldown jitter, and state that survives a restart

The dashboard keeps two tabs: **Tuyến** (routes) and **Chính sách lỗi** (error policy). Both the interface and the validation messages are in Vietnamese.

## Features

- Expose logical model aliases through CPA's model registry.
- Route an alias to an ordered priority pool or a weighted round-robin pool.
- Assign round-robin weights to give models with multiple CPA providers a proportional outer share.
- Fail over on quota, rate-limit, auth, provider, server, and recognized transport failures.
- Cool failed targets down, per target, and skip them on later requests.
- Break the circuit on a target that keeps failing, then re-enter it with a single probe.
- Stop waiting on a target that produced no first byte within a configured cap.
- Honor an upstream `Retry-After` hint when the policy asks for it.
- Preserve a requested thinking suffix unless a target defines its own suffix.
- Rewrite non-streaming and streaming response model fields back to the requested alias.
- Retry a stream only before the first upstream payload is received.
- Persist cooldown and failure state across restarts in a small JSON file.
- Preserve cooldown and round-robin state when CPA reconfigures the plugin.
- Configure ordered routes and target pools, and the error policy, through a dedicated CPA management page.

## Compatibility

The module is built against `github.com/router-for-me/CLIProxyAPI/v7` v7.3.9 and requires CPA v7.3.9 or newer. That release preserves numeric HTTP statuses returned through native host callbacks, so configured fallback rules can classify upstream errors correctly. The plugin negotiates RPC schema v2 with older compatible hosts and schema v3 when offered; schema v3 avoids resending the full request body with every streaming response chunk.

The CPA host must support native plugins, `model_router`, `executor`, `model_registrar`, and the `host.model.*` callback methods. The configuration page also requires CPA's `management_api` capability and plugin resource menus.

Build the plugin for the same operating system and architecture as CPA. A Go `c-shared` library is not portable across OS or CPU targets.

## Configuration

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    combo-router:
      enabled: true
      priority: 100
      attempt_timeout_seconds: 45
      routes:
        - alias: fast
          strategy: priority
          cooldown_seconds: 30
          targets:
            - model: provider-a/deepseek-v4-flash
              weight: 1
            - model: provider-b/deepseek-v4-flash
              weight: 1

        - alias: balanced
          strategy: round-robin
          cooldown_seconds: 30
          targets:
            - model: provider-a/glm-5
              weight: 3
            - model: provider-b/glm-5
              weight: 1
```

`plugins.enabled` and `plugins.configs.combo-router.enabled` must both be true. The library basename must be exactly `combo-router` with the host extension — `combo-router.so`, `combo-router.dylib`, or `combo-router.dll`. When a version is present in the file name it must keep the `-v` prefix (`combo-router-v1.0.0.so`); without it the host parses the version as part of the identifier.

The configuration parser is strict: an unknown key is rejected with an error naming the field, rather than ignored.

### Plugin fields

| Field | Default | Meaning |
| --- | --- | --- |
| `enabled` | `true` | Whether the plugin handles requests. |
| `priority` | `0` | Plugin ordering against other model routers. |
| `attempt_timeout_seconds` | `45` | How long one target attempt may wait for its first byte. `0` disables the cap. Range 0-3600. |
| `state_path` | `combo-router-state.json` beside the plugin | Where cooldown and failure state is persisted. Empty disables persistence. |
| `routes` | - | Ordered route list. |
| `model-routes` | - | Legacy name for `routes`. Do not provide both. |
| `fallback` | - | Legacy status lists, kept for compatibility. |
| `error_policy` | - | Status-to-action policy. |

### Route fields

| Field | Required | Meaning |
| --- | --- | --- |
| `alias` | yes | Client-visible model name. Matching is case-insensitive. Thinking suffixes are not allowed on aliases. |
| `strategy` | no | `priority` by default, or `round-robin`. |
| `cooldown_seconds` | no | Seconds a failed target remains unavailable. Omitted or zero uses 60 seconds. |
| `targets` | yes | Ordered physical model target objects. |
| `models` | legacy | Legacy ordered string targets. Each is normalized to weight 1; do not provide it together with `targets`. |

A target object takes a required `model`, an integer `weight` from 1 through 1000000 (omitted defaults to 1), and an optional `cooldown_seconds` that overrides the route value for that target alone. Empty and duplicate targets are rejected.

A route target cannot reference any configured route alias, including through a thinking suffix. This prevents recursive routes.

### Selection and failover

`priority` selects the first target that is not cooling down and ignores weights. `round-robin` advances through consecutive virtual slots according to each target's weight and skips all slots for targets that are cooling down. For example, weights `3:1` produce `A,A,A,B`; CPA then selects the actual credential configured behind each selected model.

A stream can move to the next target only when start-up fails before any payload bytes arrive. A failure after payload arrival cools the target but is returned to the client without splicing a second provider stream into the first.

### Circuit breaking and cooldown

A target's consecutive failure count feeds the exponential backoff and the circuit. After 3 consecutive failures the circuit opens: once the cooldown expires, exactly one request is allowed through to test the target. Other requests treat it as unavailable until that probe reports back, so a recovered target is discovered rather than stampeded. A success clears the target's failure history immediately.

Cooldown expiry carries up to +/-25% jitter, so a batch of targets that failed together does not return to rotation at the same instant.

State lives in a plain JSON file written atomically (write beside, then rename). It is written only when a target's state changes, and it is scoped to the routes present in the configuration: a route whose shape changed starts fresh.

### Error policy

`error_policy` maps an HTTP status to an action. Without it the plugin keeps the legacy behaviour driven by `fallback_on_status` / `no_fallback_on_status`, so an untouched configuration does not change meaning.

```yaml
error_policy:
  default:
    action: fallback
    cooldown_seconds: 30
  rules:
    - status: 429
      action: fallback
      honor_retry_after: true
      cooldown_seconds: 20
      max_cooldown_seconds: 900
    - status: [401, 403]
      action: fallback
      cooldown_seconds: 600
    - status: [500, 502, 503, 504, 520, 521, 522, 523, 524, 525, 526]
      action: fallback
      cooldown_seconds: 5
      backoff: exponential
      max_cooldown_seconds: 300
    - status: [400, 404, 409, 413, 422]
      action: stop
    - status: 499
      action: stop
```

Rules are matched in written order; a status listed twice is rejected rather than silently shadowed. The default rule answers any status no rule names.

| Action | Effect |
| --- | --- |
| `fallback` | Try the next target and cool the failed one down. |
| `fallback_no_penalty` | Try the next target, but leave the failed one immediately eligible. |
| `stop` | Return the failure without trying another target and without cooling anything. |

`stop` and `fallback_no_penalty` never penalise a target, so they reject `cooldown_seconds` and `honor_retry_after`.

| Rule field | Meaning |
| --- | --- |
| `status` | One status, or a list of statuses. Required on `rules` entries. |
| `action` | One of the three actions above. Defaults to `fallback`. |
| `cooldown_seconds` | Seconds the target stays out. When omitted the route's own `cooldown_seconds` applies, exactly as before `error_policy` existed. |
| `max_cooldown_seconds` | Ceiling for an honored `Retry-After` and for exponential backoff. Defaults to 900; the configured ceiling may not exceed 86400. |
| `honor_retry_after` | Let the upstream's own `Retry-After` header override `cooldown_seconds`, clamped to the ceiling. |
| `backoff` | `exponential` doubles the wait on each consecutive failure of the same target. |

Two statuses are always terminal regardless of the policy, because another target cannot help: a caller that hung up (`499`, or a context cancellation) and a request-scoped persisted-item miss.

### Plugin-side defaults

When `error_policy` is absent the plugin reproduces the documented `v0.5.4` behaviour:

- fails over for `401`, `402`, `403`, `408`, `429`, for `404` except request-scoped persisted-item misses, and for all `5xx` statuses;
- returns `400` and `422` immediately as request errors;
- never fails over on cancellation or deadline errors;
- when the host callback loses a numeric status, fails over only for recognized quota, auth-unavailable, model/provider-unavailable, timeout, DNS, connection, broken-pipe, reset, or EOF messages, and otherwise stops the route rather than repeating a bad request against every provider.

### Thinking suffixes

If a client asks for `fast(high)` and the selected target is `gpt-5.4-mini`, CPA executes `gpt-5.4-mini(high)`. If the selected target is already `gemini-2.5-pro(8192)`, the target suffix wins.

Successful response fields such as `model`, `modelVersion`, `response.model`, and `message.model` are rewritten to the exact requested alias, including its suffix.

### Migrating a Model Router configuration

The routing keys are the same, so a Model Router `routes` block can be reused as-is. Two changes are required:

1. Move the block under a `combo-router` key. The host looks the configuration up by the plugin's identifier, which it derives from the library file name - a block still named `model-router` will not be read, and the plugin then stays disabled with no error.
2. Drop `data_path` and `retention_days`. They no longer exist in this plugin, and the strict parser rejects them.

`model-routes` instead of `routes`, and `cooldown-seconds` instead of `cooldown_seconds`, are still accepted for staged migration. Do not provide both forms of either field; registration fails rather than choosing one silently.

## Configuration UI

The plugin registers a **Combo Router** page in CPA's management frontend. Open the Plugins section and select **Combo Router**. Two tabs appear: **Tuyến** (routes) and **Chính sách lỗi** (error policy). Routes is selected by default.

When CPAMC has a persisted authenticated session, the page reuses its management key and loads configuration automatically. It follows CPAMC's selected light, white, or dark theme, and updates when that selection changes.

The routes tab provides typed controls for route order, aliases, priority or round-robin strategy, cooldowns, ordered target pools, and round-robin weights. Target dropdowns are populated with the model IDs currently returned by CPA's `/v1/models` endpoint. Model discovery uses the management session to read CPA's configured client API keys, then keeps the first non-empty key in browser memory only while requesting `/v1/models`; the client key is not rendered or stored. Existing targets that are absent from the live catalog remain visible as disabled `<model> (unavailable)` choices until replaced, so loading the page never changes saved routes.

The policy tab edits `error_policy` and `attempt_timeout_seconds`. When no policy is configured it shows the built-in table rather than an empty form, so the effective behaviour is always visible.

The fixed **Save changes** and **Discard and reload** dock appears only while the draft differs from the loaded configuration. Saving validates duplicate aliases, recursive routes, empty pools, duplicate targets, cooldown values, the error-policy table, and the attempt timeout with the plugin's Go parser - through the same code path a YAML edit takes - before applying a shallow patch through CPA. The patch updates `enabled`, `priority`, `routes`, `error_policy`, and `attempt_timeout_seconds` without replacing plugin-store metadata or unrelated config fields. The page is available directly at:

```text
/v0/resource/plugins/combo-router/config
```

Reading or changing configuration requires the management key. The page reads CPAMC's persisted `cli-proxy-auth` value from same-origin browser storage; it does not change CPAMC's stored session. If **Remember password** is disabled, the persisted session has no key, so the page reveals a fallback key field. A fallback key is cached only in that tab's session storage and is removed when CPA rejects it. CPA's Management API must be enabled and reachable from the browser.

## Build and test

Go 1.26 and a working C compiler are required.

```bash
make check
make build
```

`make build` writes the host-platform library to `dist/combo-router.<ext>`. Pass `VERSION=` to embed a specific version in the plugin metadata.

The default suite covers strict config parsing, priority and round-robin state, reconfiguration, failure classification, error-policy resolution and validation, cooldown and circuit behaviour, attempt timeouts, header sanitization, model rewriting, non-stream failover, stream boundaries, persistence, management endpoints, the management page, and native RPC registration.

Run the opt-in black-box test against a local CPA source checkout:

```bash
CPA_SOURCE=../CLIProxyAPI \
  go test -tags=integration ./... -count=1 -v
```

The black-box test builds CPA and the native plugin in a temporary directory, starts logical providers on a local mock OpenAI-compatible server, loads the library through CPA, and verifies all of the following without real provider credentials:

- the logical alias appears in `/v1/models`;
- the Combo Router menu and parser-backed validation endpoint are available;
- a `429` from the first target fails over to the second target;
- the client sees the requested alias in the response;
- the failed target remains on cooldown for the next request;
- streaming and non-streaming requests route correctly.

The browser E2E test exercises the dashboard against a disposable local CPA instance. It changes route configuration and the error policy, so do not point it at a shared or production instance. Install its pinned browser dependency with `npm ci`, then run:

```bash
DASHBOARD_URL=http://127.0.0.1:8317/v0/resource/plugins/combo-router/config \
DASHBOARD_MANAGEMENT_KEY='replace-with-disposable-instance-key' \
DASHBOARD_E2E_ALLOW_MUTATIONS=1 \
npm run test:e2e
```

Set `PLAYWRIGHT_CHROMIUM_EXECUTABLE` when Chromium is not available at `/usr/bin/google-chrome`.

### Manual local installation

1. Run `make build` on the CPA host machine.
2. Copy `dist/combo-router.so` on Linux, `dist/combo-router.dylib` on macOS, or `dist/combo-router.dll` on Windows into the configured `plugins.dir`. CPA also scans `plugins.dir/<goos>/<goarch>`. Name it `combo-router-v<version>.<ext>` and set `store.version` to the same version when you want the host's version pin to match the file.
3. Add `plugins.configs.combo-router.enabled: true`. Add routes in YAML or through the Combo Router management page. Every target model must already be routable by CPA.
4. Start CPA with `go run ./cmd/server --config /path/to/config.yaml` or your normal CPA binary.
5. Verify that the alias is listed:

```bash
curl -fsS http://127.0.0.1:8317/v1/models \
  -H "Authorization: Bearer ***" \
  | jq '.data[] | select(.id == "fast")'
```

6. If the Management API is enabled, verify plugin registration:

```bash
curl -fsS http://127.0.0.1:8317/v0/management/plugins \
  -H "Authorization: Bearer $MANAGEMENT_KEY" \
  | jq '.plugins[] | select(.id == "combo-router")'
```

`registered`, `enabled`, and `effective_enabled` should all be true. A copied library is trusted in-process code; only load artifacts you built or verified.

## Current ABI limits

- Routed token-count requests return HTTP `501` with code `model_route_count_tokens_unsupported`. The host callback contract exposes model execution but not routed token counting. Returning an explicit error avoids reporting a false zero.
- The upstream `Retry-After` header is visible only when a target rejects before the stream opens; a mid-stream failure falls back to the configured number.
- Status-less errors lose some structured failure information at the ABI boundary. The failure classifier is conservative and may stop on an unfamiliar transient error until that error is added explicitly.
- An attempt timeout is reported with no status code, because the fault is the router's patience rather than an upstream response.

## Security notes

The plugin removes client `Authorization`, proxy authorization, cookies, host/content-length, and headers whose names contain API key, token, secret, or credential markers before calling `host.model.*`. CPA selects and applies the target provider credential. Other request headers and query parameters are preserved.

The plugin stores no request or response content. The only file it writes is the cooldown state, which holds route names, model names, timestamps, and failure counts - never a body, a header, or a credential. It is created with mode `0600`.

Native plugins execute in the CPA process with CPA's permissions. Only load artifacts you built or verified, and do not put credentials in route model names.

## Publishing and plugin store registration

The release workflow accepts tags such as `v1.0.0` and builds these CPA Plugin Store assets:

```text
combo-router_1.0.0_linux_amd64.zip
combo-router_1.0.0_linux_arm64.zip
combo-router_1.0.0_darwin_amd64.zip
combo-router_1.0.0_darwin_arm64.zip
combo-router_1.0.0_windows_amd64.zip
checksums.txt
```

Each zip contains exactly one root-level library named `combo-router.so`, `combo-router.dylib`, or `combo-router.dll`. The workflow embeds the tag version in the plugin metadata and validates the archive layout before publishing.

To publish a release:

1. Push this repository, then create and push a `v<major>.<minor>.<patch>` tag.
2. Confirm the GitHub release contains all five zips plus `checksums.txt`.

## License

MIT. See [LICENSE](LICENSE). Derived from [markhuangai/cpa-plugin-model-router](https://github.com/markhuangai/cpa-plugin-model-router), also MIT.
