# Orbit — Performance & Feature Roadmap

## 1. Performance

### 1.1 Cache a provisioned base image (biggest win) — L
**Where:** [internal/vm/provisioner.go](internal/vm/provisioner.go), [internal/vm/distro.go](internal/vm/distro.go)

VM creation is dominated by `apt-get`/`dnf`/`apk` installing `openssh-server`
on **every** `vm_create` — tens of seconds of network + package work. This is
the single largest latency cost in the system.

Provision one base machine per distro once, then clone it for subsequent
creates so the package step is skipped. Approaches, in order of preference:
- **Clone from a warm base VM** — keep a `orbit-base-<distro>` machine
  provisioned and cloned on demand (sub-second vs ~30–60s).
- **Snapshot/restore** once OrbStack exposes it in the CLI.

Expected effect: `vm_create` cold start drops from ~30–60s to a few seconds.
This is also the prerequisite for the spec's "~60s lifecycle" claim to
hold across distros, not just cached-image Ubuntu.

### 1.2 SSH connection reuse / pooling — M — **done**
**Where:** `acquire`/`release` in [internal/exec/pool.go](internal/exec/pool.go), [internal/exec/runner.go](internal/exec/runner.go)

Every `exec`, `file_push`, `file_pull`, and `exec_script` used to open a fresh TCP
connection + SSH handshake, run one operation, and tear it down. A per-VM
`*ssh.Client` pool (60s idle TTL, refcounted) now shares one connection across
bursts. `Runner.Invalidate` drops the entry on `vm_stop` / `vm_destroy`.

### 1.3 `exec_script` opens three connections for one logical op — S — **done**
**Where:** `ExecScript` in [internal/exec/runner.go](internal/exec/runner.go)

`ExecScript` now acquires one pooled client and performs SFTP upload, run, and
`rm` cleanup over that connection.

### 1.4 Stream `file_pull` instead of buffering in memory — S
**Where:** `Pull` in [internal/exec/files.go:84](internal/exec/files.go)

`Pull` does `io.ReadAll` of the whole file into a `[]byte` (capped at 32 MiB).
When the caller passed `local_path` (file_pull tool), stream the SFTP reader
straight to disk with `io.Copy` and never hold the payload in memory. Keep the
buffering path only for the inline-content case. Lets the cap rise safely and
flattens memory use.

### 1.5 Vault DB write serialization under concurrent tools — M
**Where:** `db.SetMaxOpenConns(1)` in [internal/vault/store.go:81](internal/vault/store.go)

The vault uses a single DB connection, so all reads and writes serialize. A
`exec_parallel` across N VMs does N × (`GetVM` + `GetCredentialByID`) and
contends on that one connection. WAL mode is already enabled
([store.go:83](internal/vault/store.go)), which permits concurrent readers.

Move to a small read pool (`SetMaxOpenConns(>1)`) while keeping writes
serialized, or add an in-memory read cache for hot VM/key records (invalidated
on write). Note the tradeoff: more connections = more open file handles and
careful handling of `modernc.org/sqlite` busy semantics.

### 1.6 Replace per-poll `orbctl` subprocess during create — S
**Where:** `waitState`/`State` in [internal/vm/manager.go](internal/vm/manager.go) (`pollInterval: 500ms`, [manager.go:51](internal/vm/manager.go))

`Create` polls `orbctl info … -f json` every 500ms, spawning a subprocess each
time. Cheap for one VM; under many concurrent creates it's a process storm.
Options: exponential backoff on the poll interval, or a single `orbctl list`
poll shared across concurrent creates.

### 1.7 cgo-free SQLite is a deliberate speed tradeoff — (doc only)
We use `modernc.org/sqlite` (pure Go) for a single cgo-free binary. It is
slower than the cgo `mattn`/`better-sqlite3` driver. For a credential store
(low write volume, tiny rows) this is irrelevant, but if the `cred_*` tools
ever become a high-throughput general secret store (2.7), benchmark both.
Argon2id derivation (~0.3–0.7s) is paid once at server start by design — do
**not** cache the derived key to disk.

---

## 2. Features

### 2.1 Ephemeral VMs with idle TTL / auto-destroy — M
**Where:** new `internal/vm` reaper + `vms` table (already has `created_at`)

Spec 12 + 9.1. Let `vm_create` mark a VM ephemeral with a TTL; a background
reaper destroys VMs idle past the deadline and purges their keys. Track
`last_used_at` (update on every `exec`). Prevents sandbox VMs from leaking when
an agent forgets to call `vm_destroy`.

### 2.2 `vm_snapshot` — M
**Where:** new tool in [internal/tools/vm_tools.go](internal/tools/vm_tools.go)

Listed in spec  as future. Snapshot/restore VM state by tag. Gated on
OrbStack CLI support (verify `orbctl` exposes it in the installed version);
until then, implement via clone-of-stopped-VM. Pairs with 1.1.

### 2.3 Expose resource limits & network isolation in `vm_create` — S
**Where:** `CreateSpec` in [internal/vm/types.go](internal/vm/types.go), `vm_create` args in [internal/tools/vm_tools.go](internal/tools/vm_tools.go)

`orbctl create` already supports `--cpus`, `--memory`, `--disk`, and
`--isolate-network` (block the VM from other machines and host IPs). Surface
these as optional `vm_create` parameters. `--isolate-network` in particular
strengthens the isolation story for untrusted-code sandboxes — but note it
would also break `<vm>.orb.local:22` reachability from the host, so it needs
testing against the SSH path before enabling.

### 2.4 NixOS support — M
**Where:** [internal/vm/distro.go](internal/vm/distro.go)

The one distro the auto-detect script can't handle: NixOS is declarative, so
`apt`/`dnf`-style installs don't apply. Provision by writing a
`configuration.nix` fragment (`services.openssh.enable = true;` + the agent
user's `authorizedKeys`) and running `nixos-rebuild switch` via `orb`. Detect
NixOS (`/etc/NIXOS`) and branch to this path; everything downstream (the SSH
runner) is unchanged.

### 2.5 `key_promote` — export a key to `~/.ssh` — S
**Where:** new tool in [internal/tools/key_tools.go](internal/tools/key_tools.go), [internal/keys](internal/keys)

Spec 1.3/3 explicitly anticipate keys being "promoted by the user" out of
the vault. Add an opt-in tool that writes a stored private key to `~/.ssh/`
with `0600` and prints the suggested `ssh`/config snippet — the deliberate,
audited escape hatch for using an agent VM from a normal terminal.

### 2.6 `audit_tail` — let the agent review its own actions — S
**Where:** [internal/audit](internal/audit), new tool

The audit log ([internal/audit/audit.go](internal/audit/audit.go)) is
write-only today. A read tool returning the last N entries (filtered by tool
name / time) lets an agent self-audit and lets `vm_destroy` confirm what a VM
was used for before teardown.

### 2.7 Generalize `cred_*` into a first-class secret store — M
Spec 12. The credential store already backs arbitrary secrets. Extend with:
secret **references injected as env vars** into `exec` (so a command can use a
token without the value passing through the model), per-namespace access
scoping, and TTL/expiry on secrets. The env-injection piece is the high-value
one — it keeps secrets out of the transcript.

### 2.8 Streaming command output (long-running `exec`) — M
**Where:** [internal/exec/runner.go](internal/exec/runner.go), [internal/tools/exec_tools.go](internal/tools/exec_tools.go)

`exec` buffers all stdout/stderr until the command exits. For long builds/CI
(spec 9.4), stream incremental output via MCP progress notifications, or add
an `exec_stream` that writes a growing log file the agent can `file_pull`.

### 2.9 Multi-agent namespacing & locking — L
Spec 12. Today all VMs/keys share one vault. For multiple agents on one host,
scope `vms`/`credentials` by an agent namespace and document the locking model
(WAL handles concurrent readers; writes serialize — see 1.5). Also consider
advisory locks around `vm_create` to prevent name races.

### 2.10 Master passphrase rotation — M
**Where:** [internal/vault/store.go](internal/vault/store.go), [internal/keychain](internal/keychain)

There is no way to change the vault passphrase today. Add a `rekey` operation:
derive a new key from a new passphrase and re-encrypt every credential in one
transaction. Important for the Keychain-backed master key's own lifecycle.

---

## 3. Hardening (smaller, correctness-adjacent)

- **Pull-to-`local_path` path traversal** — validate/confine the host
  destination in `file_pull` so a VM-influenced path can't write outside an
  expected directory. [internal/tools/exec_tools.go](internal/tools/exec_tools.go)
- **Host key pinning** — `connect()` uses `InsecureIgnoreHostKey()`
  ([internal/exec/runner.go](internal/exec/runner.go)). Loopback-only traffic
  makes this acceptable, but pinning each VM's host key (captured at first
  provision) on the `vms` record would close the gap and detect VM identity
  changes.
- **Provisioning idempotency on partial failure** — the script is idempotent,
  but a create that fails mid-provision destroys the VM; consider an opt-in
  "keep on failure" for debugging.
- **`exec_parallel` result streaming** — currently collected then returned all
  at once; could emit per-VM as they finish for long fan-outs.

---

## 4. Observability (future)

Today the only signal is the JSONL audit log
([internal/audit/audit.go](internal/audit/audit.go)) — a record of *what* was
called, not *how it behaved*. As an agent runs longer-lived VMs and container
stacks, it needs to see health and history. Ordered by value:

### 4.1 Structured operational logging — S
**Where:** all subsystems, currently `log.SetOutput(os.Stderr)` in [cmd/orbit/main.go](cmd/orbit/main.go)

Replace ad-hoc stderr logging with `log/slog` (stdlib — no new dep) at a level
set by `ORBIT_LOG_LEVEL`. Emit structured fields (subsystem, vm/container id,
duration, outcome) so tool latency and failures are greppable. Keep the secret
exclusion the audit log already guarantees.

### 4.2 Per-call metrics & timing in the audit log — S
**Where:** [internal/tools/tools.go](internal/tools/tools.go) `d.log(...)`

The audit entry records `ok`/`error` but not duration. Add elapsed-ms and a
coarse outcome class to every entry; an `audit_tail` tool (2.6) then doubles
as a latency/error feed the agent can reason about ("vm_create is timing out").

### 4.3 `container_stats` / `vm_stats` — M
**Where:** new tools wrapping `docker stats --no-stream` and `orbctl` resource info

Surface live CPU/memory/network per container or VM so the agent can right-size
work, detect a runaway process, or decide to scale a compose service (pairs
with `compose_up --scale`). `docker stats --format json` is a one-shot read.

### 4.4 Compose/stack health rollup — S
**Where:** extend `compose_ps` ([internal/container](internal/container))

`compose_ps` already returns per-service `state`/`health`/`orb_host`. Add a
project-level rollup (healthy / degraded / down + restart counts) so an agent
can poll one call to know whether a stack is serving, and gate dependent work
on it.

### 4.5 OpenTelemetry export — L (only if multi-agent)
**Where:** new `internal/telemetry`

If Orbit ever runs as shared infrastructure for many agents (2.9), export
spans/metrics via OTLP so tool calls show up in standard tracing backends. Out
of scope for single-user local use — listed so the logging in 4.1/4.2 can be
shaped to graduate into it rather than be rewritten.

---

## Suggested next milestone

**1.1 (base-image cache)** is next. SSH pooling (1.2) and single-conn
`exec_script` (1.3) are done. After 1.1: ephemeral TTL (2.1), then
multi-agent namespaces (2.9).
