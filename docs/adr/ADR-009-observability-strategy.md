# ADR-009: Observability strategy — minimal audit log now, structured instrumentation deferred

**Status:** Accepted  
**Date:** 2026-06

## Context

An MCP server that drives VM creation, SSH execution, and container lifecycle
over an agent's request produces a rich stream of operational events. There are
at least three audiences for this information:

1. **Security audit** — what did the agent do, when, and did it succeed?
2. **Debugging** — why did this `vm_create` take 90 seconds, or why did this
   `exec` fail?
3. **Agent self-monitoring** — can the agent inspect its own recent actions to
   decide what to do next (e.g. "vm_create is timing out, back off")?

The question is how much to build now vs defer.

The Phase-1 build also emits unstructured `log.Printf` calls to stderr for
internal events (SSH dials, provision retries, docker subprocess errors). These
are useful for manual debugging but unsearchable and not parseable by the agent
or tooling.

## Decision

**Phase 1 (current):** Implement a minimal **append-only JSONL audit log** at
`~/.orbit/audit.log` that records every tool call with a nanosecond timestamp,
tool name, call detail, outcome (ok/error), and — critically — *no secret
values*. This is the security-audit and agent-self-monitoring primitive.
Unstructured stderr logging remains for internal debugging.

**Phase 2 (ROADMAP §4):** Defer structured operational logging (`slog`), per-call
metrics in the audit log, `container_stats`/`vm_stats` tools, compose health
rollup, and OpenTelemetry export. These are explicitly placed in ROADMAP §4
with size estimates and implementation notes rather than deferred as vague "TODO"
items.

## Rationale

### Why the audit log is the right Phase-1 primitive

**Security audit is the non-negotiable requirement.** An agent that controls VMs
and containers on a user's machine must produce a tamper-evident record of its
actions. The audit log satisfies this: every tool call is written before the
handler returns, the file is append-only, and no secret values are structurally
permitted (enforced by `envNames()` in container tools and analogous patterns
elsewhere). Without this, there is no way to answer "did the agent leak a
credential?" or "which VM did this key get installed on?"

**Agent self-monitoring via `audit_tail` (ROADMAP §2.6) is the right interface.**
The agent seeing its own recent actions is more useful than raw metrics. An
`audit_tail` tool that returns the last N entries (filterable by tool or time)
lets the agent reason about its own history — "I called `vm_create` three times
in the last minute and it always failed" — without the server needing to
aggregate metrics internally.

**JSONL is the right audit format.** Each line is a self-contained JSON object,
parseable with `jq`, appendable without read-modify-write, and compatible with
log aggregation tools if the file is ever shipped to an external store. A
structured binary format (e.g. protobuf) would be harder to inspect manually and
add no benefit at this scale.

### Why structured instrumentation is deferred

**Premature instrumentation locks in the wrong granularity.** Adding slog fields
to every subsystem before the API is stable encourages instrumenting
implementation details that will change. The right moment is when the tool
interface stabilizes and the latency breakdown is known — then `slog` fields can
be added at the right boundaries (per tool call, not per internal function).

**`log/slog` requires no new dependency and is stdlib.** The cost to add it
later is low: replace `log.Printf` callsites with `slog.Info`/`slog.Debug` and
add a level check at startup. This is a mechanical change, not an architectural
one, and ROADMAP §4.1 notes this explicitly.

**OpenTelemetry is only justified for multi-agent use.** OTel export (ROADMAP
§4.5) adds a non-trivial dependency and runtime overhead that is worthless for a
single-user local server. It belongs behind the multi-agent namespacing work
(ROADMAP §2.9). Instrumenting for it now would be speculative.

**Deferral is only safe because the ROADMAP is concrete.** The observability
items are not vague intentions. Each §4 item has: a specific file location,
a size estimate (S/M/L), a concrete description of what to add, and cross-links
to related work. This makes the deferral a deliberate scope decision rather than
a forgotten TODO.

## Current audit log schema

Every entry written by `internal/audit`:

```json
{
  "ts": "2026-06-15T14:23:01.123456789Z",
  "tool": "container_run",
  "detail": {"image": "postgres:16", "secret_env": ["POSTGRES_PASSWORD"]},
  "ok": true,
  "error": ""
}
```

| Field | Type | Notes |
|---|---|---|
| `ts` | RFC3339Nano | nanosecond precision; monotonic within a session |
| `tool` | string | MCP tool name |
| `detail` | object | tool-specific metadata; secret values structurally excluded |
| `ok` | bool | true if the tool handler returned without error |
| `error` | string | error message if `ok=false`, otherwise `""` |

What is intentionally absent: duration (ROADMAP §4.2), caller identity, secret
values (by design and enforced throughout).

## Consequences

- The audit log is the **only** persistence for operational history. The vault
  holds secret values; docker and orbctl hold container/VM state; the audit log
  holds call history. These are three separate sources of truth with no overlap.
- An agent running `audit_tail` (once implemented) can detect its own failure
  patterns without needing an external monitoring system. This is the correct
  architecture for a single-user, local-first tool.
- When §4.1 (slog) is implemented, stderr output will become structured and
  level-gated. Tools like `ORBIT_LOG_LEVEL=debug orbit` will emit detailed
  internal traces without changing the audit log format.
- The audit log has no rotation or size cap today. For a local developer tool
  with low call volume this is acceptable; if the agent runs thousands of calls
  per day, log rotation should be added before §4.2 (per-call timing) makes
  entries larger.
- Secret exclusion from the audit log is an architectural invariant, not a
  runtime check. It is enforced by: (a) tool handlers calling `envNames()` to
  extract only names, (b) `vm` tools logging only IDs and distro names, (c)
  `cred_*` tools logging only key names. There is no post-hoc scrubbing step
  that could fail.
