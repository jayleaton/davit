# Installing davit

One command per machine, no source checkout, no Go and no image build. The **host** is the Linux machine
with KVM that runs the VMs; the **client** is your laptop or workstation, where your editor and coding agent
stay.

```sh
# On the Linux host, as the user who will own the VMs (not root):
curl -fsSL https://github.com/jayleaton/davit/releases/latest/download/install.sh | sh -s -- --host

# On your laptop (macOS or Linux):
curl -fsSL https://github.com/jayleaton/davit/releases/latest/download/install.sh | sh -s -- --client
davit connect user@host
```

> The repository is not public yet, so these URLs do not resolve yet. Until it is, build a release with
> `make release` and install from its directory with `--from DIR` (see [Contributors](#contributors)).
> Publishing needs: the public GitHub repository (`buildinfo.Repo`), a pushed `v*` tag to run
> `.github/workflows/release.yml`, and a first run of that workflow to confirm the runner builds the images.

## What you need

| | Host | Client |
|---|---|---|
| OS | Linux x86_64 with systemd. Tested on Ubuntu 24.04; Debian 12 and other systemd distributions are expected to work but are untested | macOS (Apple silicon or Intel) or Linux, x86_64 or arm64 |
| Virtualization | KVM: bare metal with VT-x/AMD-V on, or a cloud/VM instance with nested virtualization | none |
| Memory | 4 GiB minimum, 8 GiB or more recommended | any |
| Disk | 10 GiB free for the data dir (the dev image is a 491 MB download and unpacks to 1.7 GiB; VM disks grow from there). btrfs or XFS (reflink) makes forks and new boxes instant; ext4 works, copying whole disks | 30 MB |
| Account | a normal user that can `sudo` once for the fixes below; log in over SSH or a console (not `sudo -u`/`su`, which have no systemd user session) | SSH key access to the host (`ssh user@host true` must not prompt) |
| Tools | `curl` (or `wget`), `tar`, `zstd`; the installer prints the `apt-get`/`dnf` line for anything missing | `curl`, `tar`, `ssh` |

No davit or Tailscale account is needed. The client reaches the host over SSH; the daemon opens no network
port.

## What `--host` does

1. Downloads `SHA256SUMS` and the binary for this OS and CPU from the release, checks the binary against it,
   and installs it into `~/.local/bin` (an atomic rename, so an upgrade never leaves a half-written file).
2. Writes the config (`davit init`): runtime `firecracker`, data in `~/.local/share/davit` (or `--data-dir`).
   A host without working KVM never falls back to simulated VMs.
3. **Checks the host before any large download** (`davit doctor --fixes`) and prints the one-time root
   commands it needs, each with the reason. Without `--yes-sudo` it stops there: run them yourself, or rerun
   with `--yes-sudo`, then rerun the same install command. davit never runs `sudo` on its own.
4. Checks free space, then downloads and verifies the guest image (every blob against its digest).
5. Installs and starts `davit.service` as a systemd **user** service, and enables lingering so it keeps
   running after you log out and starts at boot.
6. Waits for the daemon, then runs `davit doctor`, which must report no failures. The installer exits non-zero
   otherwise.

### The root commands it may print

| Check | Command | Why |
|---|---|---|
| KVM module | `sudo modprobe kvm_intel` (or `kvm_amd`) | `/dev/kvm` is missing although the CPU has VT-x/AMD-V |
| KVM access | `sudo usermod -aG kvm USER` | VMs need read-write `/dev/kvm`; the `kvm` group grants it |
| | `sudo systemctl restart user@UID.service` | your systemd user manager (which runs `davit.service`) started before the group change and keeps its old groups, even across logouts once lingering is on. Restarting it applies the group; your SSH shell is not affected. It is not offered from a desktop terminal, which lives inside that manager: reboot instead |
| AppArmor (Ubuntu 23.10+) | `sudo install -m 0644 ~/.config/davit/apparmor/davit-HASH /etc/apparmor.d/` and `sudo apparmor_parser -r ...` | Ubuntu blocks unprivileged user namespaces, which give each VM its own network namespace. The profile allows them for this one binary path only; do not turn the restriction off host-wide |
| User namespaces | `sudo sysctl -w kernel.unprivileged_userns_clone=1`, persisted in `/etc/sysctl.d/` | the kernel has them switched off (some Debian setups) |
| TUN | `sudo modprobe tun`, persisted in `/etc/modules-load.d/` | guests get their network interface through a TAP device |
| Lingering | `sudo loginctl enable-linger USER` | only when the unprivileged `loginctl enable-linger` is refused |
| Tools | `sudo apt-get install -y zstd` | the image bundle is zstd-compressed |

After a `kvm` group change, **this shell** still has the old groups: the installer runs its own checks through
`sg kvm`, and tells you to log out and back in (or `newgrp kvm`) before using `davit` in that shell.

`subuid`/`subgid` ranges and `uidmap` are **not** needed: they are only for building images from source.

### Options

| Option | Meaning |
|---|---|
| `--yes-sudo` | run the printed root commands (`sudo` prompts as usual) |
| `--image NAME` | image to install, repeatable; default `ubuntu-24.04-dev` |
| `--data-dir DIR` | data dir for a new install; pick btrfs or XFS for instant clones |
| `--tailnet-owner LOGIN` | also serve on your tailnet (optional), with the auth key in `$DAVIT_TS_AUTHKEY` |
| `--version TAG` | a specific release instead of the latest |
| `--from URL\|DIR` | another release location: `https://`, `file://` or a local directory |
| `--install-dir DIR` | where the binary goes (default `~/.local/bin`) |
| `--fake-runtime` | development only: no KVM, simulated VMs |

### Images

| Bundle | Download | Unpacked | Contents |
|---|---|---|---|
| `ubuntu-24.04-dev` (default) | 491 MB | 1.7 GiB | base plus Go, Node.js, pnpm, uv, mise: the toolchains project boxes detect |
| `ubuntu-24.04-base` | 295 MB | 0.9 GiB | systemd, SSH, git, build-essential, Python 3, Docker CE |

Each bundle carries its kernel. The dev image is the default because `davit box run` sets projects up with
those toolchains; `--image ubuntu-24.04-base` installs the smaller one instead.

## Upgrading and removing

Rerun the same install command. The binary and any changed image are replaced, the config is kept, and the
service restarts on the new version; running VMs keep running across the restart. `davit version --check` tells
you whether a newer release exists.

To remove davit from a host:

```sh
systemctl --user disable --now davit.service
rm -f ~/.local/bin/davit ~/.config/systemd/user/davit.service
rm -rf ~/.local/share/davit ~/.config/davit      # VM disks, images and config
loginctl disable-linger                          # if nothing else of yours needs it
```

## Trust

- **Integrity:** every download is checked against `SHA256SUMS` from the same release. A corrupt or truncated
  file stops the install before anything is replaced.
- **Authenticity:** releases are built and published by the repository's GitHub Actions workflow
  (`.github/workflows/release.yml`), which attaches a GitHub build-provenance attestation to every asset.
  Check one with `gh attestation verify davit-linux-amd64.tar.gz --repo jayleaton/davit`. There are no
  separately managed signing keys.
- To read the installer before running it: `curl -fsSLO .../install.sh`, read it, then `sh install.sh --host`.

## When doctor is not green

`davit doctor` prints one line per problem with its fix; `davit doctor -v` shows every check. `davit doctor
--fixes` prints only the root commands. The daemon's log: `journalctl --user -u davit.service`.

## Measured

One run of `scripts/fresh-host-e2e.sh` (2026-10-26, n=1): a fresh throwaway user `davittest` on the VM host
(Ubuntu 24.04.5, kernel 6.8.0-147, a nested-KVM VM on an Intel i5-8400, 6 vCPUs, 19 GiB RAM, other users' VMs running), with
the release from a local `file://` directory (images built on that host). The data dir was
on ext4, the default home filesystem, so VM disks were copied, not reflinked.

| Step | Time |
|---|---|
| `install.sh --host --yes-sudo`: binary, root fixes (`usermod -aG kvm`, user manager restart), 491 MB dev image unpacked and verified, config, service, lingering, doctor green | 34 s |
| from the start of the install to `davit box run -- true` done (project image built in 91.8 s, first instance created in 210.7 s) | 246 s |

The download from GitHub is not included: the release came from local disk. Add the time to fetch about
520 MB (binary and dev image) over your link. Most of the first box command is copying 4 GiB disks on ext4;
with the data dir on btrfs or XFS those copies are reflinks. That is not measured here.

Doctor at the end: `davit doctor: ready (17 passed, 1 warning)`. The warning was the ext4 storage one above.
Afterwards the user, its home, service, processes, sudoers entry and kvm group membership were deleted.

## Contributors

Building from source, the real-VM test tier and the maintainers' host tooling (`make host-install`,
`make vm-tier`) are in [vm-host.md](vm-host.md) and [vm-tier.md](vm-tier.md). To try an unpublished build:

```sh
make release                                       # needs built images (images/build.sh); see packaging/README.md
d=.build/release/$(cat .build/release/latest)
sh $d/install.sh --host --from $d                  # on a host; copy $d there first
scripts/fresh-host-e2e.sh --release $d             # the whole flow as a throwaway user, on a VM host
```
