#!/bin/bash
# Runs inside the guest rootfs chroot (fake root in a user namespace), from build-rootfs.sh.
# /.provision holds this directory plus docker.asc, modules-builtin.tar, the davit-guest-agent
# binary and, for a derived image, one layer per derived recipe in /.provision/layers/<n>-<recipe>,
# root first (each layer.sh runs as /.provision/layer/layer.sh before sealing).
# Env: everything from inputs.lock, DEB_ARCH, AGENT_VERSION.
set -euxo pipefail
umask 022
export DEBIAN_FRONTEND=noninteractive
P=/.provision

# The pinned snapshot mirror answers 502/503 for minutes at a time, and apt does not retry HTTP
# errors: rerun a failed apt-get (archives it already fetched stay in its cache) after 30 s,
# 60 s, ... 210 s, about 14 minutes in all. `update` needs --error-on=any: by default a 5xx on an
# index is a warning, update exits 0 and every install after it finds no packages.
apt_get() {
  local n
  for n in 1 2 3 4 5 6 7; do
    apt-get "$@" && return 0
    echo "provision: apt-get $* failed (attempt $n), retrying in $((n * 30)) s" >&2
    sleep $((n * 30))
  done
  apt-get "$@"
}

# With DAVIT_APT_CACHE (lib.sh) the indexes of these exact sources may already be here: the snapshot
# never changes, so then there is nothing to update and no mirror to reach. The marker also covers
# inputs.lock, because the Docker repository is not a snapshot and its pinned versions live there.
apt_update() {
  local m src=(/etc/apt/sources.list.d/*.sources)
  [[ ! -f $P/apt-cache/inputs.lock ]] || src+=("$P/apt-cache/inputs.lock")
  m=/var/lib/apt/davit-sources/$(cat "${src[@]}" | sha256sum | cut -c1-16)
  if [[ -f $m ]]; then
    echo "provision: apt indexes from the build's apt cache (sources ${m##*/})" >&2
    return 0
  fi
  apt_get update -q --error-on=any
  mkdir -p "${m%/*}"
  : >"$m"
}

apt_update
apt_get -y -q dist-upgrade
mapfile -t pkgs < <(sed -e 's/#.*//' -e '/^[[:space:]]*$/d' "$P/packages.txt")
apt_get -y -q install "${pkgs[@]}"
# Each layer's own packages.txt (same format), also from the pinned snapshot: the layer.sh scripts
# run after the sources point at the live archive, so they must not apt-get themselves.
layers=()
[[ ! -d $P/layers ]] || mapfile -t layers < <(find "$P/layers" -mindepth 1 -maxdepth 1 -type d | LC_ALL=C sort)
for l in "${layers[@]}"; do
  [[ -f $l/packages.txt ]] || continue
  mapfile -t pkgs < <(sed -e 's/#.*//' -e '/^[[:space:]]*$/d' "$l/packages.txt")
  [[ ${#pkgs[@]} -eq 0 ]] || apt_get -y -q install --no-install-recommends "${pkgs[@]}"
done

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
apt_update
apt_get -y -q install \
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

# Derived image layers (e.g. ubuntu-24.04-harness), root first, before sealing so they get sealed
# too. Each runs as $P/layer, the path a layer.sh finds its recipe and stage at.
export P
for l in "${layers[@]}"; do
  [[ -f $l/layer.sh ]] || continue
  ln -sfn "$l" "$P/layer"
  bash -euxo pipefail "$P/layer/layer.sh"
done

# --- seal: nothing per-machine, nothing secret, nothing time-dependent ---
# What apt fetched goes back to the build's apt cache first (build-rootfs.sh copies it out).
if [[ -d $P/apt-cache ]]; then
  find /var/cache/apt/archives -maxdepth 1 -name '*.deb' -exec cp -t "$P/apt-cache/archives/" {} +
  find /var/lib/apt/lists -maxdepth 1 -type f ! -name lock ! -name '.*' -exec cp -a -t "$P/apt-cache/lists/" {} +
  find /var/lib/apt/davit-sources -maxdepth 1 -type f -exec cp -a -t "$P/apt-cache/markers/" {} +
fi
rm -rf /var/lib/apt/davit-sources # apt_update's markers
find /var/lib/apt/lists -mindepth 1 -maxdepth 1 -name '.*' -exec rm -rf {} + # the lists/* below skips these
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
