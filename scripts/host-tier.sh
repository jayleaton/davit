#!/usr/bin/env bash
# host-tier.sh: a VM host from SSH access to real-VM test results in one
# command (`make vm-tier HOST=user@host`). See docs/vm-host.md.
#
#   scripts/host-tier.sh [options] HOST [TESTS-REGEX]
#
# Steps, in order; the first failure stops the run:
#   1. preflight  scripts/host-preflight.sh: read-only host checks (a FAIL stops here)
#   2. images     scripts/vm-tier.sh --images: ship HEAD (rsync), images/build.sh on the
#                 host for ubuntu-24.04-base; skipped there when its image matches images/
#   3. prepare    scripts/vm-tier.sh --prepare: toolchain, blob digests, fixture
#   4. tier       scripts/vm-tier.sh: the real-VM tests (DAVIT_TEST_FIRECRACKER=1)
#
# Options:
#   --bench, --fresh-host, --timeout DUR, --pkg PKG, --dir DIR, --fs DIR   passed to vm-tier.sh
#   --rebuild-images   build the image even when the host's matches images/
#   --min-disk GIB     free space the preflight requires (default 20)
#   --skip-images      no image step: the host already has blobs (vm-tier.sh --prepare --blobs)
#   --results DIR      results root (default results/vm-tier)
#
# Results land in RESULTS/<UTC date and time>/: summary.md (steps, durations,
# test counts, failures), preflight.txt, one <step>.log per step and each
# vm-tier step's copied-back logs in <step>/logs/. Exit status: the failing
# step's (0 pass, 1 test failure, 2 host or usage problem, 3 image build
# failed, 130 interrupted, 255 SSH).
set -uo pipefail

usage() { sed -n '2,25p' "$0" >&2; exit 2; }
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPTS=$ROOT/scripts
PASS=() LAYOUT=() SKIP_IMAGES=0 REBUILD=0 RESULTS=""
while [ $# -gt 0 ]; do
	case "$1" in
	--bench | --fresh-host) PASS+=("$1"); shift ;;
	--timeout | --pkg) [ $# -ge 2 ] || usage; PASS+=("$1" "$2"); shift 2 ;;
	--dir | --fs) [ $# -ge 2 ] || usage; LAYOUT+=("$1" "$2"); shift 2 ;;
	--rebuild-images) REBUILD=1; shift ;;
	--skip-images) SKIP_IMAGES=1; shift ;;
	--min-disk) [ $# -ge 2 ] || usage; LAYOUT+=("$1" "$2"); shift 2 ;;
	--results) [ $# -ge 2 ] || usage; RESULTS=$2; shift 2 ;;
	-h | --help) usage ;;
	-*) echo "host-tier: unknown option $1" >&2; usage ;;
	*) break ;;
	esac
done
[ $# -ge 1 ] && [ $# -le 2 ] || usage
HOST=$1 RE=${2:-.}

cd "$ROOT" || exit 2
RESULTS=${RESULTS:-$ROOT/results/vm-tier}
case "$RESULTS" in /*) ;; *) RESULTS=$ROOT/$RESULTS ;; esac
STAMP=$(date -u +%Y-%m-%dT%H%M%SZ)
RES=$RESULTS/$STAMP
mkdir -p "$RES" || exit 2
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
LOCAL=""
if ! git diff --quiet HEAD 2>/dev/null || [ -n "$(git ls-files --others --exclude-standard 2>/dev/null)" ]; then
	LOCAL=" + local changes (tests only; the image builds from the commit)"
fi
STARTED=$(date -u '+%Y-%m-%d %H:%M:%S UTC')

say() { printf '\n==> host-tier: %s\n' "$*" >&2; }
STEPS=() # "name|status|exit|seconds"
RC=0

# step NAME CMD...: run CMD with its output shown and kept in NAME.log.
step() {
	local name=$1 start rc
	shift
	say "$name: $*"
	start=$(date +%s)
	"$@" 2>&1 | tee "$RES/$name.log"
	rc=${PIPESTATUS[0]}
	STEPS+=("$name|$([ "$rc" = 0 ] && echo pass || echo FAIL)|$rc|$(($(date +%s) - start))")
	return "$rc"
}

skip() { STEPS+=("$1|skipped|-|0"); }

count() { grep -c -- "$1" "$2" 2>/dev/null || true; }

summary() {
	local s name status code secs t=$RES/tier/logs/summary.txt
	{
		echo "# vm-tier on $HOST, $STARTED"
		echo
		echo "- commit: $COMMIT$LOCAL"
		echo "- tests: \`$RE\`${PASS[*]:+ (${PASS[*]})}"
		echo "- result: $([ "$RC" = 0 ] && echo PASS || echo "FAIL (exit $RC)")"
		echo
		echo "| step | status | exit | seconds |"
		echo "|---|---|---|---|"
		for s in "${STEPS[@]}"; do
			IFS='|' read -r name status code secs <<<"$s"
			echo "| $name | $status | $code | $secs |"
		done
		if [ -f "$RES/preflight.txt" ]; then
			echo
			echo "## Preflight"
			echo
			echo '```'
			grep -E '^(host |[a-z-]+ +(FAIL|WARN) |preflight:)' "$RES/preflight.txt"
			echo '```'
		fi
		if [ -f "$t" ]; then
			echo
			echo "## Tests"
			echo
			echo "pass $(count '^--- PASS' "$t"), fail $(count '^--- FAIL' "$t"), skip $(count '^--- SKIP' "$t") (top-level tests; subtests are in tier/logs/test.log)"
			if grep -q -- '^--- FAIL\|^FAIL' "$t"; then
				echo
				echo '```'
				grep -- '^--- FAIL\|^FAIL' "$t"
				echo '```'
			fi
		fi
		echo
		echo "Files: preflight.txt, <step>.log, <step>/logs/ (test.log, summary.txt, host.txt, run.txt, kernel-kills.txt, cleanup.log)."
	} >"$RES/summary.md"
}

finish() {
	trap - INT TERM
	summary
	say "results in ${RES#"$ROOT"/}/summary.md (exit $RC)"
	exit "$RC"
}
trap 'RC=130; STEPS+=("interrupted|FAIL|130|0"); finish' INT TERM

say "host $HOST, commit $COMMIT$LOCAL, results ${RES#"$ROOT"/}"
"$SCRIPTS/host-preflight.sh" "${LAYOUT[@]}" "$HOST" >"$RES/preflight.txt" 2>&1
RC=$?
cat "$RES/preflight.txt" >&2
STEPS+=("preflight|$([ "$RC" = 0 ] && echo pass || echo FAIL)|$RC|-")
case "$RC" in
0) ;;
1) RC=2; finish ;; # a host problem
*) finish ;;
esac

VT=("$SCRIPTS/vm-tier.sh")
if [ "$SKIP_IMAGES" = 1 ]; then
	skip images
else
	extra=()
	[ "$REBUILD" = 1 ] && extra=(--rebuild-images)
	step images "${VT[@]}" --images "${extra[@]}" "${LAYOUT[@]}" --out "$RES/images" "$HOST" || { RC=$?; finish; }
fi
step prepare "${VT[@]}" --prepare "${LAYOUT[@]}" --out "$RES/prepare" "$HOST" || { RC=$?; finish; }
step tier "${VT[@]}" "${PASS[@]}" "${LAYOUT[@]}" --out "$RES/tier" "$HOST" "$RE" || RC=$?
finish
