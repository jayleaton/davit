#!/usr/bin/env bash
# Rootless Ubuntu 24.04 rootfs (systemd + sshd + docker.io) -> ext4 image.
# No sudo: uses a user namespace with subuid mapping (unshare --map-auto -r),
# chroot + apt inside it, then mkfs.ext4 -d so ownership lands as root:root.
# Usage: [$HEAVY] scripts/build-rootfs.sh <out.ext4> [size]
#        [$HEAVY] scripts/build-rootfs.sh <out.tar>   # OCI/docker-archive flavor for microsandbox:
#        no static network or vsock-ssh unit (the runtime owns networking and SSH).
set -euo pipefail
S=$(cd "$(dirname "$0")/.." && pwd)
OUT=${OUT:-$(realpath -m "${1:?out.ext4}")}
SIZE=${SIZE:-${2:-4G}}
TARBALL=$S/dl/ubuntu/ubuntu-base-24.04.5-base-amd64.tar.gz
KEY=$S/work/id_ed25519
[[ -f $KEY ]] || ssh-keygen -q -t ed25519 -N '' -f "$KEY"
BUILD=$S/work/rootfs-build
if [[ ${1:-} != __inner ]]; then
  rm -rf "$BUILD" 2>/dev/null || unshare --map-auto -r rm -rf "$BUILD"
  mkdir -p "$BUILD"
  exec unshare --map-auto --map-root-user --mount --pid --fork \
    env OUT="$OUT" SIZE="$SIZE" TARBALL="$TARBALL" KEY="$KEY" BUILD="$BUILD" \
    "$0" __inner
fi
R=$BUILD/root
mkdir -p "$R"
tar -xzf "$TARBALL" -C "$R" --numeric-owner
mount -t proc proc "$R/proc"
for d in null zero full random urandom tty; do touch "$R/dev/$d"; mount --bind "/dev/$d" "$R/dev/$d"; done
cp /etc/resolv.conf "$R/etc/resolv.conf"
printf '#!/bin/sh\nexit 101\n' > "$R/usr/sbin/policy-rc.d"; chmod +x "$R/usr/sbin/policy-rc.d"

chroot "$R" /bin/bash -euxo pipefail <<'EOF'
export DEBIAN_FRONTEND=noninteractive
# _apt sandbox user cannot setgroups in a userns with one gid range; run as root.
echo 'APT::Sandbox::User "root";' > /etc/apt/apt.conf.d/99sandbox
apt-get update
apt-get install -y --no-install-recommends \
  systemd systemd-sysv systemd-resolved udev dbus openssh-server docker.io iptables \
  iproute2 ca-certificates curl sudo socat busybox-static less vim-tiny
update-alternatives --set iptables /usr/sbin/iptables-legacy
update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy
apt-get clean; rm -rf /var/lib/apt/lists/*

echo vm > /etc/hostname
printf '127.0.0.1 localhost\n127.0.1.1 vm\n' > /etc/hosts
echo '/dev/vda / ext4 defaults,noatime 0 1' > /etc/fstab
passwd -d root
mkdir -p /root/.ssh && chmod 700 /root/.ssh
cat > /etc/ssh/sshd_config.d/10-spike.conf <<C
PermitRootLogin prohibit-password
PasswordAuthentication no
UseDNS no
C
ssh-keygen -A

# Static guest network; each VM lives in its own netns so every fork can keep 172.16.0.2.
mkdir -p /etc/systemd/network
cat > /etc/systemd/network/10-eth0.network <<C
[Match]
Name=eth0
[Network]
Address=172.16.0.2/24
Gateway=172.16.0.1
DNS=1.1.1.1
LinkLocalAddressing=no
IPv6AcceptRA=no
C
systemctl enable systemd-networkd ssh.service docker.service
# Firecracker CI kernel lacks CONFIG_IP_NF_RAW; Docker >=28.0.2 can skip its raw-table rule.
# A production guest kernel should enable IP_NF_RAW (and NF_TABLES) and drop this.
mkdir -p /etc/systemd/system/docker.service.d
printf '[Service]\nEnvironment=DOCKER_INSECURE_NO_IPTABLES_RAW=1\n' \
  > /etc/systemd/system/docker.service.d/10-no-raw.conf

# vsock:22 -> sshd, so host can SSH without any guest network (Firecracker hybrid vsock).
cat > /etc/systemd/system/vsock-ssh.service <<C
[Unit]
Description=vsock port 22 -> sshd
After=ssh.service
[Service]
ExecStart=/usr/bin/socat VSOCK-LISTEN:22,reuseaddr,fork TCP:127.0.0.1:22
Restart=always
[Install]
WantedBy=multi-user.target
C
systemctl enable vsock-ssh.service
# ssh.socket would add socket-activation latency semantics; use the plain service.
systemctl disable ssh.socket 2>/dev/null || true
# Boot-time trimming: things a dev VM does not need on the critical path.
systemctl mask apt-daily.timer apt-daily-upgrade.timer motd-news.timer e2scrub_all.timer \
  fstrim.timer dpkg-db-backup.timer systemd-networkd-wait-online.service \
  getty@tty1.service systemd-timesyncd.service 2>/dev/null || true
systemctl set-default multi-user.target
EOF
cat "$KEY.pub" > "$R/root/.ssh/authorized_keys"; chmod 600 "$R/root/.ssh/authorized_keys"
rm -f "$R/usr/sbin/policy-rc.d" "$R/etc/apt/apt.conf.d/99sandbox"
ln -sf /run/systemd/resolve/stub-resolv.conf "$R/etc/resolv.conf"
for d in null zero full random urandom tty; do umount "$R/dev/$d"; rm -f "$R/dev/$d"; done
umount "$R/proc"
du -sh "$R"
rm -f "$OUT"
if [[ $OUT == *.tar ]]; then
  rm -f "$R/etc/systemd/network/10-eth0.network" "$R/etc/systemd/system/multi-user.target.wants/vsock-ssh.service"
  rm -f "$R/etc/resolv.conf"; echo 'nameserver 1.1.1.1' > "$R/etc/resolv.conf"
  tar --numeric-owner --xattrs -cf "$BUILD/layer.tar" -C "$R" .
  python3 -I "$(dirname "$0")/oci-archive.py" "$BUILD/layer.tar" "$OUT" ubuntu-dev:24.04
else
  mkfs.ext4 -q -F -L rootfs -d "$R" "$OUT" "$SIZE"
fi
rm -rf "$BUILD"
ls -lsh "$OUT"
