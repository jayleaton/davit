#!/usr/bin/env bash
# Build the davit guest kernel reproducibly, rootless, inside the pinned builder.
#   ARCH=x86_64|aarch64 JOBS=4 images/scripts/build-kernel.sh
# Outputs (content-addressed in $OUT/blobs/sha256) and $WORK/kernel-<arch>.env with their digests.
# aarch64 cross-compiles on an x86_64 host (gcc-aarch64-linux-gnu in the builder); Firecracker
# boots arch/arm64/boot/Image there.
set -euo pipefail
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_lock
set_arch
JOBS=${JOBS:-4}
# Digest of the files that shape this build, taken before it starts (make-manifest.py re-checks it).
recipe=$(python3 -I "$IMAGES_DIR/scripts/make-manifest.py" --recipe-digest kernel)

fetch "$KERNEL_URL" "$KERNEL_SHA256" "$DL/linux-$KERNEL_VERSION.tar.xz"
fc_sum=FC_CONFIG_SHA256_$ARCH
fetch "$FC_CONFIG_URL_PREFIX/microvm-kernel-ci-$ARCH-6.18.config" "${!fc_sum}" "$DL/fc-microvm-kernel-ci-$ARCH-6.18.config"
fetch "$MOBY_CHECK_CONFIG_URL" "$MOBY_CHECK_CONFIG_SHA256" "$DL/check-config.sh"

"$IMAGES_DIR/scripts/builder.sh" ensure

kwork=$WORK/kernel-$ARCH
rm_tree "$kwork"
mkdir -p "$kwork"
log "kernel: building linux-$KERNEL_VERSION for $ARCH (-j$JOBS)"
"$IMAGES_DIR/scripts/builder.sh" run \
  -b "$DL:/dl:ro" -b "$IMAGES_DIR/kernel:/kcfg:ro" -b "$IMAGES_DIR/scripts:/scripts:ro" -b "$kwork:/build" -- \
  /usr/bin/env KERNEL_VERSION="$KERNEL_VERSION" ARCH="$ARCH" KARCH="$KARCH" KIMAGE="$KIMAGE" \
  CROSS_PREFIX="$CROSS_PREFIX" JOBS="$JOBS" /bin/bash /scripts/kernel-inner.sh

o=$kwork/out
{
  echo "KERNEL_ARCH=$ARCH"
  echo "KERNEL_RECIPE=$recipe"
  echo "KERNEL_RELEASE=$(cat "$o/kernel.release")"
  echo "KERNEL_TOOLCHAIN=$(cat "$o/toolchain.txt")"
  echo "KERNEL_IMAGE_FORMAT=${KIMAGE##*/}"
  echo "KERNEL_BLOB=$(put_blob "$o/kernel")"
  echo "KERNEL_CONFIG_BLOB=$(put_blob "$o/config")"
  echo "KERNEL_MODULES_BLOB=$(put_blob "$o/modules-builtin.tar")"
} >"$WORK/kernel-$ARCH.env.part"
mv "$WORK/kernel-$ARCH.env.part" "$WORK/kernel-$ARCH.env"
cp "$o/docker-check-config.txt" "$WORK/kernel-$ARCH.docker-check-config.txt"
[[ ${KEEP_WORK:-0} == 1 ]] || rm_tree "$kwork"
log "kernel: done"
cat "$WORK/kernel-$ARCH.env" >&2
