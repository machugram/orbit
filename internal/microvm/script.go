package microvm

// Shell scripts run as root on the carrier via the CarrierRunner. They are fed
// on stdin to `sh -s` and take values as positional args ($1, $2, …) rather
// than interpolated into the script text, so an attacker-influenced value (the
// public key, the name) cannot break out of its argument and inject commands.
//
// Two boot paths: direct-kernel (the light default, Alpine) and cloud-image
// (heavier, BTF kernel). See bootMode in types.go.

// provisionScript installs the QEMU toolchain on a Debian/Ubuntu carrier,
// covering both boot paths. Idempotent: present packages are skipped.
const provisionScript = `set -eu
export DEBIAN_FRONTEND=noninteractive
need="qemu-system-arm qemu-utils qemu-efi-aarch64 cloud-image-utils e2fsprogs curl tar"
missing=""
for p in $need; do
  dpkg -s "$p" >/dev/null 2>&1 || missing="$missing $p"
done
if [ -n "$missing" ]; then
  apt-get update -qq
  apt-get install -y -qq $missing
fi
mkdir -p /var/lib/orbit-microvm
`

// --- direct-kernel (light, default) ---------------------------------------

// launchDirectKernelScript stages a tiny Alpine guest and boots it directly
// (no UEFI). The agent key is baked into the rootfs at build time, so restarts
// need no key. Args:
//
//	$1 dir  $2 sshPort  $3 memMiB  $4 cpus  $5 qemuArch
//	$6 alpineVersion  $7 mirror  $8 authorizedKey  $9 packagesCSV
const launchDirectKernelScript = `set -eu
DIR="$1"; PORT="$2"; MEM="$3"; CPUS="$4"; ARCH="$5"; AVER="$6"; MIRROR="$7"; PUB="$8"; PKGS="$9"
mkdir -p "$DIR"
BRANCH="v$(echo "$AVER" | cut -d. -f1,2)"
REL="$MIRROR/$BRANCH/releases/$ARCH"

fetch() { # name url
  [ -f "$DIR/$1" ] || { curl -fsSL "$2" -o "$DIR/$1.part" && mv "$DIR/$1.part" "$DIR/$1"; }
}
# The "virt" flavor's kernel + initramfs include virtio drivers.
fetch vmlinuz   "$REL/netboot/vmlinuz-virt"
fetch initramfs "$REL/netboot/initramfs-virt"
fetch modloop   "$REL/netboot/modloop-virt"
MR="alpine-minirootfs-$AVER-$ARCH.tar.gz"
fetch "$MR" "$REL/$MR"

# Build the root filesystem image once (cached as root.img), configured via a
# native chroot (carrier and guest share the aarch64 arch).
if [ ! -f "$DIR/root.img" ]; then
  RFS="$DIR/rootfs"
  rm -rf "$RFS"; mkdir -p "$RFS"
  tar -xzf "$DIR/$MR" -C "$RFS"
  mkdir -p "$RFS/etc/apk"
  printf '%s\n%s\n' "$MIRROR/$BRANCH/main" "$MIRROR/$BRANCH/community" > "$RFS/etc/apk/repositories"
  cp /etc/resolv.conf "$RFS/etc/resolv.conf" 2>/dev/null || echo "nameserver 1.1.1.1" > "$RFS/etc/resolv.conf"

  mount -t proc none "$RFS/proc"
  mount --rbind /sys "$RFS/sys"
  mount --rbind /dev "$RFS/dev"
  trap 'umount -R "$RFS/proc" "$RFS/sys" "$RFS/dev" 2>/dev/null || true' EXIT

  chroot "$RFS" /bin/sh -eu <<CHROOT
apk update
apk add openrc openssh busybox-suid $(echo "$PKGS" | tr ',' ' ')
rc-update add devfs sysinit
rc-update add procfs sysinit
rc-update add sysfs sysinit
rc-update add hostname boot
rc-update add bootmisc boot
rc-update add networking boot
rc-update add sshd default
printf 'auto lo\niface lo inet loopback\nauto eth0\niface eth0 inet dhcp\n' > /etc/network/interfaces
echo orbit-microvm > /etc/hostname
adduser -D -s /bin/sh agent
mkdir -p /home/agent/.ssh
printf '%s\n' "$PUB" > /home/agent/.ssh/authorized_keys
chmod 700 /home/agent/.ssh
chmod 600 /home/agent/.ssh/authorized_keys
chown -R agent:agent /home/agent/.ssh
ssh-keygen -A
CHROOT

  umount -R "$RFS/proc" "$RFS/sys" "$RFS/dev" 2>/dev/null || true
  trap - EXIT
  # Pack the configured tree into an ext4 image without loop-mounting.
  truncate -s 2G "$DIR/root.img"
  mkfs.ext4 -F -q -d "$RFS" "$DIR/root.img"
  rm -rf "$RFS"
fi

directkernel_boot "$DIR" "$PORT" "$MEM" "$CPUS" "$ARCH"
` + directKernelBootFunc

// startDirectKernelScript reboots an already-staged direct-kernel guest.
// Args: $1 dir  $2 sshPort  $3 memMiB  $4 cpus  $5 qemuArch
const startDirectKernelScript = `set -eu
DIR="$1"; PORT="$2"; MEM="$3"; CPUS="$4"; ARCH="$5"
[ -f "$DIR/root.img" ] || { echo "microvm: no staged rootfs in $DIR" >&2; exit 1; }
directkernel_boot "$DIR" "$PORT" "$MEM" "$CPUS" "$ARCH"
` + directKernelBootFunc

// directKernelBootFunc defines directkernel_boot(dir, port, mem, cpus, arch):
// root.img is vda, modloop is vdb, the Alpine init mounts both.
const directKernelBootFunc = `
directkernel_boot() {
  d="$1"; port="$2"; mem="$3"; cpus="$4"; arch="$5"
  qemu-system-"$arch" \
    -machine virt -accel tcg -cpu max -smp "$cpus" -m "$mem" \
    -kernel "$d/vmlinuz" -initrd "$d/initramfs" \
    -append "console=ttyAMA0 root=/dev/vda rootfstype=ext4 rw modloop=/dev/vdb modules=ext4,virtio_blk,virtio_net,virtio_pci" \
    -drive if=virtio,format=raw,file="$d/root.img" \
    -drive if=virtio,format=raw,file="$d/modloop" \
    -netdev user,id=n0,hostfwd=tcp:0.0.0.0:"$port"-:22 \
    -device virtio-net-pci,netdev=n0 \
    -nographic -serial file:"$d/console.log" \
    -pidfile "$d/qemu.pid" -daemonize
}
`

// --- cloud-image (heavy, BTF kernel) --------------------------------------

// launchCloudImageScript stages a full cloud image and boots it under UEFI,
// injecting the key + eBPF packages via a cloud-init NoCloud seed. Args:
//
//	$1 dir  $2 imageURL  $3 sshPort  $4 memMiB  $5 cpus  $6 qemuArch
//	$7 firmware  $8 authorizedKey  $9 packagesCSV
const launchCloudImageScript = `set -eu
DIR="$1"; URL="$2"; PORT="$3"; MEM="$4"; CPUS="$5"; ARCH="$6"; FW="$7"; PUB="$8"; PKGS="$9"
mkdir -p "$DIR"
BASE="$DIR/base.img"
if [ ! -f "$BASE" ]; then
  curl -fsSL "$URL" -o "$BASE.part"
  mv "$BASE.part" "$BASE"
fi
rm -f "$DIR/disk.qcow2"
qemu-img create -q -f qcow2 -F qcow2 -b "$BASE" "$DIR/disk.qcow2" 8G

UD="$DIR/user-data"
cat > "$UD" <<EOF
#cloud-config
users:
  - name: agent
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - $PUB
ssh_pwauth: false
EOF
if [ -n "$PKGS" ]; then
  echo "package_update: true" >> "$UD"
  echo "packages:" >> "$UD"
  OLDIFS="$IFS"; IFS=","
  for pkg in $PKGS; do
    [ -n "$pkg" ] && printf '  - %s\n' "$pkg" >> "$UD"
  done
  IFS="$OLDIFS"
fi
cat > "$DIR/meta-data" <<EOF
instance-id: $(basename "$DIR")
local-hostname: $(basename "$DIR")
EOF
cloud-localds "$DIR/seed.iso" "$UD" "$DIR/meta-data"

cloudimage_boot "$DIR" "$PORT" "$MEM" "$CPUS" "$ARCH" "$FW"
` + cloudImageBootFunc

// startCloudImageScript reboots an already-staged cloud-image guest.
// Args: $1 dir  $2 sshPort  $3 memMiB  $4 cpus  $5 qemuArch  $6 firmware
const startCloudImageScript = `set -eu
DIR="$1"; PORT="$2"; MEM="$3"; CPUS="$4"; ARCH="$5"; FW="$6"
[ -f "$DIR/disk.qcow2" ] || { echo "microvm: no staged disk in $DIR" >&2; exit 1; }
cloudimage_boot "$DIR" "$PORT" "$MEM" "$CPUS" "$ARCH" "$FW"
` + cloudImageBootFunc

// cloudImageBootFunc defines cloudimage_boot(dir, port, mem, cpus, arch, fw).
const cloudImageBootFunc = `
cloudimage_boot() {
  d="$1"; port="$2"; mem="$3"; cpus="$4"; arch="$5"; fw="$6"
  qemu-system-"$arch" \
    -machine virt -accel tcg -cpu max -smp "$cpus" -m "$mem" \
    -bios "$fw" \
    -drive if=virtio,format=qcow2,file="$d/disk.qcow2" \
    -drive if=virtio,format=raw,file="$d/seed.iso" \
    -netdev user,id=n0,hostfwd=tcp:0.0.0.0:"$port"-:22 \
    -device virtio-net-pci,netdev=n0 \
    -nographic -serial file:"$d/console.log" \
    -pidfile "$d/qemu.pid" -daemonize
}
`

// --- lifecycle (mode-independent) -----------------------------------------

// stopScript kills the QEMU process, leaving the staged guest in place.
// Args: $1 dir
const stopScript = `set -u
DIR="$1"
if [ -f "$DIR/qemu.pid" ]; then
  kill "$(cat "$DIR/qemu.pid")" 2>/dev/null || true
  rm -f "$DIR/qemu.pid"
fi
`

// destroyScript stops the guest and removes its working directory entirely.
// Args: $1 dir
const destroyScript = `set -u
DIR="$1"
if [ -f "$DIR/qemu.pid" ]; then
  kill "$(cat "$DIR/qemu.pid")" 2>/dev/null || true
fi
rm -rf "$DIR"
`
