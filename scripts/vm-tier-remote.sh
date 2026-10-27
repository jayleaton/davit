#!/usr/bin/env bash
# vm-tier-remote.sh: the VM-host half of scripts/vm-tier.sh. Not for the dev
# box; vm-tier.sh ships it with the source and runs it over SSH:
#
#   vm-tier-remote.sh check                      host requirements (exit 2 if unmet)
#   vm-tier-remote.sh prepare RUN                toolchain + fixture, once (blobs already unpacked)
#   vm-tier-remote.sh images RUN REBUILD [IMAGE...]  build images here (default ubuntu-24.04-base)
#   vm-tier-remote.sh run RUN REGEX BENCH TIMEOUT FRESH SMALL_FS COLD_FS PKGS...
#                                                sync, then the real-VM tier, serially
#                                                (SMALL_FS: DAVIT_TEST_SMALL_FS, COLD_FS:
#                                                DAVIT_TEST_COLD_FS; each or -)
#   vm-tier-remote.sh cleanup RUN                kill the run's processes, remove its files
#
# Host layout (SVM_DIR, default ~/.svm: short, unix socket paths are limited
# to 108 bytes; SVM_FS, default $SVM_DIR/fs, must be a reflink filesystem):
#   $SVM_DIR/bin       mise (if the host has none)
#   $SVM_DIR/src       the shipped checkout (HEAD + local changes)
#   $SVM_DIR/runs/RUN  payload, out/ (logs for the caller), t/ (TMPDIR)
#   $SVM_FS/blobs      image blobs (sha256/<hex>), unpacked by --prepare or built by images
#   $SVM_FS/manifests  the base manifest images built here; when present, prepare and run use
#                      it instead of the committed one (its blobs are the ones built here)
#   $SVM_FS/images-work images/build.sh scratch (DAVIT_IMAGES_WORK)
#   $SVM_FS/scratch    DAVIT_TEST_SCRATCH: pinned Firecracker and the derived
#                      image persist; everything else is removed after a run
# One run or prepare at a time per host ($SVM_DIR/lock).
set -euo pipefail

SVM_DIR=${SVM_DIR:-$HOME/.svm}
SVM_FS=${SVM_FS:-$SVM_DIR/fs}
SCRATCH=$SVM_FS/scratch
BLOBS=$SVM_FS/blobs
SRC=$SVM_DIR/src
MANIFEST=images/manifests/ubuntu-24.04-base.json
BUILT=$SVM_FS/manifests/ubuntu-24.04-base.json
HERE="$(cd "$(dirname "$0")" && pwd)"

# manifest: the base manifest the tier uses (absolute).
manifest() { if [ -f "$BUILT" ]; then echo "$BUILT"; else echo "$SRC/$MANIFEST"; fi; }

say() { printf '\n==> %s\n' "$*" >&2; }
die() { echo "vm-tier-remote: $*" >&2; exit 2; }

mise_bin() {
	if command -v mise >/dev/null; then command -v mise; else echo "$SVM_DIR/bin/mise"; fi
}

lock() {
	mkdir -p "$SVM_DIR"
	exec 9>"$SVM_DIR/lock"
	if ! flock -n 9; then
		say "another vm-tier run or prepare holds $SVM_DIR/lock; waiting"
		flock 9
	fi
}

# check: every host requirement, one line each; exit 2 when one is unmet.
check() {
	local bad=0 tmp
	ok() { printf '  ok    %-12s %s\n' "$1" "$2"; }
	no() { printf '  FAIL  %-12s %s\n' "$1" "$2"; bad=2; }
	say "host $(hostname): $(uname -sr), $(nproc) CPUs, $(awk '/MemTotal/ {printf "%.1f GiB", $2/1048576}' /proc/meminfo) RAM"
	if [ "$(uname -m)" = x86_64 ]; then ok arch x86_64; else no arch "$(uname -m): the image and fixture are x86_64"; fi
	if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then ok kvm "/dev/kvm read-write"; else no kvm "/dev/kvm missing or not read-write for $(id -un) (add the user to group kvm)"; fi
	if unshare --user --map-root-user --net true 2>/dev/null; then
		ok userns "unprivileged user + network namespaces"
	else
		no userns "unshare --user --net fails (Ubuntu: sysctl kernel.apparmor_restrict_unprivileged_userns=0, or davit init --apparmor)"
	fi
	local t missing=()
	for t in git tar flock nsenter unshare ip debugfs sha256sum curl; do
		command -v "$t" >/dev/null || missing+=("$t")
	done
	if [ ${#missing[@]} -eq 0 ]; then ok tools "git tar flock nsenter unshare ip debugfs sha256sum curl"; else no tools "missing: ${missing[*]} (debugfs: e2fsprogs; ip: iproute2)"; fi
	if command -v nft >/dev/null; then ok nft "present (netns ruleset filters too)"; else ok nft "absent: the userspace egress stack filters alone"; fi
	mkdir -p "$SCRATCH" "$BLOBS"
	tmp=$(mktemp -d "$SCRATCH/.reflink.XXXX")
	head -c 65536 /dev/zero >"$tmp/a"
	if cp --reflink=always "$tmp/a" "$tmp/b" 2>/dev/null; then
		ok reflink "$SVM_FS ($(stat -f -c %T "$SVM_FS"))"
	else
		no reflink "$SVM_FS ($(stat -f -c %T "$SVM_FS")) cannot reflink; put SVM_FS on btrfs or XFS (vm-tier.sh --fs)"
	fi
	rm -rf "$tmp"
	local free
	free=$(df -Pk "$SVM_FS" | awk 'NR==2 {print int($4/1048576)}')
	if [ "$free" -ge "${SVM_MIN_DISK:-20}" ]; then ok disk "${free} GiB free on $SVM_FS"; else no disk "${free} GiB free on $SVM_FS, want ${SVM_MIN_DISK:-20} (vm-tier.sh --min-disk)"; fi
	if [ -x "$(mise_bin)" ]; then ok mise "$("$(mise_bin)" --version 2>/dev/null | head -n1)"; else no mise "not installed (vm-tier.sh --prepare installs it)"; fi
	if [ -d "$BLOBS/sha256" ] && [ -n "$(ls -A "$BLOBS/sha256")" ]; then ok blobs "$(du -sh --apparent-size "$BLOBS" | cut -f1) in $BLOBS"; else no blobs "none in $BLOBS (vm-tier.sh --prepare uploads them)"; fi
	return "$bad"
}

# sync PAYLOAD_DIR: make $SRC exactly the shipped tree (HEAD + local changes).
sync_src() {
	local pay=$1
	say "sync $(cat "$pay/HEAD") into $SRC"
	[ -d "$SRC/.git" ] || git init -q "$SRC"
	cd "$SRC"
	git fetch -q "$pay/src.bundle" HEAD
	git -c advice.detachedHead=false checkout -q -f --detach FETCH_HEAD
	git clean -q -ffdx -e /.build/
	if [ -s "$pay/changes.patch" ]; then
		git apply --binary "$pay/changes.patch"
	fi
	if [ -s "$pay/untracked.tar" ]; then
		tar -xf "$pay/untracked.tar"
	fi
}

# goenv: PATH with the pinned Go (mise.toml) and the test environment.
goenv() {
	local mise
	mise=$(mise_bin)
	[ -x "$mise" ] || die "no mise: run vm-tier.sh --prepare first"
	"$mise" trust -q "$SRC/mise.toml" >/dev/null
	export DAVIT_TEST_FIRECRACKER=1
	DAVIT_TEST_IMAGE_MANIFEST=$(manifest)
	export DAVIT_TEST_IMAGE_MANIFEST
	export DAVIT_TEST_IMAGE_BLOBS=$BLOBS
	export DAVIT_TEST_SCRATCH=$SCRATCH
	MISE=$mise
}

ensure_mise() {
	if [ ! -x "$(mise_bin)" ]; then
		say "installing mise into $SVM_DIR/bin"
		mkdir -p "$SVM_DIR/bin"
		curl -fsSL https://mise.run | MISE_INSTALL_PATH="$SVM_DIR/bin/mise" sh >/dev/null
	fi
}

prepare() {
	local run=$1 pay=$HERE
	lock
	ensure_mise
	sync_src "$pay"
	goenv
	say "Go $(sed -n 's/^go = "\(.*\)"/\1/p' mise.toml) via mise"
	"$MISE" install -q
	say "verifying image blobs against their digests"
	local d f bad=0
	while read -r d; do
		f=$BLOBS/sha256/${d#sha256:}
		[ -f "$f" ] || continue
		if [ "$(sha256sum "$f" | cut -d' ' -f1)" != "${d#sha256:}" ]; then
			echo "blob $f did not match its digest; removed" >&2
			rm -f "$f" # so the next --prepare sends it again
			bad=1
		fi
	done < <(grep -oE 'sha256:[0-9a-f]{64}' "$(manifest)" | sort -u)
	[ "$bad" = 0 ] || die "corrupt blobs removed; rerun vm-tier.sh --prepare"
	check || die "host requirements unmet (above)"
	say "warming the fixture: pinned Firecracker, agent, derived image (no VM boots)"
	mkdir -p "$SVM_DIR/runs/$run/t"
	TMPDIR=$SVM_DIR/runs/$run/t "$MISE" exec -- go test -count=1 -v -run '^TestFixture$' ./internal/runtime/firecracker/fctest/
	say "prepared: $(hostname) is a davit VM host"
}

# refers PID PATH...: PID's cwd or command line refers to a PATH.
refers() {
	local p=$1 cwd cmd path
	shift
	cwd=$(readlink "/proc/$p/cwd" 2>/dev/null) || return 1
	cmd=$(tr '\0' ' ' 2>/dev/null <"/proc/$p/cmdline") || return 1
	for path in "$@"; do
		case "$cwd $cmd" in *"$path"*) return 0 ;; esac
	done
	return 1
}

# procs_under PATH...: PIDs that refer to a PATH, except this script, the
# subshell running this, and the shell that started the script.
procs_under() {
	local p
	for p in /proc/[0-9]*; do
		p=${p#/proc/}
		case "$p" in "$$" | "$BASHPID" | "$PPID") continue ;; esac
		if refers "$p" "$@"; then echo "$p"; fi
	done
}

# cleanup RUN: stop everything the run started (test binaries, daemons,
# VMMs, namespace holders, egress helpers), then remove its files and the
# per-run scratch. The fixture cache (Firecracker binary, derived image) and
# the blobs stay.
cleanup() {
	local run=$1 dir=$SVM_DIR/runs/$1 pids p round
	[ -d "$dir" ] || return 0
	# The test job's process group, while its leader is still this run's
	# (cwd in the checkout): never a group that reused the number.
	if [ -f "$dir/pgid" ] && p=$(cat "$dir/pgid") && [ "$(readlink "/proc/$p/cwd" 2>/dev/null)" = "$SRC" ]; then
		kill -KILL -- "-$p" 2>/dev/null || true
	fi
	local paths=("$dir/" "$SCRATCH/e2e-" "$SCRATCH/disk-" "$SCRATCH/agent/") small="" cold=""
	# The disk-low and cold-tier tests' directories on the small filesystem
	# (--small-fs) and the cold one (--cold-fs).
	[ -f "$dir/small-fs" ] && small=$(cat "$dir/small-fs") && paths+=("$small/davit-disklow-" "$small/davit-cold-")
	[ -f "$dir/cold-fs" ] && cold=$(cat "$dir/cold-fs") && paths+=("$cold/davit-cold-")
	for round in 1 2 3; do
		mapfile -t pids < <(procs_under "${paths[@]}")
		[ ${#pids[@]} -gt 0 ] || break
		for p in "${pids[@]}"; do
			# Still that process (it may have exited since the scan).
			refers "$p" "${paths[@]}" || continue
			echo "cleanup (round $round): killing $p: $(tr '\0' ' ' 2>/dev/null <"/proc/$p/cmdline" | cut -c1-160)" >>"$dir/out/cleanup.log"
			kill -KILL "$p" 2>/dev/null || true
		done
		sleep 1
	done
	# Leftover per-run scratch (test disks, e2e data dirs); the fixture's
	# bin/ and images/ persist across runs.
	find "$SCRATCH" -mindepth 1 -maxdepth 1 ! -name bin ! -name images -exec rm -rf {} + 2>/dev/null || true
	[ -z "$small" ] || find "$small" -mindepth 1 -maxdepth 1 \( -name 'davit-disklow-*' -o -name 'davit-cold-*' \) -exec rm -rf {} + 2>/dev/null || true
	[ -z "$cold" ] || find "$cold" -mindepth 1 -maxdepth 1 -name 'davit-cold-*' -exec rm -rf {} + 2>/dev/null || true
	rm -rf "$dir/t"
	# keep-out: the caller still fetches out/ and then calls cleanup again.
	[ "${2:-}" = keep-out ] || rm -rf "$dir"
	echo "cleaned run $run" >&2
}

run_tier() {
	local run=$1 re=$2 bench=$3 timeout=$4 fresh=$5 small=$6 cold=$7
	shift 7
	local dir=$SVM_DIR/runs/$run rc=0 start
	lock
	mkdir -p "$dir/out" "$dir/t"
	RUN_ID=$run # the EXIT trap runs after this function's locals are gone
	trap 'cleanup "$RUN_ID" keep-out' EXIT
	trap 'exit 130' INT TERM HUP
	sync_src "$dir/payload"
	goenv
	check >"$dir/out/host.txt" 2>&1 || { cat "$dir/out/host.txt" >&2; die "host requirements unmet: vm-tier.sh --prepare, or fix the FAIL lines"; }
	cat "$dir/out/host.txt" >&2
	{
		echo "commit $(cat "$dir/payload/HEAD")$([ -s "$dir/payload/changes.patch" ] || [ -s "$dir/payload/untracked.tar" ] && echo ' + local changes')"
		echo "go test -p 1 -count=1 -timeout $timeout -run '$re' $*"
		[ "$bench" = 1 ] && echo "DAVIT_BENCH=1"
		[ "$small" = - ] || echo "DAVIT_TEST_SMALL_FS=$small ($(df -h --output=fstype,size,avail "$small" | tail -1))"
		[ "$cold" = - ] || echo "DAVIT_TEST_COLD_FS=$cold ($(df -h --output=fstype,size,avail "$cold" | tail -1))"
		"$MISE" exec -- go version
		nproc; free -g; uptime
	} >"$dir/out/run.txt"
	export TMPDIR=$dir/t
	if [ "$small" != - ]; then
		[ -d "$small" ] || die "--small-fs $small: no such directory"
		echo "$small" >"$dir/small-fs"
		export DAVIT_TEST_SMALL_FS=$small
	fi
	if [ "$cold" != - ]; then
		[ -d "$cold" ] || die "--cold-fs $cold: no such directory"
		echo "$cold" >"$dir/cold-fs"
		export DAVIT_TEST_COLD_FS=$cold
	fi
	[ "$bench" = 1 ] && export DAVIT_BENCH=1
	start=$(date '+%Y-%m-%d %H:%M:%S')
	say "real-VM tier: -run '$re' $* (serial: one package, one test at a time)"
	# Its own process group (job control), so cleanup can end it all.
	set +e -m
	"$MISE" exec -- go test -p 1 -count=1 -timeout "$timeout" -v -run "$re" "$@" \
		> >(tee "$dir/out/test.log") 2>&1 &
	local pid=$!
	set +m
	echo "$pid" >"$dir/pgid"
	wait "$pid"
	rc=$?
	if [ "$fresh" = 1 ] && [ "$rc" = 0 ]; then
		say "fresh-host e2e on real Firecracker"
		# A release with the image built here (the payload may carry local changes).
		DAVIT_ALLOW_DIRTY=1 DAVIT_IMAGES_OUT=$SVM_FS DAVIT_MANIFESTS=$SVM_FS/manifests RELEASE_IMAGES=ubuntu-24.04-base \
			"$MISE" exec -- scripts/fresh-host-e2e.sh --transcript "$dir/out/fresh-host.txt" 2>&1 | tee "$dir/out/fresh-host.log"
		rc=${PIPESTATUS[0]}
	fi
	set -e
	# Evidence for VMs that died from outside: kernel OOM kills and other
	# kernel kill reports during the run.
	journalctl -k --since "$start" --no-pager 2>/dev/null | grep -iE 'out of memory|killed process|oom' >"$dir/out/kernel-kills.txt" || true
	grep -E '^(--- |=== RUN|PASS|FAIL|ok |panic)' "$dir/out/test.log" | grep -vE '^=== RUN' >"$dir/out/summary.txt" || true
	say "remote exit $rc"
	return "$rc"
}

# prune_blobs: drop blobs no manifest in $SVM_FS/manifests references (earlier builds' kernels
# and rootfs; each rebuild leaves GiBs). Copies elsewhere were reflinked and keep their blocks.
prune_blobs() {
	local keep h f n=0
	keep=$(grep -ohE 'sha256:[0-9a-f]{64}' "$SVM_FS"/manifests/*.json 2>/dev/null | sort -u | sed 's/^sha256://')
	[ -n "$keep" ] || return 0
	for f in "$BLOBS"/sha256/*; do
		[ -f "$f" ] || continue
		h=${f##*/}
		grep -qx "$h" <<<"$keep" && continue
		rm -f "$f"
		n=$((n + 1))
	done
	[ "$n" = 0 ] || say "pruned $n blob(s) no manifest references"
}

# images RUN REBUILD [IMAGE...]: build IMAGEs (default ubuntu-24.04-base) from the shipped commit
# into $SVM_FS (blobs) and $SVM_FS/manifests, unless each was already built here from the same
# images/ tree (stamp manifests/.<image>.tree; REBUILD=1 builds anyway). One images/build.sh call
# builds the kernel and agent once for all of them. Exit 3 when the build fails.
images() {
	local run=$1 rebuild=$2 dir=$SVM_DIR/runs/$1 tree rc=0 i todo=()
	shift 2
	local want=("$@")
	[ ${#want[@]} -gt 0 ] || want=(ubuntu-24.04-base)
	lock
	mkdir -p "$dir/out"
	RUN_ID=$run
	trap 'cleanup "$RUN_ID" keep-out' EXIT
	trap 'exit 130' INT TERM HUP
	ensure_mise
	sync_src "$dir/payload"
	goenv
	"$MISE" install -q
	tree=$(git -C "$SRC" rev-parse HEAD:images)
	for i in "${want[@]}"; do
		[ -d "$SRC/images/rootfs/$i" ] || die "no image recipe images/rootfs/$i"
		if [ "$rebuild" = 1 ] || [ ! -f "$SVM_FS/manifests/$i.json" ] || [ "$(cat "$SVM_FS/manifests/.$i.tree" 2>/dev/null)" != "$tree" ]; then
			todo+=("$i")
		fi
	done
	if [ ${#todo[@]} -eq 0 ]; then
		say "${want[*]} already built here from images/ tree ${tree:0:12}; skipping images/build.sh"
		echo "skipped: ${want[*]} built from images/ tree $tree" >"$dir/out/images.log"
	else
		say "images/build.sh ${todo[*]} on $(hostname) (images/ tree ${tree:0:12})"
		mkdir -p "$SVM_FS/manifests"
		for i in "${todo[@]}"; do rm -f "$SVM_FS/manifests/.$i.tree"; done
		set +e
		(
			cd "$SRC"
			PATH=$(dirname "$MISE"):$PATH IMAGES="${todo[*]}" DAVIT_IMAGES_OUT=$SVM_FS \
				DAVIT_IMAGES_WORK=$SVM_FS/images-work DAVIT_MANIFESTS=$SVM_FS/manifests \
				images/build.sh
		) 2>&1 | tee "$dir/out/images.log"
		rc=${PIPESTATUS[0]}
		set -e
		for i in "${todo[@]}"; do
			if [ "$rc" != 0 ] || [ ! -f "$SVM_FS/manifests/$i.json" ]; then
				echo "vm-tier-remote: images/build.sh failed (exit $rc); see images.log" >&2
				exit 3
			fi
			echo "$tree" >"$SVM_FS/manifests/.$i.tree"
		done
		prune_blobs
	fi
	for i in "${want[@]}"; do
		cp "$SVM_FS/manifests/$i.json" "$dir/out/$i.json"
		say "$i: $(grep -m1 -oE '"id": *"sha256:[0-9a-f]{12}' "$SVM_FS/manifests/$i.json" | grep -oE 'sha256:.*')"
	done
}

cmd=${1:-}
[ -n "$cmd" ] || die "usage: vm-tier-remote.sh check|prepare|run|cleanup ..."
shift
case "$cmd" in
check) check ;;
prepare) [ $# -eq 1 ] || die "prepare RUN"; prepare "$1" ;;
images) [ $# -ge 2 ] || die "images RUN REBUILD [IMAGE...]"; images "$@" ;;
run) [ $# -ge 8 ] || die "run RUN REGEX BENCH TIMEOUT FRESH SMALL_FS COLD_FS PKGS..."; run_tier "$@" ;;
cleanup) [ $# -eq 1 ] || die "cleanup RUN"; cleanup "$1" ;;
*) die "unknown command $cmd" ;;
esac
