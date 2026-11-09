package vm

// provisionScript returns the POSIX-sh script that prepares a freshly
// created OrbStack VM for agent SSH access. It is run as root via `orb` and
// is fully distro-agnostic: package and init managers are detected at
// runtime, so one script covers the Debian, RHEL/Fedora, Arch, SUSE,
// Alpine, and Void families without any per-distro configuration.
//
// Why this exists instead of cloud-init (which spec §4 assumed): OrbStack
// only ships cloud-init in its Debian/Ubuntu images — Alpine, Fedora, Arch
// and friends have no cloud-init at all (verified empirically). But the
// `orb` multiplexer grants root in every machine regardless of distro or
// isolation, so a post-create root script is the portable provisioning
// channel.
//
// The script is idempotent (safe to retry) and takes two positional
// arguments — the agent username ($1) and the authorized_keys line ($2) —
// passed as real argv entries rather than interpolated, so a key can never
// break out of the script.
//
// Steps: install an OpenSSH server, generate host keys, install the agent's
// authorized_keys, unlock the agent account (orbctl creates it
// password-locked, which OpenSSH refuses even for pubkey auth), and enable
// sshd under whatever init system is present.
func provisionScript() string {
	return `set -eu
U="$1"; KEY="$2"

# 1. Install an OpenSSH server with whatever package manager exists.
if command -v apt-get >/dev/null 2>&1; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq && apt-get install -y -qq openssh-server
elif command -v dnf >/dev/null 2>&1; then
  dnf install -y -q openssh-server
elif command -v yum >/dev/null 2>&1; then
  yum install -y openssh-server
elif command -v zypper >/dev/null 2>&1; then
  zypper -n install openssh
elif command -v pacman >/dev/null 2>&1; then
  pacman -Sy --noconfirm --needed openssh
elif command -v apk >/dev/null 2>&1; then
  apk add --no-cache openssh
elif command -v xbps-install >/dev/null 2>&1; then
  xbps-install -Sy openssh
elif ! command -v sshd >/dev/null 2>&1 && [ ! -x /usr/sbin/sshd ]; then
  echo "orbit: no supported package manager and sshd not present" >&2
  exit 1
fi

# 2. Generate host keys if the package install didn't.
ssh-keygen -A >/dev/null 2>&1 || true

# 3. Install the agent's authorized_keys in its real home directory.
H=$(awk -F: -v u="$U" '$1==u{print $6}' /etc/passwd)
[ -n "$H" ] || H="/home/$U"
mkdir -p "$H/.ssh"
printf '%s\n' "$KEY" > "$H/.ssh/authorized_keys"
chmod 700 "$H/.ssh"; chmod 600 "$H/.ssh/authorized_keys"
chown -R "$U" "$H/.ssh" 2>/dev/null || true

# 4. Unlock the account. orbctl creates it password-locked ("!"), which
#    OpenSSH treats as locked and refuses even with a valid key. Rewriting
#    the field to "*" keeps password login disabled but allows pubkey auth.
if [ -f /etc/shadow ]; then
  sed -i "s/^$U:!*:/$U:*:/" /etc/shadow 2>/dev/null || true
fi

# 5. Enable and start sshd under whatever init system is present.
if command -v systemctl >/dev/null 2>&1; then
  systemctl enable --now sshd 2>/dev/null || systemctl enable --now ssh
elif command -v rc-update >/dev/null 2>&1; then
  rc-update add sshd default; rc-service sshd start
elif command -v sv >/dev/null 2>&1 && [ -d /etc/sv/sshd ]; then
  ln -sf /etc/sv/sshd /var/service/
else
  echo "orbit: could not enable sshd (unknown init system)" >&2
  exit 1
fi

echo ORBIT_PROVISIONED
`
}

// keyInjectScript updates authorized_keys on a machine that already has
// sshd (a clone of a warm base). It skips package install and init setup.
func keyInjectScript() string {
	return `set -eu
U="$1"; KEY="$2"

H=$(awk -F: -v u="$U" '$1==u{print $6}' /etc/passwd)
[ -n "$H" ] || H="/home/$U"
mkdir -p "$H/.ssh"
printf '%s\n' "$KEY" > "$H/.ssh/authorized_keys"
chmod 700 "$H/.ssh"; chmod 600 "$H/.ssh/authorized_keys"
chown -R "$U" "$H/.ssh" 2>/dev/null || true

if [ -f /etc/shadow ]; then
  sed -i "s/^$U:!*:/$U:*:/" /etc/shadow 2>/dev/null || true
fi

echo ORBIT_PROVISIONED
`
}
