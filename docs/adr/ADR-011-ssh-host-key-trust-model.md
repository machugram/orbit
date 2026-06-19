# ADR-011: SSH host-key trust model — trust-on-connect for loopback, pinning deferred

**Status:** Accepted
**Date:** 2026-06
**Related:** ROADMAP §3 (host key pinning)

## Status Quo

The SSH task runner (`internal/exec`) authenticates *itself* to each VM with a
per-VM Ed25519 key held in the encrypted vault (ADR-002, ADR-004). The reverse
direction — verifying the VM's *host* key — is currently
`ssh.InsecureIgnoreHostKey()` at [runner.go:89](../../internal/exec/runner.go).
This means Orbit connects to whatever answers at `<vm>.orb.local:22` without
checking that it is the machine it provisioned.

A VM's host key is generated on its first boot (`ssh-keygen -A` in the
provisioning script, ADR-003). It is not known ahead of time and not
pre-distributed, so there is nothing to verify against on the first connection.
All traffic stays on the local machine: `<vm>.orb.local` resolves through
OrbStack's resolver to a loopback-reachable address, and never leaves the host.

## Goal

Make the host-key trust decision explicit and defensible rather than an
unexamined default, and define the threat model under which the current
behavior is acceptable — so that a reader (or a security reviewer) can judge the
risk without reverse-engineering it from a single line of code.

## Non-Goal

This ADR does not implement host-key pinning — that remains ROADMAP §3. It does
not address the *client* key handling (covered by ADR-002/ADR-004), nor network
isolation of VMs (`--isolate-network`, ROADMAP §2.3). It does not cover the
container path, which does not use SSH at all.

## Proposed Solution

Keep `InsecureIgnoreHostKey()` for now, but treat it as a **documented,
bounded decision** rather than a default, justified by a specific threat model:

**The attack that host-key verification prevents is a man-in-the-middle on the
connection between Orbit and the VM's sshd.** For that attack to exist, an
adversary must be positioned on the network path. Here that path is entirely
within the local machine — OrbStack's `.orb.local` names resolve to
loopback-reachable addresses served by the local OrbStack daemon. An attacker
who can MITM loopback traffic on the user's own machine already has local code
execution as (or above) the user, at which point they can read the vault
passphrase from the Keychain, the decrypted keys from process memory, or simply
issue tool calls directly. Host-key verification would protect against none of
these, because the trust root (the vault, the Keychain) is already on the
compromised machine. In this specific topology, host-key verification adds no
meaningful protection.

**Trust-on-connect is the only option available on the first connection
anyway.** Because the host key is generated at the VM's first boot and never
pre-shared, the very first SSH connection cannot verify it against a known
value. Any pinning scheme is therefore trust-on-first-use (TOFU): capture the
key on first contact, pin it, and detect changes thereafter. That is a strictly
better posture than today's "ignore always," but its security value is realized
only across *reconnections* to a *long-lived* VM — and it only matters in a
threat model where loopback MITM is possible, which (per the previous point) is
not the model Orbit operates in today.

## Alternatives

### Trust-on-first-use (TOFU) pinning now

Capture each VM's host key on the first successful connection, store it on the
`vms` record, and verify it on every subsequent connection via a custom
`HostKeyCallback`. This is the right long-term design and is the planned ROADMAP
§3 work. It was deferred for this release because it requires a vault schema
change (a `host_key` column), careful handling of the legitimate key-change case
(VM rebuilt or re-provisioned under the same name must not hard-fail in a
confusing way), and it delivers no security gain under the current loopback-only
threat model. Shipping it now would add schema-migration and edge-case surface
for no present benefit; it belongs with the network-isolation work (§2.3) that
*does* change the threat model.

### Pre-seed a known host key during provisioning

Generate the host key on the host and inject it into the VM during the `orb -u
root` provisioning step, so Orbit knows it before the first SSH connection. This
would enable strict (non-TOFU) verification from connection one. It was rejected
because it moves private host-key material through the provisioning channel and
onto the host's control plane, widening the surface of secret material Orbit
handles, in exchange for closing a gap that does not exist on loopback. It also
couples provisioning more tightly to the SSH layer for no current gain.

### Require the user to manage `known_hosts`

Defer host-key trust entirely to a user-managed `known_hosts` file, as a normal
`ssh` client would. Rejected because Orbit's whole point is that the agent
manages VM lifecycle autonomously; making host-key trust a manual user step
breaks the zero-configuration model (ADR-003) and would block the agent on human
input for every newly created VM.

## Consequences

- The single `InsecureIgnoreHostKey()` call site carries a comment pointing at
  this ADR so the decision is discoverable from the code.
- The README security section documents this as a known, bounded limitation
  rather than implying host-key verification is performed.
- **The threat model changes the moment a VM becomes reachable off-loopback.**
  If a future feature exposes a VM on a non-loopback interface, or if
  `--isolate-network` work (ROADMAP §2.3) alters reachability, this decision
  must be revisited and TOFU pinning (ROADMAP §3) implemented before that ships.
  This is the explicit trigger condition for reopening this ADR.
