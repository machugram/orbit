# Orbit — Autonomous Agent Infrastructure on OrbStack
## Technical Specification v0.1

---

## 0. Vision

An agent runtime where Claude (or any MCP-compatible agent) can:
1. **Generate and own its own SSH keypairs** — stored in a purpose-built credential store
2. **Spin up isolated OrbStack Linux VMs on demand** — provisioned with its own keys
3. **Execute arbitrary work** inside those VMs — code, builds, tests, chaos, CI
4. **Tear down VMs cleanly** — with optional result capture
5. **Persist nothing sensitive on the host** — the credential store is the single source of truth

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
│                  Orbit MCP Server                  │
│                                                         │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │  VM Manager  │  │  Key Manager │  │  Task Runner │  │
│  │  (orbctl)    │  │  (cred store)│  │  (SSH exec)  │  │
│  └──────┬───────┘  └──────┬───────┘  └──────┬───────┘  │
└─────────┼─────────────────┼─────────────────┼──────────┘
          │                 │                 │
          ▼                 ▼                 ▼
   OrbStack VMs      Credential Store    SSH Sessions
   (isolated)        (SQLite + AES-GCM)  (per-task)
```

### 1.1 Orbit MCP Server
The core process. Exposes MCP tools to the agent. Written in **Golang** . Runs as a local stdio MCP server.

### 1.2 VM Manager
Wraps `orbctl` and `orb` CLI. Handles:
- VM lifecycle: create, start, stop, destroy
- Cloud-init provisioning (inject agent's public key at creation time)
- VM inventory (name → state → assigned keypair)
- Isolation enforcement (no macOS file sharing mounts by default)

### 1.3 Key Manager / Credential Store
The agent's own credential vault. Handles:
- Ed25519 keypair generation (via `node:crypto` or `ssh-keygen`)
- AES-256-GCM encrypted SQLite store (via `better-sqlite3` + `node:crypto`)
- Key lifecycle: generate → associate with VM → revoke → rotate
- No keys ever hit `~/.ssh` unless explicitly promoted by the user

### 1.4 Task Runner
Handles SSH command execution into VMs:
- Opens SSH session using the VM's registered keypair (from cred store)
- Streams stdout/stderr back to the agent
- Supports: single commands, multi-step scripts, file upload/download (scp)
- Timeout and kill support

---

## 2. MCP Tools Exposed to the Agent

### VM Lifecycle
| Tool | Description | Key Params |
|------|-------------|-----------|
| `vm_create` | Spin up a new isolated OrbStack VM | `distro`, `name?`, `cloud_init?` |
| `vm_list` | List all agent-managed VMs with status | — |
| `vm_start` | Start a stopped VM | `vm_id` |
| `vm_stop` | Gracefully stop a VM | `vm_id` |
| `vm_destroy` | Destroy VM and optionally revoke its keys | `vm_id`, `purge_keys?` |
| `vm_snapshot` | (future) Snapshot VM state | `vm_id`, `tag?` |

### Key Management
| Tool | Description | Key Params |
|------|-------------|-----------|
| `key_generate` | Generate a new Ed25519 keypair | `label`, `associate_vm?` |
| `key_list` | List all keys in the store | — |
| `key_rotate` | Generate new key, re-provision VM, revoke old | `vm_id` |
| `key_revoke` | Delete key from store and VM authorized_keys | `key_id` |
| `key_export_public` | Return public key string (safe to share) | `key_id` |

### Task Execution
| Tool | Description | Key Params |
|------|-------------|-----------|
| `exec` | Run a shell command on a VM | `vm_id`, `command`, `timeout?` |
| `exec_script` | Upload and run a script file | `vm_id`, `script`, `interpreter?` |
| `file_push` | Upload a file to VM | `vm_id`, `local_path`, `remote_path` |
| `file_pull` | Download a file from VM | `vm_id`, `remote_path` |
| `exec_parallel` | Run same command on multiple VMs | `vm_ids[]`, `command` |

### Credential Store (for agent's broader use)
| Tool | Description | Key Params |
|------|-------------|-----------|
| `cred_set` | Store an arbitrary secret | `key`, `value`, `namespace?` |
| `cred_get` | Retrieve a secret | `key`, `namespace?` |
| `cred_delete` | Delete a secret | `key`, `namespace?` |
| `cred_list` | List keys (not values) in a namespace | `namespace?` |

---

## 3. Credential Store Design

### Storage
- **Backend:** SQLite via `better-sqlite3` — single file, portable, zero server
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
- **Master key:** Derived from a passphrase via Argon2id (or loaded from macOS Keychain via `keytar`)
- **Per-secret:** AES-256-GCM with a fresh 12-byte IV per write
- **Private keys:** stored as encrypted blobs — never written to `~/.ssh` unless user promotes them
- **Public keys:** stored plaintext alongside (safe)

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

When `vm_create` is called:

```
1. key_generate(label="vm-{name}-{ts}")
     → Ed25519 keypair created in memory
     → Private key → encrypted → stored in vault
     → Public key → stored plaintext in vault

2. Build cloud-init user-data:
     #cloud-config
     users:
       - name: agent
         ssh_authorized_keys:
           - <public key from step 1>
         sudo: ALL=(ALL) NOPASSWD:ALL
     packages: [git, curl, build-essential]
     runcmd:
       - systemctl disable --now ssh  # re-enable only OrbStack's multiplexed SSH

3. orbctl create <distro> <name> --cloud-init <user-data-file>

4. Poll until VM is "running" (orbctl status)

5. Write VM record to vault DB: { id, distro, state: running, key_id }

6. Return { vm_id, ssh_host, status: ready } to agent
```

---

## 5. Task Execution Flow

When `exec` is called:

```
1. Look up vm_id in vault DB → get key_id
2. Decrypt private key from vault (requires master key in memory)
3. Write private key to temp file: mkstemp() → chmod 600
4. ssh -i <tmpkey> -p 32222 agent@localhost "command"
   (using OrbStack's built-in SSH multiplexer)
5. Stream stdout/stderr
6. Unlink temp key file (in finally block)
7. Return { stdout, stderr, exit_code }
```

The private key exists on disk for < 100ms per invocation.

---

## 6. Project Structure



---

## 9. Use Case Implementations

### 9.1 Ephemeral Code Execution Sandbox
```
Agent: vm_create(distro="ubuntu", name="sandbox-xyz")
Agent: exec(vm_id, "curl https://... | python3")
Agent: file_pull(vm_id, "/tmp/results.json")
Agent: vm_destroy(vm_id, purge_keys=true)
```
Total lifecycle: ~60s including VM spin-up.

### 9.2 Multi-Distro Compatibility Test
```
Agent: vm_create × 4 (ubuntu, fedora, arch, debian)
Agent: exec_parallel(all_vm_ids, "bash /tmp/test.sh")
Agent: diff results
Agent: vm_destroy × 4
```
All four run concurrently. Results compared. VMs gone in minutes.

### 9.3 Stateful Dev Environment
```
Agent: vm_create(distro="ubuntu", name="dev-main")
       → persists in vault
Agent: exec(vm_id, "git clone ... && npm install")
Agent: [sessions later] vm_start(vm_id)
Agent: exec(vm_id, "npm test")
```

### 9.4 Local CI
```
[file watcher triggers agent]
Agent: vm_create(distro="ubuntu", name="ci-{sha}")
Agent: exec_script(vm_id, ci_script.sh)
Agent: file_pull(vm_id, "test-results.xml")
Agent: vm_destroy(vm_id)
Agent: report results
```

### 9.5 Chaos Testing
```
Agent: vm_create(distro="ubuntu", name="chaos-target")
Agent: exec(vm_id, "start my service")
Agent: exec(vm_id, "kill -9 $(pgrep myservice)")   ← deliberate chaos
Agent: exec(vm_id, "check recovery state")
Agent: vm_destroy(vm_id)
```

---

## 10. Security Model

| Concern | Mitigation |
|---------|-----------|
| Private keys on disk | Only decrypted to tmpfile for <100ms per SSH call, then unlinked |
| Vault master key | Stored in macOS Keychain via `keytar`; prompted on first use |
| VM isolation | OrbStack isolated mode — no macOS filesystem mounts |
| Agent autonomy scope | MCP tools only manage VMs in agent's own namespace in vault |
| Key rotation | `key_rotate` tool for regular keypair cycling |
| Audit trail | All tool calls logged with timestamp to `~/.orbit/audit.log` |

---

---

## 12. Open Questions / Future Work

- **Persistent vs ephemeral VMs:** Should the vault distinguish between "persistent" VMs (dev environments) and "ephemeral" (sandbox tasks)? Ephemeral could auto-destroy after N minutes of inactivity.
- **VM templates / snapshots:** Cache a "base" VM image post-provisioning to skip the cloud-init wait on repeat uses.
- **Multi-agent:** Multiple agents sharing a vault namespace — needs locking (SQLite WAL mode already handles concurrent reads).
- **Remote OrbStack:** OrbStack currently localhost-only. Could tunnel via SSH for remote Mac scenarios.
- **Credential store generalization:** The `cred_*` tools are a general-purpose secret store — could extend to manage API keys, tokens, env vars for agent tasks beyond just SSH keys.
- **OrbStack Kubernetes:** `vm_create` could optionally deploy to OrbStack's built-in k8s cluster for containerised workloads.
