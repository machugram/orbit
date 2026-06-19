# ADR-001: Go instead of TypeScript

**Status:** Accepted  
**Date:** 2025-06

## Context

The original spec (ORBIT_SPEC.md) assumed a TypeScript/Node.js implementation,
following the conventional MCP server pattern. Before implementation began,
the language choice was reconsidered.

The core operations are:
1. Argon2id key derivation and AES-256-GCM encryption
2. Ed25519 keypair generation and OpenSSH PEM serialization
3. SSH client connections with in-memory private keys
4. SQLite for the credential store
5. Subprocess invocation of `orbctl` and `orb`

## Decision

Implement in **Go**.

## Rationale

**Private keys never touch disk.** The spec's TypeScript design required
writing decrypted private keys to a temp file so the Node `ssh2` library or
system `ssh` binary could use them. `golang.org/x/crypto/ssh` accepts an
`ssh.Signer` constructed directly from in-memory key material — no temp file,
no disk exposure window.

**No native addons.** Argon2id in Node requires `argon2` (a native C addon).
`modernc.org/sqlite` is pure Go, eliminating CGO entirely. The result is a
single, statically-linked binary with no `node_modules`, no native module
rebuild step, and no platform-specific addon compatibility issues.

**Stdlib coverage.** `golang.org/x/crypto` provides Argon2id, AES-GCM,
Ed25519, and the SSH client in one dependency group maintained by the Go team.
Equivalent Node coverage requires at least three separate packages with
separate maintainers.

**Single binary deployment.** `go build -o orbit ./cmd/orbit` produces a
16 MB self-contained binary. Registration is `claude mcp add orbit --
/path/to/orbit`. No runtime, no package manager, no PATH manipulation.

## Consequences

- The spec's TypeScript project layout (§7) is not followed; the Go layout
  is documented in README.md instead.
- Contributors need Go 1.25+, not Node.
- The SQLite driver (`modernc.org/sqlite`) is slower than `better-sqlite3`
  under high write load, but the credential store has negligible write volume
  so this tradeoff is acceptable (see ROADMAP §1.7).
- Argon2id cost (~300ms) is paid once at server start, not per operation.
  This is intentional and the derived key must never be persisted.
