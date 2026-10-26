#!/usr/bin/env bash
# box-bench.sh: measure project-box timings and footprint on a VM host where `make host-install`
# put davit (docs/benchmarks.md). Drives the installed daemon through its CLI over SSH, the way
# an agent on a developer machine would, and cleans up the boxes it made.
#
#   scripts/box-bench.sh [--image NAME] [-n N] [--config FILE] [--bin-dir DIR] HOST
#
# --config and --bin-dir as given to host-install.sh (relative: under the host user's home).
#
# Measures (each "→ first command" is the CLI call plus `davit exec BOX -- true` returning):
#   create     `new` → first command; the first box on a fresh image also captures its golden
#   resume     `stop`, then `resume` → first command (the disk is kept)
#   restore    `snapshot create` of a stopped box, then `new --from SNAPSHOT` → first command
#   exec       `exec BOX -- true` on a running box
#   memory     each running box's Firecracker VMM after 30 s idle: RSS, PSS, private dirty (MiB);
#              a stopped box has no VMM
#   disk       free space on the data dir's filesystem before and after N idle boxes
# Prints one JSON object on stdout. Default image ubuntu-24.04-dev, N=5.
set -euo pipefail

IMAGE=ubuntu-24.04-dev N=5 CONFIG=.config/davit/config.json BINDIR=.local/bin
while [ $# -gt 0 ]; do
	case "$1" in
	--image) IMAGE=$2; shift 2 ;;
	-n) N=$2; shift 2 ;;
	--config) CONFIG=$2; shift 2 ;;
	--bin-dir) BINDIR=$2; shift 2 ;;
	-h | --help) sed -n '2,18p' "$0" >&2; exit 2 ;;
	*) break ;;
	esac
done
[ $# -eq 1 ] || { sed -n '2,18p' "$0" >&2; exit 2; }
case "$IMAGE$N" in *[!a-z0-9.-]*) echo "box-bench: bad --image or -n" >&2; exit 2 ;; esac
case "$CONFIG$BINDIR" in *[!A-Za-z0-9._/-]*) echo "box-bench: --config/--bin-dir must be plain paths" >&2; exit 2 ;; esac

# bench IMAGE N CONFIG BINDIR: runs on the host.
bench() {
	set -euo pipefail
	local image=$1 n=$2 config=$3 bindir=$4 i p data sock
	case "$config" in /*) ;; *) config=$HOME/$config ;; esac
	case "$bindir" in /*) ;; *) bindir=$HOME/$bindir ;; esac
	export XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}
	data=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["dataDir"])' "$config")
	sock=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["socket"])' "$config")
	local S=("$bindir/davit" --api-url "unix://$sock")
	ms() { echo $((($(date +%s%N) - $1) / 1000000)); }
	timed() { local t0; t0=$(date +%s%N); "$@" >/dev/null; echo "$t0"; }
	first() { # first BOX T0: exec true, print ms since T0
		"${S[@]}" exec "$1" -- true >/dev/null
		ms "$2"
	}
	list() { local IFS=,; echo "$*"; }
	vmms() { # this daemon's Firecracker VMMs: their working dir is under its data dir
		local q
		for q in $(pgrep -u "$(id -u)" -f 'firecracker-v[0-9]' || true); do
			case "$(readlink "/proc/$q/cwd" 2>/dev/null)" in "$data"/*) echo "$q" ;; esac
		done
	}
	freekib() { df -Pk "$data" | awk 'NR == 2 {print $4}'; }
	vmm_mem() { # PIDs... -> "rss pss dirty" MiB per VMM, one line each
		for p in "$@"; do
			awk '/^Rss:/ {r=$2} /^Pss:/ {s=$2} /^Private_Dirty:/ {d=$2} END {printf "%.0f %.0f %.0f\n", r/1024, s/1024, d/1024}' "/proc/$p/smaps_rollup"
		done
	}
	for i in $(seq 0 "$n"); do "${S[@]}" delete "bb$i" --yes >/dev/null 2>&1 || true; "${S[@]}" delete "br$i" --yes >/dev/null 2>&1 || true; done
	"${S[@]}" snapshot delete bbsnap --yes >/dev/null 2>&1 || true

	local free0 golden creates=() execs=() resumes=() restores=() t0
	free0=$(freekib)
	t0=$(timed "${S[@]}" new bb0 --image "$image")
	golden=$(first bb0 "$t0")
	for i in $(seq 1 "$n"); do
		t0=$(timed "${S[@]}" new "bb$i" --image "$image")
		creates+=("$(first "bb$i" "$t0")")
	done
	for i in $(seq 1 20); do
		t0=$(date +%s%N)
		"${S[@]}" exec bb1 -- true >/dev/null
		execs+=("$(ms "$t0")")
	done
	sleep 30
	local pids=() mem free_running avail
	mapfile -t pids < <(vmms)
	mem=$(vmm_mem "${pids[@]}" | tr '\n' ';')
	free_running=$(freekib)
	avail=$(awk '/^MemAvailable:/ {print int($2/1024)}' /proc/meminfo)
	local shape
	shape=$("${S[@]}" --json info bb1 | tr -d '\n ' | grep -oE '"shape":\{[^}]*\}' | head -n1)

	for i in $(seq 1 "$n"); do
		"${S[@]}" stop "bb$i" >/dev/null
		t0=$(timed "${S[@]}" resume "bb$i")
		resumes+=("$(first "bb$i" "$t0")")
	done
	for i in $(seq 0 "$n"); do "${S[@]}" stop "bb$i" >/dev/null; done
	local stopped_vmms
	stopped_vmms=$(vmms | wc -l)
	"${S[@]}" snapshot create bb1 bbsnap >/dev/null
	for i in $(seq 1 "$n"); do
		t0=$(timed "${S[@]}" new "br$i" --from bbsnap)
		restores+=("$(first "br$i" "$t0")")
	done
	for i in $(seq 0 "$n"); do "${S[@]}" delete "bb$i" --yes >/dev/null 2>&1; done
	for i in $(seq 1 "$n"); do "${S[@]}" delete "br$i" --yes >/dev/null 2>&1; done
	"${S[@]}" snapshot delete bbsnap --yes >/dev/null 2>&1 || true
	printf '{"image":"%s","n":%s,"shape":%s,"host":"%s","cpus":%s,"memTotalMiB":%s,"memAvailableMiBWithBoxesRunning":%s,"load":"%s",' \
		"$image" "$n" "${shape#\"shape\":}" "$(hostname)" "$(nproc)" "$(awk '/^MemTotal:/ {print int($2/1024)}' /proc/meminfo)" "$avail" "$(cut -d' ' -f1-3 /proc/loadavg)"
	printf '"firstCreateWithGoldenMs":%s,"createToFirstCommandMs":[%s],"execRunningMs":[%s],"resumeToFirstCommandMs":[%s],"restoreToFirstCommandMs":[%s],' \
		"$golden" "$(list "${creates[@]}")" "$(list "${execs[@]}")" "$(list "${resumes[@]}")" "$(list "${restores[@]}")"
	printf '"runningVMMs":%s,"vmmRssPssDirtyMiB":"%s","vmmsWhileStopped":%s,"diskFreeKiB":{"before":%s,"withBoxesRunning":%s}}\n' \
		"${#pids[@]}" "$mem" "$stopped_vmms" "$free0" "$free_running"
}

{ declare -f bench; printf 'bench %q %q %q %q\n' "$IMAGE" "$N" "$CONFIG" "$BINDIR"; } | ssh -o BatchMode=yes "$1" bash -s
