#!/usr/bin/env bash
# check-stage-test.sh: regression test for scripts/check-stage.sh and
# `make fmt-check`, run by `make check-lint` (stage-test). It works in a
# throwaway git repository under TMPDIR (never this one) holding a copy of
# check-stage.sh:
#   - a pass of every stage name, the unsharded "go" included, writes a
#     stamp with the .stamp suffix and never a source extension (a stamp
#     named TREE.go once made gofmt parse it);
#   - shard stamps of one N count as a "go" pass; old suffix-less stamps do
#     not count and are removed by the next stamp;
#   - a dirty tree neither uses nor writes stamps;
#   - `make fmt-check` stays clean after an unsharded go pass, even with a
#     broken .go file under .build/, and still fails on real source.
set -euo pipefail
# The stamps are under test: a CHECK_FORCE from the caller (CI sets it) would
# make every stamped stage rerun.
unset CHECK_FORCE

root="$(cd "$(dirname "$0")/.." && pwd)"
if ! gofmt_bin="$(cd "$root" && mise which gofmt 2>/dev/null)"; then
	gofmt_bin="$(command -v gofmt)"
fi
tmp="$(mktemp -d "${TMPDIR:-/tmp}/check-stage-test.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

fail() {
	echo "check-stage-test: FAIL: $*" >&2
	exit 1
}

# A hermetic git: no user config, nothing inherited that locates a repository.
git_() {
	env -u GIT_DIR -u GIT_WORK_TREE -u GIT_INDEX_FILE -u GIT_OBJECT_DIRECTORY \
		-u GIT_ALTERNATE_OBJECT_DIRECTORIES -u GIT_COMMON_DIR \
		GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_CEILING_DIRECTORIES="$tmp" \
		GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com \
		git -C "$repo" -c init.defaultBranch=main -c maintenance.auto=false -c gc.auto=0 -c commit.gpgSign=false "$@"
}

repo="$tmp/repo"
mkdir -p "$repo/scripts"
cp "$root/scripts/check-stage.sh" "$repo/scripts/"
printf 'package x\n' >"$repo/x.go"
printf '/.build/\n' >"$repo/.gitignore"
git_ init -q
git_ add -A
git_ commit -q -m init
stage="$repo/scripts/check-stage.sh"
stamps="$repo/.build/check-stamps"

# Every stage name, unsharded go included.
for s in lint go go-1of2 go-2of2 cross sdk; do
	"$stage" "$s" true >/dev/null || fail "stage $s did not pass"
done
n=0
for f in "$stamps"/*; do
	n=$((n + 1))
	case "$f" in
	*.stamp) ;;
	*) fail "stamp $f has no .stamp suffix" ;;
	esac
	# Tools pick files by their last extension.
	case "${f##*.}" in
	go | ts | tsx | js | mjs | cjs | py | sh | json | yaml) fail "stamp $f reads as a source file" ;;
	esac
done
[ "$n" -eq 6 ] || fail "want 6 stamps, have $n"
"$stage" go false >/dev/null || fail "a stamped stage ran again"
tree="$(git_ rev-parse 'HEAD^{tree}')"

# Shards alone count as a go pass.
rm "$stamps/$tree.go.stamp"
gostatus() { "$stage" --status | grep '^check-go:'; } # whole output read: no SIGPIPE under pipefail
[ "$(gostatus)" = "check-go: passed on tree ${tree:0:12}" ] || fail "go shard stamps do not count: $(gostatus)"
rm "$stamps/$tree.go-2of2.stamp"
[ "$(gostatus)" = "check-go: not passed" ] || fail "an incomplete shard set counts"

# Old suffix-less stamps are not read and are removed by the next stamp.
: >"$stamps/$tree.cross-old"
: >"$stamps/$tree.go"
[ "$(gostatus)" = "check-go: not passed" ] || fail "an old-format go stamp counts"
"$stage" go true >/dev/null
[ ! -e "$stamps/$tree.go" ] && [ ! -e "$stamps/$tree.cross-old" ] || fail "old-format stamps not removed"

# A dirty tree never uses or writes stamps.
printf '// dirty\n' >>"$repo/x.go"
before="$(ls "$stamps")"
"$stage" lint false >/dev/null && fail "a dirty tree used a stamp"
"$stage" probe true >/dev/null
[ "$(ls "$stamps")" = "$before" ] || fail "a dirty tree wrote a stamp"
git_ checkout -q x.go

# fmt-check ignores .build (stamps and anything else) but not source.
printf 'this is not Go\n' >"$repo/.build/broken.go"
out="$(make --no-print-directory -s -C "$repo" -f "$root/Makefile" fmt-check GOFMT="$gofmt_bin" 2>&1)" ||
	fail "fmt-check failed after a go pass: $out"
[ -z "$out" ] || fail "fmt-check output is not clean: $out"
printf 'package x\nfunc  f() {}\n' >"$repo/y.go"
make --no-print-directory -s -C "$repo" -f "$root/Makefile" fmt-check GOFMT="$gofmt_bin" >/dev/null 2>&1 &&
	fail "fmt-check passed an unformatted source file"
printf 'not Go either\n' >"$repo/y.go"
make --no-print-directory -s -C "$repo" -f "$root/Makefile" fmt-check GOFMT="$gofmt_bin" >/dev/null 2>&1 &&
	fail "fmt-check passed a source file gofmt cannot parse"

echo "check-stage-test: ok"
