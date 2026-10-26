#!/usr/bin/env bash
# First-command latency from a remote client over the tailnet gateway (G2).
#
# Run on the laptop or CI runner, not on the host. Uses only the shared remote
# configuration: DAVIT_API_URL (https://<gateway>), optional DAVIT_TOKEN
# and DAVIT_CA_FILE. Each sample creates a VM from the cached golden image,
# times its first command, times one more (warm) command, and deletes it.
#
#   scripts/g2-first-command.sh [-n SAMPLES] [-b DAVIT_BIN] [-o OUT.tsv]
#
# Prints p50/p95 for: new (create, accepted to ready), first exec after new,
# new+first exec together (the SPEC <1s p50 / <2s p95 target), and warm exec.
# Raw samples go to OUT.tsv (default ./g2-first-command-<UTC time>.tsv).
set -euo pipefail

n=20
davit=davit
out=""
while getopts "n:b:o:h" opt; do
	case $opt in
	n) n=$OPTARG ;;
	b) davit=$OPTARG ;;
	o) out=$OPTARG ;;
	*)
		sed -n '2,13p' "$0"
		exit 2
		;;
	esac
done
out=${out:-g2-first-command-$(date -u +%Y%m%dT%H%M%SZ).tsv}

: "${DAVIT_API_URL:?set DAVIT_API_URL=https://<gateway MagicDNS name>}"
case $DAVIT_API_URL in
https://*) ;;
*) echo "DAVIT_API_URL must be https:// for this run" >&2 && exit 2 ;;
esac

# Milliseconds since the epoch; macOS date has no %N, so fall back to perl.
if [[ $(date +%s%3N) =~ ^[0-9]+$ ]]; then
	now_ms() { date +%s%3N; }
else
	now_ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time() * 1000'; }
fi

# pct FILE COLUMN P: nearest-rank percentile of a TSV column (header skipped).
pct() {
	tail -n +2 "$1" | cut -f "$2" | sort -n | awk -v p="$3" '
		{ v[NR] = $1 }
		END { if (NR == 0) { print "-"; exit } r = int((p / 100) * NR + 0.999999); if (r < 1) r = 1; print v[r] }'
}

echo "== client"
uname -srm
{ command -v lscpu >/dev/null && lscpu | grep -m1 'Model name'; } || sysctl -n machdep.cpu.brand_string 2>/dev/null || true
"$davit" --version 2>/dev/null || true
echo "endpoint $DAVIT_API_URL token=${DAVIT_TOKEN:+yes} ca_file=${DAVIT_CA_FILE:-none}"
echo "== status"
"$davit" status
echo "== host"
"$davit" capabilities | head -20

printf 'sample\tnew_ms\tfirst_exec_ms\tnew_plus_first_ms\twarm_exec_ms\n' >"$out"
for i in $(seq 1 "$n"); do
	vm="g2-lat-$$-$i"
	t0=$(now_ms)
	"$davit" new "$vm" >/dev/null
	t1=$(now_ms)
	"$davit" exec "$vm" -- true
	t2=$(now_ms)
	"$davit" exec "$vm" -- true
	t3=$(now_ms)
	"$davit" delete "$vm" --yes >/dev/null
	printf '%d\t%d\t%d\t%d\t%d\n' "$i" $((t1 - t0)) $((t2 - t1)) $((t2 - t0)) $((t3 - t2)) | tee -a "$out"
done

echo "== $n samples ($out), milliseconds, nearest-rank"
printf '%-18s %8s %8s\n' metric p50 p95
col=2
for m in new first_exec new_plus_first warm_exec; do
	printf '%-18s %8s %8s\n' "$m" "$(pct "$out" $col 50)" "$(pct "$out" $col 95)"
	col=$((col + 1))
done
