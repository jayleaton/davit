#!/bin/bash
# Runs inside the builder chroot (see build-kernel.sh). Mounts:
#   /dl (ro) downloads   /kcfg (ro) images/kernel   /scripts (ro)   /build (rw) work + out
# Env: KERNEL_VERSION ARCH KARCH KIMAGE CROSS_PREFIX JOBS SOURCE_DATE_EPOCH
set -euo pipefail

src=/build/linux-$KERNEL_VERSION
out=/build/out
rm -rf "$src" "$out" /build/modroot
mkdir -p "$out"
tar -xJf "/dl/linux-$KERNEL_VERSION.tar.xz" -C /build --no-same-owner
cd "$src"

# Reproducible kbuild: fixed timestamp/user/host and no absolute build paths in the output.
export KBUILD_BUILD_TIMESTAMP KBUILD_BUILD_USER=davit KBUILD_BUILD_HOST=davit KBUILD_BUILD_VERSION=1
KBUILD_BUILD_TIMESTAMP=$(date -u -d "@$SOURCE_DATE_EPOCH" '+%a %b %e %H:%M:%S UTC %Y')
mk=(make -s ARCH="$KARCH" CROSS_COMPILE="$CROSS_PREFIX" -j"$JOBS"
  KCFLAGS="-ffile-prefix-map=$src/=")

frags=(/kcfg/davit.config)
[[ -f /kcfg/davit-$ARCH.config ]] && frags+=("/kcfg/davit-$ARCH.config")
cp "/dl/fc-microvm-kernel-ci-$ARCH-6.18.config" .config
# -m: merge only; olddefconfig resolves dependencies with this kernel's Kconfig.
KCONFIG_CONFIG=.config scripts/kconfig/merge_config.sh -m .config "${frags[@]}" >/dev/null
"${mk[@]}" olddefconfig

# Every fragment line must survive olddefconfig (catches unmet dependencies / renamed symbols).
bad=0
while IFS= read -r line; do
  if [[ $line =~ ^(CONFIG_[A-Za-z0-9_]+)=(.*)$ ]]; then
    want="${BASH_REMATCH[1]}=${BASH_REMATCH[2]}"
  elif [[ $line =~ ^#\ (CONFIG_[A-Za-z0-9_]+)\ is\ not\ set$ ]]; then
    want="# ${BASH_REMATCH[1]} is not set"
  else
    continue
  fi
  sym=${want#\# }; sym=${sym%%[= ]*}
  got=$(grep -E "^(# )?${sym}[= ]" .config || echo "# $sym is not set")
  if [[ $got != "$want" ]]; then
    echo "kernel config: wanted '$want', got '$got'" >&2
    bad=1
  fi
done < <(cat "${frags[@]}")
[[ $bad == 0 ]] || { echo "kernel config fragment did not apply cleanly" >&2; exit 1; }

# Docker's own requirement list: fail on anything in "Generally Necessary".
bash /dl/check-config.sh .config >"$out/docker-check-config.txt" 2>&1 || true
for h in 'Generally Necessary:' 'Optional Features:'; do
  grep -q "^$h" "$out/docker-check-config.txt" || { echo "docker check-config: no '$h' section" >&2; exit 1; }
done
if sed -n '/^Generally Necessary:/,/^Optional Features:/p' "$out/docker-check-config.txt" \
  | sed 's/\x1b\[[0-9;]*m//g' | grep -E 'CONFIG_[A-Z0-9_]+: missing'; then
  echo "docker check-config: required kernel features missing" >&2
  exit 1
fi

# Contract: everything built in, nothing loadable.
if grep -E '=m$' .config; then echo "kernel config: modules (=m) are not allowed" >&2; exit 1; fi

"${mk[@]}" "${KIMAGE##*/}" modules
# No =m options exist; modules_install still ships modules.builtin* so modprobe of a
# built-in succeeds inside the guest.
"${mk[@]}" INSTALL_MOD_PATH=/build/modroot DEPMOD=/sbin/depmod modules_install
kver=$(cat include/config/kernel.release)
rm -f "/build/modroot/lib/modules/$kver/build" "/build/modroot/lib/modules/$kver/source"
tar --sort=name --mtime="@$SOURCE_DATE_EPOCH" --owner=0 --group=0 --numeric-owner \
  --pax-option=exthdr.name=%d/PaxHeaders/%f,delete=atime,delete=ctime \
  -C /build/modroot -cf "$out/modules-builtin.tar" lib/modules

cp "$KIMAGE" "$out/kernel"
cp .config "$out/config"
echo "$kver" >"$out/kernel.release"
"${CROSS_PREFIX}gcc" --version | head -1 >"$out/toolchain.txt"
rm -rf "$src" /build/modroot
