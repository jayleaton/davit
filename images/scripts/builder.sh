#!/usr/bin/env bash
# Pinned, rootless build environment: Ubuntu 24.04 (ubuntu-base + snapshot.ubuntu.com at
# UBUNTU_SNAPSHOT) in a user-namespace chroot. Kernel compile and mkfs.ext4 run in here so the
# toolchain is pinned by inputs.lock instead of whatever the host distro ships.
#
#   builder.sh ensure                                   create/refresh $WORK/builder-<arch>
#   builder.sh run [-b HOST:GUEST[:ro]]... -- CMD...    run CMD as fake root inside it
#
# Host requirements: util-linux unshare + newuidmap/newgidmap with a /etc/subuid range, curl.
set -euo pipefail
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_lock
set_arch

BUILDER=$WORK/builder-$HOST_DEB_ARCH
BROOT=$BUILDER/root
PKGS=(build-essential bc bison flex libelf-dev libssl-dev cpio kmod xz-utils python3 e2fsprogs)
# Always the other arch's cross compiler too, so one builder serves both kernel arches (the
# package list is part of the cache stamp: per-build lists would rebuild it on every switch).
case $HOST_DEB_ARCH in
  amd64) PKGS+=(gcc-aarch64-linux-gnu) ;;
  arm64) PKGS+=(gcc-x86-64-linux-gnu) ;;
esac

stamp() {
  { cat "$IMAGES_DIR/inputs.lock" "${BASH_SOURCE[0]}" "$IMAGES_DIR/scripts/lib.sh"; printf '%s\n' "${PKGS[@]}"; } | sha256sum | cut -c1-64
}

inner_ensure() {
  rm -rf "$BROOT"
  mkdir -p "$BROOT"
  tar -xzf "$DL/ubuntu-base-$UBUNTU_BASE_VERSION-$HOST_DEB_ARCH.tar.gz" -C "$BROOT" --numeric-owner
  apt_setup "$BROOT" "$HOST_DEB_ARCH"
  ln -sfn /proc/self/mounts "$BROOT/etc/mtab" # mke2fs checks it before writing an image
  chroot_mount "$BROOT"
  # A failed apt-get reruns after 30 s, 60 s, ... (provision.sh's apt_get).
  # shellcheck disable=SC2016 # expands inside the chroot's shell
  chroot "$BROOT" /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin DEBIAN_FRONTEND=noninteractive \
    /bin/sh -euxc 'r() { n=0; until apt-get "$@"; do n=$((n + 1)); [ $n -le 7 ] || exit 1; sleep $((n * 30)); done; }
      r update -q; r -y -q dist-upgrade; r -y -q install "$@"; apt-get clean' \
    sh "${PKGS[@]}"
  # Pinned mke2fs -> /opt/e2fsprogs/sbin (static against e2fsprogs' own libs).
  mkdir -p "$BROOT/src"
  tar -xJf "$DL/e2fsprogs-$E2FSPROGS_VERSION.tar.xz" -C "$BROOT/src" --no-same-owner
  # shellcheck disable=SC2016 # $V expands inside the chroot's shell
  chroot "$BROOT" /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin V="$E2FSPROGS_VERSION" /bin/sh -euxc '
    cd "/src/e2fsprogs-$V"
    ./configure -q --prefix=/opt/e2fsprogs --disable-nls --disable-fuse2fs --disable-defrag --disable-debugfs --disable-imager --disable-resizer
    make -s -j"$(nproc)" libs
    make -s -C misc mke2fs
    install -D -m 0755 misc/mke2fs /opt/e2fsprogs/sbin/mke2fs
    /opt/e2fsprogs/sbin/mke2fs -V'
  rm -rf "$BROOT/src"
  chroot_umount "$BROOT"
}

inner_run() {
  local binds=() b src dst ro
  while [[ $# -gt 0 && $1 != -- ]]; do
    [[ $1 == -b ]] || die "builder run: unexpected $1"
    binds+=("$2")
    shift 2
  done
  shift
  chroot_mount "$BROOT"
  for b in "${binds[@]}"; do
    IFS=: read -r src dst ro <<<"$b"
    mkdir -p "$BROOT$dst"
    mount --rbind "$src" "$BROOT$dst"
    # Best effort: in a userns the remount fails if the source has locked flags (e.g. nosuid media).
    if [[ $ro == ro ]] && ! mount -o remount,bind,ro "$BROOT$dst" 2>/dev/null; then
      log "builder: could not remount $dst read-only; continuing read-write"
    fi
  done
  local rc=0
  chroot "$BROOT" /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin HOME=/root LC_ALL=C.UTF-8 TZ=UTC \
    SOURCE_DATE_EPOCH="$SOURCE_DATE_EPOCH" "$@" || rc=$?
  for b in "${binds[@]}"; do
    IFS=: read -r src dst ro <<<"$b"
    umount -l "$BROOT$dst"
  done
  chroot_umount "$BROOT"
  return "$rc"
}

case ${1:-} in
  ensure)
    want=$(stamp)
    if [[ -f $BUILDER/stamp && $(cat "$BUILDER/stamp") == "$want" ]]; then exit 0; fi
    fetch_ubuntu_base "$HOST_DEB_ARCH"
    fetch "$E2FSPROGS_URL" "$E2FSPROGS_SHA256" "$DL/e2fsprogs-$E2FSPROGS_VERSION.tar.xz"
    log "builder: creating $BROOT (snapshot $UBUNTU_SNAPSHOT)"
    rm_tree "$BUILDER"
    mkdir -p "$BUILDER"
    in_userns "$0" __ensure
    echo "$want" >"$BUILDER/stamp"
    ;;
  run)
    shift
    [[ -f $BUILDER/stamp ]] || die "builder missing: run builder.sh ensure"
    in_userns "$0" __run "$@"
    ;;
  __ensure) inner_ensure ;;
  __run) shift; inner_run "$@" ;;
  *) die "usage: builder.sh ensure | run [-b HOST:GUEST[:ro]]... -- CMD..." ;;
esac
