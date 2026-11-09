# ADR-015: Job-shaped MCP tools

**Status:** Accepted
**Date:** 2026-11-09

## Context

Orbit registered about forty MCP tools, one verb per resource: `vm_create` /
`list` / `start` / `stop` / `destroy`, the same five for microVMs, six
container tools, six compose tools, five key tools, four credential tools,
and `state_plan` / `apply` / `destroy`. The model had to drive that list
the way a person drives `orbctl` and `docker`. Key ids, container ids, and
`cred_get` were part of the conversation even though the vault, provisioner,
and `secret_env` already knew how to keep those details on the server.

## Decision

The MCP server exposes jobs:

| Tool | Job |
|------|-----|
| `run` | Run a command or script and return stdout, stderr, and exit code |
| `workspace_open` | Open a durable container, VM, or microVM |
| `workspace_close` | Destroy it and delete its SSH key |
| `service_up` | Start a container or compose stack and return endpoints |
| `service_down` | Stop that service |
| `file_put`, `file_get` | Move file bytes. The model sends contents, not a host path |
| `secret_put` | Store a secret by name. Nothing reads the value back |
| `converge` | Create whatever a desired-state document names that is missing |

Inventory and diagnostics are resources: `orbit://workspaces`,
`orbit://secrets`, `orbit://info`, and `orbit://doctor`.

The agent does not choose a vault namespace, an idle TTL, a concurrency
limit, or whether a VM is isolated. Those stay server policy: the agent
namespace, a 30-minute idle reap, eight-wide fan-out, and isolated VMs.

`run` with no target is a throwaway container. `isolation=vm` or
`isolation=microvm` is how the model asks for a machine or its own kernel,
and that throwaway is destroyed after the command. A `target` reuses a
workspace and creates it if it is missing. `secret_env` stays on `run` and
`service_up`.

The Go packages (`vm`, `container`, `exec`, `vault`, `state`) are unchanged
in role. They are still the control plane. Only `internal/tools` speaks MCP.

## Consequences

- A client that called `vm_create`, `exec`, or `cred_get` has to switch to
  the jobs above. There is no compatibility alias.
- The model no longer receives a key id, and it cannot fetch a secret into
  the transcript.
- `converge` does not delete. Removal is `workspace_close` or `service_down`.
- A standalone `docker build` of an inline Dockerfile is not a tool. A
  compose project can still set `build` on `service_up`.
- SSH key rotation is not a tool. Keys are created on open and deleted on close.
