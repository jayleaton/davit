#!/bin/bash
# Runs inside the guest rootfs chroot (fake root in a user namespace), from build-rootfs.sh.
# /.provision holds this directory plus docker.asc, modules-builtin.tar, the davit-guest-agent
# binary and, for a derived image, its layer in /.provision/layer (layer.sh runs before sealing).
# Env: everything from inputs.lock, DEB_ARCH, AGENT_VERSION.
set -euxo pipefail
umask 022
export DEBIAN_FRONTEND=noninteractive
P=/.provision

apt-get update -q
apt-get -y -q dist-upgrade
mapfile -t pkgs < <(sed -e 's/#.*//' -e '/^[[:space:]]*$/d' "$P/packages.txt")
apt-get -y -q install "${pkgs[@]}"
# A derived image's own packages.txt (same format), also from the pinned snapshot: its layer.sh
# runs after the sources point at the live archive, so it must not apt-get itself.
if [[ -f $P/layer/packages.txt ]]; then
  mapfile -t pkgs < <(sed -e 's/#.*//' -e '/^[[:space:]]*$/d' "$P/layer/packages.txt")
  [[ ${#pkgs[@]} -eq 0 ]] || apt-get -y -q install --no-install-recommends "${pkgs[@]}"
fi

# Docker CE from download.docker.com, every package version pinned (that repo has no snapshots).
install -D -m 0644 "$P/docker.asc" /etc/apt/keyrings/docker.asc
cat >/etc/apt/sources.list.d/docker.sources <<EOF
Types: deb
URIs: $DOCKER_APT_URL
Suites: $UBUNTU_SUITE
Components: stable
Architectures: $DEB_ARCH
Signed-By: /etc/apt/keyrings/docker.asc
EOF
apt-get update -q
apt-get -y -q install \
  "docker-ce=$DOCKER_CE_VERSION" "docker-ce-cli=$DOCKER_CE_VERSION" "containerd.io=$CONTAINERD_VERSION" \
  "docker-buildx-plugin=$DOCKER_BUILDX_VERSION" "docker-compose-plugin=$DOCKER_COMPOSE_VERSION"
dpkg-query -W -f '${Package}\t${Version}\t${Architecture}\n' | LC_ALL=C sort >"$P/packages.lock"

# Built-in module list for the paired kernel, so `modprobe <built-in>` succeeds.
tar -xf "$P/modules-builtin.tar" -C / --no-same-owner

# Configuration overlay (root-owned; sudoers must be 0440).
cp -r --no-preserve=mode,ownership "$P/overlay/." /
chmod 0440 /etc/sudoers.d/davit

useradd --create-home --uid 1000 --user-group --shell /bin/bash --groups sudo,docker davit
# '*' = no password but not "locked" ('!'), which sshd would refuse even for key logins without PAM.
usermod --password '*' davit
install -d -m 0700 -o davit -g davit /home/davit/.ssh

# Guest agent: the repo's davit binary (scripts/build-agent.sh). Re-check its version here so a
# stale or swapped blob can never be sealed into an image.
install -m 0755 "$P/davit-guest-agent" /usr/local/bin/davit-guest-agent
agent_v=$(/usr/local/bin/davit-guest-agent -version) # argv[0] dispatch: agent flags only
[[ $agent_v == "davit guest-agent $AGENT_VERSION protocol "* ]] ||
  { echo "guest agent reports '$agent_v', want $AGENT_VERSION" >&2; exit 1; }
# The agent writes /etc/ssh/sshd_config.d/50-davit.conf; sshd must read that directory.
grep -qxF 'Include /etc/ssh/sshd_config.d/*.conf' /etc/ssh/sshd_config ||
  { echo "sshd_config does not Include sshd_config.d" >&2; exit 1; }

ln -sf /usr/share/zoneinfo/Etc/UTC /etc/localtime
echo Etc/UTC >/etc/timezone

systemctl enable systemd-networkd systemd-resolved ssh.service docker.service containerd.service \
  davit-ssh-hostkeys.service davit-guest-agent.service
# Plain sshd service, not socket activation: listening sockets must survive RAM snapshots.
systemctl disable ssh.socket
# Off the boot critical path / meaningless in an ephemeral microVM. Time is set by the host
# agent after restore, not by timesyncd.
systemctl mask apt-daily.timer apt-daily-upgrade.timer motd-news.timer e2scrub_all.timer \
  fstrim.timer dpkg-db-backup.timer systemd-networkd-wait-online.service getty@tty1.service \
  systemd-timesyncd.service
systemctl set-default multi-user.target

# Users get the live archive; the build itself only ever saw the pinned snapshot.
if [[ $DEB_ARCH == amd64 ]]; then mirror=http://archive.ubuntu.com/ubuntu; else mirror=http://ports.ubuntu.com/ubuntu-ports; fi
cat >/etc/apt/sources.list.d/ubuntu.sources <<EOF
Types: deb
URIs: $mirror
Suites: $UBUNTU_SUITE $UBUNTU_SUITE-updates $UBUNTU_SUITE-security
Components: main restricted universe multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
EOF

# Derived image layer (e.g. ubuntu-24.04-harness), before sealing so it gets sealed too.
if [[ -f $P/layer/layer.sh ]]; then
  export P
  bash -euxo pipefail "$P/layer/layer.sh"
fi

# --- seal: nothing per-machine, nothing secret, nothing time-dependent ---
apt-get clean
rm -rf /var/lib/apt/lists/* /var/cache/apt/*.bin /var/cache/debconf/*-old /var/lib/dpkg/*-old \
  /var/cache/ldconfig/aux-cache /var/lib/systemd/random-seed /var/lib/systemd/catalog/database \
  /etc/ssh/ssh_host_* /root/.bash_history /root/.cache /root/.npm /var/lib/dbus/machine-id
# Empty (not missing) machine-id: systemd generates one on each VM's first boot.
: >/etc/machine-id
find /var/log -type f -delete
# systemd-resolved's postinst backs up the build-time resolv.conf (the *host's* resolver).
rm -f /etc/.resolv.conf.systemd-resolved.bak
rm -f /usr/sbin/policy-rc.d /etc/apt/apt.conf.d/99davit-build
ln -sfn ../run/systemd/resolve/stub-resolv.conf /etc/resolv.conf
