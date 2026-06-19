# ADR-010: Vault passphrase management via macOS Keychain with env-var override and auto-generation

**Status:** Accepted  
**Date:** 2026-06

## Status Quo

ADR-004 established that the vault is encrypted with AES-256-GCM under a key
derived from a user passphrase via Argon2id, and that the derived key is held
only in memory — never persisted. It deliberately deferred the question of *how
the passphrase reaches the server at startup*, noting only that it could be
supplied via env var or Keychain. At the time of ADR-004 no mechanism was
implemented; the passphrase was expected to come from the environment. This left
the user experience undefined: on a fresh install with no env var set, the
server would fail to open the vault, or would prompt interactively — neither of
which is acceptable for an MCP server that is typically launched non-interactively
by a host application.

## Goal

The server must be able to open the vault without any manual intervention on
subsequent startups on macOS, while keeping the passphrase retrievable and
replaceable by the operator. On first run with an empty vault and no passphrase
configured, it must bootstrap itself — generating a secure passphrase, storing
it in Keychain, and sealing the vault — without user interaction. The
`ORBIT_PASSPHRASE` environment variable must provide a complete, unsurprising
override path for scripted and CI environments.

## Non-Goal

This ADR does not address passphrase rotation or vault re-key (tracked separately
in ROADMAP §2.10). It does not define a cross-platform Keychain strategy: macOS
is the only target platform for Phase 1, and a different secret-store mechanism
(e.g. libsecret on Linux, DPAPI on Windows) would be a separate ADR. It also
does not cover multi-user or multi-process vault access — the vault is a
single-user, single-process local store.

## Proposed Solution

The vault open sequence follows a three-step priority chain:

1. **`ORBIT_PASSPHRASE` env var** — if set, use it verbatim and skip Keychain
   entirely. This is the escape hatch for CI, Docker, and any context where a
   Keychain is unavailable or undesirable.
2. **macOS Keychain via the `security` CLI** — if the env var is absent, call
   `security find-generic-password -s orbit -a vault -w` to retrieve a
   previously stored passphrase. The `security` binary is part of macOS and
   requires no additional dependency, keeping the build cgo-free in line with
   ADR-001 and ADR-007.
3. **Auto-generation on first run** — if neither source yields a passphrase (the
   Keychain item does not exist), generate a cryptographically random 32-byte
   passphrase encoded as a 64-character lowercase hex string via `crypto/rand`,
   store it in Keychain with `security add-generic-password -s orbit -a vault -w
   <passphrase>`, and proceed to create the vault. Subsequent startups will
   retrieve the passphrase from Keychain via step 2.

**The Keychain is the right primary store for a local developer tool.** macOS
Keychain encrypts items at rest under the user's login keychain, which is itself
sealed by the user's account password and optionally by Secure Enclave on Apple
Silicon. The passphrase is therefore protected at rest without any additional key
management on the server's part. The `security` CLI is a stable, documented
interface that has been present on macOS since 10.3; it imposes no cgo
requirement and no new `go.mod` entry.

**`ORBIT_PASSPHRASE` provides a complete, predictable override.** Operators
running the server in a container, a CI job, or a scripted environment where
Keychain is not available can set this variable and the Keychain path is never
touched. The variable is checked first so that there is no Keychain lookup race,
no error to suppress, and no ambiguity about which source wins.

**Auto-generation eliminates the new-user setup cliff.** Without auto-generation,
a first-time user with no passphrase configured would encounter a startup failure
with no clear remediation path. Generating a 32-byte random passphrase and
sealing it in Keychain is the correct default: the passphrase is never shown to
the user (they don't need to remember it), it is immediately durable across
restarts, and it satisfies the Argon2id KDF security margin established in
ADR-004.

**The derived key is never persisted — the Keychain holds only the passphrase.**
Persisting the derived AES key would eliminate the cost of the Argon2id KDF at
startup, but it would also eliminate the security property that the KDF provides:
an attacker who exfiltrates the Keychain item still cannot read vault contents
without running the KDF themselves. Storing the passphrase and running the KDF
once at startup (≈300ms on a modern laptop, per ADR-004) is the correct
tradeoff: the startup cost is imperceptible to a user and negligible to an MCP
host, and the security model remains sound.

## Alternatives

### Always prompt the user at startup

Prompting via `bufio.Scanner` on stdin is the simplest possible implementation
and requires no platform-specific code. It was rejected because the server is
launched non-interactively by MCP host applications (Claude Desktop, VS Code
extensions, and similar); there is no terminal attached to read from. A blocking
stdin read in this context hangs the server permanently. Even in interactive
contexts, requiring the user to type a passphrase on every server restart is
unacceptable UX for a local developer tool.

### Store the derived AES key in Keychain instead of the passphrase

This would eliminate the Argon2id KDF cost on every startup. It was rejected
because the derived key is the encryption key for every credential in the vault:
persisting it, even in Keychain, means that anyone who can read the Keychain
item (the logged-in user, any process running as that user without a separate
Keychain ACL) can decrypt the vault without the KDF's brute-force resistance.
The passphrase in Keychain + KDF at open time preserves the full security model
at the cost of ~300ms, which is acceptable. This reasoning is also consistent
with ADR-004's explicit statement that "the derived key is never cached to disk."

### Use a dedicated Go Keychain library (e.g. `keychain` or `go-keychain`)

A pure-Go Keychain binding would give more structured error handling and avoid
shelling out. Both available options require cgo, which conflicts with the
cgo-free build posture established in ADR-001 and reinforced in ADR-007. The
`security` CLI is a stable interface with predictable exit codes and output
format, and shelling out to it is the pattern already established elsewhere in
the codebase for OrbStack CLI interactions. The subprocess overhead (fork + exec)
is negligible for an operation that happens once at startup.
