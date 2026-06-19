# Orbit — System Design

## Overview

Orbit is a single-binary MCP server that gives an AI agent the ability to
create isolated Linux VMs on OrbStack, execute work inside them over SSH, and
store all secrets in a local encrypted vault — with every tool call appended
to an append-only audit log.

The design goal is **minimal attack surface with maximum agent autonomy**: the
agent controls the entire VM lifecycle without ever seeing a raw private key
or needing network access beyond the local OrbStack socket.

---

## Layer Architecture

```
Claude Agent (MCP client)
        │  stdio JSON-RPC (go-sdk StdioTransport)
        ▼
┌──────────────────────────────────────────────────────────┐
│  orbit binary  (cmd/orbit)                                │
│  ┌────────┬────────┬───────┬───────┬────────────────────┐ │
│  │  vm_*  │ key_*  │exec_* │cred_* │ container_*/compose_*│ │
│  │ tools  │ tools  │file_* │ tools │ image_build          │ │
│  └───┬────┴───┬────┴───┬───┴───┬───┴──────────┬───────────┘ │
│      │        │        │       │              │             │
│ vm.Manager vault+keys exec.  (vault)   container.Manager    │
│ Provisioner          Runner            audit (cross-cutting)│
└──────┬────────┬────────┬───────────────────┬────────────────┘
       │        │        │                   │
  orbctl CLI  SQLite   SSH to            docker /
  orb -u root vault.db <vm>.orb.local:22 docker compose
  (OrbStack)  (disk)   (in-VM sshd)      (OrbStack engine)
```

---

## Component Responsibilities

### `internal/vault`
Encrypted credential store and VM registry. Every secret value is
AES-256-GCM encrypted before it touches SQLite. The master key is derived
from a passphrase via Argon2id — it lives only in process memory and is never
written to disk. A `vault_meta` canary row verifies the passphrase at open
time so a wrong passphrase is rejected immediately rather than silently
decrypting to garbage.

Schema: three tables — `vault_meta` (salt + canary), `credentials` (arbitrary
secrets keyed by namespace+key), `vms` (VM records with state and key_id
foreign key).

### `internal/keys`
Ed25519 keypair generation and vault-backed key manager. Private keys are
stored encrypted in the vault and decrypted into memory only for the lifetime
of an SSH connection — they never touch disk unencrypted. Public keys are
derived from the private key on demand rather than stored separately.

### `internal/vm`
Two cooperating types:

**`Manager`** — thin wrapper around `orbctl`. Knows how to `create`, `start`,
`stop`, `destroy`, and poll state. State of truth is always OrbStack; the
vault's `vms` table caches metadata (distro, key_id, timestamps).

**`Provisioner`** — orchestrates the `vm_create` lifecycle: generate a
keypair → orbctl create → run a root script via `orb -u root sh -s` →
set the key on the VM record. On any failure both the VM and key are cleaned
up so nothing leaks. The provisioning script (`distro.go`) is a single
distro-agnostic POSIX sh script that detects the package manager and init
system at runtime — it covers apt, dnf, yum, zypper, pacman, apk, xbps and
systemd, OpenRC, runit.

### `internal/exec`
SSH task runner. Connects directly to `<vm>.orb.local:22` (not OrbStack's
multiplexer) using the per-VM Ed25519 key decrypted from the vault. Exposes:

- `Exec` — single command, returns stdout/stderr/exit code
- `ExecScript` — push a script, execute it, delete it (3 connections today;
  see ROADMAP §1.3)
- `Push` / `Pull` — SFTP file transfer (pull capped at 32 MiB)
- `ExecParallel` — fan-out with a semaphore-bounded WaitGroup

### `internal/container`
Docker + Compose driver for OrbStack's built-in engine — the fast,
ephemeral counterpart to full VMs (containers start in 1–3s). Wraps `docker`
and `docker compose` via an injectable `DockerRunner`. Exposes container
lifecycle (`Run`/`Exec`/`Logs`/`List`/`Stop`/`Remove`), `Build`, and full
multi-service stack management: `ComposeUp` (with per-service `Scale`),
`ComposeDown`, `ComposeLogs`, `ComposePs`, and `ComposeBuild`, from a host
file or inline YAML. Services in a project share a network and resolve each
other by name. `ComposePs` surfaces each service's state, health, and
`<container>.orb.local` host endpoint; `ComposeExec` runs a command in a
service *by service name* by resolving its container via `ComposePs` and
reusing the plain `Exec` path (avoiding an inline-file-on-stdin conflict).

Its distinguishing feature is **vault secret injection**: `RunSpec.SecretEnv`
maps an env-var name to a vault key. The value is read from the vault, placed
only in the docker subprocess's environment, and handed to docker as a bare
`-e NAME` flag — so it never appears in argv, the model context, or the audit
log (see ADR-005). Containers are not mirrored in the vault; `docker ps` is
the source of truth.

`compose_up`, `compose_down`, `compose_logs`, and `compose_build` accept either
a host file path or **inline YAML piped on stdin** via `docker compose -f -`,
so an agent can define and launch a stack without writing host files (see
ADR-008). `ComposeExec` resolves a service's running container via `ComposePs`
and reuses the plain `docker exec` path, avoiding a stdin conflict with the
inline-YAML approach (see ADR-006). Credential IDs use `crypto/rand` hex via
`newID()`, keeping the module's direct dependency list minimal (see ADR-007).

### `internal/audit`
Append-only JSONL log at `~/.orbit/audit.log`. Every tool call is written
with a nanosecond timestamp, tool name, call detail, and success/error. No
secret values are ever logged — the constraint is enforced by design: tool
handlers log only metadata (VM IDs, key labels, namespaces). This is the
Phase-1 observability primitive; structured `slog` logging, per-call timing,
and `container_stats`/`vm_stats` are deferred to ROADMAP §4 (see ADR-009).

### `internal/keychain`
Resolves the vault passphrase at startup: `ORBIT_PASSPHRASE` env var takes
priority, then the macOS Keychain (`security` CLI, service `orbit-vault`).
On first run with neither set, a 32-byte random hex passphrase is generated
and stored in the Keychain.

---

## Data Flow: `vm_create`

```
agent calls vm_create(distro="alpine", name="worker-1")
  │
  ├─ Provisioner.Create()
  │    ├─ keys.Generate("vm-worker-1", "worker-1")
  │    │    └─ Ed25519 keygen → vault.SetCredential (encrypted)
  │    │
  │    ├─ vm.Manager.Create() → orbctl create --isolated worker-1
  │    │    └─ poll orbctl until state=running
  │    │
  │    ├─ Provisioner.provision() (up to 5 retries, 3 min budget)
  │    │    └─ orb -m worker-1 -u root sh -s [distro.go script] agent <pubkey>
  │    │         ├─ apk add openssh
  │    │         ├─ ssh-keygen -A
  │    │         ├─ install authorized_keys
  │    │         ├─ sed /etc/shadow  (unlock account)
  │    │         ├─ rc-update add sshd; rc-service sshd start
  │    │         └─ echo ORBIT_PROVISIONED
  │    │
  │    └─ vm.Manager.SetKey("worker-1", keyID)
  │
  └─ returns {vm_id, state, key_id, ssh_user, distro}

audit: {"tool":"vm_create","detail":{"name":"worker-1","distro":"alpine"},"ok":true}
```

## Data Flow: `exec`

```
agent calls exec(vm_id="worker-1", command="uname -a")
  │
  ├─ exec.Runner.Exec()
  │    ├─ vault.GetVM("worker-1")       → get key_id
  │    ├─ keys.LoadByID(key_id)         → decrypt Ed25519 private key into memory
  │    ├─ TCP dial worker-1.orb.local:22 (15s timeout)
  │    ├─ SSH handshake (key in memory, never touches disk)
  │    ├─ session.Run("uname -a")
  │    └─ return stdout/stderr/exit_code
  │
  └─ key material GC'd when *ssh.Client goes out of scope
```

## Data Flow: `container_run` with secret injection

```
agent calls container_run(image="postgres:16",
                          secret_env={"POSTGRES_PASSWORD": "prod-db-pw"})
  │   (the agent supplies only the env-var NAME and the vault KEY name)
  │
  ├─ container.Manager.Run()
  │    ├─ resolveSecrets("default", {...})
  │    │    └─ vault.GetCredential("default", "prod-db-pw")  → value (in memory)
  │    │    └─ env  = {POSTGRES_PASSWORD: <value>}     (subprocess env only)
  │    │       flags = ["-e", "POSTGRES_PASSWORD"]      (bare, NO value in argv)
  │    │
  │    └─ docker run -d -e POSTGRES_PASSWORD postgres:16
  │           with subprocess env carrying the value
  │           → docker copies POSTGRES_PASSWORD from its env into the container
  │
  └─ returns {container_id, image}

audit: {"tool":"container_run","detail":{"image":"postgres:16",
        "secret_env":["POSTGRES_PASSWORD"]},"ok":true}   ← name only, no value
```

The secret value exists only in the vault (encrypted at rest) and transiently
in the docker subprocess's environment. It is never in a tool argument, a
result, argv (`ps`), or the audit log.

---

## Security Model

| Property | Mechanism |
|---|---|
| Secrets at rest | AES-256-GCM, fresh 12-byte IV per write, 16-byte auth tag |
| Key derivation | Argon2id: salt=32B, m=64MiB, t=3, p=4 — ~300ms on modern hardware |
| Wrong passphrase detection | Encrypted canary in `vault_meta`; rejected at open time |
| Keys in memory | Ed25519 private keys decrypted per SSH dial, never written to disk |
| VM isolation | `orbctl create --isolated` disables macOS file-sharing mounts |
| Audit trail | Every tool call logged; secret values structurally excluded |
| No SSH multiplexer | Direct `<vm>.orb.local:22` preserves per-VM credential isolation |
| Container secret injection | `secret_env` → bare `docker -e NAME`; value in subprocess env only, never argv/model/audit |
| Passphrase bootstrap | macOS Keychain or env var; auto-generated 32-byte hex on first run |

---

## Injectable Interfaces

All external dependencies are injectable for testing without OrbStack:

| Interface | Default | Test double |
|---|---|---|
| `vm.CmdRunner` | `orbctl` subprocess | `fakeOrbctl` func |
| `vm.VMExecer` | `orb -u root sh -s` | `fakeExecer` struct |
| `exec.TargetResolver` | `<vmID>.orb.local:22` | custom func |
| `container.DockerRunner` | `docker` subprocess | `recorder` struct |

Unit and integration tests run with no OrbStack dependency. Only
`cmd/smoke` touches a real OrbStack instance.

---

## Concurrency

- **SQLite**: single write connection (`SetMaxOpenConns(1)`) with WAL mode.
  Reads are fast but serialize behind writes. See ROADMAP §1.5 for the
  read-pool improvement path.
- **`exec_parallel`**: WaitGroup + buffered channel semaphore, default
  parallelism 8 (`DefaultParallelism`). Each goroutine opens its own SSH
  connection.
- **MCP server**: `go-sdk` handles concurrent tool calls from the client.
  The vault's single connection is the natural serialization point.
