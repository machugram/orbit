# Orbit — System Design

## Overview

Orbit is a single-binary MCP server that gives an AI agent the ability to
create isolated Linux VMs on OrbStack, run containers and compose stacks,
boot nested microVMs for kernel-level work, reconcile a declared desired
state, execute work over SSH, and store all secrets in a local encrypted
vault — with every tool call appended to an append-only audit log.

The design goal is **minimal attack surface with maximum agent autonomy**: the
agent controls the entire VM lifecycle without ever seeing a raw private key
or needing network access beyond the local OrbStack socket.

---

## Layer Architecture

```
Claude Agent (MCP client)
        │  stdio JSON-RPC (go-sdk StdioTransport)
        ▼
┌─────────────────────────────────────────────────────────────────┐
│  orbit binary  (cmd/orbit)                                       │
│  run  workspace_open/close  service_up/down  file_put/get        │
│  secret_put  converge          orbit://info doctor workspaces    │
│                                                                  │
│  vm.Manager + Provisioner   vault + keys   exec.Runner           │
│  container.Manager          microvm.Manager   state.Reconciler   │
│  diagnostics + audit (cross-cutting)                             │
└──────┬──────────────┬────────────┬──────────────┬───────────────┘
       │              │            │              │
  orbctl / orb    SQLite       SSH to         docker /
  (OrbStack)      vault.db     <vm>.orb.local:22  compose
                               or <carrier>.orb.local:<port>
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

Schema: four tables — `vault_meta` (salt + canary), `credentials` (arbitrary
secrets keyed by namespace+key), `vms` (VM records with state and key_id
foreign key), `microvms` (nested guests: carrier, image, forwarded SSH port,
key_id).

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
SSH task runner. How an id maps to an endpoint and key is delegated to an
`exec.Resolver`. Production wiring chains `microvm.NewResolver` in front of
the vault resolver, so `run`, `file_put`, and `file_get` drive
both OrbStack VMs (`<vm>.orb.local:22`) and nested guests
(`<carrier>.orb.local:<port>`) by name. Connections use the per-target
Ed25519 key decrypted from the vault — never OrbStack's multiplexer.
Exposes:

- `Exec` — single command, returns stdout/stderr/exit code
- `ExecScript` — push a script, execute it, delete it (3 connections today;
  see ROADMAP §1.3)
- `Push` / `Pull` — SFTP file transfer (pull capped at 32 MiB)
- `ExecParallel` — fan-out with a semaphore-bounded WaitGroup

`Exec`, `ExecScript`, and `ExecParallel` also support vault-backed
`secret_env`. Tool handlers resolve vault keys to values, log only env-var
names, and the runner injects the values over SSH stdin via `sh -s`, so they
do not appear in the MCP transcript or the SSH command argv (ADR-014).

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

### `internal/microvm`
Nested QEMU/TCG guests running inside a carrier OrbStack VM, each with its
**own kernel** so eBPF and other kernel-level work cannot touch the shared
OrbStack kernel (see ADR-012). `Manager` stages an image on the carrier
(default `alpine` direct-kernel; opt-in `ubuntu` cloud image), boots QEMU,
and records the guest in the vault `microvms` table. The carrier must be a
Debian/Ubuntu machine. There is no `/dev/kvm` inside OrbStack, so the guest
is software-emulated and slow; that is accepted.

A `microvm.Resolver` maps a microVM id to `<carrier>.orb.local:<ssh_port>`
and the guest's vault-held key, falling through to the vault VM resolver for
everything else.

### `internal/state`
Declarative desired-state reconciliation over the imperative VM and
container primitives. A JSON document lists `vms`, `containers`, and
`composes`; `Reconciler.Plan` diffs that against live resources,
`Apply` creates/runs/ups what is missing (never deleting unlisted
resources), and `Destroy` tears down only what the document names.
Nested microVMs are out of scope for this document — they stay on
`workspace_open` / `workspace_close` with `kind=microvm`.

The MCP handlers receive a typed `state.State`, so clients see a real object
schema with `vms`, `containers`, and `composes`. `State.Validate()` is exported
so the typed tool path and file/JSON path share the same validation rules
(ADR-013).

### `internal/tools`
The MCP surface is nine jobs (`run`, `workspace_open`, `workspace_close`,
`service_up`, `service_down`, `file_put`, `file_get`, `secret_put`,
`converge`) plus resources. `orbit://info` reports version, platform, vault
path, and audit path. `orbit://doctor` checks the macOS/OrbStack/Docker/path
prerequisites. `orbit://workspaces` and `orbit://secrets` are the inventory.
See ADR-015. Typed `converge` arguments still share `State.Validate()` with
file parsing (ADR-013).

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

## Data Flow: `workspace_open` (vm)

```
agent calls workspace_open(kind="vm", distro="alpine", name="worker-1")
  │
  ├─ Provisioner.Create()
  │    ├─ ensureBase("orbit-base-alpine")
  │    │    └─ (cold) Create + full provision once per distro
  │    │
  │    ├─ keys.Generate("vm-worker-1", "worker-1")
  │    │    └─ Ed25519 keygen → vault.SetCredential (encrypted)
  │    │
  │    ├─ vm.Manager.Clone(orbit-base-alpine → worker-1)
  │    │    └─ orbctl clone; poll until running
  │    │
  │    ├─ Provisioner.injectKey() (authorized_keys only; sshd already present)
  │    │    └─ orb -m worker-1 -u root sh -s [keyInjectScript] agent <pubkey>
  │    │
  │    └─ vm.Manager.SetKey("worker-1", keyID)
  │
  └─ returns {name, kind, state, ssh_user, public_key, distro}

Warm bases (orbit-base-*) cannot be destroyed via workspace_close.
Ephemeral VMs (`ephemeral` + idle TTL) are reaped in-process; `last_used_at`
updates on successful exec/file ops.
Agent namespace (`ORBIT_AGENT_NAMESPACE`) soft-scopes VM/microVM rows; bases are shared.

audit: {"tool":"workspace_open","detail":{"name":"worker-1","distro":"alpine"},"ok":true}
```

## Data Flow: `run` on a VM

```
agent calls run(target="worker-1", command="uname -a")
  │
  ├─ exec.Runner.Exec()
  │    ├─ resolver.Resolve("worker-1")
  │    │    ├─ microvm.Resolver: not a microVM → chain to vault resolver
  │    │    └─ vaultResolver: GetVM → SignerByID → worker-1.orb.local:22
  │    ├─ TCP dial + SSH handshake (key in memory, never touches disk)
  │    ├─ session.Run("uname -a")
  │    └─ return stdout/stderr/exit_code
  │
  └─ key material GC'd when *ssh.Client goes out of scope
```

A microVM id takes the other branch: `GetMicroVM("bpf-lab")` → guest key →
`<carrier>.orb.local:<ssh_port>`. Same runner, no extra tools.

## Data Flow: `service_up` with secret injection

```
agent calls service_up(image="postgres:16", name="db",
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
  └─ returns {name, host, kind}

audit: {"tool":"service_up","detail":{"image":"postgres:16",
        "secret_env":["POSTGRES_PASSWORD"]},"ok":true}   ← name only, no value
```

The secret value exists only in the vault (encrypted at rest) and transiently
in the docker subprocess's environment. It is never in a tool argument, a
result, argv (`ps`), or the audit log.

## Data Flow: `converge`

```
agent calls converge(state={vms:[{name:"w1",distro:"alpine"}], ...})
  │
  ├─ MCP SDK decodes typed state.State
  ├─ State.Validate()    → validate names, distros, compose file-or-yaml
  ├─ Reconciler.diff()   → list live VMs/containers; emit create/run/up or noop
  └─ Reconciler.execute()
       ├─ missing VM        → Provisioner.Create (same path as workspace_open)
       ├─ missing container → container.Manager.Run
       ├─ compose project   → container.Manager.ComposeUp
       └─ already present   → noop
```

`converge` does not delete. `workspace_close` and `service_down` remove a
workspace or service; closing a VM purges its key.

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
| No SSH multiplexer | Direct `<vm>.orb.local:22` (or `<carrier>.orb.local:<port>` for microVMs) preserves per-target credential isolation |
| Nested kernel isolation | microVM guests run under QEMU TCG with their own kernel; eBPF cannot reach the shared OrbStack kernel (ADR-012) |
| Container secret injection | `secret_env` → bare `docker -e NAME`; value in subprocess env only, never argv/model/audit |
| Exec secret injection | `secret_env` → SSH stdin wrapper; value in remote environment only, never model/audit/SSH argv |
| Passphrase bootstrap | macOS Keychain or env var; auto-generated 32-byte hex on first run |
| MCP diagnostics | `orbit://info` / `orbit://doctor` are read-only and return paths/status only, never secret values |
| Secret reads | There is no tool that returns a secret value. `orbit://secrets` lists names |

---

## Injectable Interfaces

All external dependencies are injectable for testing without OrbStack:

| Interface | Default | Test double |
|---|---|---|
| `vm.CmdRunner` | `orbctl` subprocess | `fakeOrbctl` func |
| `vm.VMExecer` | `orb -u root sh -s` | `fakeExecer` struct |
| `exec.Resolver` | microVM resolver chained to vault resolver (`<vm>.orb.local:22`) | custom `Resolver` / `WithTarget` |
| `container.DockerRunner` | `docker` subprocess | `recorder` struct |

Unit and integration tests run with no OrbStack dependency. Only
`cmd/smoke` touches a real OrbStack instance;
`ORBIT_DOCKER_E2E=1 go test ./internal/container/` is the opt-in real-Docker
path for vault secret injection.

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
