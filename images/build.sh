#!/usr/bin/env bash
# One command, clean checkout -> kernel + guest agent + rootfs blobs + manifests. Rootless; no
# sudo, no Docker.
#
#   images/build.sh                 kernel, agent, then every image recipe (or $IMAGES)
#   images/build.sh kernel          kernel only (ARCH=aarch64 cross-builds the arm64 kernel)
#   images/build.sh agent           guest agent only (the repo's davit binary, at HEAD)
#   images/build.sh rootfs          rootfs for each of $IMAGES (needs kernel + agent)
#   images/build.sh manifest        manifests for each of $IMAGES
#   images/build.sh clean           drop scratch ($DAVIT_IMAGES_WORK) except downloads; keeps out/
#
# Env: IMAGES (all recipes in rootfs/: ubuntu-24.04-base ubuntu-24.04-ci ubuntu-24.04-dev
#      ubuntu-24.04-harness) ARCH (host)
#      SIZE (4G) JOBS (4) KEEP_WORK=1 DAVIT_ALLOW_DIRTY=1 (development agent from a dirty tree)
#      DAVIT_IMAGES_WORK (default ~/.cache/davit/images) / DAVIT_IMAGES_OUT (images/out)
#      DAVIT_MANIFESTS (images/manifests; point elsewhere for development builds).
# Heavy steps run under $DAVIT_HEAVY when set: an optional, word-split command prefix (job lock,
# nice/ionice wrapper). Unset means run directly.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=scripts/lib.sh
source "$here/scripts/lib.sh"
load_lock
set_arch
read -ra images <<<"${IMAGES:-$(cd "$here/rootfs" && echo *)}"

heavy=()
[[ -n ${DAVIT_HEAVY:-} ]] && read -ra heavy <<<"$DAVIT_HEAVY"

rootfs() { local i; for i in "${images[@]}"; do IMAGE=$i "${heavy[@]}" "$here/scripts/build-rootfs.sh"; done; }
manifests() { local i; for i in "${images[@]}"; do python3 -I "$here/scripts/make-manifest.py" "$i" "$ARCH"; done; }

case ${1:-all} in
  all)
    "${heavy[@]}" "$here/scripts/build-kernel.sh"
    "${heavy[@]}" "$here/scripts/build-agent.sh"
    rootfs
    manifests
    ;;
  kernel) "${heavy[@]}" "$here/scripts/build-kernel.sh" ;;
  agent) "${heavy[@]}" "$here/scripts/build-agent.sh" ;;
  rootfs) rootfs ;;
  manifest) manifests ;;
  clean)
    for d in "$WORK"/*; do [[ $d == "$DL" ]] || rm_tree "$d"; done
    ;;
  *) die "usage: build.sh [all|kernel|agent|rootfs|manifest|clean]" ;;
esac
