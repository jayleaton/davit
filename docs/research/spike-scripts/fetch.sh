#!/usr/bin/env bash
# Download official binaries + guest kernel + ubuntu-base into ./dl (no installs, no sudo).
set -euo pipefail
S=$(cd "$(dirname "$0")/.." && pwd)
FC_V=v1.17.0 CH_V=v54.0 K=6.1.155 UB=24.04.5
mkdir -p "$S"/dl/{fc,ch,kernel,ubuntu} "$S/bin"
cd "$S/dl/fc"
curl -sSLf -o fc.tgz "https://github.com/firecracker-microvm/firecracker/releases/download/$FC_V/firecracker-$FC_V-x86_64.tgz"
tar xzf fc.tgz
cd "$S/dl/ch"
curl -sSLf -o cloud-hypervisor-static "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/$CH_V/cloud-hypervisor-static"
chmod +x cloud-hypervisor-static
cd "$S/dl/kernel"   # Firecracker CI guest kernel (PVH-capable, also boots on CH)
for f in "vmlinux-$K" "vmlinux-$K.config"; do
  curl -sSLf -O "http://spec.ccfc.min.s3.amazonaws.com/firecracker-ci/v1.15/x86_64/$f"
done
cd "$S/dl/ubuntu"
curl -sSLf -O "https://cdimage.ubuntu.com/ubuntu-base/releases/24.04/release/ubuntu-base-$UB-base-amd64.tar.gz"
curl -sSLf https://cdimage.ubuntu.com/ubuntu-base/releases/24.04/release/SHA256SUMS \
  | grep "ubuntu-base-$UB-base-amd64.tar.gz" | sha256sum -c
ln -sf "../dl/fc/release-$FC_V-x86_64/firecracker-$FC_V-x86_64" "$S/bin/firecracker"
ln -sf ../dl/ch/cloud-hypervisor-static "$S/bin/cloud-hypervisor"
"$S/bin/firecracker" --version | head -1; "$S/bin/cloud-hypervisor" --version
