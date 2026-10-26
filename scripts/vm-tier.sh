#!/usr/bin/env bash
# vm-tier.sh: run the real-VM test tier (DAVIT_TEST_FIRECRACKER=1) on a
# separate Linux VM host over SSH. Real VMs never run on the development machine. See
# docs/vm-tier.md.
#
#   scripts/vm-tier.sh [options] HOST [TESTS-REGEX]   run the tier (default regex: all tests)
#   scripts/vm-tier.sh --prepare [options] HOST       install Go, Firecracker, image blobs once
#   scripts/vm-tier.sh --images [options] HOST        build images on the host (images/build.sh)
#   scripts/vm-tier.sh --check HOST                   host requirements only
#
# HOST is any SSH destination (tailnet name, IP, user@host, ~/.ssh/config
# alias); key auth, no prompts.
#
# Options:
#   --bench            also DAVIT_BENCH=1 (benchmarks; numbers land in the test log)
#   --fresh-host       also scripts/fresh-host-e2e.sh on real Firecracker (if the tests pass)
#   --timeout DUR      go test timeout (default 90m)
#   --pkg PKG          package to test, repeatable (default: ./internal/runtime/firecracker/
#                      ./cmd/davit/ ./internal/runtime/firecracker/fctest/, the real-VM ones)
#   --dir DIR          work dir on the host (default ~/.svm; keep it short: socket paths)
#   --fs DIR           reflink filesystem dir on the host for blobs and scratch
#                      (default DIR/fs; btrfs or XFS)
#   --blobs DIR        local image blobs for --prepare (default $DAVIT_VM_BLOBS,
#                      else images/out/blobs); only the base manifest's blobs are sent.
#                      Not needed once --images built the image on the host
#   --rebuild-images   --images rebuilds even when the host's image matches images/
#   --image NAME       --images builds NAME (repeatable; default ubuntu-24.04-base)
#   --min-disk GIB     free space the host check requires on the reflink filesystem (default 20)
#   --out DIR          local results dir (default .build/vm-tier/<run>)
#
# --images ships the committed HEAD only (the baked agent must match a commit)
# and runs images/build.sh for ubuntu-24.04-base on the host, with blobs in
# the reflink filesystem and the manifest in DIR/fs/manifests; prepare and
# run then use that manifest. It skips the build when the host's image was
# built from the same images/ tree.
#
# It ships the committed HEAD plus every uncommitted change (staged, unstaged,
# untracked-but-not-ignored; never .build/, bin/, images/out/), checks the host
# (KVM, user namespaces, reflink scratch, tools, disk), runs the tests one
# package and one test at a time with live output, copies the logs back to
# .build/vm-tier/<run>/, and always stops what the run started on the host
# (VMMs, daemons, namespace holders) and removes its scratch. The exit status
# is the remote one: 0 pass, 1 test failure, 2 host or usage problem, 3 image
# build failed (--images), 255 SSH.
set -euo pipefail

MODE=run BENCH=0 FRESH=0 TIMEOUT=90m DIR="" FS="" PKGS=() REBUILD=0 OUT="" IMAGES=() MIN_DISK=""
BLOBS=${DAVIT_VM_BLOBS:-}
usage() { sed -n '2,44p' "$0" >&2; exit 2; }
while [ $# -gt 0 ]; do
	case "$1" in
	--prepare) MODE=prepare; shift ;;
	--check) MODE=check; shift ;;
	--images) MODE=images; shift ;;
	--rebuild-images) REBUILD=1; shift ;;
	--image) IMAGES+=("$2"); shift 2 ;;
	--min-disk) [[ $2 =~ ^[0-9]+$ ]] || usage; MIN_DISK=$2; shift 2 ;;
	--out) OUT=$2; shift 2 ;;
	--bench) BENCH=1; shift ;;
	--fresh-host) FRESH=1; shift ;;
	--timeout) TIMEOUT=$2; shift 2 ;;
	--pkg) PKGS+=("$2"); shift 2 ;;
	--dir) DIR=$2; shift 2 ;;
	--fs) FS=$2; shift 2 ;;
	--blobs) BLOBS=$2; shift 2 ;;
	-h | --help) usage ;;
	-*) echo "vm-tier: unknown option $1" >&2; usage ;;
	*) break ;;
	esac
done
[ $# -ge 1 ] || usage
HOST=$1
RE=${2:-.}
[ $# -le 2 ] || usage
[ "$MODE" = run ] || [ $# -eq 1 ] || usage
[ ${#PKGS[@]} -gt 0 ] || PKGS=(./internal/runtime/firecracker/ ./cmd/davit/ ./internal/runtime/firecracker/fctest/)

ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
cd "$ROOT"
MANIFEST=images/manifests/ubuntu-24.04-base.json
[ -n "$BLOBS" ] || BLOBS=$ROOT/images/out/blobs
RUN="$(date -u +%Y%m%dT%H%M%SZ)-$MODE-$$"
[ -n "$OUT" ] || OUT="$ROOT/.build/vm-tier/$RUN"
case "$OUT" in /*) ;; *) OUT="$ROOT/$OUT" ;; esac
PAY="$OUT/payload"
mkdir -p "$PAY"

say() { printf '==> %s\n' "$*" >&2; }

# One SSH connection for the whole run; BatchMode never prompts.
# The control socket is this run's own: runs from the same user to the same host
# (another worktree, another agent) must not share a master, or one run's
# `ssh -O exit` at its end cuts every other run's connection (exit 255).
CTL="$HOME/.ssh/vt-$$-%C"
SSH=(ssh -o BatchMode=yes -o ControlMaster=auto -o "ControlPath=$CTL" -o ControlPersist=120 -o ServerAliveInterval=30)
# The host layout, as the remote script resolves it. --dir/--fs are paths on
# the host; a leading ~/ is the remote home. Single quotes throughout are
# deliberate: $HOME and ${SVM_*} expand on the host, not here.
# shellcheck disable=SC2016,SC2088
hostpath() {
	case "$1" in
	*[!A-Za-z0-9._/~-]*) echo "vm-tier: $2 must be a plain path (letters, digits, . _ - / ~): $1" >&2; exit 2 ;;
	"~/"*) printf '$HOME/%s' "${1#\~/}" ;;
	*) printf '%s' "$1" ;;
	esac
}
[ -z "$DIR" ] || hostpath "$DIR" --dir >/dev/null
[ -z "$FS" ] || hostpath "$FS" --fs >/dev/null
# shellcheck disable=SC2016
RENV='export SVM_DIR="${SVM_DIR:-$HOME/.svm}";'
[ -z "$DIR" ] || RENV="export SVM_DIR=\"$(hostpath "$DIR" --dir)\";"
[ -z "$FS" ] || RENV+=" export SVM_FS=\"$(hostpath "$FS" --fs)\";"
# shellcheck disable=SC2016
RENV+=' export SVM_FS="${SVM_FS:-$SVM_DIR/fs}";'
[ -z "$MIN_DISK" ] || RENV+=" export SVM_MIN_DISK=$MIN_DISK;"
RDIR="\$SVM_DIR/runs/$RUN"
# rsh SCRIPT: run SCRIPT (bash) on the host with the layout exported.
rsh() { "${SSH[@]}" "$HOST" "$RENV $1"; }
# rsh_long SCRIPT: rsh for the long steps. bash runs a trap only after the
# foreground command ends, so it runs in the background and is waited for:
# wait returns at once on INT/TERM, and cleanup stops the run on the host.
RPID=""
rsh_long() {
	local rc=0
	rsh "$1" </dev/null &
	RPID=$!
	wait "$RPID" || rc=$?
	RPID=""
	return "$rc"
}

say "host $HOST"
if ! "${SSH[@]}" "$HOST" true; then
	echo "vm-tier: cannot ssh to $HOST without a prompt (key auth, BatchMode)" >&2
	exit 255
fi

# The payload: a bundle of HEAD, the binary diff of tracked files against
# it, the untracked files, and the remote half of this script. An image
# build gets HEAD alone.
local_changes() { ! git diff --quiet HEAD || [ -n "$(git ls-files --others --exclude-standard)" ]; }
git rev-parse HEAD >"$PAY/HEAD"
git bundle create -q "$PAY/src.bundle" HEAD
if [ "$MODE" = images ]; then
	say "packing $(git rev-parse --short HEAD)$(local_changes && echo ' (local changes stay here: images build from commits)')"
	: >"$PAY/changes.patch"
	tar -cf "$PAY/untracked.tar" -T /dev/null
else
	say "packing $(git rev-parse --short HEAD)$(local_changes && echo ' + local changes')"
	git diff --binary HEAD >"$PAY/changes.patch"
	# grep exits 1 when nothing is untracked: a clean tree is not an error.
	git ls-files -z --others --exclude-standard |
		{ grep -zvE '^(\.build|bin|images/out)/' || true; } |
		tar --null -T - -cf "$PAY/untracked.tar"
fi
cp scripts/vm-tier-remote.sh "$PAY/"

STARTED=0
cleanup() {
	local rc=$?
	trap - EXIT INT TERM
	if [ -n "$RPID" ]; then
		kill "$RPID" 2>/dev/null || true
		say "interrupted: stopping the run on $HOST"
	fi
	if [ "$STARTED" = 1 ]; then
		# Logs first, then the host forgets the run (VMs included).
		mkdir -p "$OUT/logs"
		rsh "tar -C $RDIR/out -cf - . 2>/dev/null" | tar -C "$OUT/logs" -xf - 2>/dev/null || echo "vm-tier: no logs copied back" >&2
		if ! rsh "bash $RDIR/payload/vm-tier-remote.sh cleanup $RUN"; then
			echo "vm-tier: WARNING: host cleanup failed; run: ssh $HOST bash $RDIR/payload/vm-tier-remote.sh cleanup $RUN" >&2
		fi
	fi
	rm -rf "$PAY"
	"${SSH[@]}" -O exit "$HOST" 2>/dev/null || true
	say "results in ${OUT#"$ROOT"/}; exit $rc"
	exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

STARTED=1
rsh "mkdir -p $RDIR/payload $RDIR/out"
# rsync over the shared connection when both ends have it (the bundle is the
# bulk), else a tar stream.
if command -v rsync >/dev/null && rsh "command -v rsync >/dev/null"; then
	rdir=$(rsh "echo $RDIR")
	rsync -a -e "${SSH[*]}" "$PAY/" "$HOST:$rdir/payload/"
else
	tar -C "$PAY" -cf - . | rsh "tar -C $RDIR/payload -xf -"
fi
REMOTE="bash $RDIR/payload/vm-tier-remote.sh"

case "$MODE" in
check)
	rsh "$REMOTE check"
	;;
images)
	for i in "${IMAGES[@]}"; do
		case "$i" in *[!a-z0-9.-]*) echo "vm-tier: bad image name $i" >&2; exit 2 ;; esac
	done
	rsh_long "$REMOTE images $RUN $REBUILD ${IMAGES[*]}"
	;;
prepare)
	# An image built on the host (--images) needs no upload.
	if rsh "test -f \"\${SVM_FS}/manifests/ubuntu-24.04-base.json\""; then
		say "host has its own built base image (--images); sending no blobs"
		rsh_long "$REMOTE prepare $RUN"
		exit 0
	fi
	[ -d "$BLOBS/sha256" ] || { echo "vm-tier: no blob dir $BLOBS/sha256 (pass --blobs, set DAVIT_VM_BLOBS, or build on the host with --images)" >&2; exit 2; }
	# Only the base manifest's blobs, only those the host lacks; sparse.
	have=$(rsh "ls \"\${SVM_FS}/blobs/sha256\" 2>/dev/null || true")
	send=()
	while read -r d; do
		h=${d#sha256:}
		if [ -f "$BLOBS/sha256/$h" ] && ! grep -qx "$h" <<<"$have"; then
			send+=("sha256/$h")
		fi
	done < <(grep -oE 'sha256:[0-9a-f]{64}' "$MANIFEST" | sort -u)
	if [ ${#send[@]} -gt 0 ]; then
		say "sending ${#send[@]} image blob(s) from $BLOBS (sparse)"
		tar -C "$BLOBS" -cSf - "${send[@]}" | rsh "mkdir -p \"\${SVM_FS}/blobs\" && tar -C \"\${SVM_FS}/blobs\" -xSf -"
	else
		say "host already has the base image blobs"
	fi
	rsh_long "$REMOTE prepare $RUN"
	;;
run)
	say "running the real-VM tier on $HOST: -run '$RE' ${PKGS[*]}$([ "$BENCH" = 1 ] && echo ' (bench)')"
	rsh_long "$REMOTE run $RUN $(printf '%q' "$RE") $BENCH $TIMEOUT $FRESH $(printf '%q ' "${PKGS[@]}")"
	;;
esac
