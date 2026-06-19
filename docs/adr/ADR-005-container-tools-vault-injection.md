# ADR-005: Container tools with vault env-injection

**Status:** Accepted  
**Date:** 2026-06

## Context

The original design gave the agent isolated VMs via `vm_create`. VMs are the
right tool for full-OS work and strong isolation, but they cost ~30–60s to
provision (package install, sshd setup — see ADR-003). Many agent tasks are
ephemeral: run a build, test an API, execute a script, stand up a throwaway
service. For those, a full VM is overkill.

OrbStack ships a Docker engine that is a drop-in replacement for Docker
Desktop — `docker` and `docker compose` work with no extra configuration.
Exposing it to the agent gives a fast path (containers start in 1–3s) that
complements the VM path.

The question was not *whether* to wrap Docker — that part is mechanical — but
how to handle **secrets**. An agent frequently needs to give a container a
token, password, or API key. Passing it as `docker run -e NAME=value` puts the
secret in the process argv (visible to any `ps` on the host) and, more
importantly, forces the value to pass through the model's context and the
audit log.

## Decision

Add a `container` subsystem (`internal/container`) and a `container_*` /
`compose_*` / `image_build` tool group. For secrets, add a `secret_env`
parameter that maps an environment variable name to a **vault key**. Orbit:

1. reads the secret value from the vault at run time (`vault.GetCredential`),
2. places it in the docker **subprocess's environment** (not argv),
3. passes docker a **bare `-e NAME` flag** (no value), which tells docker to
   read `NAME` from its own environment and forward it into the container.

The model supplies only the env-var name and the vault key name. The secret
value is never serialized into a tool argument, never returned in a result,
and the audit log records only the names (`envNames()` in
`internal/tools/container_tools.go`).

## Rationale

**The bare-`-e` mechanism is the crux.** `docker run -e POSTGRES_PASSWORD`
(without `=value`) instructs Docker to copy `POSTGRES_PASSWORD` from the
runner process's environment. Verified directly:
`DEMO_SECRET=x docker run --rm -e DEMO_SECRET alpine sh -c 'echo $DEMO_SECRET'`
prints `x` while `x` never appears on the command line. This lets Orbit keep
the value entirely out of argv. A real end-to-end test
(`TestE2ERealDocker`, gated behind `ORBIT_DOCKER_E2E`) confirms the value
reaches a live container, and `TestRunSecretEnvNeverHitsArgv` asserts it never
appears in the docker arguments.

**Reuses the existing vault.** Secrets already live in the AES-256-GCM vault
(ADR-004) via the `cred_*` tools. `secret_env` is just a second consumer of
the same encrypted store — no new secret storage, no new trust boundary.

**Injectable runner, consistent with `vm`.** The `DockerRunner` interface
mirrors `vm.CmdRunner`: the default shells out to `docker`, tests substitute a
recorder. Unit tests need no Docker daemon; only the gated e2e test does. This
matches the project-wide pattern (ADR-003's `VMExecer`, the SSH
`TargetResolver`).

**Inline definitions via stdin.** `compose_up` accepts inline YAML and
`image_build` accepts an inline Dockerfile, both piped on stdin (`-f -`), so
the agent can define a stack or image without first writing a host file. A
host file path is also supported for existing projects.

**Containers are not tracked in the vault.** Unlike VMs, container state is
read straight from `docker ps`. Docker is the source of truth; adding a
mirror table would be redundant complexity.

## Consequences

- The tool count grows from 19 to 32. The first container increment added 10
  tools (`container_*`, lifecycle + `compose_up/down/logs`, `image_build`); a
  follow-up added the stack-management trio `compose_ps` / `compose_exec` /
  `compose_build` plus `compose_up`'s `scale`. `TestAllSpecToolsRegistered`
  enumerates the full set.
- `compose_exec` does **not** run `docker compose exec`. Piping an inline
  compose file on stdin (`-f -`) conflicts with the exec'd command's own
  stdin, so `compose_exec` instead resolves the service's container via
  `compose_ps` and reuses the existing plain `docker exec` path. This sidesteps
  the stdin conflict and avoids materializing a temp file.
- `compose_ps` surfaces each service's `<container>.orb.local` endpoint, so the
  agent can reach a running service from the host without parsing port maps.
  A project-level health rollup is left for ROADMAP §4.4.
- Containers share the OrbStack VM kernel, so they are weaker isolation than
  `orbctl --isolated` VMs. This is documented: untrusted code → `vm_create`;
  builds/tests/ephemeral services → containers.
- Literal (non-secret) `env` values still pass through argv as `NAME=value`;
  this is acceptable because the model supplied them directly. Only
  `secret_env` gets the argv-free treatment.
- The error from the default runner is scrubbed of `inv.Env` so a failing
  `docker` invocation cannot echo a secret into an error string.
- `--isolate-network` and resource limits are not yet surfaced for containers
  (a natural follow-up, paralleling ROADMAP §2.3 for VMs).
