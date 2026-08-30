#!/usr/bin/env bash
# prep-host.sh — one-time preparation of a single-board computer for the pool.
#
# Run as root on the board, over the login you already have:
#
#   ssh pi@<board> "sudo env KPSSH_PUBKEY='$(cat pool_ed25519.pub)' bash -s" < examples/sbc/prep-host.sh
#
# It does only what the join profile cannot do for itself:
#   1. the pool login — a dedicated user, your pool public key, passwordless sudo
#   2. the memory cgroup controller — Raspberry Pi OS boots with it OFF; the
#      fix is a cmdline.txt edit and one reboot
#   3. Wi-Fi power save OFF — the Pi's brcmfmac otherwise drops off the network
#      after idle minutes (dmesg: "power save enabled")
#   4. legacy swap off
# Everything else (containerd, kubelet, CNI plugins, sysctls) is the join
# profile's install phase (profile-sbc.yaml), cached per host. Idempotent —
# run it again whenever. cloud-init.yaml is the same thing at image time.
set -euo pipefail

: "${KPSSH_PUBKEY:?set KPSSH_PUBKEY to the pool public key line (contents of pool_ed25519.pub)}"
KPSSH_USER="${KPSSH_USER:-kpssh}"
KPSSH_REBOOT="${KPSSH_REBOOT:-0}"   # 1: reboot by itself when the cgroup change needs it
[ "$(id -u)" -eq 0 ] || { echo "run as root (sudo)" >&2; exit 1; }

# 1. pool login ---------------------------------------------------------------
if ! id -u "${KPSSH_USER}" >/dev/null 2>&1; then
  useradd --create-home --shell /bin/bash --comment "karpenter-provider-ssh pool login" "${KPSSH_USER}"
fi
home=$(getent passwd "${KPSSH_USER}" | cut -d: -f6)
install -d -m 700 -o "${KPSSH_USER}" -g "${KPSSH_USER}" "${home}/.ssh"
touch "${home}/.ssh/authorized_keys"
grep -qxF "${KPSSH_PUBKEY}" "${home}/.ssh/authorized_keys" \
  || echo "${KPSSH_PUBKEY}" >> "${home}/.ssh/authorized_keys"
chmod 600 "${home}/.ssh/authorized_keys"
chown "${KPSSH_USER}:${KPSSH_USER}" "${home}/.ssh/authorized_keys"
# Raw mode runs profile scripts as `sudo bash -s`: full sudo, no password, no
# TTY. Verified mode narrows this to the shim binary — docs/verified-exec.md.
printf '%s ALL=(root) NOPASSWD: ALL\n' "${KPSSH_USER}" > "/etc/sudoers.d/${KPSSH_USER}"
chmod 0440 "/etc/sudoers.d/${KPSSH_USER}"
visudo -cf "/etc/sudoers.d/${KPSSH_USER}" >/dev/null

# 2. memory cgroup ------------------------------------------------------------
reboot_needed=0
if ! grep -qw memory /sys/fs/cgroup/cgroup.controllers 2>/dev/null; then
  cmdline=""
  for f in /boot/firmware/cmdline.txt /boot/cmdline.txt; do
    if [ -f "$f" ]; then cmdline="$f"; break; fi
  done
  if [ -z "$cmdline" ]; then
    echo "memory cgroup controller is off and this is not a Raspberry Pi boot layout:" >&2
    echo "add 'cgroup_enable=cpuset cgroup_enable=memory cgroup_memory=1' to the kernel command line your bootloader uses, reboot, run again" >&2
    exit 1
  fi
  if ! grep -q 'cgroup_enable=memory' "$cmdline"; then
    cp "$cmdline" "$cmdline.kpssh.bak"
    sed -i '1 s/$/ cgroup_enable=cpuset cgroup_enable=memory cgroup_memory=1/' "$cmdline"
  fi
  reboot_needed=1
fi

# 3. Wi-Fi power save ---------------------------------------------------------
wifi=0
for w in /sys/class/net/wl*; do [ -e "$w" ] && wifi=1; done
if [ "$wifi" = 1 ] && [ -d /etc/NetworkManager ]; then
  install -d -m 755 /etc/NetworkManager/conf.d
  cat > /etc/NetworkManager/conf.d/wifi-powersave-off.conf <<'EOF'
# brcmfmac (Raspberry Pi) enables 802.11 power save by default; the board then
# misses beacons on a busy channel and drops off the network. 2 = off.
[connection]
wifi.powersave = 2
EOF
  nmcli general reload conf 2>/dev/null || true      # persistent; live below
fi
if [ "$wifi" = 1 ]; then
  for w in /sys/class/net/wl*; do
    [ -e "$w" ] || continue
    PATH="$PATH:/usr/sbin:/sbin" iw dev "$(basename "$w")" set power_save off 2>/dev/null || true
  done
fi

# 4. swap ---------------------------------------------------------------------
systemctl disable --now dphys-swapfile 2>/dev/null || true
swapoff -a 2>/dev/null || true

# summary ---------------------------------------------------------------------
mem_mi=$(( $(awk '/MemTotal/ {print $2}' /proc/meminfo) / 1024 ))
addr=$(ip -4 -o route get 1.1.1.1 2>/dev/null | sed -nE 's/.* src ([0-9.]+).*/\1/p')
cat <<EOF
kpssh prep: OK
  SSHHost spec.user:      ${KPSSH_USER}
  SSHHost spec.address:   ${addr:-unknown}   (reserve it in DHCP — the field is immutable)
  arch:                   $(uname -m)
  SSHHost spec.capacity:  cpu "$(nproc)", memory ≤ ${mem_mi}Mi (probe-observed; keep headroom, e.g. $(( mem_mi * 9 / 10 ))Mi)
EOF
if [ "$reboot_needed" = 1 ]; then
  echo "  REBOOT REQUIRED: memory cgroup enabled in $cmdline, effective on the next boot"
  if [ "$KPSSH_REBOOT" = 1 ]; then echo "  rebooting now"; systemctl reboot; fi
fi
