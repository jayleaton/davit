#!/usr/bin/env bash
# Lint every shell script under images/, packaging/ and scripts/ (tracked or
# new and not ignored, so an uncommitted script is linted too) with a pinned
# ShellCheck, fetched into .build/tools like the other dev tools. Any finding
# fails. The binary comes from, in order: .build/tools, a release tarball in
# $DAVIT_TOOLS_CACHE (default ~/.cache/davit-tools), or a download from
# the GitHub release; every tarball is checked against the pinned SHA-256.
# Only when none of those works (offline, no cache) does it skip, loudly.
set -euo pipefail

VERSION=v0.11.0
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEST="$ROOT/.build/tools/shellcheck-$VERSION"
SC="$DEST/shellcheck"
CACHE="${DAVIT_TOOLS_CACHE:-$HOME/.cache/davit-tools}"

case "$(uname -s).$(uname -m)" in
Linux.x86_64) ASSET=linux.x86_64 SUM=8c3be12b05d5c177a04c29e3c78ce89ac86f1595681cab149b65b97c4e227198 ;;
Linux.aarch64 | Linux.arm64) ASSET=linux.aarch64 SUM=12b331c1d2db6b9eb13cfca64306b1b157a86eb69db83023e261eaa7e7c14588 ;;
Darwin.arm64) ASSET=darwin.aarch64 SUM=56affdd8de5527894dca6dc3d7e0a99a873b0f004d7aabc30ae407d3f48b0a79 ;;
Darwin.x86_64) ASSET=darwin.x86_64 SUM=3c89db4edcab7cf1c27bff178882e0f6f27f7afdf54e859fa041fca10febe4c6 ;;
*) ASSET="" SUM="" ;;
esac

sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}

# unpack TARBALL: verify and unpack into $DEST; fails on a hash mismatch.
unpack() {
	[ -f "$1" ] && [ "$(sha256 "$1")" = "$SUM" ] || return 1
	mkdir -p "$DEST"
	tar -xJf "$1" -C "$DEST" --strip-components=1 "shellcheck-$VERSION/shellcheck"
}

warn() {
	printf '\n!!!!!!!!\n!!! WARNING: shell scripts NOT linted: %s\n!!!!!!!!\n\n' "$1" >&2
	exit 0
}

if [ ! -x "$SC" ]; then
	[ -n "$ASSET" ] || warn "no pinned ShellCheck $VERSION build for $(uname -s)/$(uname -m)"
	name="shellcheck-$VERSION.$ASSET.tar.xz"
	found=""
	# sc.tar.xz: an older cache name, still accepted.
	for t in "$CACHE/$name" "$CACHE/sc.tar.xz"; do
		if unpack "$t"; then found=1 && break; fi
	done
	if [ -z "$found" ]; then
		tmp="$(mktemp "${TMPDIR:-/tmp}/shellcheck.XXXXXX")"
		trap 'rm -f "$tmp"' EXIT
		if curl -fsSL --max-time 60 -o "$tmp" "https://github.com/koalaman/shellcheck/releases/download/$VERSION/$name"; then
			unpack "$tmp" || { echo "shellcheck: $name does not match the pinned SHA-256" >&2; exit 1; }
		else
			warn "ShellCheck $VERSION is not in .build/tools or $CACHE and could not be downloaded"
		fi
	fi
fi

cd "$ROOT"
files=()
while IFS= read -r f; do
	case "$f" in
	*.sh) files+=("$f") ;;
	*) head -n1 "$f" | grep -Eq '^#!.*(/|env )(ba|da)?sh( |$)' && files+=("$f") ;;
	esac
done < <(git ls-files --cached --others --exclude-standard images packaging scripts | sort -u)
"$SC" "${files[@]}"
echo "shellcheck $VERSION: ${#files[@]} scripts clean"
