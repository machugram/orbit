# ADR-003: Root provisioning script instead of cloud-init

**Status:** Accepted  
**Date:** 2025-06

## Context

The spec (§4) assumed cloud-init for VM provisioning: a `user-data` YAML
passed to `orbctl create --cloud-init` would install openssh-server, create
the agent user, and install the authorized key before first boot.

During implementation it was found that OrbStack only ships cloud-init in its
**Debian and Ubuntu** images. Alpine, Fedora, Arch, Void, and other distros
have no cloud-init agent; passing `--cloud-init` to `orbctl create` is silently
ignored on those images.

## Decision

Replace cloud-init with a **post-create root script** executed via
`orb -m <vm> -u root sh -s`. The script is a single POSIX sh file
(`internal/vm/distro.go`) that detects the package manager and init system
at runtime and branches accordingly.

## Rationale

**Cloud-init is not universally available.** The discovery that Alpine and
Fedora images lack it meant the spec's approach would silently skip
provisioning on those distros — no error, just a VM with no sshd and a locked
agent account. Multi-distro support was a stated goal (spec §1.2).

**`orb -u root` works across all distros and isolation modes.** OrbStack
grants root access through its own multiplexer channel regardless of the
distro or whether the VM is isolated. This is a reliable, distro-independent
root channel that does not depend on any in-VM agent.

**One script, zero configuration.** The auto-detecting script handles
`apt-get` / `dnf` / `yum` / `zypper` / `pacman` / `apk` / `xbps` for
packages and `systemctl` / `rc-update` / `sv` for init. Adding a new
distro family means adding one branch to the script, not a new provisioner
class or configuration option.

**Idempotent and retry-safe.** The script can be run multiple times without
harm. The provisioner retries up to 5 times with 3-second delays to absorb
the window between "VM running" and "package mirrors reachable". Cloud-init
would have run once at boot with no retry mechanism exposed to the caller.

**Positional args, not interpolation.** The agent username and the public key
are passed as `$1` and `$2` (real argv entries via `orb … sh -s arg1 arg2`),
not interpolated into the script body. A key that contains shell-special
characters cannot break out of its `printf '%s\n'` into the script.

## Consequences

- VMs take 20–60s longer on first create than they would with a pre-baked
  image, because package installation runs live. Base-image caching (ROADMAP
  §1.1) is the mitigation.
- NixOS cannot be handled by this approach: its declarative model has no
  imperative package manager. It requires a separate `configuration.nix`
  injection path (ROADMAP §2.4).
- The `VMExecer` interface is injectable so tests replace `orb` with a fake
  that returns `ORBIT_PROVISIONED` without touching OrbStack.
- Cleanup on failure is the provisioner's responsibility: if provisioning
  fails after all retries, the VM is destroyed and its key revoked so nothing
  leaks.
