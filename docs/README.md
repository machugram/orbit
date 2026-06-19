# Orbit — Documentation

| Document | What it covers |
|---|---|
| [SYSTEM_DESIGN.md](SYSTEM_DESIGN.md) | Component responsibilities, data flows, security model, concurrency |
| [adr/ADR-001](adr/ADR-001-go-over-typescript.md) | Why Go instead of TypeScript |
| [adr/ADR-002](adr/ADR-002-direct-ssh-not-multiplexer.md) | Why direct VM SSH instead of OrbStack's multiplexer |
| [adr/ADR-003](adr/ADR-003-provisioning-script-not-cloud-init.md) | Why a root script instead of cloud-init |
| [adr/ADR-004](adr/ADR-004-encrypted-vault-sqlite.md) | Vault encryption: AES-256-GCM + Argon2id + SQLite |
| [adr/ADR-005](adr/ADR-005-container-tools-vault-injection.md) | Container/Compose tools with vault secret injection |

For the performance and feature backlog see [../ROADMAP.md](../ROADMAP.md).
