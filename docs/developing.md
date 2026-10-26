# Developing davit

The rules for contributors and coding agents (domain boundaries, shared contracts, what may run where) are in
[AGENTS.md](../AGENTS.md). The code layout is in [architecture.md](architecture.md). Go is pinned in
`mise.toml`; run Go through `mise exec -- go ...`.

```sh
make check        # gofmt, vet (linux+darwin), staticcheck, shellcheck, link check, tests, CGO-free cross builds, SDK checks
make sdk-check    # SDKs only: regen drift, ruff, mypy --strict, pytest, tsc, node --test (real daemon)
make build        # bin/davit (static; version and commit stamped)
make release      # local release artifacts in .build/release (needs images/build.sh first)
scripts/fresh-host-e2e.sh   # first-run UX in a throwaway HOME, fake runtime (real VMs: vm-tier.sh --fresh-host)
```

`make check` is four stages (`check-lint`, `check-go`, `check-cross`, `check-sdk`), each runnable alone, and a
stage that passed on a clean tree is not run again for that tree. AGENTS.md has the details and timings.

`DAVIT_HEAVY` is an optional command prefix for the heavy steps (`make`'s `go test`, `images/build.sh`,
`images/smoke.sh`, `scripts/host-install.sh`, `make release`), for example a job lock or
`nice -n 15 ionice -c 3`. Unset, they run directly.

The link check runs alone with `go run ./internal/tools/linkcheck`. It checks every relative link and
`#anchor` in the repository's Markdown files, except the historical archives under `docs/research`.

## The fake runtime and the real-VM tier

The fake runtime runs the whole API without VMs (`"runtime": "fake"`; `"fake": {"guestAgent": true}` makes
`exec`/`cp`/`prompt` run on this host as the daemon user, for development only). The real-VM test tier is
opt-in so `make check` stays hermetic, and runs on a separate KVM host over SSH
([vm-tier.md](vm-tier.md), [vm-host.md](vm-host.md)):

```sh
scripts/vm-tier.sh --prepare user@host                      # once: Go, Firecracker, image blobs, fixture
scripts/vm-tier.sh user@host ['TestM1Acceptance$']          # the tier (or a -run regex); --bench, --fresh-host
```

## Building a release from a checkout

Users install with the host installer ([install-host.md](install-host.md)). To test a release before it is
published, build the artifacts once from a checkout (Go from `mise.toml`; the image build is rootless and needs
the tools in [images/README.md](../images/README.md#root-requirements-none)):

```sh
IMAGES="ubuntu-24.04-base ubuntu-24.04-dev" images/build.sh   # rootless; about 27 min for base + harness on the reference dev machine (sample count not recorded)
make release        # .build/release/<version>/: binaries, image bundles, install.sh, release.json, SHA256SUMS
```

Then install from that directory exactly as a user would, with `--from` pointing at it ([install-host.md](install-host.md),
[packaging/README.md](../packaging/README.md)). Copy the directory to another machine to install there:

```sh
d=$PWD/.build/release/$(cat .build/release/latest)
sh $d/install.sh --host --from $d       # on a KVM host; --client on a laptop
```

`scripts/fresh-host-e2e.sh --release $d` runs that whole host install as a throwaway Linux user on a VM host,
then `davit doctor` and a first `davit box run`, and deletes the user afterwards.

The guest agent is the `davit` binary itself, baked into each image, so install the images of the same release
when you upgrade. Without `make release`, `make build && install -m 0755 bin/davit ~/.local/bin/davit` plus
`davit init --image images/manifests/<image>.json` (blobs found in `images/out/blobs`) does the same by hand.
