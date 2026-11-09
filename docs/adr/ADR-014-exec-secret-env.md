# ADR-014: Secret environment injection for exec tools

**Status:** Accepted  
**Date:** 2026-10-14

## Context

`cred_get` intentionally returns a secret value to the MCP caller. That is an
escape hatch, but it also means the value enters the model/tool transcript. For
container and compose workloads, ADR-005 introduced `secret_env` so the model
can name a vault key without seeing the underlying secret value.

The same safety property is useful for VM and microVM execution. A command or
script often needs `GITHUB_TOKEN`, `DATABASE_URL`, or another short-lived
credential, but the agent should not need to read the value first.

## Decision

Add `secret_env` and `namespace` to `exec`, `exec_script`, and
`exec_parallel`.

The MCP tool handler resolves each vault key into an environment map, logs only
the environment variable names, and calls the runner with resolved values. The
runner injects those values by sending a small shell wrapper over SSH stdin:

```sh
export TOKEN='...'
exec sh -c 'requested command'
```

The SSH command itself is `sh -s`, so the secret value is not present in the
MCP transcript or the SSH exec request's command line.

## Rationale

**This makes `cred_get` less necessary.** Callers can run workloads that need a
secret without first asking Orbit to print the secret back to the model.

**SSH stdin is safer than argv.** Prefixing the command as
`TOKEN=value command` would be easy, but it can expose the secret in remote
process listings. Sending the export wrapper over stdin avoids that command
line exposure while keeping the implementation portable across normal POSIX
shells.

**The model still controls the command.** If the requested command prints the
environment variable, Orbit cannot prevent that. `secret_env` prevents passive
transcript/argv leakage; it does not sandbox an intentionally exfiltrating
command.

## Consequences

- `exec`, `exec_script`, and `exec_parallel` can consume vault secrets without
  `cred_get`.
- Audit entries include `secret_env` names only, never values.
- The remote process receives the secret in its environment, so process-level
  visibility inside that VM follows normal Unix rules.
