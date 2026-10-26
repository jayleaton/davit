#!/bin/sh
# Project image setup for davit's own boxes (`davit box image build davit`
# runs this in the builder VM, in the synced tree, then snapshots it): the
# pinned Go toolchain through mise, the Go modules and build tools, and the
# Python and TypeScript SDK dev dependencies. Idempotent: `box image update`
# runs it again on top of the previous image.
# shellcheck disable=SC2016 # the lines added to ~/.profile expand there, not here
set -eu

export PATH="$HOME/.local/bin:$PATH"
if ! command -v mise >/dev/null 2>&1; then
	curl -fsSL https://mise.run | sh
fi
grep -q 'local/bin' "$HOME/.profile" 2>/dev/null || echo 'export PATH="$HOME/.local/bin:$PATH"' >>"$HOME/.profile"

mise trust -q mise.toml
mise install -q
mise use -g -q node@22 # the TypeScript SDK's toolchain (not pinned in mise.toml)
grep -q 'mise activate' "$HOME/.profile" || echo 'eval "$(mise activate bash --shims)"' >>"$HOME/.profile"

# Modules and the pinned staticcheck tool (go.mod `tool`), built into the cache.
mise exec -- go mod download
mise exec -- go build -o /dev/null honnef.co/go/tools/cmd/staticcheck
# Warm the build and vet caches (linux and darwin, as make check-lint does), so
# an instance's first check is incremental. Failures here are the tree's,
# not the image's: they only leave the caches colder.
mise exec -- go vet ./... || true
GOOS=darwin GOARCH=arm64 mise exec -- go vet ./... || true

# SDK dev dependencies (make sdk-setup: a venv under .build and node_modules).
if ! python3 -c 'import ensurepip' 2>/dev/null; then
	sudo -n apt-get update -qq
	sudo -n DEBIAN_FRONTEND=noninteractive apt-get install -y -qq python3-venv >/dev/null
fi
mise exec node@22 -- make -s sdk-setup
echo "davit project image ready: $(mise exec -- go version)"
