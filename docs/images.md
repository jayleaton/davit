# Guest images for project boxes

The pipeline itself is described in [images/README.md](../images/README.md): recipes, pinning, determinism and
manifests. This page covers what is installed in each image and how a project gets its own.

## The images

| Image | What it adds | Use |
|---|---|---|
| `ubuntu-24.04-base` | systemd, sshd, the guest agent, git, build-essential, python3, curl, Docker CE (dockerd starts at boot) | the smallest bootable box |
| `ubuntu-24.04-dev` | base, plus Go (the version `mise.toml` pins), Node.js LTS with npm/npx/corepack, pnpm (standalone), uv/uvx, mise, and `pkg-config`, `libssl-dev`, `zlib1g-dev`, `libffi-dev`, `python3-venv`, `python3-dev`, `jq`, `ripgrep`, `sqlite3`, `zip`/`unzip` | the default for project boxes (`make host-install` makes it the first image) |
| `ubuntu-24.04-harness` | base, plus Node.js and the Claude Code and Codex CLIs | prompt harnesses |
| `ubuntu-24.04-ci` | dev, plus the GitHub Actions runner for a user `runner`, `python3-pip` and the runner's .NET libraries | GitHub Actions jobs ([below](#ubuntu-2404-ci-github-actions-runners)) |

In `ubuntu-24.04-dev`:
- **Versions.** Every toolchain is pinned by version and sha256 in `images/inputs.lock`, and its layer checks
  each one (`go version`, `node --version`, …) through a login shell as the image user.
  The manifest's `capabilities` (`toolchain.go`, `toolchain.node`, `toolchain.pnpm`, `toolchain.uv`,
  `toolchain.mise`) record those versions.
- **Install locations.** The tools are root-owned and live under `/usr/local/bin`, `/usr/local/go` and
  `/opt/node`. If a project's `go.mod` asks for a newer Go, Go fetches it once into `~/go/pkg/mod`
  (`GOTOOLCHAIN=auto`, Go's default).
- **Project versions.** A project that wants other versions pins them in its own `mise.toml`. Interactive
  bash/zsh shells activate mise; scripts use `mise exec -- …`. A version other than the image's is downloaded
  on first use and kept on the box's disk.
- **Docker.** Docker is the in-guest `dockerd`, already running at boot. The image user is in the `docker`
  group, and nothing is rootless.

## ubuntu-24.04-ci: GitHub Actions runners

`ubuntu-24.04-ci` is `ubuntu-24.04-dev` plus the [GitHub Actions runner](https://github.com/actions/runner), so
a job gets dev's toolchains, Docker and sudo:
- **Runner.** The release pinned in `images/inputs.lock` (`ACTIONS_RUNNER_VERSION`, with the sha256 of each
  tarball) is unpacked in `/opt/actions-runner`. The image registers nothing: it is started as
  `/opt/actions-runner/run.sh` with a just-in-time config. The build checks that `config.sh --version` and
  `run.sh --version`, run as `runner`, print the pinned version, which also proves the runner's .NET libraries
  resolve.
- **User.** Jobs run as `runner` (uid 1001, home `/home/runner`, group `docker`, passwordless sudo through
  `/etc/sudoers.d/runner`), as on GitHub's hosted runners. The runner owns `/opt/actions-runner`, where it
  writes `_diag` and `_work`.
- **Tool cache.** `/opt/actions-runner/.env` sets `RUNNER_TOOL_CACHE` and `AGENT_TOOLSDIRECTORY` to
  `/opt/hostedtoolcache` (owned by `runner`), `LANG=C.UTF-8` and `ImageOS=ubuntu24`. `actions/setup-node`,
  `setup-go` and `setup-python` install there. `setup-python` picks its build from the OS version in
  `lsb_release`/`/etc/os-release`.
- **Updating the runner.** GitHub stops accepting a runner some time after a newer release, so the pin has to
  follow actions/runner releases. Change `ACTIONS_RUNNER_VERSION` and both `ACTIONS_RUNNER_SHA256_*` in
  `images/inputs.lock` (the release notes list each tarball's sha256), then rebuild the image
  (`IMAGES=ubuntu-24.04-ci images/build.sh`). A runner that GitHub tells to update downloads the new release
  into its own directory at run time; that copy is gone when the VM is.

Compared with GitHub's `ubuntu-latest` (ubuntu-24.04) image, it does not include:
- **Preinstalled language versions.** The tool cache starts empty. GitHub's image ships several Node.js,
  Python, Go, Ruby and PyPy versions there; here each `setup-*` step downloads its version on first use. The
  only languages already present are dev's (Go, Node.js LTS, Ubuntu's Python 3) on `PATH`.
- **Other languages and SDKs.** No Java/JDKs, Maven, Gradle, .NET SDK, Ruby, PHP, Rust, Swift, Haskell,
  Julia or R, and no Android SDK or NDK.
- **Browsers and test tooling.** No Chrome, Chromium, Firefox, Edge, their drivers, or Xvfb.
- **Databases and services.** No PostgreSQL, MySQL or MongoDB. Run them with Docker or job `services:`.
- **Cloud and deployment CLIs.** No GitHub CLI (`gh`), AWS, Azure or Google Cloud CLIs, kubectl, Helm,
  Terraform, Packer or Pulumi.
- **Other utilities.** No Git LFS, Homebrew, Podman/Buildah/Skopeo, or the `apt` package lists: run
  `sudo apt-get update` before installing a package.

Docker CE with buildx and compose is there and `dockerd` starts at boot, so container actions, `services:`
and `docker build` work.

## Caches: where a project image keeps its warm state

Every tool keeps its cache at its own default on the box's disk. Nothing is relocated, so a setup step needs no
configuration, and a snapshot of the builder carries all of it to every instance:

| Cache | Path |
|---|---|
| Go build cache | `~/.cache/go-build` |
| Go modules (and toolchains `go.mod` asks for) | `~/go/pkg/mod` |
| pnpm content-addressed store (hard-linked into `node_modules`) | `~/.local/share/pnpm/store` |
| npm cache | `~/.npm` |
| uv cache (wheels and built packages; venvs link from it) | `~/.cache/uv` |
| Docker images, layers and volumes (in-guest dockerd) | `/var/lib/docker` |

These are the paths a real box on the dev image reported (`go env`, `pnpm store path`, `uv cache dir`, `docker info`). They
are all on the box's root disk, which is the instance's own reflinked disk, so nothing is shared between
instances at run time and nothing leaks between projects.

A plain `davit exec BOX -- CMD` gets `/usr/local/bin` (every toolchain) but not the login profile: no
`~/go/bin`, no pnpm home on `PATH`, no mise activation. Run setup and build steps as `bash -lc '…'` when they
use tools installed into the home directory.

## Disk size

The image's ext4 is 4 GiB. `davit new --disk SIZE` (and resize at resume) makes the disk file larger, and
`systemd-growfs-root` grows the filesystem online at boot (`x-systemd.growfs` in the base fstab). Only written
blocks cost host space: a box's disk is a reflink clone of the image.

## A project's own profile

Pick the lightest option that works.

1. **A project image: a setup step baked into a snapshot** (`box image build/update`). Use this for project dependencies such as a `pnpm install`, `go mod
   download` or a database. Create a box from `ubuntu-24.04-dev`, run the project's setup in it, stop it and
   `davit snapshot` it. New boxes for the project start from that snapshot (`davit new --from SNAPSHOT`), so
   the setup cost is paid once. Re-snapshot when the lockfiles change. Nothing in `images/` changes.
   Instances (one per worktree) are created from it, each with its own reflinked disk, so they share the
   image's blocks and pay only for what they write.
   - **First instance:** the first `new --from` of a snapshot, for each shape, boots the snapshot once and
     captures a golden of it (its memory and the booted disk), which takes as long as a cold boot plus the memory
     write.
   - **Later instances:** they restore that golden, start like a fresh box, and share its memory pages.
   - **Another generation:** a new snapshot (`box image update`) gets its own golden. Goldens whose snapshot is
     deleted are removed by the daemon's sweep.
   - **Resume:** an instance's `stop`/`resume` still cold-boots its own committed disk.

   Measured sizes and times are in [benchmarks.md](benchmarks.md).
2. **A derived recipe.** Use this for system packages or tools that every box of the project needs.
   Add `images/rootfs/<name>/` with:

   | File | Required | What |
   |---|---|---|
   | `base` | yes | the parent recipe, e.g. `ubuntu-24.04-base`, or a derived one (`ubuntu-24.04-ci` names `ubuntu-24.04-dev`) |
   | `packages.txt` | no | extra Ubuntu packages, one per line, installed from the pinned snapshot |
   | `stage.sh` | no | host-side downloads into `$STAGE`, each through `fetch URL SHA256 DEST` with the pin in `inputs.lock` |
   | `layer.sh` | no | runs in the chroot before sealing; installs from `/.provision/layer/stage`; must not `apt-get` (the archive is live by then) |
   | `overlay/` | no | files copied into the root, e.g. `/etc/profile.d/*.sh` |
   | `capabilities.json` | no | what the manifest advertises: `{"name": {"command": "...", "versionVar": "LOCK_VAR", "versionFormat": "v{}"}}` (or `npmPackage` with an `npm/` lockfile) |

   `ubuntu-24.04-dev` is the worked example: it has a `packages.txt`, a pinned `stage.sh` and a `layer.sh` that
   checks every version. To build it on a VM host and install it there, run
   `make host-install HOST=user@host HOST_INSTALL_FLAGS='--image <name> --image ubuntu-24.04-dev'`; the first
   `--image` is the default. A recipe may derive from a derived one: each parent's packages and layer go in
   first, and the image keeps the parents' capabilities.

Keep derived images lean. Everything in the rootfs is copied to every box's disk by reflink, so that costs
nothing until it is written. But a cold boot reads what the guest touches, and golden snapshots capture memory
after boot, so services that start at boot cost every box RAM.
