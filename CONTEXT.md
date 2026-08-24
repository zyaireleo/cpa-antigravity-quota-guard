# CPA Antigravity Quota Guard Context

## Terms

**Credential / Auth identity**
: CPA credential identified by `AuthID` and `AuthIndex`. Persisted breaker state is fenced by a non-secret identity fingerprint.

**Model Group**
: Independent Antigravity quota unit: `gemini` or `claude_gpt`.

**Quota Evidence**
: Successful quota probe result containing remaining percentage, observation time, and reset time. Probe errors are unknown evidence, not zero quota.

**Breaker State**
: `uninitialized`, `closed`, `open`, or `half_open` for one `(auth_index, model_group)` key.

**Quota-zero breaker**
: Hard cooldown opened by trusted zero remaining quota with a future reset. It clears only after a fresh positive probe or a successful reset-time half-open request.

**Explicit 429**
: Upstream HTTP 429 whose sanitized error data clearly indicates quota exhaustion. Trusted reset metadata is preferred.

**Generic 429**
: Unstructured HTTP 429. The default policy opens after two failures within 60 seconds, initially for 15 minutes and at most 30 minutes.

**Half-open lease**
: A single-request recovery lease started atomically by Scheduler.Pick. Delayed successes that began before the lease cannot close it.

**Observe mode**
: Computes and reports exclusions but returns `Handled=false`; it must not consume round-robin cursors or half-open leases and must not change request behavior.

**Enforce mode**
: Scheduler owns Antigravity selection and excludes open entries. It requires uniform priority, fresh evidence, unique Scheduler ownership, Home disabled, healthy plugin state, and the Phase 0 deployment gates.

**Native CPA cooldown**
: CPA's own auth/model cooldown. It remains necessary for same-request retries because `usage.handle` is asynchronous in CPA v7.2.141.

## Non-goals

- No priority scoring or `priority=-1` fallback.
- No automatic or manual auth `disabled` mutation.
- No `host.auth.save` from the official runtime path.
- No Management operation that clears all cooldowns without a guarded probe.
- No persistence of tokens, complete auth JSON, request bodies, or complete upstream error bodies.
