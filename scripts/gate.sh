#!/usr/bin/env bash
# gate.sh STEP COMMAND...: run one `make check` step and, if it fails, tell an
# OS kill (OOM killer, cgroup MemoryMax) apart from a real failure. A killed
# compiler or test binary shows up in Go's output only as "signal: killed" next
# to a bare FAIL / [build failed]; this names those packages and says so.
# The step's exit status is kept: nothing is retried. Output is also kept in
# .build/check-logs/STEP.log.
set -uo pipefail

step=$1
shift
root="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$root/.build/check-logs"
log="$root/.build/check-logs/$step.log"

"$@" 2>&1 | tee "$log"
rc=${PIPESTATUS[0]}
[ "$rc" -eq 0 ] && exit 0

# Classify the failures. `go test` prints each failing test as "--- FAIL: T"
# followed by its indented log, then the package's "FAIL<TAB>pkg" line. A
# killed compiler or vet names its package ("pkg[.test]: .../compile: signal:
# killed" or "# pkg"); a killed test binary prints a bare "signal: killed".
# A test whose own log shows a SIGKILLed process (the e2e harness reports
# the daemon or CLI that way) failed because of the kill, not on its own.
# Output: "K pkg" (killed), "R pkg test" (real failure), "D pkg test" (real
# failure that ran into a deadline or timeout).
report="$(awk '
	function flush() {
		if (name != "") { n++; bname[n] = name; bkill[n] = bk; bdead[n] = bd }
		name = ""; bk = 0; bd = 0
	}
	function settle(pkg,   i) {
		for (i = first; i <= n; i++) bpkg[i] = pkg
		first = n + 1
	}
	BEGIN { first = 1; killre = "signal: killed|fatal error: (runtime: )?out of memory|cannot allocate memory" }
	/^# / { flush(); cur = $2; next }
	/^ *--- FAIL: / { flush(); name = $3; next }
	/^[ \t]/ && name != "" {
		if ($0 ~ killre) bk = 1
		if ($0 ~ /deadline exceeded|timed out|i\/o timeout/) bd = 1
		next
	}
	$0 ~ killre {
		flush()
		if ($1 != "signal:" && match($0, /^[^ :\t]+: /)) {
			p = substr($0, 1, RLENGTH - 2)
			sub(/\.test$/, "", p)
			print "K " p
		} else if (cur != "") print "K " cur
		else pkgkill = 1
		next
	}
	/^panic: / { flush(); n++; bname[n] = "panic"; bkill[n] = 0; bdead[n] = ($0 ~ /timed out/); next }
	/^(ok|FAIL|PASS|\?)[ \t]|^(FAIL|PASS)$/ {
		flush()
		if ($1 == "FAIL" && NF > 1) {
			settle($2)
			if (pkgkill) print "K " $2
		}
		pkgkill = 0; cur = ""
		next
	}
	/^(=== |--- )/ { flush() }
	END {
		flush(); settle("?")
		for (i = 1; i <= n; i++) {
			# A parent test fails because a subtest did; report the subtest.
			parent = 0
			for (j = 1; j <= n; j++) if (index(bname[j], bname[i] "/") == 1) parent = 1
			if (parent) continue
			if (bkill[i]) print "K " bpkg[i] " (" bname[i] ")"
			else print (bdead[i] ? "D " : "R ") bpkg[i] " " bname[i]
		}
	}
' "$log" | sort -u)"
# prefix P: print each stdin line after P.
prefix() { while IFS= read -r l; do printf '%s%s\n' "$1" "$l"; done; }

killed="$(sed -n 's/^K //p' <<<"$report")"
real="$(sed -n 's/^[RD] //p' <<<"$report")"
deadline="$(sed -n 's/^D //p' <<<"$report")"

if [ "$rc" -gt 128 ] || [ -n "$killed" ]; then
	{
		echo
		if [ -n "$real" ]; then
			echo "!!! make check: step '$step': some failures were KILLED BY THE OS (memory pressure), others are real."
		else
			echo "!!! make check: step '$step' was KILLED BY THE OS (memory pressure) — not a test failure."
		fi
		[ "$rc" -gt 128 ] && echo "!!!   the whole step died by signal $((rc - 128)) (exit $rc)."
		[ -n "$killed" ] && prefix '!!!   killed: ' <<<"$killed"
		if [ -n "$real" ]; then
			echo "!!!   but these are real failures too:"
			prefix '!!!     ' <<<"$real"
		fi
		echo "!!!   free memory or lower DAVIT_CHECK_PROCS (now ${DAVIT_CHECK_PROCS:-unset}), then rerun. Log: $log"
	} >&2
fi

# A deadline that fires on an overloaded host is a hint, not a verdict: the
# exit status stays.
if [ -n "$deadline" ]; then
	cpus="$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 1)"
	load="$(cut -d' ' -f1 /proc/loadavg 2>/dev/null || sysctl -n vm.loadavg 2>/dev/null | tr -d '{}' | awk '{print $1}')"
	if [ -n "$load" ] && awk -v l="$load" -v c="$cpus" 'BEGIN { exit !(l > 2 * c) }'; then
		{
			echo
			echo "!!! make check: possibly load-related: these failed on a deadline or timeout while the"
			echo "!!!   1-minute load average was $load (> 2 x $cpus CPUs):"
			prefix '!!!     ' <<<"$deadline"
			echo "!!!   rerun them alone (go test -run ...) before treating them as regressions. Log: $log"
		} >&2
	fi
fi
exit "$rc"
