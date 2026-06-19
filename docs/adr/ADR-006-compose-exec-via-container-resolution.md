# ADR-006: `compose_exec` via container-name resolution instead of `docker compose exec`

**Status:** Accepted  
**Date:** 2026-06

## Context

`compose_exec` must run a shell command inside a named service — e.g. `compose_exec("api", "ls /app")`. The natural implementation is:

```sh
docker compose exec -f - api ls /app
```

where the `-f -` flag tells compose to read the project definition from stdin, allowing inline YAML (the same pattern used successfully for `compose_up`, `compose_down`, and `compose_logs`).

The problem: `docker compose exec` inherits its stdin for the **command's own input**, and `-f -` simultaneously requires stdin to deliver the compose file. Docker reads from stdin for the file first, but once the file is consumed the process blocks waiting for stdin on behalf of the exec'd command — or, when stdin is closed before the command reads it, the behavior is undefined and varies between Docker versions.

Attempting to work around this by writing a temp file introduces a new class of problem: temp files on the host that must be cleaned up, and a race if the server handles concurrent `compose_exec` calls. We already made the deliberate choice to avoid temp files in the provisioner (ADR-003).

## Decision

`ComposeExec` does **not** call `docker compose exec`. Instead:

1. Call `ComposePs(ctx, spec)` to get the list of running containers for the project.
2. Find the entry whose `Service` field matches the requested service name.
3. Call the existing `Exec(ctx, containerName, command)` method on that container's resolved name.

The `compose_exec` MCP tool description is updated to say "Run a shell command inside a project's service by service name (resolves the running container for you)".

## Rationale

**Eliminates the stdin conflict entirely.** `docker exec <containerName> <cmd>` does not need a compose file at all — the container is already running and identified by name. stdin is unconflicted.

**Reuses existing, tested infrastructure.** `Exec` in `internal/container/manager.go` is already used by `container_exec` and its behavior is covered by unit and e2e tests. `ComposeExec` is a thin resolution step on top of proven code.

**No temp files.** The design stays consistent with the project-wide no-temp-file principle (see ADR-003). Container identity is resolved in memory from `ComposePs` output; nothing touches disk.

**Correct semantics for scaled services.** When a service has multiple replicas (`scale: {api: 3}`), `ComposePs` returns all of them. `ComposeExec` targets the first `running` one, which is the same choice `docker compose exec` would make by default (it picks one replica). Future work could expose a replica index.

**`ComposePs` is a prerequisite for `compose_exec` anyway.** An agent calling `compose_exec` likely wants to know the service is running first. The internal `ComposePs` call provides that health gate for free.

## Consequences

- `ComposeExec` requires the project to be up (services must appear in `compose ps`). Calling it against a stopped project returns a "service not found" error — the correct behavior for "exec into a running service".
- Resolution is by exact `Service` name match. Service names in Docker Compose are the keys in the `services:` block, not the generated container names (e.g. service `api` → container `myproject-api-1`). This is the user-facing name and the right abstraction level.
- `docker exec` does not inherit the compose project's network DNS aliases. However, services within a running stack already use the compose-created network, and `exec` into a container in that network sees the same DNS. This is only relevant if a command inside the container refers to other services by name — which works correctly.
- There is no `docker compose exec --no-TTY` flag concern: `docker exec` defaults to no-TTY, which is correct for programmatic use.
