# Security Policy

Orbit is a local MCP server that holds SSH private keys and arbitrary secrets
in an encrypted vault. Please treat reports about vault crypto, key handling,
SSH trust, and secret leakage (audit log, argv, tool results) as in-scope.

## Reporting a vulnerability

Do **not** file a public GitHub issue for an unreleased vulnerability.

Use [GitHub private vulnerability reporting](https://github.com/machugram/orbit/security/advisories/new)
so the report stays out of the public issue tracker until a fix is ready.

If private reporting is unavailable, email the maintainer listed on the
[GitHub profile](https://github.com/machugram) with a subject that starts
`[orbit security]`.

## What this project does and does not promise

- **Local-only.** Orbit is meant to run on the same Mac as OrbStack. It is not
  a multi-tenant or remotely exposed service.
- **Secrets at rest.** Vault values are AES-256-GCM under an Argon2id-derived
  key. The passphrase lives in the macOS Keychain or `ORBIT_PASSPHRASE`.
- **Keys in memory.** Private keys are decrypted only for the lifetime of an
  SSH connection and are never written to disk unencrypted.
- **`cred_get` returns values to the caller.** That includes the model. Use
  `secret_env` on `container_*` / `compose_*` when a secret must reach a
  process without entering the transcript. `cred_list` / `key_list` return
  names and metadata only.
- **SSH host keys are trust-on-connect.** Orbit uses `InsecureIgnoreHostKey`
  because traffic stays on loopback (`.orb.local` or a carrier-local port).
  TOFU pinning is planned and is required before any target is reachable
  off-loopback. See [ADR-011](docs/adr/ADR-011-ssh-host-key-trust-model.md).
- **Audit log.** Every tool call is appended to `~/.orbit/audit.log`. Secret
  *values* are excluded by design; names and metadata are not.

## Supported versions

There is no long-term support window yet. Reports against the latest `main`
are welcome.
