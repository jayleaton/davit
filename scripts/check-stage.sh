#!/usr/bin/env bash
# check-stage.sh STAGE COMMAND...: run one `make check` stage unless it
# already passed on exactly this tree, and record a pass.
#
# A pass is a stamp .build/check-stamps/TREE.STAGE.stamp, where TREE is the git
# tree hash of HEAD. Stamps are used and written only for a clean work tree
# (nothing modified, staged, or untracked and not ignored): a dirty tree
# always runs the stage, so a stamp can never vouch for content it did not
# see. Any commit, amend or rebase changes TREE.
#
# The Go test stage may run in shards (CHECK_SHARD=I/N, stage go-IofN): a
# complete set of shard stamps for one N counts as a pass of "go".
#
# CHECK_FORCE=1 ignores existing stamps (the stage runs, and a pass is
# recorded again). `check-stage.sh --status` lists the stages that passed on
# the current tree.
set -uo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
stamps="$root/.build/check-stamps"

# The repository is the one this script lives in, never one an inherited
# variable points at.
git_() {
	env -u GIT_DIR -u GIT_WORK_TREE -u GIT_INDEX_FILE -u GIT_OBJECT_DIRECTORY \
		-u GIT_ALTERNATE_OBJECT_DIRECTORIES -u GIT_COMMON_DIR -u GIT_CEILING_DIRECTORIES \
		git -C "$root" "$@"
}

# tree_id prints HEAD's tree hash if the work tree is clean, else nothing.
tree_id() {
	local dirty
	dirty="$(git_ status --porcelain --untracked-files=all 2>/dev/null)" || return 0
	[ -z "$dirty" ] || return 0
	git_ rev-parse -q --verify 'HEAD^{tree}' 2>/dev/null || true
}

# stamp TREE STAGE prints the stamp path. The .stamp suffix keeps a stage
# name from reading as a source file ("TREE.go" was one, and gofmt parsed
# it). Stamps of the older suffix-less form are not read: those stages run
# once more, and the files are removed by the next stamp written.
stamp() { printf '%s/%s.%s.stamp' "$stamps" "$1" "$2"; }

# passed TREE STAGE: the stage has a pass on TREE.
passed() {
	local tree=$1 stage=$2 f n i
	[ -f "$(stamp "$tree" "$stage")" ] && return 0
	[ "$stage" = go ] || return 1
	for f in "$stamps/$tree".go-1of*.stamp; do
		[ -f "$f" ] || continue
		n=${f##*of}
		n=${n%.stamp}
		for ((i = 2; i <= n; i++)); do
			[ -f "$(stamp "$tree" "go-${i}of$n")" ] || continue 2
		done
		return 0
	done
	return 1
}

tree="$(tree_id)"

if [ "${1:-}" = --status ]; then
	if [ -z "$tree" ]; then
		echo "check: work tree is dirty; no stage counts as passed"
		exit 0
	fi
	for s in lint go cross sdk; do
		if passed "$tree" "$s"; then echo "check-$s: passed on tree ${tree:0:12}"; else echo "check-$s: not passed"; fi
	done
	exit 0
fi

stage=$1
shift

if [ -z "$tree" ]; then
	echo "check-$stage: work tree is dirty; running without a stamp"
	exec "$@"
fi
if [ "${CHECK_FORCE:-}" != 1 ] && passed "$tree" "$stage"; then
	echo "check-$stage: already passed on tree ${tree:0:12} (CHECK_FORCE=1 reruns it)"
	exit 0
fi

start=$(date +%s)
"$@"
rc=$?
[ "$rc" -eq 0 ] || exit "$rc"
secs=$(($(date +%s) - start))
# The tree must not have changed while the stage ran.
if [ "$(tree_id)" != "$tree" ]; then
	echo "check-$stage: passed, but the tree changed while it ran; no stamp"
	exit 0
fi
mkdir -p "$stamps"
# Old stamps are worthless once their tree is gone from view; keep a week.
find "$stamps" -type f \( -mtime +7 -o ! -name '*.stamp' \) -delete 2>/dev/null || true
printf 'stage=%s tree=%s secs=%s procs=%s at=%s\n' "$stage" "$tree" "$secs" \
	"${DAVIT_CHECK_PROCS:-}" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$(stamp "$tree" "$stage")"
echo "check-$stage: passed in ${secs}s on tree ${tree:0:12}"
