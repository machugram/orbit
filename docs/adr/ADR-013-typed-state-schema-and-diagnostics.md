# ADR-013: Typed state schemas and Orbit diagnostics

**Status:** Accepted  
**Date:** 2026-10-14

## Context

The `state_*` tools originally accepted the desired-state document as
`json.RawMessage`. That let the handler reuse `state.ParseState`, but it gave
MCP clients a poor input schema: the state argument was exposed as raw bytes
instead of an object with `vms`, `containers`, and `composes`. In Codex this
showed up as an unusable schema, so the model could not reliably infer the
shape of the state document.

At the same time, installation debugging had no first-class MCP path. A user
could run `which orbit`, `orbctl status`, or `docker ps` by hand, but an MCP
client could not ask the loaded server which binary, version, vault path, or
audit path it was actually using.

## Decision

`state_plan`, `state_apply`, and `state_destroy` accept a typed
`state.State` argument instead of `json.RawMessage`. Validation lives on
`State.Validate()` so both JSON-file parsing (`ParseState`) and typed MCP tool
arguments share the same checks.

Orbit also exposes two non-mutating diagnostic tools:

- `orbit_info` returns server name, version, platform, vault path, and audit
  path.
- `orbit_doctor` checks the host platform, `orbctl status`, `docker ps`, and
  configured vault/audit directories, then returns structured pass/fail
  results.

## Rationale

**Typed MCP schemas are part of the product surface.** The tool schema is what
an agent sees before it calls a tool. If the schema says "array of integers"
instead of "object with vms/containers/composes", the implementation may be
correct but the agent experience is broken.

**Validation should have one source of truth.** Exporting `State.Validate()` is
intentional: the `tools` package is outside `internal/state`, so it cannot call
an unexported `validate()` method after the MCP SDK has already decoded typed
arguments. Keeping the method on `State` avoids duplicating validation logic in
the tool handlers.

**Diagnostics belong in MCP, not only in shell commands.** Shell commands are
still useful, but `orbit_info` and `orbit_doctor` let the client verify the
loaded server from inside the same control plane that will later create VMs or
containers.

## Consequences

- State tool schemas now advertise `vms`, `containers`, and `composes` as
  object fields.
- Callers that pass malformed state still receive the same validation errors.
- `State.Validate()` is exported as a small package boundary specifically for
  typed MCP arguments and `ParseState`.
- The diagnostic tools must remain read-only and must not log or return secret
  values.
