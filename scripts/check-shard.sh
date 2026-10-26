#!/usr/bin/env bash
# check-shard.sh I/N: read Go package paths (`go list ./...`) on stdin and
# print the I-th of N shards, for `make check-go CHECK_SHARD=I/N`.
#
# The shards are a deterministic function of the package list and the
# weights below (both part of the tree), so on one tree the N shards cover
# every package exactly once. Packages are placed heaviest first on the
# lightest shard. Weights are rough `go test` seconds at
# DAVIT_CHECK_PROCS=1 (measured 2026-10-22 on a Ryzen 7 6800U laptop); an unlisted
# package weighs 1. Stale weights only unbalance the shards, never drop or
# repeat a package.
set -euo pipefail

if [[ $# -ne 1 || ! $1 =~ ^([1-9][0-9]*)/([1-9][0-9]*)$ ]] || ((BASH_REMATCH[1] > BASH_REMATCH[2])); then
	echo "usage: check-shard.sh I/N (1 <= I <= N), package list on stdin" >&2
	exit 64
fi
shard=${BASH_REMATCH[1]} shards=${BASH_REMATCH[2]}

awk -v shard="$shard" -v shards="$shards" '
	BEGIN {
		w["davit/internal/network"] = 90
		w["davit/cmd/davit"] = 85
		w["davit/internal/lifecycle"] = 45
		w["davit/internal/harness"] = 27
		w["davit/internal/guestexec"] = 15
		w["davit/internal/ext4"] = 13
		w["davit/internal/guestagent"] = 12
		w["davit/internal/webhooks"] = 10
		w["davit/internal/runs"] = 8
		w["davit/internal/daemon"] = 7
		w["davit/internal/cli"] = 5
		w["davit/internal/runtime/firecracker"] = 5
		w["davit/internal/storage"] = 3
	}
	NF { n++; pkg[n] = $1; wt[n] = ($1 in w) ? w[$1] : 1 }
	END {
		if (n == 0) { print "check-shard.sh: no packages on stdin" > "/dev/stderr"; exit 1 }
		# Heaviest first, ties by path: a stable order.
		for (i = 1; i <= n; i++) ord[i] = i
		for (i = 2; i <= n; i++) {
			k = ord[i]
			for (j = i - 1; j >= 1 && (wt[ord[j]] < wt[k] || wt[ord[j]] == wt[k] && pkg[ord[j]] > pkg[k]); j--) ord[j + 1] = ord[j]
			ord[j + 1] = k
		}
		for (s = 1; s <= shards; s++) load[s] = 0
		for (i = 1; i <= n; i++) {
			k = ord[i]; best = 1
			for (s = 2; s <= shards; s++) if (load[s] < load[best]) best = s
			load[best] += wt[k]
			if (best == shard) { print pkg[k]; got++ }
		}
		if (!got) { print "check-shard.sh: shard " shard "/" shards " is empty" > "/dev/stderr"; exit 1 }
	}
'
