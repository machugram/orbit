# ADR-008: Inline compose definitions via stdin (`-f -`)

**Status:** Accepted  
**Date:** 2026-06

## Context

The `compose_up`, `compose_down`, `compose_logs`, and `compose_build` tools must
accept a compose project definition. There are three plausible sources:

1. **Host file path** — the agent supplies `/path/to/docker-compose.yml`.
2. **Inline YAML string** — the agent passes the compose file content directly
   as a tool argument; the server writes it to a temp file and passes the path
   to docker.
3. **Inline YAML via stdin** — the server pipes the content to `docker compose`
   on stdin using the `-f -` flag.

The primary use case driving this decision is **ephemeral, agent-defined stacks**
— a model that needs to spin up a two-service stack (e.g. a web server + a
database) for a task it is currently working on. The model cannot be expected to
have pre-written compose files at known host paths; it needs to define the stack
inline in the same tool call that starts it.

## Decision

Accept both a **host file path** (`file` parameter) and an **inline YAML
string** (`yaml` parameter) in the `ComposeSpec`. When `yaml` is provided, pass
it to `docker compose` via **stdin** using `docker compose -f - <subcommand>`.
When only `file` is provided, pass the path directly with `-f <path>`. Exactly
one must be set; the manager validates this before invoking docker.

Never write a temp file. The compose YAML flows from the tool argument directly
into the subprocess stdin pipe.

## Rationale

**Temp files violate the no-ephemeral-disk-artifact principle.** The
provisioner (ADR-003) and compose_exec (ADR-006) both avoided temp files for the
same reason: they require cleanup logic, are racy under concurrent calls, and can
leak if the server crashes between write and delete. An inline YAML approach that
bypasses the filesystem has none of these failure modes.

**`-f -` is a stable Docker feature.** `docker compose -f -` is documented,
widely used in CI pipelines for exactly this pattern (injecting config via
shell), and works the same way across Docker Compose v1 and v2. It is not an
obscure flag.

**Stdio is already the server's natural I/O mode.** The MCP server communicates
over stdio; the subprocess infrastructure in `internal/container` already handles
stdin injection (via `Invocation.Stdin []byte`). Piping YAML on stdin is
idiomatic within the existing design.

**Host file path is still supported.** Existing projects with a checked-in
`docker-compose.yml` should not be forced to re-express their configuration
inline. The `file` parameter makes `compose_up` usable as a thin wrapper around
an on-disk project with no extra copying.

**The stdin approach constrains `compose_exec`.** Using `-f -` for inline
definitions means the compose file is consumed on stdin, leaving stdin
unavailable for the exec'd command (see ADR-006). This is the known
consequence: `compose_exec` resolves the service via `compose_ps` and calls
`docker exec` directly, fully sidesteps the constraint, and uses no stdin.

## Consequences

- `compose_up`, `compose_down`, `compose_logs`, `compose_build` each accept
  exactly one of `{file, yaml}`. The manager returns an error if both or neither
  is set.
- Inline YAML is held in memory for the duration of the subprocess call — a
  typical compose file is a few kilobytes, well within process memory budgets.
- The `yaml` parameter is logged in the audit log as its full content (it is
  not a secret — it is configuration). Secret env-var **values** are still never
  logged; only their names appear via `envNames()`.
- `docker compose -f -` does not preserve a project directory, so relative
  `build: context:` paths in the YAML are evaluated against the current working
  directory of the orbit process (typically the user's home directory). Inline
  YAML stacks should use absolute paths or pre-built image references for build
  contexts, or use the `file` parameter instead.
- Switching a running project from inline to file (or vice versa) changes the
  project identifier that docker compose uses. Docker compose identifies projects
  by name (the `project` field in `ComposeSpec`), so as long as the project name
  is consistent, `compose_up` and `compose_down` will find the same containers
  regardless of which parameter was used.
