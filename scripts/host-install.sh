#!/usr/bin/env bash
# host-install.sh: install davit on a VM host and leave its daemon running as a service that
# survives reboots (`make host-install HOST=user@host`). See docs/vm-host.md.
#
#   scripts/host-install.sh [options] HOST       install or upgrade (idempotent)
#   scripts/host-install.sh --uninstall [--purge] HOST
#
# Install steps; the first failure stops it:
#   0. migrate    a host set up before the slipway -> davit rename (~/.svm/slipway,
#                 ~/.config/slipway*, slipway.service) moves to the davit paths first, VMs and
#                 boxes included (`davit init --migrate-from-slipway`; docs/migrating-from-slipway.md)
#   1. preflight  scripts/host-preflight.sh (read-only; a FAIL stops here)
#   2. binary     `make build`'s flags, CGO-free, for the host's arch, built here (under $DAVIT_HEAVY when set)
#   3. images     scripts/vm-tier.sh --images: the image recipes at HEAD, built on the host
#                 (skipped there while images/ is unchanged)
#   4. install    on the host: the binary into ~/.local/bin (atomic rename), the images
#                 reflinked into DATA/images, `davit init --data-dir DATA --image ... --service`
#                 (a systemd --user unit), lingering so it starts at boot, restart on upgrade
#   5. verify     the service is active and `davit images` lists the images
#
# Options:
#   --data DIR         host data dir (default ~/.svm/davit: the reflink disk vm-tier uses)
#   --config FILE      daemon config (default ~/.config/davit/config.json)
#   --bin-dir DIR      where the binary goes (default ~/.local/bin)
#                      Relative paths are under the host user's home. An existing config or
#                      davit.service that belongs to another setup is never changed: install
#                      beside it with --config and --bin-dir (the socket always lives in DATA)
#   --image NAME       image to build and install, repeatable; the first is the default
#                      (default: ubuntu-24.04-dev, ubuntu-24.04-base)
#   --skip-images      install the images the host already built
#   --rebuild-images   build the images even when unchanged
#   --min-disk GIB     free space the preflight requires (default 20 with an image build, else 10)
#   --stop-vms         migrating: stop the old daemon's live VMs cleanly (disks kept) instead of refusing
#   --uninstall        stop and remove the service and the binary; keeps the data dir
#   --purge            with --uninstall, also delete the data dir and the daemon config
#
# Exit status: 0 done, 1 a step failed, 2 usage or host problem, 255 SSH.
set -euo pipefail

usage() { sed -n '2,37p' "$0" >&2; exit 2; }
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DATA="" CONFIG="" BINDIR="" IMAGES=() MIN_DISK="" SKIP_IMAGES=0 REBUILD=0 UNINSTALL=0 PURGE=0 STOP_VMS=0
while [ $# -gt 0 ]; do
	case "$1" in
	--data) [ $# -ge 2 ] || usage; DATA=$2; shift 2 ;;
	--config) [ $# -ge 2 ] || usage; CONFIG=$2; shift 2 ;;
	--bin-dir) [ $# -ge 2 ] || usage; BINDIR=$2; shift 2 ;;
	--image) [ $# -ge 2 ] || usage; IMAGES+=("$2"); shift 2 ;;
	--skip-images) SKIP_IMAGES=1; shift ;;
	--min-disk) [ $# -ge 2 ] && [[ $2 =~ ^[0-9]+$ ]] || usage; MIN_DISK=$2; shift 2 ;;
	--rebuild-images) REBUILD=1; shift ;;
	--stop-vms) STOP_VMS=1; shift ;;
	--uninstall) UNINSTALL=1; shift ;;
	--purge) PURGE=1; shift ;;
	-h | --help) usage ;;
	-*) echo "host-install: unknown option $1" >&2; usage ;;
	*) break ;;
	esac
done
[ $# -eq 1 ] || usage
HOST=$1
[ ${#IMAGES[@]} -gt 0 ] || IMAGES=(ubuntu-24.04-dev ubuntu-24.04-base)
for i in "${IMAGES[@]}"; do
	case "$i" in *[!a-z0-9.-]*) echo "host-install: bad image name $i" >&2; exit 2 ;; esac
done
for p in "$DATA" "$CONFIG" "$BINDIR"; do
	case "$p" in *[!A-Za-z0-9._/-]*) echo "host-install: --data/--config/--bin-dir must be plain paths (relative: under the host user's home)" >&2; exit 2 ;; esac
done
[ "$PURGE" = 0 ] || [ "$UNINSTALL" = 1 ] || usage
cd "$ROOT"

say() { printf '\n==> host-install: %s\n' "$*" >&2; }
SSH=(ssh -o BatchMode=yes -o ConnectTimeout=15)
# remote FUNCTION ARGS...: run one function of this file on the host (bash, stdin script).
remote() {
	local fn=$1
	shift
	{
		declare -f say hpath cfg_field guard remote_install remote_uninstall remote_verify
		printf '%s' "$fn"
		printf ' %q' "$@"
		echo
	} | "${SSH[@]}" "$HOST" bash -s
}

# --- host half: runs on the host as the SSH user (these functions travel over SSH) ---

# hpath PATH DEFAULT: PATH (or DEFAULT) as an absolute path; a relative one is under $HOME.
hpath() {
	local p=${1:-$2}
	case "$p" in /*) echo "$p" ;; *) echo "$HOME/$p" ;; esac
}

# cfg_field CONFIG FIELD: a top-level string field of the daemon config ("" if absent).
cfg_field() { python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get(sys.argv[2], ""))' "$1" "$2" 2>/dev/null || true; }

# guard DATA CONFIG UNIT: refuse to touch a daemon setup this script did not make: a config whose
# data dir is another one, or a davit.service running another config. Exit 2 explains.
guard() {
	local data=$1 config=$2 unit=$3 other
	if [ -f "$config" ] && other=$(cfg_field "$config" dataDir) && [ -n "$other" ] && [ "$other" != "$data" ]; then
		echo "host-install: $config belongs to another setup (data dir $other, not $data); nothing changed." >&2
		echo "  Install alongside it: --config .config/davit-host/config.json --bin-dir .svm/davit/bin" >&2
		echo "  (or remove that setup first if it is yours)." >&2
		exit 2
	fi
	if [ -f "$unit" ] && ! grep -qF -- "--config \"$config\"" "$unit"; then
		echo "host-install: $unit runs another config; nothing changed. Use the same --config, or remove that unit." >&2
		exit 2
	fi
}

# remote_install STOP_VMS DATA CONFIG BINDIR BINARY_TMP IMAGE...: migrate a pre-rename setup, put
# the binary in place, reflink the images into DATA/images, init (idempotent), service +
# lingering, restart when anything changed.
remote_install() {
	set -euo pipefail
	local stop_vms=$1 data config bindir tmp=$5 fs=$HOME/.svm/fs bin changed=0 i h unit=$HOME/.config/systemd/user/davit.service
	data=$(hpath "$2" .svm/davit) config=$(hpath "$3" .config/davit/config.json) bindir=$(hpath "$4" .local/bin)
	bin=$bindir/davit
	shift 5
	tmp=$(hpath "$tmp" "")
	# shellcheck disable=SC2064 # expand now: the upload to drop on any failure
	trap "rm -f '$tmp'" EXIT
	chmod 0755 "$tmp"
	# A host set up as slipway: move it (data, config, unit) to these paths first.
	local migrate=(init --migrate-from-slipway --config "$config" --data-dir "$data")
	[ "$stop_vms" = 1 ] && migrate+=(--stop-vms)
	"$tmp" "${migrate[@]}" || exit 2
	# The old CLI, wherever it was installed; davit replaces it.
	for i in "$bindir/slipway" "$HOME/.local/bin/slipway"; do
		if [ -f "$i" ] && "$i" version 2>/dev/null | grep -q '^slipway '; then rm -f "$i"; fi
	done
	guard "$data" "$config" "$unit"
	mkdir -p "$bindir" "$data/images/manifests" "$data/images/blobs/sha256"
	if ! cmp -s "$tmp" "$bin"; then
		mv -f "$tmp" "$bin" # same directory: an atomic rename
		changed=1
	else
		rm -f "$tmp"
	fi
	say "binary $bin: $("$bin" version 2>/dev/null | head -n1)$([ "$changed" = 1 ] && echo ' (updated)')"
	local found
	found=$(bash -lc 'command -v davit' 2>/dev/null || true)
	[ -z "$found" ] || [ "$found" = "$bin" ] || echo "host-install: note: a login shell's davit is $found, not $bin" >&2
	# Images: blobs are content-addressed, so an existing one is already right; the manifest
	# goes last, so the daemon never sees one whose blobs are missing.
	local args=() old_rootfs=()
	rootfs_digest() { python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["rootfs"]["digest"].split(":")[1])' "$1"; }
	for i in "$@"; do
		[ -f "$fs/manifests/$i.json" ] || { echo "host-install: no built image $i in $fs/manifests" >&2; exit 2; }
		while read -r h; do
			[ -f "$data/images/blobs/sha256/$h" ] && continue
			[ -f "$fs/blobs/sha256/$h" ] || continue # digests of things that are not blobs
			cp --reflink=auto --sparse=always "$fs/blobs/sha256/$h" "$data/images/blobs/sha256/.$h.part"
			mv "$data/images/blobs/sha256/.$h.part" "$data/images/blobs/sha256/$h"
		done < <(grep -oE 'sha256:[0-9a-f]{64}' "$fs/manifests/$i.json" | sort -u | sed 's/^sha256://')
		if ! cmp -s "$fs/manifests/$i.json" "$data/images/manifests/$i.json"; then
			# The rootfs the replaced version used: only its creates and its goldens read it
			# (a resume boots the VM's own committed disk; kernels are kept for those).
			[ ! -f "$data/images/manifests/$i.json" ] || old_rootfs+=("$(rootfs_digest "$data/images/manifests/$i.json")")
			cp "$fs/manifests/$i.json" "$data/images/manifests/.$i.json.part"
			mv "$data/images/manifests/.$i.json.part" "$data/images/manifests/$i.json"
			changed=1
		fi
		args+=(--image "$data/images/manifests/$i.json")
	done
	# Drop replaced rootfs blobs no installed manifest uses any more; the daemon's golden sweep then
	# drops the goldens captured from them.
	for h in "${old_rootfs[@]}"; do
		[ -n "$h" ] || continue
		if ! grep -qF "sha256:$h" "$data"/images/manifests/*.json; then
			rm -f "$data/images/blobs/sha256/$h"
			say "removed the replaced rootfs blob ${h:0:12}"
		fi
	done
	# First init without XDG_RUNTIME_DIR: the socket goes in DATA, so it never collides with
	# another daemon's /run/user/UID/davit socket. A later init keeps the existing config.
	say "davit init --config $config --data-dir $data ${args[*]}"
	env -u XDG_RUNTIME_DIR "$bin" init --config "$config" --data-dir "$data" "${args[@]}" --image-blobs "$data/images/blobs"
	export XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}
	"$bin" init --config "$config" --data-dir "$data" --service >/dev/null
	# Lingering starts the user's systemd (and so the unit) at boot, without a login.
	if [ "$(loginctl show-user "$(id -un)" -p Linger --value 2>/dev/null)" != yes ]; then
		loginctl enable-linger "$(id -un)" 2>/dev/null || sudo -n loginctl enable-linger "$(id -un)" ||
			{ echo "host-install: enable lingering once as root: sudo loginctl enable-linger $(id -un)" >&2; exit 2; }
		say "lingering enabled for $(id -un)"
	fi
	systemctl --user enable davit.service >/dev/null 2>&1 || true
	systemctl --user reset-failed davit.service 2>/dev/null || true
	if [ "$changed" = 1 ]; then
		say "restarting davit.service (binary or images changed; VMs keep running)"
		systemctl --user restart davit.service
	else
		systemctl --user start davit.service
	fi
}

# remote_verify CONFIG BINDIR: the service runs and answers on its socket.
remote_verify() {
	set -euo pipefail
	local config bin sock i
	config=$(hpath "$1" .config/davit/config.json) bin=$(hpath "$2" .local/bin)/davit
	sock=$(cfg_field "$config" socket)
	export XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}
	for i in $(seq 60); do
		"$bin" --api-url "unix://$sock" --json capabilities >/dev/null 2>&1 && break
		sleep 1
	done
	echo "service=$(systemctl --user is-active davit.service) enabled=$(systemctl --user is-enabled davit.service) linger=$(loginctl show-user "$(id -un)" -p Linger --value)"
	echo "socket=$sock (clients: --api-url unix://$sock, or DAVIT_API_URL)"
	"$bin" --api-url "unix://$sock" capabilities
	[ "$(systemctl --user is-active davit.service)" = active ]
}

# remote_uninstall PURGE DATA CONFIG BINDIR: stop the VMs, then remove the service and binary
# (and with PURGE the data dir and the config).
remote_uninstall() {
	set -euo pipefail
	local purge=$1 data config bin unit=$HOME/.config/systemd/user/davit.service vm sock
	data=$(hpath "$2" .svm/davit) config=$(hpath "$3" .config/davit/config.json) bin=$(hpath "$4" .local/bin)/davit
	guard "$data" "$config" "$unit"
	export XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}
	sock=$(cfg_field "$config" socket)
	if [ -x "$bin" ] && [ -n "$sock" ] && "$bin" --api-url "unix://$sock" --json list >/dev/null 2>&1; then
		while read -r vm; do
			"$bin" --api-url "unix://$sock" stop "$vm" >/dev/null 2>&1 || true
		done < <("$bin" --api-url "unix://$sock" --json list 2>/dev/null | grep -oE '"name": *"[^"]+"' | sed 's/.*"\([^"]*\)"$/\1/')
	fi
	systemctl --user disable --now davit.service 2>/dev/null || true
	rm -f "$unit"
	systemctl --user daemon-reload 2>/dev/null || true
	rm -f "$bin"
	say "service and binary removed"
	if [ "$purge" = 1 ]; then
		rm -rf "$data" "$config"
		say "purged $data and $config"
	else
		echo "kept $data and $config (--purge deletes them)" >&2
	fi
}

# --- local half ---

if [ "$UNINSTALL" = 1 ]; then
	remote remote_uninstall "$PURGE" "$DATA" "$CONFIG" "$BINDIR" || exit $?
	exit 0
fi

say "preflight $HOST"
rc=0
# An install copies no image when it skips the build; building one needs the tier's room.
min_disk=20
[ "$SKIP_IMAGES" = 1 ] && min_disk=10
[ -z "$MIN_DISK" ] || min_disk=$MIN_DISK
"$ROOT/scripts/host-preflight.sh" --min-disk "$min_disk" "$HOST" || rc=$?
case "$rc" in 0) ;; 1) exit 2 ;; *) exit "$rc" ;; esac

arch=$("$ROOT/scripts/host-preflight.sh" --probe-only "$HOST" | sed -n 's/^arch=//p')
case "$arch" in x86_64) goarch=amd64 ;; aarch64) goarch=arm64 ;; *) echo "host-install: unsupported arch $arch" >&2; exit 2 ;; esac
if [ -n "$(git status --porcelain)" ]; then
	echo "host-install: WARNING: the tree has local changes; the binary is built from them, the images from HEAD" >&2
fi
out=$ROOT/.build/host-install
mkdir -p "$out"
say "binary linux/$goarch at $(git describe --tags --always --dirty)"
heavy=()
[ -n "${DAVIT_HEAVY:-}" ] && read -ra heavy <<<"$DAVIT_HEAVY"
version=$(git describe --tags --always --dirty)
"${heavy[@]}" env -u GOFLAGS GOTMPDIR="$ROOT/.build/gotmp" CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" GOAMD64=v1 GOARM64=v8.0 \
	mise exec -- go build -trimpath -ldflags "-s -w -X davit/internal/buildinfo.Version=$version -X davit/internal/buildinfo.Commit=$(git rev-parse HEAD)" \
	-o "$out/davit-linux-$goarch" ./cmd/davit

if [ "$SKIP_IMAGES" = 1 ]; then
	say "images: --skip-images"
else
	vt=("$ROOT/scripts/vm-tier.sh" --images --out "$out/images" --min-disk "$min_disk")
	[ "$REBUILD" = 1 ] && vt+=(--rebuild-images)
	for i in "${IMAGES[@]}"; do vt+=(--image "$i"); done
	say "images ${IMAGES[*]} on $HOST"
	"${vt[@]}" "$HOST" || exit $?
fi

say "install on $HOST"
# Staged inside the bin dir, so moving it into place is a same-filesystem rename.
tmp="${BINDIR:-.local/bin}/.davit-install.$$"
"${SSH[@]}" "$HOST" "mkdir -p '${BINDIR:-.local/bin}'" || exit 255
scp -q -o BatchMode=yes "$out/davit-linux-$goarch" "$HOST:$tmp" || exit 255
# The images come from vm-tier.sh's layout on the host (~/.svm/fs).
remote remote_install "$STOP_VMS" "$DATA" "$CONFIG" "$BINDIR" "$tmp" "${IMAGES[@]}" || exit 1
say "verify"
remote remote_verify "$CONFIG" "$BINDIR" || exit 1
say "done: davit runs on $HOST as a systemd --user service (default image ${IMAGES[0]})"
