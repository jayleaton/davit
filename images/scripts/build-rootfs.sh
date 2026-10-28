#!/usr/bin/env bash
# Build a guest rootfs ext4 image reproducibly and rootless.
#   IMAGE=ubuntu-24.04-base SIZE=4G images/scripts/build-rootfs.sh
# Needs the paired kernel's $WORK/kernel-<arch>.env (build-kernel.sh) for modules.builtin and the
# guest agent's $WORK/agent-<arch>.env (build-agent.sh).
#
# Image recipes live in rootfs/<IMAGE>/. A derived image has a `base` file naming its parent
# recipe, which may be derived too (a chain such as ci -> dev -> base): the root recipe provisions
# as usual and every derived one, root first, runs as a layer (its packages.txt from the snapshot,
# then its layer.sh, before sealing); an optional stage.sh in each stages verified downloads
# host-side first.
# The rootfs arch must match the host (apt runs natively inside the chroot, no qemu-user).
#
# Steps, all inside one user namespace (fake root via /etc/subuid, no sudo):
#   ubuntu-base tarball -> chroot + apt from the pinned snapshot (provision.sh)
#   -> seal -> builder's pinned mke2fs -d with SOURCE_DATE_EPOCH, fixed UUID and hash seed.
set -euo pipefail
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_lock
set_arch
IMAGE=${IMAGE:-ubuntu-24.04-base}
SIZE=${SIZE:-4G}
DEF=$IMAGES_DIR/rootfs/$IMAGE
RWORK=$WORK/rootfs-$IMAGE-$DEB_ARCH

inner() {
  local r=$RWORK/root b=$WORK/builder-$HOST_DEB_ARCH/root
  mkdir -p "$r"
  tar -xzf "$DL/ubuntu-base-$UBUNTU_BASE_VERSION-$DEB_ARCH.tar.gz" -C "$r" --numeric-owner
  apt_setup "$r" "$DEB_ARCH"
  chroot_mount "$r"
  mkdir -p "$r/.provision"
  cp -r "$BASE_DEF/." "$r/.provision/"
  # Layers in /.provision/layers/<n>-<recipe>, numbered root first; provision.sh runs each one
  # as /.provision/layer.
  local layers=() n=0 l d
  read -ra layers <<<"$CHAIN"
  for l in "${layers[@]:1}"; do
    n=$((n + 1))
    d=$r/.provision/layers/$(printf %02d "$n")-$l
    mkdir -p "$d"
    cp -r "$IMAGES_DIR/rootfs/$l/." "$d/"
    if [[ -d $RWORK/stage/$l ]]; then cp -r "$RWORK/stage/$l" "$d/stage"; fi
  done
  cp "$DL/docker.asc" "$r/.provision/docker.asc"
  cp "$RWORK/modules-builtin.tar" "$r/.provision/modules-builtin.tar"
  cp "$RWORK/davit-guest-agent" "$r/.provision/davit-guest-agent"
  if [[ -n $APT_CACHE ]]; then
    apt_cache_seed "$r" "$DEB_ARCH"
    # provision.sh hands what apt fetched back here before the seal empties it.
    mkdir -p "$r/.provision/apt-cache/archives" "$r/.provision/apt-cache/lists" "$r/.provision/apt-cache/markers"
    cp "$IMAGES_DIR/inputs.lock" "$r/.provision/apt-cache/inputs.lock"
  fi
  local env=(PATH=/usr/sbin:/usr/bin:/sbin:/bin HOME=/root LC_ALL=C.UTF-8 TZ=UTC DEB_ARCH="$DEB_ARCH"
    AGENT_VERSION="$AGENT_VERSION") k
  while read -r k; do env+=("$k=${!k}"); done < <(sed -nE 's/^([A-Za-z_][A-Za-z0-9_]*)=.*/\1/p' "$IMAGES_DIR/inputs.lock")
  chroot "$r" /usr/bin/env -i "${env[@]}" /bin/bash /.provision/provision.sh
  mv "$r/.provision/packages.lock" "$RWORK/packages.lock"
  if [[ -n $APT_CACHE ]]; then
    apt_cache_harvest "$r/.provision/apt-cache/archives" "$r/.provision/apt-cache/lists" \
      "$r/.provision/apt-cache/markers" "$DEB_ARCH"
  fi
  rm -rf "$r/.provision"
  chroot_umount "$r"
  # Guard: the build borrows the host's resolv.conf; its search domains must not reach the image
  # (systemd-resolved's postinst once copied it to /etc/.resolv.conf.systemd-resolved.bak).
  local doms=() d pat=()
  read -ra doms < <(sed -n 's/^search //p' /etc/resolv.conf) || true
  # `search .` (systemd-resolved with no search domain) is the root, not a domain: as a
  # pattern "." would match every file. A trailing dot is dropped; a domain needs a dot between labels.
  for d in "${doms[@]}"; do d=${d%.}; [[ $d == ?*.?* ]] && pat+=(-e "$d"); done
  if [[ ${#pat[@]} -gt 0 ]] && grep -rlsF "${pat[@]}" "$r/etc" "$r/var" "$r/root" "$r/home"; then
    die "host resolver configuration leaked into the rootfs (files above)"
  fi
  # mke2fs >= 1.47.1 clamps inode times to SOURCE_DATE_EPOCH; clamp the tree as well so the
  # input to mkfs is itself stable.
  find "$r" -newermt "@$SOURCE_DATE_EPOCH" -exec touch -h -d "@$SOURCE_DATE_EPOCH" {} +

  # mkfs inside the builder chroot: pinned mke2fs, deterministic layout.
  mkdir -p "$b/rootfs" "$b/out"
  mount --rbind "$r" "$b/rootfs"
  mount --bind "$RWORK" "$b/out"
  chroot "$b" /usr/bin/env -i PATH=/usr/sbin:/usr/bin SOURCE_DATE_EPOCH="$SOURCE_DATE_EPOCH" \
    /opt/e2fsprogs/sbin/mke2fs -q -t ext4 -F -L rootfs -U "$FS_UUID" \
    -E "root_owner=0:0,hash_seed=$FS_UUID,lazy_itable_init=0,lazy_journal_init=0" \
    -d /rootfs /out/rootfs.ext4 "$SIZE"
  umount -l "$b/rootfs" "$b/out"
}

case ${1:-} in
  __inner) inner; exit ;;
esac

[[ -d $DEF ]] || die "unknown IMAGE=$IMAGE (no $DEF)"
CHAIN=$(python3 -I "$IMAGES_DIR/scripts/make-manifest.py" --chain "$IMAGE")
BASE_DEF=$IMAGES_DIR/rootfs/${CHAIN%% *}
[[ -f $BASE_DEF/provision.sh ]] || die "$IMAGE: base recipe $BASE_DEF has no provision.sh"
[[ $DEB_ARCH == "$HOST_DEB_ARCH" ]] || die "rootfs arch $DEB_ARCH must match host $HOST_DEB_ARCH"
[[ -f $WORK/kernel-$ARCH.env ]] || die "build the kernel first (scripts/build-kernel.sh)"
load_kv "$WORK/kernel-$ARCH.env"
[[ -f $WORK/agent-$ARCH.env ]] || die "build the guest agent first (scripts/build-agent.sh)"
load_kv "$WORK/agent-$ARCH.env"
# The agent must be this commit's (a leftover build from an older or dirty tree must not be baked).
if [[ ${DAVIT_ALLOW_DIRTY:-0} != 1 ]]; then
  head=$(git -C "$IMAGES_DIR" rev-parse HEAD)
  [[ $AGENT_COMMIT == "$head" && $AGENT_DIRTY == false ]] ||
    die "agent was built from ${AGENT_COMMIT:0:12} (dirty=$AGENT_DIRTY), not clean HEAD ${head:0:12}: run build-agent.sh"
fi
recipe=$(python3 -I "$IMAGES_DIR/scripts/make-manifest.py" --recipe-digest "$IMAGE")
fetch_ubuntu_base "$DEB_ARCH"
fetch "$DOCKER_GPG_URL" "$DOCKER_GPG_SHA256" "$DL/docker.asc"
gpg --show-keys --with-colons "$DL/docker.asc" 2>/dev/null | grep -q "^fpr:::::::::$DOCKER_GPG_FINGERPRINT:" \
  || die "docker.asc fingerprint is not $DOCKER_GPG_FINGERPRINT"
"$IMAGES_DIR/scripts/builder.sh" ensure

# Filesystem UUID/hash seed derived from the image name + inputs: stable, but distinct per image.
FS_UUID=$(cat "$IMAGES_DIR/inputs.lock" <(echo "$IMAGE $DEB_ARCH $SIZE") | sha256sum \
  | sed -E 's/^(.{8})(.{4}).(.{3}).(.{3})(.{12}).*/\1-\2-4\3-8\4-\5/')
export IMAGE SIZE DEF BASE_DEF CHAIN RWORK FS_UUID AGENT_VERSION

rm_tree "$RWORK"
mkdir -p "$RWORK"
cp "$OUT/blobs/sha256/$KERNEL_MODULES_BLOB" "$RWORK/modules-builtin.tar"
cp "$OUT/blobs/sha256/$AGENT_BLOB" "$RWORK/davit-guest-agent"
read -ra chain <<<"$CHAIN"
for l in "${chain[@]:1}"; do
  [[ -f $IMAGES_DIR/rootfs/$l/stage.sh ]] || continue
  STAGE=$RWORK/stage/$l
  mkdir -p "$STAGE"
  # shellcheck disable=SC1090 # per-image recipe hook
  source "$IMAGES_DIR/rootfs/$l/stage.sh"
done
log "rootfs: building $IMAGE ($DEB_ARCH, $SIZE) from snapshot $UBUNTU_SNAPSHOT (recipes: $CHAIN)"
in_userns "$0" __inner

{
  echo "ROOTFS_IMAGE=$IMAGE"
  echo "ROOTFS_ARCH=$DEB_ARCH"
  echo "ROOTFS_SIZE=$SIZE"
  echo "ROOTFS_RECIPE=$recipe"
  echo "ROOTFS_FS_UUID=$FS_UUID"
  echo "ROOTFS_BLOB=$(put_blob "$RWORK/rootfs.ext4")"
  echo "ROOTFS_PACKAGES_BLOB=$(put_blob "$RWORK/packages.lock")"
  # The agent this rootfs was built with (the agent env may be rebuilt later).
  echo "ROOTFS_AGENT_BLOB=$AGENT_BLOB"
  echo "ROOTFS_AGENT_VERSION=$AGENT_VERSION"
  echo "ROOTFS_AGENT_COMMIT=$AGENT_COMMIT"
  echo "ROOTFS_AGENT_DIRTY=$AGENT_DIRTY"
  echo "ROOTFS_AGENT_GO=$AGENT_GO"
  echo "ROOTFS_AGENT_PROTOCOL=$AGENT_PROTOCOL"
  echo "ROOTFS_KERNEL_BLOB=$KERNEL_BLOB"
} >"$WORK/rootfs-$IMAGE-$DEB_ARCH.env.part"
mv "$WORK/rootfs-$IMAGE-$DEB_ARCH.env.part" "$WORK/rootfs-$IMAGE-$DEB_ARCH.env"
cp "$RWORK/packages.lock" "$WORK/rootfs-$IMAGE-$DEB_ARCH.packages.lock"
[[ ${KEEP_WORK:-0} == 1 ]] || rm_tree "$RWORK"
log "rootfs: done"
cat "$WORK/rootfs-$IMAGE-$DEB_ARCH.env" >&2
