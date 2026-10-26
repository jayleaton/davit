# davit build and checks. `make check` is the single gate before every
# commit/PR: format, vet and staticcheck (linux + darwin), shellcheck, tests,
# static build.
# Go comes from mise.toml; override with `make GO=go` if go is already pinned.

GO      ?= mise exec -- go
GOFMT   ?= mise exec -- gofmt
# Optional command prefix for heavy steps (a job lock or a nice/ionice
# wrapper); empty runs them directly. See AGENTS.md.
HEAVY   ?= $(DAVIT_HEAVY)
BIN     := bin/davit
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null)
LDFLAGS := -s -w -X davit/internal/buildinfo.Version=$(VERSION) -X davit/internal/buildinfo.Commit=$(COMMIT)
# Go's build scratch lives in the repo (git-ignored), not /tmp: building
# tailscale.com (especially with -race) outgrows small or quota'd tmpfs /tmp.
export GOTMPDIR := $(CURDIR)/.build/gotmp
# Since Go 1.26 t.TempDir also follows GOTMPDIR. Test binaries run without it
# so their temp dirs stay under TMPDIR: short enough for Unix sockets (108-byte
# sun_path) and cleaned like any other test temp data.
TESTEXEC := -exec 'env -u GOTMPDIR'
# Build/test parallelism. Compiling tailscale.com and gVisor several packages
# at a time can exhaust memory on a loaded box, and the OS then kills the
# compiler or a test binary. Raise it on an idle machine: make check
# DAVIT_CHECK_PROCS=8. -p caps concurrent package builds and test binaries;
# -parallel caps t.Parallel tests inside one binary.
DAVIT_CHECK_PROCS ?= 2
export DAVIT_CHECK_PROCS
export GOFLAGS += -p=$(DAVIT_CHECK_PROCS)
TESTFLAGS := $(TESTEXEC) -count=1 -parallel=$(DAVIT_CHECK_PROCS)
# Each Go step runs under scripts/gate.sh, which reports a step the OS killed
# (memory pressure) as such instead of a bare FAIL. It never retries.
GATE := scripts/gate.sh

.PHONY: check check-lint check-go check-cross check-sdk check-status fmt-check vet lint shellcheck actionlint secretscan stage-test linkcheck test race build cross clean release sdk-setup sdk-check sdk-py sdk-ts vm-tier host-preflight host-install host-uninstall client-install

# `make check` is four stages, each runnable alone (one call per stage keeps
# every call well under an agent's tool timeout; see AGENTS.md):
#   check-lint   gofmt, vet and staticcheck (linux + darwin), shellcheck,
#                actionlint (GitHub workflows), gitleaks (tree), Markdown links
#   check-go     go test (CHECK_SHARD=I/N runs the I-th of N package shards)
#   check-cross  CGO-free builds for linux/amd64, linux/arm64, darwin/arm64
#   check-sdk    Python and TypeScript SDK checks and suites
# A stage that passes on a clean tree leaves a stamp keyed by the tree hash
# (scripts/check-stage.sh), so `make check` after all four passed on the same
# commit only confirms the stamps. A dirty tree never uses or writes stamps.
# CHECK_FORCE=1 reruns stages that already passed.
STAGE := scripts/check-stage.sh
SUBMAKE := $(MAKE) --no-print-directory

check: check-lint check-go check-cross check-sdk

check-lint:
	@$(STAGE) lint $(SUBMAKE) fmt-check vet lint shellcheck actionlint secretscan stage-test linkcheck

CHECK_SHARD ?=
GO_STAGE := go$(if $(CHECK_SHARD),-$(subst /,of,$(CHECK_SHARD)))
check-go:
	@$(STAGE) $(GO_STAGE) $(SUBMAKE) test

check-cross:
	@$(STAGE) cross $(SUBMAKE) cross

check-sdk:
	@$(STAGE) sdk $(SUBMAKE) sdk-check

check-status:
	@$(STAGE) --status

# Generated and cached trees (.build: stamps, logs, tools, venv; node_modules)
# are not source. `go vet` and staticcheck skip them already: `./...` never
# enters directories starting with '.' or '_' or named testdata.
GO_SOURCES = find . \( -name .build -o -name node_modules -o -name .git \) -prune -o -name '*.go' -type f -print
fmt-check:
	@out="$$($(GO_SOURCES) | xargs -r $(GOFMT) -l)" || { echo "gofmt failed (see above)"; exit 1; }; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet: | $(GOTMPDIR)
	$(GATE) vet $(GO) vet ./...
	GOOS=darwin GOARCH=arm64 $(GATE) vet-darwin $(GO) vet ./...

# staticcheck is pinned as a `tool` in go.mod (no system install). Darwin-
# tagged code is linted too: `go tool` would build staticcheck itself for
# GOOS=darwin, so the pinned version is built for this host into .build/tools
# and pointed at darwin/arm64 (it runs `go list`, hence the $(GO) prefix).
STATICCHECK_HOST := .build/tools/staticcheck-host
lint: | $(GOTMPDIR)
	GOMAXPROCS=$(DAVIT_CHECK_PROCS) $(GATE) lint $(GO) tool staticcheck ./...
	@mkdir -p $(dir $(STATICCHECK_HOST))
	GOOS= GOARCH= $(GATE) lint-darwin-build $(GO) build -o $(STATICCHECK_HOST) honnef.co/go/tools/cmd/staticcheck
	GOOS=darwin GOARCH=arm64 GOMAXPROCS=$(DAVIT_CHECK_PROCS) $(GATE) lint-darwin $(GO:go=) $(STATICCHECK_HOST) ./...

# Shell scripts (images/, packaging/, scripts/) under a pinned ShellCheck
# fetched into .build/tools; skips with a loud warning only when offline
# with no cached copy.
shellcheck:
	scripts/shellcheck.sh

# Relative links and #anchors in every Markdown file (internal/tools/linkcheck).
linkcheck: | $(GOTMPDIR)
	$(GO) run ./internal/tools/linkcheck

# GitHub workflows (.github/workflows/*.yml) under a pinned actionlint, which
# checks run: blocks with the pinned ShellCheck above; same fetch and skip rules.
actionlint:
	scripts/actionlint.sh

# Secrets in the tree with a pinned gitleaks and .gitleaks.toml (CI also scans
# the full history: scripts/gitleaks.sh --history).
secretscan:
	scripts/gitleaks.sh

# The stage stamps and fmt-check, in a throwaway repository under TMPDIR.
stage-test:
	scripts/check-stage-test.sh

# Tests build the binary and run daemons; they run under $(HEAVY) when set.
# CHECK_SHARD=I/N tests only the I-th of N package shards
# (scripts/check-shard.sh: deterministic, balanced by rough test time).
test: | $(GOTMPDIR)
	@pkgs=./...; \
	if [ -n "$(CHECK_SHARD)" ]; then pkgs="$$($(GO) list ./... | scripts/check-shard.sh $(CHECK_SHARD))" || exit 1; fi; \
	set -x; $(GATE) test$(if $(CHECK_SHARD),-$(subst /,of,$(CHECK_SHARD))) $(HEAVY) $(GO) test $(TESTFLAGS) $$pkgs

# Race detector run (not part of check: it rebuilds every dependency).
race: | $(GOTMPDIR)
	$(GATE) race $(HEAVY) $(GO) test $(TESTFLAGS) -race ./...

build: | $(GOTMPDIR)
	CGO_ENABLED=0 $(GATE) build $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/davit

# The host binary must stay CGO-free and build for Linux and Apple silicon.
cross: | $(GOTMPDIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GATE) cross $(GO) build -o /dev/null ./cmd/davit
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GATE) cross $(GO) build -o /dev/null ./cmd/davit
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GATE) cross $(GO) build -o /dev/null ./cmd/davit

# The real-VM tier on a separate VM host, in one command (docs/vm-host.md):
# preflight, image build on the host, prepare, tests; results in
# results/vm-tier/<date>/. The recipe reads HOST and TESTS from the
# environment. Pass a regex as an environment variable, not a make argument:
# make expands `$` in arguments (TESTS='A$|B' would arrive as AB).
#   TESTS='TestM1Acceptance$' make vm-tier HOST=user@host [VM_TIER_FLAGS='--bench --fs /data/svm']
#   make host-preflight HOST=user@host     read-only host checks only
vm-tier:
	@[ -n "$$HOST" ] || { echo "usage: [TESTS=regex] make vm-tier HOST=user@host [VM_TIER_FLAGS=...]" >&2; exit 2; }
	scripts/host-tier.sh $(VM_TIER_FLAGS) "$$HOST" "$${TESTS:-.}"

host-preflight:
	@[ -n "$$HOST" ] || { echo "usage: make host-preflight HOST=user@host" >&2; exit 2; }
	scripts/host-preflight.sh "$$HOST"

# Install davit on a VM host as a systemd --user service that survives reboots, with the
# images built on the host (docs/vm-host.md). Idempotent: rerun to upgrade.
#   make host-install HOST=user@host [HOST_INSTALL_FLAGS='--image ubuntu-24.04-dev --data DIR']
#   make host-uninstall HOST=user@host [HOST_INSTALL_FLAGS=--purge]
host-install:
	@[ -n "$$HOST" ] || { echo "usage: make host-install HOST=user@host [HOST_INSTALL_FLAGS=...]" >&2; exit 2; }
	scripts/host-install.sh $(HOST_INSTALL_FLAGS) "$$HOST"

host-uninstall:
	@[ -n "$$HOST" ] || { echo "usage: make host-uninstall HOST=user@host [HOST_INSTALL_FLAGS=--purge]" >&2; exit 2; }
	scripts/host-install.sh --uninstall $(HOST_INSTALL_FLAGS) "$$HOST"

# Install the CLI for this user (no sudo) and, with HOST, make that host the default, so
# `davit box ...` works from any directory with no flags or environment (docs/project-boxes.md):
#   make client-install [HOST=user@host] [CLIENT_BIN_DIR=~/.local/bin]
# HOST is reached over ssh:// (key auth); pass a full URL (tailnet name, https://...) to use another.
CLIENT_BIN_DIR ?= $(HOME)/.local/bin
client-install: build
	@mkdir -p "$(CLIENT_BIN_DIR)"
	cp $(BIN) "$(CLIENT_BIN_DIR)/.davit.new" && mv -f "$(CLIENT_BIN_DIR)/.davit.new" "$(CLIENT_BIN_DIR)/davit"
	@# The CLI from before the slipway -> davit rename (its client config moves on first use).
	@if [ -f "$(CLIENT_BIN_DIR)/slipway" ] && "$(CLIENT_BIN_DIR)/slipway" version 2>/dev/null | grep -q '^slipway '; then \
		rm -f "$(CLIENT_BIN_DIR)/slipway"; echo "removed the old $(CLIENT_BIN_DIR)/slipway"; \
	fi
	@if [ -n "$$HOST" ]; then \
		case "$$HOST" in *://*) h="$$HOST" ;; *) h="ssh://$$HOST" ;; esac; \
		"$(CLIENT_BIN_DIR)/davit" client-config --set-host "$$h"; \
	fi
	@case ":$$PATH:" in *":$(CLIENT_BIN_DIR):"*) ;; *) echo "note: add $(CLIENT_BIN_DIR) to PATH" ;; esac

# Local release artifacts in .build/release/ (binaries, image bundles, install.sh,
# SHA256SUMS); see packaging/release.sh. Needs built images (images/build.sh).
# Not part of check: it compresses GiBs of image bundles.
release: | $(GOTMPDIR)
	$(HEAVY) packaging/release.sh

$(GOTMPDIR):
	@mkdir -p $@

# Client SDKs (sdk/python, sdk/typescript). Dev tools live in a repo-local
# venv and node_modules; the first run downloads pinned dev dependencies,
# later runs are offline. Both suites spawn the real daemon (fake runtime).
PY_VENV := .build/py-venv
PY      := $(PY_VENV)/bin/python

$(PY_VENV)/.stamp: sdk/python/pyproject.toml
	test -x $(PY) || python3 -m venv $(PY_VENV)
	$(PY) -m pip install -q --disable-pip-version-check -e 'sdk/python[dev]'
	touch $@

sdk/typescript/node_modules/.stamp: sdk/typescript/package-lock.json
	cd sdk/typescript && npm ci --no-audit --no-fund --silent
	touch $@

sdk-setup: $(PY_VENV)/.stamp sdk/typescript/node_modules/.stamp

sdk-check: sdk-py sdk-ts

sdk-py: $(PY_VENV)/.stamp build
	$(PY) sdk/python/scripts/gen_models.py --check
	$(PY_VENV)/bin/ruff check sdk/python
	$(PY_VENV)/bin/ruff format --check sdk/python
	$(PY_VENV)/bin/mypy --strict sdk/python/src
	$(PY_VENV)/bin/mypy --config-file sdk/python/pyproject.toml sdk/python/src
	DAVIT_BIN=$(CURDIR)/$(BIN) $(PY) -m pytest -q sdk/python/tests

# Test files run DAVIT_CHECK_PROCS at a time (sdk/typescript/test/run.ts):
# each is a process and the integration files start daemons.
sdk-ts: sdk/typescript/node_modules/.stamp build
	cd sdk/typescript && npm run --silent typecheck
	cd sdk/typescript && DAVIT_BIN=$(CURDIR)/$(BIN) DAVIT_TEST_CONCURRENCY=$(DAVIT_CHECK_PROCS) npm test --silent

clean:
	rm -rf bin .build sdk/typescript/node_modules sdk/typescript/dist
