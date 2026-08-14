# ADR-012: Nested microVMs for eBPF — a QEMU/TCG guest with its own kernel

**Status:** Accepted
**Date:** 2026-06
**Related:** ADR-002 (direct SSH), ADR-003 (provisioning), ADR-005 (container isolation), ROADMAP §2.3 (network isolation)

## Status Quo

Orbit gives an agent two ways to run code: OrbStack **VMs** (`vm_create`,
`internal/vm`) and **containers** on OrbStack's Docker engine (`container_*`,
`internal/container`). Both look like separate machines, but on Apple Silicon
they share **one Linux kernel** — OrbStack runs a single guest under Apple's
Virtualization.framework and presents each "machine" as a namespaced
environment on top of it. A quick probe confirms this: every machine reports the
same `*-orbstack-*` kernel release, and `/dev/kvm` does not exist inside any of
them.

That shared kernel is fine for ordinary workloads but wrong for anything that
loads code **into** the kernel. eBPF is the motivating case: a program loaded
with `bpf()` (via bpftrace, bcc, libbpf, …) attaches to the *running* kernel.
Inside an OrbStack machine that is the kernel shared by every other machine and
by OrbStack itself, so a buggy or hostile eBPF program — or an exploit of a
verifier/JIT bug — is not contained to the "machine" that loaded it. There is no
boundary there to contain it; the boundary an agent reaches for (`--isolated`,
ADR-005's container caveat) isolates *files and processes*, not the *kernel*.

## Goal

Let an agent run eBPF (and other kernel-level work) in an environment whose
**kernel is its own**, so that loading BPF, tracing syscalls, or crashing the
kernel affects only that environment — not the OrbStack kernel, not other
machines, not the macOS host. Expose it through Orbit's existing model so the
agent drives it with the tools it already has (`exec`, `exec_script`,
`file_push`, `file_pull`).

## Non-Goal

This ADR does not make the nested guest *fast*; under the constraints below it is
emulated and slow, and that is accepted. It does not replace VMs or containers
for ordinary work — those remain the right tools when a shared kernel is fine. It
does not add a general nested-virtualization framework (live migration,
snapshots, GPU passthrough, arbitrary guest arches); it adds exactly enough to
get an isolated kernel. It does not change the host-key trust model (ADR-011
still applies; see Consequences).

## Proposed Solution

Run a **nested guest under QEMU inside a carrier OrbStack machine**, giving the
layered containment the kernel boundary requires:

```
macOS host (Virtualization.framework)
└── OrbStack shared kernel  ← carrier machine, --isolated
    └── QEMU process (TCG)
        └── nested guest kernel  ← eBPF loads HERE, contained
```

The carrier is an ordinary agent VM (created by `vm_create`); the nested guest
is a new first-class object managed by `internal/microvm` and the `microvm_*`
tools.

**Emulation, not acceleration — forced by the platform.** There is no `/dev/kvm`
inside an OrbStack machine (Apple's hypervisor does not expose nested hardware
virt), so the guest runs under QEMU **TCG**, pure software emulation. TCG is
correct and fully isolated but slow *per instruction* — so the cost of a boot is
dominated by how much userspace runs. The mitigation is to run *less*: the
default guest is a tiny Alpine system booted **direct-kernel** (`-kernel`/
`-initrd`, no UEFI/GRUB) with OpenRC instead of systemd, which brings a boot down
from minutes (full cloud image) to tens of seconds, and the footprint from
~700 MB to tens of MB. Correctness of the isolation boundary is the requirement,
not throughput; the boot timeout stays generous (10 minutes) to absorb the worst
case.

**The guest is reached over SSH at `<carrier>.orb.local:<port>`.** QEMU user
networking forwards a carrier-local port into the guest's port 22
(`hostfwd=tcp:0.0.0.0:<port>-:22`). A freshly generated Ed25519 key from the
vault (one per microVM, ADR-004) is injected via a cloud-init NoCloud seed, and
the guest's eBPF toolchain is preinstalled the same way. Crucially, this means a
microVM is *just another SSH target*: the exec runner was generalized behind a
`Resolver` interface (an id → endpoint + signer), and a `microvm.Resolver` maps
microVM ids to the carrier endpoint while chaining to the vault resolver for
everything else. So `exec_script`, `file_push`, etc. drive a microVM by name
with **no new code in the exec path and no new tools to run code** — only
lifecycle tools (`microvm_create/list/start/stop/destroy`) are added.

**Image is a parameter, with two boot modes.** A small registry of `imageSpec`s
selects the guest and its `bootMode`. The default `alpine` uses **direct-kernel**
mode — a minirootfs staged into an ext4 image (built with `mkfs.ext4 -d`, no
loop mount) and configured via a native chroot (carrier and guest share the
aarch64 arch), with the agent's public key **baked into the rootfs at build
time** so restarts need no key material on the cmdline. Its stock kernel may lack
BTF, so CO-RE/libbpf may not work, but raw `bpf()` and bpftrace-from-source do.
The opt-in `ubuntu` uses **cloud-image** mode — a full UEFI image keyed via a
cloud-init seed — whose kernel ships `CONFIG_DEBUG_INFO_BTF` for out-of-the-box
CO-RE eBPF, at the cost of size and boot time. New images (more distros, a
BTF-enabled direct-kernel build) drop into the registry without touching the
manager, which dispatches purely on `bootMode`.

All carrier-side work runs through root scripts fed to `orb -m <carrier> -u root
sh -s` with values passed **only as positional args** — the agent's public key,
the name, the URL — so an attacker-influenced value cannot break out of its
argument into the shell (the same discipline as ADR-005's secret injection).

## Alternatives

### Just load eBPF inside an OrbStack machine

The status quo path. Rejected because it does not meet the goal at all: the BPF
program attaches to the shared OrbStack kernel, so there is no containment. This
is precisely the gap the ADR exists to close.

### QEMU on the macOS host via Hypervisor.framework (HVF), not nested

Run an *accelerated* separate-kernel VM directly on the host with `-accel hvf`.
This would be far faster than TCG and still give the guest its own kernel.
Rejected as the primary design because it is not *nested* and sits outside the
OrbStack model the agent operates in: it would need a second host-level VM
lifecycle, host-side networking, and its own isolation story, duplicating what
OrbStack already provides for the carrier. It is, however, the natural escape
hatch if speed becomes the binding constraint — noted in Consequences as the
documented next step, not built now.

### Firecracker / cloud-hypervisor microVMs

Lighter and faster to boot than full QEMU. Rejected because both require KVM
(`/dev/kvm`), which does not exist inside an OrbStack machine — so they cannot
run nested here at all on this platform. If a future OrbStack/Apple release
exposes nested KVM, revisiting this (and dropping TCG) is the obvious win.

### gVisor / user-space kernels

gVisor intercepts syscalls in user space and would isolate many workloads from
the host kernel. Rejected for the eBPF use case specifically: gVisor does not
implement a real `bpf()`-loadable kernel, so the very programs this feature
exists to run would not load. It solves a different (and valuable) problem than
"give eBPF its own real kernel."

## Consequences

- New subsystem `internal/microvm` (manager, image registry, carrier-side
  scripts, exec `Resolver`) and `microvm_*` MCP tools; a `microvms` table in the
  vault; one generalization of `exec.Runner` to resolve targets via a `Resolver`
  (the default vault behavior is preserved, existing tools unchanged).
- **Performance is the headline limitation:** guest CPU is emulated (TCG), so
  it is slow regardless of image. The default direct-kernel Alpine guest keeps
  *boot* to tens of seconds; the `ubuntu` cloud image trades minutes of boot for
  a BTF kernel. Documented in the tool descriptions and README so the agent and
  the user set expectations correctly.
- The carrier must be a Debian/Ubuntu-family machine (the provision script uses
  `apt`); this is validated against the carrier's recorded distro before launch.
- ADR-011's host-key trust model still holds: the carrier-local forwarded port
  is still loopback-only traffic on the user's machine, so trust-on-connect
  remains acceptable. **If a microVM is ever exposed off the carrier (e.g. a
  bridged guest NIC), that decision must be revisited together with ADR-011.**
- **Trigger to revisit:** the day nested KVM becomes available inside OrbStack
  machines, replace TCG with KVM acceleration (and reconsider Firecracker),
  which would turn this from a slow lab tool into a fast one. Until then, TCG is
  the only correct option and is accepted as-is.
