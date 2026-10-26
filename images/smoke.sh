#!/usr/bin/env bash
# Boot the built image under Firecracker (rootless) and run the acceptance checks.
#   images/smoke.sh [--restores N] [--mem MiB] [--keep]
# Needs: /dev/kvm and /dev/net/tun accessible, unprivileged user namespaces, socat, ssh, debugfs,
# and the repo's Go toolchain (mise) to build images/smokeprobe.
#   IMAGE=ubuntu-24.04-harness images/smoke.sh   also checks the harness capabilities
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=scripts/lib.sh
source "$here/scripts/lib.sh"
load_lock
set_arch
IMAGE=${IMAGE:-ubuntu-24.04-base}
mdir=${DAVIT_MANIFESTS:-$here/manifests}
manifest=$mdir/$IMAGE.json
[[ $ARCH == x86_64 ]] || manifest=$mdir/$IMAGE-$ARCH.json
[[ -f $manifest ]] || die "no manifest $manifest: run images/build.sh first"

fc_sum=FIRECRACKER_SHA256_$ARCH
fc_tgz=$DL/firecracker-$FIRECRACKER_VERSION-$ARCH.tgz
fetch "https://github.com/firecracker-microvm/firecracker/releases/download/$FIRECRACKER_VERSION/firecracker-$FIRECRACKER_VERSION-$ARCH.tgz" \
  "${!fc_sum}" "$fc_tgz"
fc=$WORK/firecracker-$FIRECRACKER_VERSION/release-$FIRECRACKER_VERSION-$ARCH/firecracker-$FIRECRACKER_VERSION-$ARCH
if [[ ! -x $fc ]]; then
  mkdir -p "$WORK/firecracker-$FIRECRACKER_VERSION"
  tar -xzf "$fc_tgz" -C "$WORK/firecracker-$FIRECRACKER_VERSION"
fi
fetch "$MOBY_CHECK_CONFIG_URL" "$MOBY_CHECK_CONFIG_SHA256" "$DL/check-config.sh"

# Heavy steps run under $DAVIT_HEAVY when set (optional, word-split command prefix).
heavy=()
[[ -n ${DAVIT_HEAVY:-} ]] && read -ra heavy <<<"$DAVIT_HEAVY"

# Host-side barrier driver: the product's guestclient code (images/smokeprobe), host arch.
repo=$(git -C "$here" rev-parse --show-toplevel)
probe=$WORK/smokeprobe
mkdir -p "$WORK/gotmp"
(cd "$repo" && GOTMPDIR=$WORK/gotmp CGO_ENABLED=0 "${heavy[@]}" mise exec -- go build -o "$probe" ./images/smokeprobe)

exec "${heavy[@]}" python3 -I "$here/scripts/smoke.py" --manifest "$manifest" --firecracker "$fc" \
  --check-config "$DL/check-config.sh" --probe "$probe" "$@"
