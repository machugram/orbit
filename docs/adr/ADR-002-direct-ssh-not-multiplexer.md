# ADR-002: Direct VM SSH instead of OrbStack multiplexer

**Status:** Accepted  
**Date:** 2025-06

## Context

OrbStack exposes a convenience SSH multiplexer at `127.0.0.1:32222`. The
original spec (§5) assumed this would be the SSH transport: an agent could
connect as `agent@<machine>@127.0.0.1` and reach any VM without knowing its
IP. This is how `orb ssh` works for interactive use.

During implementation it was discovered that the multiplexer only authenticates
with **OrbStack's own host key** (`~/.orbstack/ssh/id_ed25519`). It ignores
`authorized_keys` inside each VM entirely.

## Decision

Connect directly to each VM's own sshd at **`<vmname>.orb.local:22`**, user
`agent`, using the per-VM Ed25519 key stored in the vault.

The provisioning script installs and enables openssh-server in every VM
specifically to provide this endpoint.

## Rationale

**Per-VM credential isolation is the product's core security property.**
If all connections went through the OrbStack multiplexer and authenticated
with OrbStack's single key, every VM would share one credential. A compromised
VM would give access to all others. Using the multiplexer would make the vault
and per-VM keypair system meaningless.

**The multiplexer cannot be made to work.** OrbStack's multiplexer performs
its own authentication before proxying; there is no configuration knob to
make it respect in-VM `authorized_keys`. This was verified empirically:
`ssh -p 32222 agent@<machine>@127.0.0.1` always fails with
`unable to authenticate, attempted methods [publickey]` when using a key
that is correctly installed in `~/.ssh/authorized_keys` inside the VM.

**`.orb.local` resolution is reliable.** OrbStack registers each machine in
the host's DNS resolver under `<name>.orb.local` and keeps it updated as VMs
start and stop. The address is always reachable from the host without
additional configuration.

## Consequences

- Every provisioned VM must have sshd installed, running, and listening on
  port 22 — this is guaranteed by the provisioning script in `distro.go`.
- The `TargetResolver` function is injectable so tests substitute a fake SSH
  server without needing `.orb.local` DNS.
- `InsecureIgnoreHostKey()` is used currently since VMs are ephemeral and
  only reachable over loopback. Host key pinning is the planned hardening
  step (ROADMAP §3).
- If OrbStack's `--isolate-network` flag is set, `.orb.local` reachability
  from the host must be re-verified before enabling that option in `vm_create`
  (ROADMAP §2.3).
