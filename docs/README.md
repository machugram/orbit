# Orbit — Documentation

| Document | What it covers |
|---|---|
| [USAGE.md](USAGE.md) | Tools, resources, configuration, secrets, layout, tests |
| [SYSTEM_DESIGN.md](SYSTEM_DESIGN.md) | Component responsibilities, data flows, security model, concurrency |
| [../orbit-presentation.html](../orbit-presentation.html) | Recruiter / technical walkthrough (open in a browser) |
| [adr/ADR-001](adr/ADR-001-go-over-typescript.md) | Why Go instead of TypeScript |
| [adr/ADR-002](adr/ADR-002-direct-ssh-not-multiplexer.md) | Why direct VM SSH instead of OrbStack's multiplexer |
| [adr/ADR-003](adr/ADR-003-provisioning-script-not-cloud-init.md) | Why a root script instead of cloud-init |
| [adr/ADR-004](adr/ADR-004-encrypted-vault-sqlite.md) | Vault encryption: AES-256-GCM + Argon2id + SQLite |
| [adr/ADR-005](adr/ADR-005-container-tools-vault-injection.md) | Container/Compose tools with vault secret injection |
| [adr/ADR-006](adr/ADR-006-compose-exec-via-container-resolution.md) | Why `compose_exec` resolves a container instead of `docker compose exec` |
| [adr/ADR-007](adr/ADR-007-dependency-minimization.md) | Why `crypto/rand` over `uuid` and a small direct-dep surface |
| [adr/ADR-008](adr/ADR-008-inline-compose-via-stdin.md) | Inline compose YAML piped on stdin (`-f -`) |
| [adr/ADR-009](adr/ADR-009-observability-strategy.md) | Audit log now, structured instrumentation deferred |
| [adr/ADR-010](adr/ADR-010-vault-passphrase-keychain.md) | Vault passphrase via macOS Keychain with env-var override |
| [adr/ADR-011](adr/ADR-011-ssh-host-key-trust-model.md) | Trust-on-connect for loopback SSH; pinning deferred |
| [adr/ADR-012](adr/ADR-012-nested-microvm-ebpf-isolation.md) | Nested QEMU/TCG guests with their own kernel for eBPF |
| [adr/ADR-013](adr/ADR-013-typed-state-schema-and-diagnostics.md) | Typed state tool schemas and `orbit_*` diagnostics |
| [adr/ADR-014](adr/ADR-014-exec-secret-env.md) | Vault secret injection for `exec*` tools |
| [adr/ADR-015](adr/ADR-015-job-shaped-mcp.md) | Job-shaped MCP tools; inventory and diagnostics are resources |

This tree and the ADRs are the source of truth for what shipped.
