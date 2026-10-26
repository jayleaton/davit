# shellcheck shell=bash
# Shared helpers for the davit image pipeline. Source, don't execute.
#
# Layout (all overridable via env, never under /tmp because /tmp is often RAM-backed):
#   DAVIT_IMAGES_WORK  scratch: downloads, builder chroot, kernel/rootfs trees
#                        (default ${XDG_CACHE_HOME:-~/.cache}/davit/images: outside the source
#                        tree, whose tools (gofmt -l ., editors) choke on subuid-owned chroot dirs)
#   DAVIT_IMAGES_OUT   content-addressed outputs: blobs/sha256/<hex>  (default images/out)
set -euo pipefail
# File modes in the image must not depend on the caller's umask.
umask 022

IMAGES_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
WORK=${DAVIT_IMAGES_WORK:-${XDG_CACHE_HOME:-$HOME/.cache}/davit/images}
OUT=${DAVIT_IMAGES_OUT:-$IMAGES_DIR/out}
DL=$WORK/dl

log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
die() { log "error: $*"; exit 1; }

# load_kv <file>: export KEY=VALUE lines verbatim (values are never shell-evaluated).
load_kv() {
  local line key
  while IFS= read -r line; do
    [[ $line =~ ^[A-Za-z_][A-Za-z0-9_]*= ]] || continue
    key=${line%%=*}
    printf -v "$key" '%s' "${line#*=}"
    export "${key?}"
  done <"$1"
}

load_lock() { load_kv "$IMAGES_DIR/inputs.lock"; }

# ARCH is the kernel/Firecracker name (x86_64|aarch64); DEB_ARCH the Debian one.
set_arch() {
  ARCH=${ARCH:-$(uname -m)}
  case $ARCH in
    x86_64 | amd64) ARCH=x86_64 DEB_ARCH=amd64 KARCH=x86 KIMAGE=vmlinux CROSS_PREFIX=x86_64-linux-gnu- ;;
    aarch64 | arm64) ARCH=aarch64 DEB_ARCH=arm64 KARCH=arm64 KIMAGE=arch/arm64/boot/Image CROSS_PREFIX=aarch64-linux-gnu- ;;
    *) die "unsupported ARCH=$ARCH" ;;
  esac
  HOST_DEB_ARCH=$(case $(uname -m) in x86_64) echo amd64 ;; aarch64) echo arm64 ;; esac)
  # The builder always runs natively; only the kernel is cross-compiled.
  if [[ $DEB_ARCH == "$HOST_DEB_ARCH" ]]; then CROSS_PREFIX=''; fi
  export ARCH DEB_ARCH KARCH KIMAGE CROSS_PREFIX HOST_DEB_ARCH
}

ubuntu_mirror() { # <deb-arch>
  if [[ $1 == amd64 ]]; then echo "https://snapshot.ubuntu.com/ubuntu/$UBUNTU_SNAPSHOT"; else echo "https://snapshot.ubuntu.com/ubuntu-ports/$UBUNTU_SNAPSHOT"; fi
}

# apt_setup <root> <deb-arch>: point a fresh ubuntu-base tree at the pinned snapshot.
# Runs inside the user namespace (builder and guest rootfs).
apt_setup() {
  local r=$1 a=$2
  rm -f "$r/etc/apt/sources.list" "$r/etc/apt/sources.list.d/"*
  cat >"$r/etc/apt/sources.list.d/ubuntu.sources" <<EOF
Types: deb
URIs: $(ubuntu_mirror "$a")
Suites: $UBUNTU_SUITE $UBUNTU_SUITE-updates $UBUNTU_SUITE-security
Components: main universe
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
EOF
  cat >"$r/etc/apt/apt.conf.d/99davit-build" <<'EOF'
// Build-time only; removed before the image is sealed.
APT::Sandbox::User "root";
Acquire::https::CAInfo "/.host-ca.crt";
Acquire::Check-Valid-Until "false";
Acquire::Languages "none";
APT::Install-Recommends "false";
APT::Install-Suggests "false";
Dpkg::Use-Pty "false";
EOF
  printf '#!/bin/sh\nexit 101\n' >"$r/usr/sbin/policy-rc.d"
  chmod 0755 "$r/usr/sbin/policy-rc.d"
}

fetch_ubuntu_base() { # <deb-arch>
  local sum=UBUNTU_BASE_SHA256_$1
  fetch "$UBUNTU_BASE_URL_PREFIX/ubuntu-base-$UBUNTU_BASE_VERSION-base-$1.tar.gz" "${!sum}" \
    "$DL/ubuntu-base-$UBUNTU_BASE_VERSION-$1.tar.gz"
}

# fetch <url> <sha256> <dest>: download once, verify every time.
fetch() {
  local url=$1 sum=$2 dest=$3
  if [[ ! -f $dest ]] || ! echo "$sum  $dest" | sha256sum -c --status; then
    mkdir -p "$(dirname "$dest")"
    log "fetch $url"
    curl -sSLf --retry 3 -o "$dest.part" "$url"
    echo "$sum  $dest.part" | sha256sum -c --status || die "sha256 mismatch for $url (got $(sha256sum "$dest.part" | cut -c1-64))"
    mv "$dest.part" "$dest"
  fi
}

# Run a command as fake root in a user namespace with the caller's subuid/subgid range
# mapped (unshare --map-auto), plus private mount and pid namespaces. No real root needed.
in_userns() {
  unshare --map-auto --map-root-user --mount --pid --fork --kill-child -- "$@"
}

# Remove a tree that may contain subuid-owned files.
rm_tree() {
  local d
  for d; do
    [[ -e $d ]] || continue
    rm -rf "$d" 2>/dev/null || in_userns rm -rf "$d"
  done
}

# Inside the userns only: prepare a chroot for apt/dpkg and tear it down again.
chroot_mount() { # <root>
  local r=$1 d
  mount -t proc proc "$r/proc"
  mount -t tmpfs -o mode=1777 tmpfs "$r/tmp"
  # DNS: host resolver (same network namespace), also behind systemd-resolved's stub path in
  # case a package switches /etc/resolv.conf to that symlink mid-install.
  mkdir -p "$r/run"
  mount -t tmpfs -o mode=0755 tmpfs "$r/run"
  mkdir -p "$r/run/systemd/resolve"
  cp -L /etc/resolv.conf "$r/run/systemd/resolve/stub-resolv.conf"
  rm -f "$r/etc/resolv.conf"
  cp -L /etc/resolv.conf "$r/etc/resolv.conf"
  for d in null zero full random urandom tty; do
    : >"$r/dev/$d"
    mount --bind "/dev/$d" "$r/dev/$d"
  done
  ln -sfn /proc/self/fd "$r/dev/fd"
  ln -sfn /proc/self/fd/0 "$r/dev/stdin"
  ln -sfn /proc/self/fd/1 "$r/dev/stdout"
  ln -sfn /proc/self/fd/2 "$r/dev/stderr"
  # TLS to snapshot.ubuntu.com / download.docker.com before ca-certificates is installed.
  # Transport only: package integrity comes from the pinned apt signing keys.
  : >"$r/.host-ca.crt"
  mount --bind /etc/ssl/certs/ca-certificates.crt "$r/.host-ca.crt"
}

chroot_umount() { # <root>
  local r=$1 d
  # Separate statements so a failed umount trips errexit instead of being swallowed by &&.
  umount "$r/.host-ca.crt"
  rm -f "$r/.host-ca.crt"
  for d in null zero full random urandom tty; do
    umount "$r/dev/$d"
    rm -f "$r/dev/$d"
  done
  rm -f "$r/dev/fd" "$r/dev/stdin" "$r/dev/stdout" "$r/dev/stderr"
  umount "$r/run" "$r/tmp" "$r/proc"
}

sha256_of() { sha256sum "$1" | cut -c1-64; }

# Store a file in OUT as blobs/sha256/<hex> (reflink when possible, sparse-preserving) and print the hex.
put_blob() { # <file>
  local f=$1 h
  h=$(sha256_of "$f")
  mkdir -p "$OUT/blobs/sha256"
  if [[ ! -f $OUT/blobs/sha256/$h ]]; then
    cp --reflink=auto --sparse=always "$f" "$OUT/blobs/sha256/$h.part"
    chmod 0444 "$OUT/blobs/sha256/$h.part"
    mv "$OUT/blobs/sha256/$h.part" "$OUT/blobs/sha256/$h"
  fi
  echo "$h"
}
