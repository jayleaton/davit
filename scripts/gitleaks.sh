#!/usr/bin/env bash
# Secret scan with a pinned gitleaks and the repo's .gitleaks.toml, fetched
# into .build/tools like the other dev tools (from .build/tools, a release
# tarball in $DAVIT_TOOLS_CACHE (default ~/.cache/davit-tools), or the GitHub
# release; every tarball is checked against the pinned SHA-256).
#
#   scripts/gitleaks.sh             the tree: tracked files plus new, not ignored ones
#   scripts/gitleaks.sh --history   every commit reachable from any ref
#
# Any finding fails. Offline with no cached copy it skips loudly, except under
# CI (CI set), where a scanner that cannot run fails.
set -euo pipefail

VERSION=8.30.1
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEST="$ROOT/.build/tools/gitleaks-$VERSION"
GL="$DEST/gitleaks"
CACHE="${DAVIT_TOOLS_CACHE:-$HOME/.cache/davit-tools}"

case "$(uname -s).$(uname -m)" in
Linux.x86_64) ASSET=linux_x64 SUM=551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb ;;
Linux.aarch64 | Linux.arm64) ASSET=linux_arm64 SUM=e4a487ee7ccd7d3a7f7ec08657610aa3606637dab924210b3aee62570fb4b080 ;;
Darwin.arm64) ASSET=darwin_arm64 SUM=b40ab0ae55c505963e365f271a8d3846efbc170aa17f2607f13df610a9aeb6a5 ;;
Darwin.x86_64) ASSET=darwin_x64 SUM=dfe101a4db2255fc85120ac7f3d25e4342c3c20cf749f2c20a18081af1952709 ;;
*) ASSET="" SUM="" ;;
esac

sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}

# unpack TARBALL: verify and unpack into $DEST; fails on a hash mismatch.
unpack() {
	[ -f "$1" ] && [ "$(sha256 "$1")" = "$SUM" ] || return 1
	mkdir -p "$DEST"
	tar -xzf "$1" -C "$DEST" gitleaks
}

warn() {
	if [ -n "${CI:-}" ]; then
		echo "gitleaks: $1" >&2
		exit 1
	fi
	printf '\n!!!!!!!!\n!!! WARNING: secrets NOT scanned: %s\n!!!!!!!!\n\n' "$1" >&2
	exit 0
}

if [ ! -x "$GL" ]; then
	[ -n "$ASSET" ] || warn "no pinned gitleaks $VERSION build for $(uname -s)/$(uname -m)"
	name="gitleaks_${VERSION}_$ASSET.tar.gz"
	if ! unpack "$CACHE/$name"; then
		tmp="$(mktemp "${TMPDIR:-/tmp}/gitleaks.XXXXXX")"
		trap 'rm -f "$tmp"' EXIT
		if curl -fsSL --retry 3 --max-time 60 -o "$tmp" "https://github.com/gitleaks/gitleaks/releases/download/v$VERSION/$name"; then
			unpack "$tmp" || { echo "gitleaks: $name does not match the pinned SHA-256" >&2; exit 1; }
		else
			warn "gitleaks $VERSION is not in .build/tools or $CACHE and could not be downloaded"
		fi
	fi
fi

cd "$ROOT"
flags=(--config "$ROOT/.gitleaks.toml" --ignore-gitleaks-allow --redact=100 --no-banner --exit-code 1)
case "${1:-}" in
--history)
	"$GL" git "${flags[@]}" --log-opts='--all --full-history -m' .
	echo "gitleaks $VERSION: history clean ($(git rev-list --all --count) commits)"
	;;
"")
	# Only the files git would commit: no build output, caches or node_modules.
	tree="$(mktemp -d "${TMPDIR:-/tmp}/gitleaks-tree.XXXXXX")"
	trap 'rm -rf "$tree"' EXIT
	git ls-files -z --cached --others --exclude-standard | while IFS= read -r -d '' f; do
		[ -f "$f" ] && [ ! -L "$f" ] && printf '%s\0' "$f"
	done | tar --null -T - -cf - | tar -xf - -C "$tree"
	(cd "$tree" && "$GL" dir "${flags[@]}" .)
	echo "gitleaks $VERSION: tree clean"
	;;
*)
	echo "usage: scripts/gitleaks.sh [--history]" >&2
	exit 2
	;;
esac
