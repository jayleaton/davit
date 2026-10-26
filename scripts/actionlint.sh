#!/usr/bin/env bash
# Lint the GitHub Actions workflows (.github/workflows/*.yml) with a pinned
# actionlint and the repo's .github/actionlint.yaml. Any finding fails.
# Same scheme as scripts/shellcheck.sh: the binary comes from, in order,
# .build/tools, a release tarball in $DAVIT_TOOLS_CACHE (default
# ~/.cache/davit-tools), or a download from the GitHub release; every tarball
# is checked against the pinned SHA-256 (from the release's checksums file).
# Only when none of those works (offline, no cache) does it skip, loudly.
#
# actionlint runs ShellCheck over `run:` blocks: it is pointed at the pinned
# ShellCheck from scripts/shellcheck.sh, never whatever is on PATH, so the
# result does not depend on the machine. pyflakes is off for the same reason.
set -euo pipefail

VERSION=1.7.12
SC_VERSION=v0.11.0 # scripts/shellcheck.sh
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEST="$ROOT/.build/tools/actionlint-v$VERSION"
AL="$DEST/actionlint"
SC="$ROOT/.build/tools/shellcheck-$SC_VERSION/shellcheck"
CACHE="${DAVIT_TOOLS_CACHE:-$HOME/.cache/davit-tools}"

case "$(uname -s).$(uname -m)" in
Linux.x86_64) ASSET=linux_amd64 SUM=8aca8db96f1b94770f1b0d72b6dddcb1ebb8123cb3712530b08cc387b349a3d8 ;;
Linux.aarch64 | Linux.arm64) ASSET=linux_arm64 SUM=325e971b6ba9bfa504672e29be93c24981eeb1c07576d730e9f7c8805afff0c6 ;;
Darwin.arm64) ASSET=darwin_arm64 SUM=aba9ced2dee8d27fecca3dc7feb1a7f9a52caefa1eb46f3271ea66b6e0e6953f ;;
Darwin.x86_64) ASSET=darwin_amd64 SUM=5b44c3bc2255115c9b69e30efc0fecdf498fdb63c5d58e17084fd5f16324c644 ;;
*) ASSET="" SUM="" ;;
esac

sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}

# unpack TARBALL: verify and unpack into $DEST; fails on a hash mismatch.
unpack() {
	[ -f "$1" ] && [ "$(sha256 "$1")" = "$SUM" ] || return 1
	mkdir -p "$DEST"
	tar -xzf "$1" -C "$DEST" actionlint
}

warn() {
	printf '\n!!!!!!!!\n!!! WARNING: workflows NOT linted: %s\n!!!!!!!!\n\n' "$1" >&2
	exit 0
}

if [ ! -x "$AL" ]; then
	[ -n "$ASSET" ] || warn "no pinned actionlint $VERSION build for $(uname -s)/$(uname -m)"
	name="actionlint_${VERSION}_$ASSET.tar.gz"
	if ! unpack "$CACHE/$name"; then
		tmp="$(mktemp "${TMPDIR:-/tmp}/actionlint.XXXXXX")"
		trap 'rm -f "$tmp"' EXIT
		if curl -fsSL --max-time 60 -o "$tmp" "https://github.com/rhysd/actionlint/releases/download/v$VERSION/$name"; then
			unpack "$tmp" || { echo "actionlint: $name does not match the pinned SHA-256" >&2; exit 1; }
		else
			warn "actionlint $VERSION is not in .build/tools or $CACHE and could not be downloaded"
		fi
	fi
fi

# The pinned ShellCheck: scripts/shellcheck.sh fetches it (and lints the shell
# scripts, which check-lint does anyway). Without it (offline, no cache), the
# `run:` blocks go unchecked rather than checked by an unpinned copy.
if [ ! -x "$SC" ]; then
	"$ROOT/scripts/shellcheck.sh" >&2
fi
if [ -x "$SC" ]; then
	sc_flag="-shellcheck=$SC"
else
	printf '\n!!!!!!!!\n!!! WARNING: workflow run: blocks NOT shellchecked: no pinned ShellCheck %s\n!!!!!!!!\n\n' "$SC_VERSION" >&2
	sc_flag="-shellcheck="
fi

cd "$ROOT"
files=(.github/workflows/*.yml)
"$AL" -config-file .github/actionlint.yaml "$sc_flag" -pyflakes= "${files[@]}"
echo "actionlint $VERSION: ${#files[@]} workflows clean"
