// Package microvm runs a nested QEMU guest inside an OrbStack carrier machine
// so that workloads needing their own kernel — eBPF programs above all — load
// into the guest kernel, not the kernel OrbStack shares across every machine.
//
// OrbStack machines on Apple Silicon all share one kernel and expose no
// /dev/kvm, so the nested guest runs under QEMU TCG (software emulation). It is
// correct and fully isolated, but emulated; the default image is therefore a
// tiny Alpine system booted directly (-kernel/-initrd, no UEFI, OpenRC instead
// of systemd) so a boot is tens of seconds and tens of MB, not minutes and
// ~700 MB. A full cloud image (kernel with BTF, for CO-RE eBPF) is available as
// an opt-in. The guest is reached over SSH at <carrier>.orb.local:<port>, a
// port QEMU forwards into the guest, so Orbit's existing exec/file tooling
// drives it unchanged. See docs/adr/ADR-012-nested-microvm-ebpf-isolation.md.
package microvm

import (
	"context"
	"fmt"
	"hash/fnv"
	"regexp"
	"strconv"
	"strings"
)

// CarrierRunner runs a script as root inside a carrier OrbStack machine, with
// the script fed on stdin and args passed as positional parameters ($1, $2, …).
// Injectable so tests run against a fake instead of the real `orb` CLI.
type CarrierRunner func(ctx context.Context, carrier, script string, args ...string) (stdout, stderr []byte, err error)

// Spec describes a nested guest to launch.
type Spec struct {
	// Name is the microVM's logical name; generated ("uvm-xxxxxxxx") if empty.
	Name string
	// Carrier is the name of an existing, running agent-managed VM that will
	// host the QEMU process. Required.
	Carrier string
	// Image selects a built-in guest image; defaults to "alpine" (tiny,
	// direct-kernel boot, fast). Use "ubuntu" for a BTF kernel (CO-RE eBPF).
	Image string
	// ImageURL overrides a cloud-image's download URL (cloud-image mode only).
	ImageURL string
	// MemoryMiB and CPUs size the guest; sensible defaults applied if zero.
	MemoryMiB int
	CPUs      int
}

// bootMode is how a guest image is staged and booted.
type bootMode int

const (
	// bootDirectKernel: tiny rootfs + a separate kernel/initrd, booted with
	// -kernel/-initrd (no UEFI). Fast and small; kernel may lack BTF.
	bootDirectKernel bootMode = iota
	// bootCloudImage: a full UEFI cloud image keyed via a cloud-init seed.
	// Heavier, but ships a BTF kernel for CO-RE eBPF.
	bootCloudImage
)

// imageSpec is a built-in guest image.
type imageSpec struct {
	Mode     bootMode
	QEMUArch string   // qemu-system-<arch> suffix, e.g. "aarch64"
	BTF      bool     // kernel ships CONFIG_DEBUG_INFO_BTF (CO-RE eBPF)
	Packages []string // guest packages to preinstall for eBPF work

	// direct-kernel mode:
	AlpineVersion string // full version, e.g. "3.20.3"
	Mirror        string // Alpine mirror base, e.g. "https://dl-cdn.alpinelinux.org/alpine"

	// cloud-image mode:
	URL      string // qcow2 cloud image URL
	Firmware string // UEFI firmware path on the carrier
}

// images are the built-in guest images. aarch64 matches the Apple-Silicon host
// so guest userspace is native; TCG could emulate other arches but slower.
var images = map[string]imageSpec{
	"alpine": {
		Mode:          bootDirectKernel,
		QEMUArch:      "aarch64",
		BTF:           false, // Alpine's virt kernel may lack BTF; raw bpf()/bpftrace still work
		Packages:      []string{"bpftrace", "strace", "clang", "llvm"},
		AlpineVersion: "3.20.3",
		Mirror:        "https://dl-cdn.alpinelinux.org/alpine",
	},
	"ubuntu": {
		Mode:     bootCloudImage,
		QEMUArch: "aarch64",
		BTF:      true,
		Packages: []string{"bpftrace", "bpfcc-tools", "linux-tools-common", "clang", "llvm", "libbpf-dev"},
		URL:      "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-arm64.img",
		Firmware: "/usr/share/qemu-efi-aarch64/QEMU_EFI.fd",
	},
}

// DefaultImage is used when Spec.Image is empty: the fast, light path.
const DefaultImage = "alpine"

const (
	defaultMemoryMiB = 1024
	defaultCPUs      = 2
	// portBase + (hash(name) % portSpan) gives each guest a stable forwarded
	// SSH port on its carrier without a central allocator.
	portBase = 23000
	portSpan = 20000
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

func (s *Spec) validate() error {
	if s.Name != "" && !nameRe.MatchString(s.Name) {
		return fmt.Errorf("microvm: invalid name %q", s.Name)
	}
	if s.Carrier == "" {
		return fmt.Errorf("microvm: carrier is required")
	}
	if !nameRe.MatchString(s.Carrier) {
		return fmt.Errorf("microvm: invalid carrier %q", s.Carrier)
	}
	if _, ok := images[s.imageKey()]; !ok {
		return fmt.Errorf("microvm: unknown image %q (have %s)", s.Image, strings.Join(imageNames(), ", "))
	}
	return nil
}

func (s *Spec) imageKey() string {
	if s.Image == "" {
		return DefaultImage
	}
	return s.Image
}

func imageNames() []string {
	out := make([]string, 0, len(images))
	for k := range images {
		out = append(out, k)
	}
	return out
}

// bootParams are the runtime inputs to a launch/restart, independent of image.
type bootParams struct {
	dir     string
	port    int
	mem     int
	cpus    int
	pub     string // agent authorized_keys line (launch only; baked into the rootfs)
	url     string // cloud-image URL override ("" = image default)
	pkgsCSV string
}

// bootPlan returns the carrier-side script and its positional args to launch a
// fresh guest (fresh=true) or restart a staged one (fresh=false). Keeping arg
// order next to the scripts that consume it avoids drift.
func (im imageSpec) bootPlan(p bootParams, fresh bool) (script string, args []string) {
	itoa := strconv.Itoa
	switch im.Mode {
	case bootDirectKernel:
		if fresh {
			return launchDirectKernelScript, []string{
				p.dir, itoa(p.port), itoa(p.mem), itoa(p.cpus), im.QEMUArch,
				im.AlpineVersion, im.Mirror, p.pub, p.pkgsCSV,
			}
		}
		return startDirectKernelScript, []string{
			p.dir, itoa(p.port), itoa(p.mem), itoa(p.cpus), im.QEMUArch,
		}
	default: // bootCloudImage
		url := p.url
		if url == "" {
			url = im.URL
		}
		if fresh {
			return launchCloudImageScript, []string{
				p.dir, url, itoa(p.port), itoa(p.mem), itoa(p.cpus),
				im.QEMUArch, im.Firmware, p.pub, p.pkgsCSV,
			}
		}
		return startCloudImageScript, []string{
			p.dir, itoa(p.port), itoa(p.mem), itoa(p.cpus), im.QEMUArch, im.Firmware,
		}
	}
}

// portFor derives a stable forwarded SSH port from the microVM name.
func portFor(name string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return portBase + int(h.Sum32()%portSpan)
}

// handleDir is the per-microVM working directory on the carrier holding the
// staged kernel/rootfs (or base image), console log, and QEMU pid.
func handleDir(name string) string {
	return "/var/lib/orbit-microvm/" + name
}
