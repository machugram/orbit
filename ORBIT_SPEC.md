# Orbit — Autonomous Agent Infrastructure on OrbStack
## Technical Specification v0.1

---

## 0. Vision

An agent runtime where Claude (or any MCP-compatible agent) can:
1. **Generate and own its own SSH keypairs** — stored in a purpose-built credential store
2. **Spin up isolated OrbStack Linux VMs on demand** — provisioned with its own keys
3. **Run fast container and compose workloads** against OrbStack's Docker engine
4. **Boot nested microVMs with their own kernel** for eBPF and kernel-level work
5. **Declare desired local infrastructure state** and let Orbit plan/apply/destroy it
6. **Execute arbitrary work** inside VMs or microVMs — code, builds, tests, chaos, CI
7. **Tear down resources cleanly** — with optional result capture
8. **Persist nothing sensitive on the host** — the credential store is the single source of truth

This is a local-first, privacy-preserving alternative to E2B, Daytona, Modal, or GitHub Actions — with sub-minute cold start, zero egress cost, and full isolation from your Mac.

---

## 1. System Components

```
┌─────────────────────────────────────────────────────────┐
│                    Claude / MCP Client                  │
│             (Claude Desktop · Claude Code)              │
└───────────────────────┬─────────────────────────────────┘
                        │ MCP stdio/SSE
                        ▼
┌─────────────────────────────────────────────────────────┐
│                  Orbit MCP Server                       │
│                                                         │
│  VM + key + exec      containers + compose             │
│  nested microVMs      desired state + diagnostics       │
└─────────┬─────────────────┬─────────────────┬──────────┘
          │                 │                 │
          ▼                 ▼                 ▼
   OrbStack VMs      SQLite AES-GCM      Docker / QEMU
   (isolated)        vault + audit       direct SSH
```

### 1.1 Orbit MCP Server
The core process. Exposes MCP tools to the agent. Written in **Go**. Runs as a local stdio MCP server.

### 1.2 VM Manager
Wraps `orbctl` and `orb` CLI. Handles:
- VM lifecycle: create, start, stop, destroy
- Multi-distro provisioning via a root script run through `orb`
- VM inventory (name → state → assigned keypair)
- Isolation enforcement (no macOS file sharing mounts by default)

### 1.3 Key Manager / Credential Store
The agent's own credential vault. Handles:
- Ed25519 keypair generation in Go
- AES-256-GCM encrypted SQLite store
- Key lifecycle: generate → associate with VM → revoke → rotate
- No keys ever hit `~/.ssh` unless explicitly promoted by the user

### 1.4 Task Runner
Handles SSH command execution into VMs:
- Opens SSH session using the VM's registered keypair (from cred store)
- Streams stdout/stderr back to the agent
- Supports: single commands, multi-step scripts, file upload/download (SFTP)
- Timeout and kill support

---

## 2. MCP surface exposed to the agent

Tools are jobs. Inventory and diagnostics are resources. See ADR-015.

| Tool | Job |
|------|-----|
| `run` | Run a command or script. No target: throwaway container (seconds), removed after. `isolation=vm` (about a minute) or `microvm` (minutes, needs `carrier`) when the work needs a machine or its own kernel. `target` reuses a workspace, creating it if missing. `targets` runs one command on several existing workspaces. |
| `workspace_open` | Open a durable container, VM, or microVM. Returns name, address, and the SSH public key. The private key stays in the vault. |
| `workspace_close` | Destroy that workspace and delete its SSH key. |
| `service_up` | Start one container (`image`) or a compose project (`project` + `yaml` or `file`) and return endpoints. `build` builds compose images first. |
| `service_down` | Stop a container by `name` or a compose project by `project`. |
| `file_put` / `file_get` | Write or read file bytes in a workspace. Text is `content`; other bytes are `content_base64`. |
| `secret_put` | Store a secret under a name. The value is not returned. |
| `converge` | Create any VMs, containers, and compose stacks named in a typed `state` document that are missing. Does not delete. |

`run` and `service_up` take `secret_env` (environment variable name → vault secret name). Orbit injects the value and does not return it.

| Resource | What it returns |
|----------|-----------------|
| `orbit://info` | Server version, platform, vault path, audit path |
| `orbit://doctor` | macOS, OrbStack, Docker, and path readiness |
| `orbit://workspaces` | Live workspaces (warm base images omitted) |
| `orbit://secrets` | Secret names, never values |

The `converge` state object exposes `vms`, `containers`, and `composes` directly. See ADR-013.

---

## 3. Credential Store Design

### Storage
- **Backend:** SQLite — single file, portable, zero server
- **Location:** `~/.orbit/vault.db` (configurable via env)
- **Schema:**

```sql
CREATE TABLE credentials (
  id          TEXT PRIMARY KEY,        -- UUID
  namespace   TEXT NOT NULL DEFAULT 'default',
  key         TEXT NOT NULL,
  ciphertext  BLOB NOT NULL,           -- AES-256-GCM encrypted value
  iv          BLOB NOT NULL,           -- 12-byte nonce
  tag         BLOB NOT NULL,           -- 16-byte GCM auth tag
  label       TEXT,
  vm_id       TEXT,                    -- FK to vms.id if key belongs to a VM
  created_at  INTEGER NOT NULL,
  rotated_at  INTEGER,
  UNIQUE(namespace, key)
);

CREATE TABLE vms (
  id          TEXT PRIMARY KEY,        -- matches OrbStack machine name
  distro      TEXT NOT NULL,
  state       TEXT NOT NULL,           -- creating|running|stopped|destroyed
  key_id      TEXT,                    -- FK to credentials.id
  created_at  INTEGER NOT NULL,
  destroyed_at INTEGER
);
```

### Encryption
- **Master key:** Derived from a passphrase via Argon2id; passphrase comes from `ORBIT_PASSPHRASE` or macOS Keychain
- **Per-secret:** AES-256-GCM with a fresh 12-byte IV per write
- **Private keys:** stored as encrypted blobs and decrypted into memory only
- **Public keys:** derived from private keys on demand

### Key Derivation
```
passphrase ──► Argon2id(salt=random 32B, m=65536, t=3, p=4)
                    │
                    ▼
              masterKey (32B)
                    │
               used for all AES-GCM encrypt/decrypt in vault
```

---

## 4. VM Provisioning Flow

When a VM workspace is opened:

```
1. key_generate(label="vm-{name}-{ts}")
     → Ed25519 keypair created in memory
     → Private key → encrypted → stored in vault
     → Public key → derived on demand for provisioning

2. Create or clone an isolated OrbStack machine.

3. Run the root provisioning script via `orb -u root sh -s`.

4. Inject the agent user's authorized key and confirm sshd is available.

5. Write VM record to vault DB: { id, distro, state: running, key_id }.

6. Return { name, kind, state, ssh_user, public_key, distro } to the agent. The key id stays inside the vault.
```

---

## 5. Task Execution Flow

When `run` targets a VM or microVM:

```
1. Look up vm_id in vault DB → get key_id
2. Decrypt private key from vault (requires master key in memory)
3. Build an in-memory `ssh.Signer`
4. SSH directly to `<vm>.orb.local:22` as `agent`
5. Stream stdout/stderr
6. Return { stdout, stderr, exit_code }
```

The private key never touches disk unencrypted. Nested microVMs use the same
runner through `<carrier>.orb.local:<forwarded-port>`.

---

## 6. Project Structure



---

## 9. Use Case Implementations

### 9.1 Ephemeral Code Execution Sandbox
```
Agent: run(isolation="vm", command="curl https://... | python3")
```
Orbit creates the VM, runs the command, destroys the VM, and deletes the key. About a minute.

### 9.2 Multi-Distro Compatibility Test
```
Agent: workspace_open × 4 (ubuntu, fedora, arch, debian)
Agent: run(targets=[...], command="bash /tmp/test.sh")
Agent: diff results
Agent: workspace_close × 4
```
All four run concurrently. Results compared. VMs gone in minutes.

### 9.3 Stateful Dev Environment
```
Agent: workspace_open(kind="vm", distro="ubuntu", name="dev-main")
Agent: run(target="dev-main", command="git clone ... && npm install")
Agent: [sessions later] run(target="dev-main", command="npm test")
```
Opening an existing workspace starts it if it is stopped.

### 9.4 Local CI
```
[file watcher triggers agent]
Agent: workspace_open(kind="vm", distro="ubuntu", name="ci-{sha}")
Agent: run(target="ci-{sha}", script=ci_script)
Agent: file_get(target="ci-{sha}", path="test-results.xml")
Agent: workspace_close(name="ci-{sha}")
Agent: report results
```

### 9.5 Chaos Testing
```
Agent: workspace_open(kind="vm", distro="ubuntu", name="chaos-target")
Agent: run(target="chaos-target", command="start my service")
Agent: run(target="chaos-target", command="kill -9 $(pgrep myservice)")
Agent: run(target="chaos-target", command="check recovery state")
Agent: workspace_close(name="chaos-target")
```

---

## 10. Security Model

| Concern | Mitigation |
|---------|-----------|
| Private keys on disk | Never written unencrypted; Go SSH uses in-memory signers |
| Vault master key | Derived from env/keychain passphrase; derived key stays in process memory |
| VM isolation | OrbStack isolated mode — no macOS filesystem mounts |
| Agent autonomy scope | MCP tools only manage VMs in agent's own namespace in vault |
| Key lifecycle | Generated when a VM or microVM workspace opens; deleted on close. No tool returns a private key |
| Audit trail | All tool calls logged with timestamp to `~/.orbit/audit.log` |
| MCP diagnostics | `orbit://info` and `orbit://doctor` are read-only and return no secret values |
| Command secrets | `secret_env` injects vault values; `secret_put` does not return the value |

---

---

## 12. Open Questions / Future Work

- **Persistent vs ephemeral VMs:** Should the vault distinguish between "persistent" VMs (dev environments) and "ephemeral" (sandbox tasks)? Ephemeral could auto-destroy after N minutes of inactivity.
- **VM templates / snapshots:** Cache a provisioned base VM image to skip repeat setup work.
- **Multi-agent:** Multiple agents sharing a vault namespace — needs locking (SQLite WAL mode already handles concurrent reads).
- **Remote OrbStack:** OrbStack currently localhost-only. Could tunnel via SSH for remote Mac scenarios.
- **Credential store generalization:** The `cred_*` tools are a general-purpose secret store — could extend to manage API keys, tokens, env vars for agent tasks beyond just SSH keys.
- **OrbStack Kubernetes:** `vm_create` could optionally deploy to OrbStack's built-in k8s cluster for containerised workloads.
