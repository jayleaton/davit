#!/usr/bin/env bash
# box-image-bench.sh: measure a per-project image on a VM host where `make host-install` put
# davit (docs/benchmarks.md): a builder box runs a heavy setup, its disk becomes a snapshot
# (the project image), and instances (one per worktree) are created from it. Drives the
# installed daemon through its CLI over SSH and removes what it made.
#
#   scripts/box-image-bench.sh [--config FILE] [--bin-dir DIR] HOST
#
# Setup (in the builder, as the image user through a login shell; about 2.5 GB of dependencies):
#   docker pull node:24-bookworm; a Go module requiring k8s.io/client-go (go mod download + build);
#   a pnpm project with next, react, typescript, @mui/material, esbuild; a uv venv with numpy,
#   pandas, scipy, scikit-learn.
# Measures: setup wall time; the builder's own disk; stop + snapshot create; the first
# `new --from` (it captures the project image's golden: its id, boot mode, memory file and own
# disk); 5 more `new --from` → first command; each running VMM's RSS, PSS and private dirty after
# 30 s idle with 3 and with 6 instances; each of 3 instances' own disk idle and after a warm
# rebuild (go build, pnpm install --offline, uv pip sync, docker run); stop → resume → first
# command; and that a file survives stop/resume, a daemon restart and a resume after it.
# Disk "own" = physical blocks outside the image's, golden's and snapshot's extents (xfs_io
# fiemap); an instance's live disk is measured while it runs (a stop would commit a copy sharing
# those blocks). The daemon restart restarts davit.service: run it on a daemon with no other VMs
# with no other VMs. Prints one JSON object.
set -euo pipefail

CONFIG=.config/davit/config.json BINDIR=.local/bin
while [ $# -gt 0 ]; do
	case "$1" in
	--config) CONFIG=$2; shift 2 ;;
	--bin-dir) BINDIR=$2; shift 2 ;;
	-h | --help) sed -n '2,19p' "$0" >&2; exit 2 ;;
	*) break ;;
	esac
done
[ $# -eq 1 ] || { sed -n '2,19p' "$0" >&2; exit 2; }
case "$CONFIG$BINDIR" in *[!A-Za-z0-9._/-]*) echo "box-image-bench: plain paths only" >&2; exit 2 ;; esac

# The setup a project would bake into its image (runs in the builder box).
setup_script() {
	cat <<'EOF'
set -euo pipefail
docker pull -q node:24-bookworm >/dev/null
mkdir -p ~/proj/goapp ~/proj/web ~/proj/py
cd ~/proj/goapp
go mod init example.com/goapp >/dev/null 2>&1
cat >main.go <<'GO'
package main

import (
	"fmt"

	_ "k8s.io/client-go/kubernetes"
)

func main() { fmt.Println("ok") }
GO
go get k8s.io/client-go@v0.34.1 >/dev/null 2>&1
go mod tidy >/dev/null 2>&1
go build -o /dev/null .
cd ~/proj/web
cat >package.json <<'JSON'
{"name": "web", "private": true, "dependencies": {"next": "15.5.4", "react": "19.1.1", "react-dom": "19.1.1",
 "typescript": "5.9.3", "@mui/material": "7.3.4", "@emotion/react": "11.14.0", "@emotion/styled": "11.14.1", "esbuild": "0.25.10"}}
JSON
pnpm install --silent --config.strict-dep-builds=false >/dev/null
cd ~/proj/py
uv venv -q .venv
uv pip install -q --python .venv numpy pandas scipy scikit-learn
uv pip freeze -q --python .venv >requirements.txt
sync
EOF
}

# The work an instance does against the warm caches.
work_script() {
	cat <<'EOF'
set -euo pipefail
cd ~/proj/goapp && go build -o /tmp/goapp . && /tmp/goapp >/dev/null
cd ~/proj/web && rm -rf node_modules && pnpm install --offline --silent --config.strict-dep-builds=false >/dev/null
cd ~/proj/py && rm -rf .venv && uv venv -q .venv && uv pip sync -q --python .venv --offline requirements.txt
docker run --rm node:24-bookworm node -e 1
sync
EOF
}

# bench M K CONFIG BINDIR SETUP WORK: runs on the host.
bench() {
	set -euo pipefail
	local config=$1 bindir=$2 setup=$3 work=$4 data sock i t0
	case "$config" in /*) ;; *) config=$HOME/$config ;; esac
	case "$bindir" in /*) ;; *) bindir=$HOME/$bindir ;; esac
	export XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}
	data=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["dataDir"])' "$config")
	sock=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["socket"])' "$config")
	local S=("$bindir/davit" --api-url "unix://$sock")
	ms() { echo $((($(date +%s%N) - $1) / 1000000)); }
	list() { local IFS=,; echo "$*"; }
	vmid() { "${S[@]}" --json info "$1" | grep -oE '"id": *"vm_[a-z0-9]+"' | head -n1 | grep -oE 'vm_[a-z0-9]+'; }
	vmm_of() { # VM ID -> its VMM PID
		local q
		for q in $(pgrep -u "$(id -u)" -f 'firecracker-v[0-9]' || true); do
			case "$(readlink "/proc/$q/cwd" 2>/dev/null)" in "$data"/*"$1"*) echo "$q" ;; esac
		done
	}
	# own BASEFILES(comma) FILES...: MiB of FILES' physical blocks outside BASEFILES' extents.
	own() {
		python3 - "$@" <<'PY'
import subprocess, sys
def ext(p):
    out = subprocess.run(["xfs_io", "-r", "-c", "fiemap -v", p], capture_output=True, text=True).stdout.splitlines()[2:]
    r = []
    for l in out:
        f = l.split()
        if len(f) < 5 or f[2] == "hole":
            continue
        a, b = f[2].split("..")
        r.append((int(a), int(b) + 1))
    return r
def merge(rs):
    m = []
    for a, b in sorted(rs):
        if m and a <= m[-1][1]:
            m[-1] = (m[-1][0], max(m[-1][1], b))
        else:
            m.append((a, b))
    return m
base = merge([x for p in sys.argv[1].split(",") if p for x in ext(p)])
tot = 0
for a, b in merge([x for p in sys.argv[2:] for x in ext(p)]):
    n = b - a
    for c, d in base:
        if d <= a:
            continue
        if c >= b:
            break
        n -= min(b, d) - max(a, c)
    tot += n
print(tot * 512 >> 20)
PY
	}
	files_of() { find "$data/storage/disks" -type f -name "*$1*" 2>/dev/null | sort; } # a VM's live disk

	for i in $(seq 1 6); do "${S[@]}" delete "pi$i" --yes >/dev/null 2>&1 || true; done
	"${S[@]}" delete pbuild --yes >/dev/null 2>&1 || true
	"${S[@]}" snapshot delete pimage --yes >/dev/null 2>&1 || true

	local manifest blob golden base
	manifest=$data/images/manifests/ubuntu-24.04-dev.json
	blob=$data/images/blobs/sha256/$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["rootfs"]["digest"].split(":")[1])' "$manifest")
	# Any failure leaves nothing behind.
	# shellcheck disable=SC2329 # invoked by the trap
	cleanup() {
		local j
		for j in $(seq 1 6); do "${S[@]}" delete "pi$j" --yes >/dev/null 2>&1 || true; done
		"${S[@]}" delete pbuild --yes >/dev/null 2>&1 || true
		"${S[@]}" snapshot delete pimage --yes >/dev/null 2>&1 || true
	}
	trap cleanup EXIT
	# The default shape (2 vCPUs, 2 GiB, 8 GiB disk): its golden is shared with every other box.
	"${S[@]}" new pbuild --image ubuntu-24.04-dev >/dev/null 2>&1
	"${S[@]}" exec pbuild -- true
	golden=$(find "$data/runtime/firecracker/golden" -name root.img 2>/dev/null | tr '\n' ',')
	base=$blob,$golden
	local setup_ms build_own
	t0=$(date +%s%N)
	printf '%s\n' "$setup" | "${S[@]}" exec --stdin pbuild -- bash -l >/dev/null
	setup_ms=$(ms "$t0")
	local used
	used=$("${S[@]}" exec pbuild -- sh -c 'du -sm ~/.cache/go-build ~/go/pkg/mod ~/.local/share/pnpm/store ~/.cache/uv ~/proj /var/lib/docker 2>/dev/null' | tr '\t\n' ' ;' || true)
	t0=$(date +%s%N)
	"${S[@]}" stop pbuild >/dev/null
	local stop_ms snap_ms
	stop_ms=$(ms "$t0")
	t0=$(date +%s%N)
	"${S[@]}" snapshot create pbuild pimage >/dev/null
	snap_ms=$(ms "$t0")
	# The builder's disk and its commits (this daemon runs nothing else during the bench).
	# shellcheck disable=SC2046 # one path per word
	build_own=$(own "$base" $(find "$data/storage" -type f | sort))
	"${S[@]}" delete pbuild --yes >/dev/null
	# What is left in commits/ is the project image (the snapshot).
	local snapfiles image_own
	snapfiles=$(find "$data/storage/commits" -type f | sort | tr '\n' ',')
	# shellcheck disable=SC2046
	image_own=$(own "$base" $(find "$data/storage/commits" -type f | sort))

	local G=$data/runtime/firecracker/golden
	local before forks=() ids=() first gid="" gmode gmem groot mem3="" mem6="" owns="" owns_work="" work_ms=()
	before=$(ls "$G")
	t0=$(date +%s%N)
	"${S[@]}" new pi1 --from pimage >/dev/null
	"${S[@]}" exec pi1 -- true
	first=$(ms "$t0")
	gid=$(comm -13 <(echo "$before") <(ls "$G") | head -n1)
	gmode=$("${S[@]}" --json info pi1 | grep -oE '"bootMode": *"[a-z_]+"' | grep -oE '[a-z_]+"$' | tr -d '"')
	gmem=0 groot=0
	if [ -n "$gid" ]; then
		gmem=$(($(stat -c %b "$G/$gid/mem") * 512 >> 20))
		groot=$(own "$base,$snapfiles" "$G/$gid/root.img")
	fi
	vmm_mem() { # running instances pi1..piN -> "rss pss dirty;" each, after 30 s idle
		local j out="" id p
		sleep 30
		for j in $(seq 1 "$1"); do
			id=$(vmid "pi$j")
			p=$(vmm_of "$id")
			out+=$(awk '/^Rss:/ {r=$2} /^Pss:/ {s=$2} /^Private_Dirty:/ {d=$2} END {printf "%.0f %.0f %.0f;", r/1024, s/1024, d/1024}' "/proc/$p/smaps_rollup")
		done
		echo "$out"
	}
	for i in $(seq 2 6); do
		t0=$(date +%s%N)
		"${S[@]}" new "pi$i" --from pimage >/dev/null
		"${S[@]}" exec "pi$i" -- true
		forks+=("$(ms "$t0")")
		[ "$i" != 3 ] || mem3=$(vmm_mem 3)
	done
	mem6=$(vmm_mem 6)
	for i in 4 5 6; do "${S[@]}" delete "pi$i" --yes >/dev/null; done
	for i in 1 2 3; do ids+=("$(vmid "pi$i")"); done
	local imgbase=$base,$snapfiles,$G/$gid/root.img
	local id
	for id in "${ids[@]}"; do
		# shellcheck disable=SC2046
		owns+="$(own "$imgbase" $(files_of "$id")),"
	done
	for i in 1 2 3; do
		t0=$(date +%s%N)
		printf '%s\n' "$work" | "${S[@]}" exec --stdin "pi$i" -- bash -l >/dev/null
		work_ms+=("$(ms "$t0")")
	done
	for id in "${ids[@]}"; do
		# shellcheck disable=SC2046
		owns_work+="$(own "$imgbase" $(files_of "$id")),"
	done
	# Persistence: down/up keeps the instance's disk; a daemon restart keeps running instances
	# and a later resume.
	local resume_ms persist="ok"
	"${S[@]}" exec pi1 -- sh -c 'echo kept-1 >~/marker && sync'
	"${S[@]}" stop pi1 >/dev/null
	t0=$(date +%s%N)
	"${S[@]}" resume pi1 >/dev/null
	"${S[@]}" exec pi1 -- true
	resume_ms=$(ms "$t0")
	[ "$("${S[@]}" exec pi1 -- cat /home/davit/marker)" = kept-1 ] || persist="marker lost after stop/resume"
	"${S[@]}" exec pi2 -- sh -c 'echo kept-2 >~/marker && sync'
	systemctl --user restart davit.service
	for i in $(seq 60); do "${S[@]}" --json capabilities >/dev/null 2>&1 && break; sleep 1; done
	[ "$("${S[@]}" exec pi2 -- cat /home/davit/marker)" = kept-2 ] || persist="running instance lost over a daemon restart"
	"${S[@]}" stop pi2 >/dev/null
	"${S[@]}" resume pi2 >/dev/null
	[ "$("${S[@]}" exec pi2 -- cat /home/davit/marker)" = kept-2 ] || persist="marker lost on resume after a daemon restart"
	for i in 1 2 3; do "${S[@]}" delete "pi$i" --yes >/dev/null; done
	"${S[@]}" snapshot delete pimage --yes >/dev/null || true
	printf '{"host":"%s","load":"%s","setupMs":%s,"setupDuMiB":"%s","builderStopMs":%s,"snapshotCreateMs":%s,' \
		"$(hostname)" "$(cut -d' ' -f1-3 /proc/loadavg)" "$setup_ms" "$used" "$stop_ms" "$snap_ms"
	printf '"builderOwnMiB":%s,"imageOwnMiB":%s,"firstForkMs":%s,"firstForkBootMode":"%s","goldenID":"%s","goldenMemMiB":%s,"goldenRootOwnMiB":%s,' \
		"$build_own" "$image_own" "$first" "$gmode" "$gid" "$gmem" "$groot"
	printf '"forkToFirstCommandMs":[%s],"vmm3RssPssDirtyMiB":"%s","vmm6RssPssDirtyMiB":"%s",' \
		"$(list "${forks[@]}")" "$mem3" "$mem6"
	printf '"instanceOwnMiBIdle":"%s","instanceWorkMs":[%s],"instanceOwnMiBAfterWork":"%s","resumeToFirstCommandMs":%s,"persistence":"%s"}\n' \
		"${owns%,}" "$(list "${work_ms[@]}")" "${owns_work%,}" "$resume_ms" "$persist"
}

{
	declare -f bench
	printf 'bench %q %q %q %q\n' "$CONFIG" "$BINDIR" "$(setup_script)" "$(work_script)"
} | ssh -o BatchMode=yes "$1" bash -s
