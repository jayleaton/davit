# davit guest images

Deterministic, rootless pipeline for the Firecracker guest: a custom kernel, Ubuntu 24.04 ext4 rootfs images with the repo's own guest agent baked in, and content-addressed manifests. Design inputs are in [SPEC §5 "Images and networking" and §6](../docs/research/SPEC.md) and the [spike findings](../docs/research/SPIKE.md).

```sh
images/build.sh                              # kernel + agent + every image -> out/blobs/sha256/* + manifests/*.json
images/smoke.sh                              # boot ubuntu-24.04-base, run every gate, restore barrier, forks, stop
IMAGE=ubuntu-24.04-harness images/smoke.sh   # same, plus the harness capabilities
```

On the reference dev machine a clean `build.sh` (kernel, agent, both images) took 26 min 44 s from empty dirs at load ≈ 20; the kernel at `-j4` is most of it, and each rootfs takes about 3.5 min. A smoke run took 60–75 s. Measured numbers are in [RESULTS.md](RESULTS.md).

## Root requirements: none

Nothing in `build.sh` or `smoke.sh` uses sudo, setuid helpers beyond the distro's `newuidmap`/`newgidmap`, Docker, or system package installs.

| Step | Needs | Why |
|---|---|---|
| builder, kernel, rootfs | unprivileged user namespaces (`unshare --map-auto --map-root-user`) plus a `/etc/subuid` and `/etc/subgid` range for your user (65536 ids) | apt/dpkg in a chroot need `root` and real uid/gid ownership. A userns gives fake root, and mke2fs `-d … -E root_owner=0:0` writes the true ownership into the ext4 image |
| smoke | `/dev/kvm` and `/dev/net/tun` read-write for your user, plus unprivileged user+net namespaces | Firecracker needs KVM. Each VM's TAP is created inside its own `unshare -rn` namespace |
| guest agent | git, and the Go toolchain pinned in `mise.toml` (`mise exec -- go`) | The agent is this repo's `davit` binary, built CGO-free at HEAD |
| host tools | bash, util-linux, coreutils, curl, tar/xz/gzip, gpg, python3, plus `socat`, `ssh`, `debugfs` for smoke | — |

Distro notes:
- Ubuntu 24.04 hosts block unprivileged userns by default (`kernel.apparmor_restrict_unprivileged_userns=1`). They need an AppArmor profile for the builder or a one-time sysctl change. That is an admin decision; the scripts never change it.
- On Debian/Ubuntu, `/dev/kvm` access usually means membership in the `kvm` group.

The *product* still needs privilege for things the image doesn't do: guest internet egress (veth/NAT), the jailer, and UFFD. See SPIKE "What needed root".

## Layout

| Path | What |
|---|---|
| `inputs.lock` | Every pinned input: source tarballs and their sha256, the Ubuntu snapshot timestamp, Docker CE package versions, Firecracker release, `SOURCE_DATE_EPOCH` |
| `kernel/davit.config`, `davit-<arch>.config` | Fragments merged onto Firecracker's `microvm-kernel-ci-<arch>-6.18.config` |
| `rootfs/ubuntu-24.04-base/` | `packages.txt`, `provision.sh` (runs in the chroot) and `overlay/` (config files) |
| `rootfs/ubuntu-24.04-harness/` | Derived image: `base` (parent recipe), `stage.sh` (host-side verified downloads), `layer.sh` (runs in the chroot before sealing), `npm/` (package.json + lockfile), `capabilities.json`, `overlay/` |
| `rootfs/ubuntu-24.04-dev/` | Derived image for project boxes: `base`, `packages.txt` (extra Ubuntu packages from the pinned snapshot, installed by the root recipe's provision.sh; a layer must not apt-get, the archive is live by then), `stage.sh` (Go, Node.js, mise, pnpm, uv, each pinned in `inputs.lock`), `layer.sh` (installs them and checks each version through a login shell), `capabilities.json` (`toolchain.*`, version from an `inputs.lock` variable), `overlay/` |
| `rootfs/ubuntu-24.04-ci/` | Derived from `ubuntu-24.04-dev`: `base`, `packages.txt` (the runner's .NET dependencies, pip), `stage.sh` (the GitHub Actions runner, pinned in `inputs.lock`), `layer.sh` (user `runner`, `/opt/actions-runner`, `/opt/hostedtoolcache`; checks the runner's version as `runner`), `overlay/` (runner's sudoers) |
| `scripts/build-agent.sh` | Guest agent: the repo's `davit` binary at HEAD, with a version gate |
| `smokeprobe/` | Go helper for smoke: runs the product's `guestclient` restore barrier and exec over Firecracker hybrid vsock |
| `scripts/builder.sh` | Pinned build environment: an Ubuntu 24.04 userns chroot from the same snapshot, plus mke2fs 1.47.4 built from source |
| `scripts/build-kernel.sh`, `kernel-inner.sh` | Kernel build, config verification and Docker `check-config.sh` gate |
| `scripts/build-rootfs.sh` | Rootfs build, seal and mkfs |
| `scripts/make-manifest.py` | Writes `manifests/<image>.json` |
| `scripts/smoke.py` | Firecracker boot, checks, snapshot and restore |
| `manifests/` | Committed manifests (x86_64: `<image>.json`; other arches: `<image>-<arch>.json`). `DAVIT_MANIFESTS=<dir>` writes and reads them elsewhere, e.g. for development builds |
| `manifests/profiles/` | Declared image profiles that have no build yet, with any files they would install in `profiles/<image>/` (see [Declared profiles](#declared-profiles)); `images/build.sh` does not read them |
| `results/` (ignored) | Raw smoke JSON written by each smoke run; [RESULTS.md](RESULTS.md) summarizes them. Never committed |
| `$DAVIT_IMAGES_WORK` (default `~/.cache/davit/images`, outside the repo) | Downloads, builder chroot, kernel/rootfs trees, smoke scratch. `build.sh clean` drops all of it except downloads. Kept out of the source tree because its subuid-owned chroot dirs break repo-wide tools such as `gofmt -l .` |
| `out/blobs/sha256/` (ignored) | Built artifacts, named by digest. Never committed |
| `$DAVIT_APT_CACHE` (optional, unset by default) | The pinned snapshot's package indexes and `.deb`s, kept between builds in `<dir>/<deb-arch>/`. A build with the same sources then neither runs `apt-get update` nor downloads a package it already has, so it needs no mirror; the release workflow keeps it in the Actions cache. The images do not change: the seal empties apt's cache and lists either way |

`DAVIT_IMAGES_WORK` and `DAVIT_IMAGES_OUT` relocate the last two. Keep them off `/tmp` when it is RAM-backed. Retained scratch is about 0.75 GB: downloads 191 MB, builder 542 MB, Firecracker 24 MB. The kernel and rootfs trees are deleted after each step unless `KEEP_WORK=1`; their peak size wasn't measured. Outputs per build: the 29 MiB kernel and 35 MB agent are shared, and each rootfs is a sparse 4 GiB file (base ≈ 1.0 GiB allocated, harness ≈ 1.9 GiB).

## Determinism

- **Inputs are pinned by digest.** Tarballs, Firecracker configs, Docker GPG key (and its fingerprint), and moby `check-config.sh`. All Ubuntu packages come from `snapshot.ubuntu.com/<UBUNTU_SNAPSHOT>`. Docker CE packages are pinned by exact version, because download.docker.com has no snapshots. The resolved package set is recorded in the manifest (`rootfs.packages`).
- **The toolchain is pinned too**: the kernel compiles and mke2fs runs inside the builder, never on the host toolchain. The host's gcc, e2fsprogs and distro don't affect the output.
- **Kernel**: `KBUILD_BUILD_TIMESTAMP`, `KBUILD_BUILD_USER` and `KBUILD_BUILD_HOST` are fixed, and `-ffile-prefix-map` is set. The fragment asserts that module signing (which would embed a random key) and randstruct (a random seed) are off, and the build fails on any `=m`.
- **Rootfs**: `SOURCE_DATE_EPOCH` is honoured by dpkg/shadow and by mke2fs ≥ 1.47.1, which clamps every inode time. Ubuntu's own 1.47.0 copies each file's build-time ctime, which is why the builder compiles 1.47.4. The fs UUID and htree hash seed are fixed and derived from the inputs. Nothing per-machine or time-dependent survives sealing. Builds run with `umask 022`. The rootfs build fails if the build host's DNS search domains appear anywhere in the tree: the chroot borrows the host's resolv.conf, and systemd-resolved's postinst once backed it up into the image.
- **Known limit**: `mke2fs -d` copies extended attributes from the build tree. A host whose filesystem adds SELinux labels or default ACLs (e.g. Fedora/RHEL) would get different bytes. The reference build ran on btrfs without SELinux.
- **Manifest**: it has no timestamps. Its `id` is the sha256 of its canonical JSON. RESULTS.md records whether two clean builds produced identical digests.

## Kernel

Linux **6.18 LTS** (pinned point release in `inputs.lock`), on top of Firecracker v1.17's recommended 6.18 guest config. Everything is built in (`=y`), and no loadable modules are shipped. The kernel's `modules.builtin*` files are installed in the rootfs, so `modprobe br_netfilter` and similar calls from Docker and kube-proxy succeed.

The fragment adds what the spike found missing or what SPEC §5 requires:
- raw tables (`IP_NF_RAW` / `IP6_NF_RAW`): Docker ≥ 28 needs these, and the spike worked around them with `DOCKER_INSECURE_NO_IPTABLES_RAW`, which this image does **not** use
- full nftables `inet`/`ip6`/`bridge`/`netdev` families with masq/redir/reject/fib/socket/tproxy
- IPVS and ipset
- macvlan, ipvlan, dummy, WireGuard
- `BRIDGE_VLAN_FILTERING`
- IPsec ESP for encrypted overlay networks
- BPF JIT, CUSE, `/proc/config.gz`

vsock, virtio-balloon with free-page reporting, VMGenID, overlayfs, FUSE and VXLAN were already in the FC config. The fragment re-asserts them so a future base-config bump can't silently drop them.

x86_64 also gets the i8042 keyboard controller and AT keyboard (`davit-x86_64.config`). Firecracker's `SendCtrlAltDel` action injects the chord there, so a VM can be stopped cleanly **without the guest agent**. systemd runs `ctrl-alt-del.target`, the guest shuts down cleanly and reboots, and Firecracker exits on the reboot (boot arg `reboot=k`). Firecracker implements that action on x86_64 only.

`build-kernel.sh` fails if any fragment line doesn't survive `olddefconfig` (an unmet dependency or a renamed symbol), or if moby's `check-config.sh` reports a "Generally Necessary" option missing.

arm64: `ARCH=aarch64 images/build.sh kernel` cross-compiles `arch/arm64/boot/Image` on an x86_64 host. The builder always carries the other arch's cross compiler, so switching arches never rebuilds it. The cross-built kernel and its fragment verification are recorded in RESULTS.md.
- **Booting it needs an arm64 host.** Firecracker only runs guests of the host's arch, under KVM, and there is no emulation path.
- **The arm64 rootfs must also be built on an arm64 host**, because apt runs natively in the chroot.

## Image contracts

These are what the runtime adapter, guest agent and network adapter can rely on. Changing them is a shared-contract change (AGENTS.md: dedicated PR).

| Contract | Value |
|---|---|
| Kernel boot args | `manifest.kernel.bootArgs`: root on `/dev/vda` (ext4, label `rootfs`), console `ttyS0` |
| Root size | The image's ext4 is 4 GiB (`SIZE`). `/etc/fstab` marks `/` `x-systemd.growfs`, so `systemd-growfs-root` grows it online at boot to fill a larger disk (`davit new --disk`, or a resize at resume) |
| Guest agent binary | `/usr/local/bin/davit-guest-agent`: this repo's static `davit` binary, built by `build-agent.sh` from HEAD (CGO-free, `-trimpath`, the same `-X buildinfo.Version=$(git describe --tags --always --dirty)` stamp as `make build`). The manifest records its digest, version, commit and Go version (`guest.agent`) |
| **Agent protocol skew** | The manifest records the baked agent's protocol: `guest.agent.protocol = {min, max, features}`, from `images/agentinfo` at the agent's commit, cross-checked against the binary's `-version`. The daemon's image catalog and `davit doctor` grade it with `guestclient.CheckAgent`. **No common protocol version ("major")** refuses the image: the manifest fails to load, so no boot or golden preparation can start. **Missing features ("minor")**, or a manifest that predates protocol tracking, only warn: the daemon log and `doctor` name the missing features. Smoke checks that the live handshake version is in that range and that the negotiated features equal manifest ∩ host |
| Agent version gate | `build-agent.sh` fails unless the binary reports exactly `git describe` (`davit guest-agent -version`). A tree with uncommitted or untracked files is refused unless `DAVIT_ALLOW_DIRTY=1`. `build-rootfs.sh` refuses an agent that isn't clean HEAD's, and the version is re-checked inside the chroot. `make-manifest.py` won't write a dirty-agent image into `images/manifests`, and a development manifest says `"dirty": true`. Smoke fails unless the running agent's handshake version equals the manifest's |
| Guest agent unit | `davit-guest-agent.service`, the unit from `internal/guestagent/doc.go` plus `-default-user davit` and explicit shutdown ordering: `ExecStart=/usr/local/bin/davit-guest-agent -default-user davit`. The binary acts as the agent when invoked under that name, and its arguments are agent flags. `DefaultDependencies=no`, after `local-fs.target`, before `ssh.service`/`shutdown.target`, `Conflicts=shutdown.target`, `Restart=always`. Runs as root |
| **Guest agent vsock port** | **1024** (`guestagent.Port`), AF_VSOCK, host CID 2 only. With Firecracker hybrid vsock, the host connects to the VM's vsock UDS and sends `CONNECT 1024\n` |
| User | `davit` (SPEC §3 default image user), uid/gid 1000, groups `sudo` and `docker`, passwordless sudo, no password (`*`), `~/.ssh/` present but empty: the agent or host installs `authorized_keys` |
| SSH | OpenSSH as plain `ssh.service` (socket activation disabled), key-only, no root login (`sshd_config.d/10-davit.conf`). `sshd_config` must `Include sshd_config.d/*.conf`, and the build checks it: the agent writes `50-davit.conf` (HostKey, TrustedUserCAKeys). Host keys: ed25519 only |
| Network | `eth0` gets `172.16.0.2/24` via `172.16.0.1`, DNS `1.1.1.1` (systemd-networkd). Every VM uses the same address inside its own netns (SPEC §5) |
| Identity | `/etc/machine-id` is empty: systemd generates one on first boot. The ed25519 host key is generated on first boot by `davit-ssh-hostkeys.service`, ordered before the agent so the two never race. The agent's restore barrier then assigns the VM's hostname and rotates machine-id and host key for new VMs and forks. A RAM snapshot clones them, and the barrier replaces them per fork (smoke proves it) |
| Stop | x86_64: Firecracker `SendCtrlAltDel` → clean shutdown → VMM exits; works without the agent |
| Time | `systemd-timesyncd` is masked. The restore barrier steps the clock to host time |
| Docker | Docker CE (pinned), containerd, buildx and compose plugins. Default config, iptables-nft backend. `dockerd` and `containerd` are enabled at boot |
| apt | Sources point at the live Ubuntu archive and download.docker.com, so users get updates. The build itself only ever saw the snapshot |

### Images and capabilities

| Image | Contents | Capabilities |
|---|---|---|
| `ubuntu-24.04-base` | Everything above | — |
| `ubuntu-24.04-harness` | base + Node.js (pinned tarball, `/opt/node`) + Claude Code and Codex CLIs (`/opt/davit-harness`, linked into `/usr/local/bin`) | `harness.claude`, `harness.codex` |
| `ubuntu-24.04-dev` | base + Go (mise.toml's pin), Node.js LTS, pnpm, uv, mise, common build libraries; see [docs/images.md](../docs/images.md) | `toolchain.go`, `toolchain.node`, `toolchain.pnpm`, `toolchain.uv`, `toolchain.mise` |
| `ubuntu-24.04-ci` | dev + the GitHub Actions runner (`/opt/actions-runner`, user `runner`, uid 1001, passwordless sudo, group `docker`) and a tool cache at `/opt/hostedtoolcache`; see [docs/images.md](../docs/images.md#ubuntu-2404-ci-github-actions-runners) | dev's `toolchain.*` (inherited) |

A capability in `capabilities.json` is either an npm package (`npmPackage`, version and integrity from the recipe's `npm/package-lock.json`) or a pinned input (`versionVar`: an `inputs.lock` variable, `versionFormat` such as `"go{}"` for the token the command prints). The smoke test checks `harness.*` capabilities through `--version`; a layer proves the others at build time.

The harness capabilities match what `internal/harness` expects: it runs a bare `claude` / `codex` through `sh -lc 'exec "$0" "$@"'` as the exec user. The CLIs are therefore on every login PATH, and the build and the smoke both prove it through exactly that argv. The manifest's `capabilities` map gives each capability's command, npm package, version, lockfile integrity and Node version.
- **Pinning:** `npm ci` runs against the committed `package-lock.json`, so every tarball, including each platform binary, is sha512-pinned.
- **No package code runs at build time** (`--ignore-scripts`). Claude Code's postinstall only copies its platform binary, so the layer links that binary directly.
- **Updates:** the CLIs are root-owned, and `DISABLE_AUTOUPDATER=1` is set in `/etc/profile.d`. To get newer CLIs, bump the lockfile and rebuild the image.
- **What's tested:** neither CLI is run against a model, because that needs credentials.

A derived image names its parent in `base`, and the parent may be derived too (`ubuntu-24.04-ci` → `ubuntu-24.04-dev` → `ubuntu-24.04-base`; a cycle is refused). The root recipe's `provision.sh` runs as usual. It installs every layer's `packages.txt` from the snapshot, then runs each derived recipe's `layer.sh`, root first, as `/.provision/layer/layer.sh` before sealing (so sealing covers them too). Each recipe's `stage.sh` runs host-side first. The recipe digests cover every recipe in the chain, the manifest's `base` is the immediate parent, and its `capabilities` are the chain's, root first.

Docs and man pages are excluded, as in ubuntu-base's minimized default (`unminimize` restores them). Masked for boot speed: apt timers, `motd-news`, `e2scrub`, `fstrim`, `getty@tty1`, `systemd-networkd-wait-online`.

### Declared profiles

`manifests/profiles/<image>.json` declares an image before it has a recipe or a build: its parent, arches,
capabilities with the pinned npm package, version and registry integrity, the Node version and the
environment its layer would set. It has no kernel, rootfs, guest or input digests, so it is not a manifest
and cannot be passed to `davit init --image`. Building one means adding `rootfs/<image>/` the way
`ubuntu-24.04-harness` does (with a committed `package-lock.json`), after which `images/build.sh` writes the
real `manifests/<image>.json`.

| Profile | Contents | Capabilities | Notes |
|---|---|---|---|
| `ubuntu-24.04-desktop` | harness + Xvfb, Openbox, x11vnc on guest loopback, Chromium for Testing (G15) | `desktop.vnc`, `browser.chromium` | `profiles/ubuntu-24.04-desktop/` holds its startup unit; [docs/network.md](../docs/network.md) covers the desktop endpoint |
| `ubuntu-24.04-opencode` | base + Node.js + OpenCode CLI (`opencode-ai` 1.19.3) | `harness.opencode` | The npm launcher's postinstall only picks the platform binary, so a layer built with `--ignore-scripts` links `opencode-linux-{x64,arm64}/bin/opencode` (each pinned in the profile) like the Claude Code binary. `OPENCODE_DISABLE_AUTOUPDATE=1` |
| `ubuntu-24.04-pi` | base + Node.js + pi coding agent (`@earendil-works/pi-coding-agent` 1.4.0; the `@mariozechner/` package is deprecated) | `harness.pi` | Needs Node ≥ 22.19.0 (the pinned v24.22.1 qualifies). `PI_SKIP_VERSION_CHECK=1`, `PI_TELEMETRY=0` |

Both harness adapters are `experimental` in `capabilities.harnesses` until their steps in
`docs/research/G5-ACCEPTANCE.md` pass on a built image.

## Manifest

`manifests/<image>.json` contains:
- `id`: sha256 of its canonical JSON
- `base` (parent recipe, if derived) and `capabilities`
- `kernel`: digest, size, release, boot args, config digest, modules.builtin digest, toolchain
- `rootfs`: digest, size (apparent; the file is sparse), fs UUID, root device, package list with versions
- `guest`: the contracts above, including `agent` (binary digest, version, commit, dirty, Go version, command)
- The manifest step refuses a rootfs built against a different kernel than the current one, because `modules.builtin` must pair with it
- `inputs`: the full `inputs.lock`, plus the digest of every recipe file and a combined `recipeDigest`
- Release bundles: `make release` packs each committed manifest with its blobs (see `packaging/README.md`)
- **Recipe binding:** the kernel and rootfs builds record the digest of the files that shaped them, and `make-manifest.py` refuses to write a manifest if those files changed since, so a manifest can't describe a recipe its blobs weren't built from

Consumers fetch the blobs by digest and must verify them before use (`smoke.py` does).

## Smoke test

`images/smoke.sh` runs `scripts/smoke.py`, under the optional `DAVIT_HEAVY` command prefix when it is set (for example a nice/ionice wrapper on a shared machine). Each VM gets 1 GiB RAM and 2 vCPUs, and only one VM runs at a time.

Each step's VM runs in its own `unshare -rn` netns, with a TAP at 172.16.0.1. Socat bridges inside the netns expose guest `:22` and `:8080` as Unix sockets and serve a fake "internet" at `172.16.0.1:9000`. The test SSH key is injected into a reflinked copy of the rootfs with `debugfs -w`, so the image itself is never modified. Results go to `results/smoke-<ts>.json`, and the run exits non-zero if any gate fails.

| Group | Gates |
|---|---|
| Image and boot | blob digests; image sealed (empty machine-id, no host keys, no docker.service drop-ins); cold boot → SSH banner and → `ssh true`; `systemctl is-system-running` = `running`; machine-id and host keys generated on boot |
| Kernel | `uname -r` matches the manifest; moby `check-config.sh` against the *running* kernel's `/proc/config.gz`; `iptables -V` is nf_tables; raw tables for v4 and v6; an nft inet table with set/fib/ct/masquerade; VXLAN; IPVS; FUSE; overlayfs; guest→host vsock; balloon bound with the free-page-reporting feature; VMGenID bound |
| Docker (offline) | BuildKit `FROM scratch` build from the guest's static busybox; `docker run` on the default bridge; container egress through MASQUERADE (the host listener sees the guest IP); a published port reached **from the host over the TAP** (PREROUTING DNAT + FORWARD); Docker's raw-table DROP rules present; a user-defined network with embedded DNS; no daemon errors in the journal |
| Guest agent | baked binary == manifest digest; unit runs the agent with `-default-user davit`; the **restore barrier runs over vsock with the product's own host code** (`images/smokeprobe` → `guestclient.Prober.Barrier`), started together with the VM. It must complete with an authenticated session, the agent version == manifest, vmId/hostname assigned, readiness (agent + sshd), a second session authenticating with the rotated secret, exec as root (`uid 0`) and as the default user (`davit`), and a guest clock within 2 s of the host; plus the identity stamp on disk and the kernel hostname |
| Snapshot, restore, fork | Full snapshot taken after the source's barrier. N restores (default 3) as forks, after letting the snapshot age ≥ 10 s. Each fork's barrier must rotate machine-id and host key away from the source and every earlier fork, and step the clock. After restore: disk marker, the published container still serving to the host, and `docker run` |
| Stop | On the last fork: `SendCtrlAltDel` → Firecracker exits, and a write made without `sync` is on the disk afterwards, so shutdown was clean |
| Capabilities | For each manifest capability, `<command> --version` through the agent exec + login shell path == the pinned version |

Informational only (never gates):
- Docker's native nftables firewall backend, a Docker-side feature whose status varies by release
- the optional `check-config.sh` items still missing
- whether a fork shares the source's `boot_id`. It does: `boot_id` isn't writable, and SPEC uses the generation as the run identity instead

## Known issues

- `docker run` of an image created with `docker import` (a single-layer tarball) hung in the guest on Docker CE 29.9.2. `docker build` images ran fine, so the smoke builds its test image with BuildKit. The spike's import path worked on Docker 29.1.3. Not yet investigated, and not known to be kernel-related.
