# CPA Antigravity Quota Guard

`cpa-antigravity-quota-guard` is a Google Antigravity quota circuit-breaker plugin for [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI). It uses the official CPA plugin ABI and tracks quota state per **credential × model group**. Exhausted or repeatedly rate-limited credentials are excluded by the Scheduler without modifying `priority` or `disabled` in auth JSON.

Development baselines:

- Upstream plugin baseline: `ygq-future/antigravity-priority` `v1.2.9` / `373f44b430c5eb770fb63657da9a7983c5cdabff`
- CPA ABI baseline: CLIProxyAPI `v7.2.141` / `dc3c3b1ec3ed04bb0917e76451eaf98c6842674d`
- Current version: `0.1.1`

> The default mode is `observe`. Stock CLIProxyAPI `v7.2.141` supports `observe` only; `enforce` requires the companion CLIProxyAPI Core extensions plus host-feature negotiation and the `required-scheduler-for` gate.

## Core behavior

- Independent `gemini` and `claude_gpt` model groups.
- State machine: `uninitialized → closed → open → half_open`.
- Trusted zero quota with reset: open immediately and recover at the reset time.
- Explicit quota-exhaustion 429: open immediately.
- Generic 429: two failures within 60 seconds open a 15-minute breaker; repeated failures escalate to at most 30 minutes.
- At recovery, exactly one request receives the half-open lease; success closes a transient breaker and failure reopens it.
- Probe failures only mark evidence stale and increment metrics; they do not change scheduling state.
- Unknown credentials and models fail closed by default.
- Mixed-provider routes exclude cooling Antigravity credentials while preserving other providers.
- Manually disabled credentials remain under CPA's native candidate filter. The plugin never re-enables them.

## Official ABI capabilities

The plugin registers only:

```text
scheduler
usage_plugin
request_interceptor
management_api
```

Legacy `filter.*`, priority write-back, scheduled write-back, and reset routes are not registered. Compatibility methods return `runtime: legacy auth mutation is disabled`.

## Build

```bash
go build -buildmode=c-shared -trimpath -ldflags="-s -w" \
  -o cpa-antigravity-quota-guard.so .
sha256sum cpa-antigravity-quota-guard.so
```

CPA derives the plugin ID from the shared-library filename, so the basename must remain `cpa-antigravity-quota-guard`.

Do not rely on third-party prebuilt binaries or automatic updates from an external `main/registry.json`. Production artifacts should be built by this repository and verified with SHA-256.

## Configuration

```yaml
plugins:
  configs:
    cpa-antigravity-quota-guard:
      enabled: true
      priority: -100
      required-scheduler-for:
        - antigravity
      mode: observe
      managed_auth: all_antigravity
      enforced_groups:
        - gemini
        - claude_gpt
      require_uniform_priority: true
      probe_interval: 15m
      evidence_max_age: 30m
      generic_429:
        threshold: 2
        window: 60s
        initial_cooldown: 15m
        max_cooldown: 30m
      half_open_lease: 30s
      unknown_auth_policy: fail_closed
      unknown_model_policy: fail_closed
      state_cache_path: data/cpa-antigravity-quota-guard/quota-cache.json
      state_path: data/cpa-antigravity-quota-guard/state.json
```

`auto_apply: true` is explicitly rejected. The plugin never changes auth `priority`, `disabled`, tokens, or unrelated fields.

### `enforce` deployment gates

All gates must pass:

1. Run the Core extensions based on CLIProxyAPI `v7.2.141`, with all host features advertised:
   - `required_scheduler_v1`
   - `scheduler_request_id_v1`
   - `scheduler_direct_response_v1`
   - `auth_inventory_ready_v1`
2. Configure `required-scheduler-for: [antigravity]`. The host routes Antigravity requests directly to this plugin; missing, fused, unloaded, individually or globally disabled, declined, or invalid results produce a local 503 instead of built-in fallback. Schedulers for unrelated providers may remain active. Routes without a required Scheduler still use only the globally highest plugin priority, so keep this plugin below provider-specific Schedulers such as `codex-token-usage` (for example, quota guard `-100`, token usage `0`). Only explicitly removing the marker retires the protection.
3. Disable CPA Home mode. The extended Core fails Antigravity closed if Home remains enabled, but that is a safety response rather than a supported steady state.
4. All managed Antigravity credentials use the same priority.
5. Both `gemini` and `claude_gpt` have fresh baseline quota evidence.
6. The state file is safely writable and there are no unknown or identity-conflicting credentials.
7. All Core/plugin integration gates in `docs/phase0-abi-gate.md` pass.

Missing host features, `required-scheduler-for`, or an unsafe state path reject `enforce`. During cold start, the plugin may register in an **armed-not-ready** state while CPA is still loading its initial auth inventory or baseline evidence is unavailable: Management APIs remain available, protected Antigravity-only requests return a local 503, and persisted breaker state is not erased. After the host publishes `inventory_ready=true` and roster reconciliation completes, a fresh baseline automatically promotes the runtime to `enforcement_ready=true`. A Management API transition from `observe` to `enforce` still requires every gate to pass immediately.

After a baseline has been established, a single probe failure or evidence aging does not globally disable healthy credentials; existing breaker state remains unchanged. New or replaced credentials stay independently `uninitialized` and excluded until their first successful quota probe.

## Management API

```text
GET  /v0/management/cpa-antigravity-quota-guard/status
GET  /v0/management/cpa-antigravity-quota-guard/config
PUT  /v0/management/cpa-antigravity-quota-guard/config
POST /v0/management/cpa-antigravity-quota-guard/actions/probe
POST /v0/management/cpa-antigravity-quota-guard/actions/half-open
GET  /v0/resource/plugins/cpa-antigravity-quota-guard/status
```

The Management Key is not accepted through URL query parameters and is never written to `localStorage` or `sessionStorage`. The minimal status page keeps it only in page memory.

## State and security

- Breaker state: `data/cpa-antigravity-quota-guard/state.json`
- Quota evidence/cache: `data/cpa-antigravity-quota-guard/quota-cache.json`
- State file mode: `0600`
- Writes use a temporary file, file `fsync`, atomic rename, directory `fsync`, and read-back verification.
- Relative state paths reject absolute paths, `..`, target symlinks, and parent symlinks.
- Auth material is read only from the `JSON` returned by `host.auth.get`; `AuthDocument.Path` is never followed.
- State and logs do not retain access tokens, refresh tokens, complete auth JSON, request bodies, or complete upstream error bodies.

## CPA Core compatibility boundary

In CLIProxyAPI `v7.2.141`, `usage.handle` is dispatched asynchronously. The plugin alone therefore cannot guarantee that a newly observed 429 reaches the guard before the same inbound request performs its next credential retry; that path still relies on CPA's native cooldown.

Long-term `enforce` requires the Core extensions:

- Scheduler and Usage carry a `RequestID` so half-open leases cannot be closed by an older request.
- Scheduler rejection can safely return HTTP 429, a numeric `Retry-After`, and a valid JSON body capped at 64 KiB.
- `required-scheduler-for` routes Antigravity directly to this plugin and returns a local 503 when it is missing, fused, inactive, declines, or returns an invalid result, with no built-in fallback; unrelated provider Schedulers may remain active.
- `auth_inventory_ready_v1` distinguishes an early bootstrap-time empty auth list from a fully loaded empty roster, preventing cold start from erasing persisted cooldown state.
- A required Scheduler can return `DelegateBuiltin=configured` in `observe` mode or for unenforced model groups, preserving the host's configured routing strategy and cursor without allowing built-in selection to reintroduce excluded credentials.
- Home mode also fails the required route closed.

Stock Core does not provide these guarantees, so the plugin refuses `enforce` on stock `v7.2.141`; `observe` remains compatible.

## Local Dev Server

```bash
go run ./cmd/devserver
```

Open:

```text
http://localhost:8080/v0/resource/plugins/cpa-antigravity-quota-guard/status
```

The Dev Server simulates only the auth inventory and quota endpoint. Probe, status, and guard Management APIs use the production Runtime. All legacy apply and auto-apply operations are disabled.

It listens on `127.0.0.1:8080` by default and prints a generated Dev Management Key at startup. The resource page remains readable, while `/v0/management/` calls require that key through the page or a Bearer header. Non-loopback listening requires the explicit `-unsafe-listen` flag.

## Verification

```bash
golangci-lint run --timeout=5m
go build ./...
go vet ./...
go test -v ./...
go test -race ./...
```

See `docs/release-checklist.md` for release gates and `docs/phase0-abi-gate.md` for the Phase 0 ABI gate.

## Upstream and license

This project is forked from `ygq-future/antigravity-priority` and retains the original MIT license and attribution. The fork is maintained by `zyaireleo`, with an architecture focused on read-only quota circuit breaking instead of priority write-back.
