# Contributor VM host: source installs and the real-VM tier

**Installing davit to use it?** Follow [install-host.md](install-host.md): one command, prebuilt binary and
image, no checkout. This page is for contributors: installing a host from a source checkout (images built on the
host) and running the real-VM test tier.

The real-VM test tier needs a separate Linux machine with KVM. The development machine never boots guests. Once you have SSH
access to such a host, a single command takes it from nothing to test results:

```sh
make host-preflight HOST=user@host                 # read-only: can this machine be a VM host?
make vm-tier HOST=user@host                        # preflight, image build, prepare, the whole tier
TESTS='TestM1Acceptance$' make vm-tier HOST=user@host
make vm-tier HOST=user@host VM_TIER_FLAGS='--bench --fs /data/svm-fs'
```

Pass `TESTS` as an environment variable, as above, not as a make argument: make expands `$` in arguments, so
`TESTS='A$|B'` would arrive as `AB`. `HOST` is any SSH destination (tailnet name, IP, `user@host`, an
`~/.ssh/config` alias). It needs key auth that never prompts: every script uses `BatchMode`.

## Installing davit on the host from source

`make vm-tier` only borrows the host. To keep a source build running there (for project boxes, while developing
davit itself), install it; everyone else uses [install-host.md](install-host.md):

```sh
make host-install HOST=user@host                   # preflight, binary, images, service; rerun to upgrade
make host-install HOST=user@host HOST_INSTALL_FLAGS='--image ubuntu-24.04-dev --image my-project'
make host-uninstall HOST=user@host                 # stop the boxes, remove the service and binary
make host-uninstall HOST=user@host HOST_INSTALL_FLAGS=--purge   # also the data dir and config
```

`scripts/host-install.sh` does these steps:

1. **Preflight.** It runs `scripts/host-preflight.sh`. It needs 20 GiB free when it builds images and 10 GiB with
   `--skip-images`.
2. **Binary.** It builds the `davit` binary for the host's arch on the development machine (CGO-free, `make build`'s
   flags, under `DAVIT_HEAVY` when set). That is the light part of a release. Image bundles are not built locally.
3. **Images.** It builds the images on the host from the committed `HEAD`: `ubuntu-24.04-dev` (the default)
   and `ubuntu-24.04-base`, through `vm-tier.sh --images`. The build is skipped while `images/` is unchanged.
4. **Install.** The binary goes into the bin dir by an atomic rename. The images are reflinked into
   `DATA/images` (default `DATA` is `~/.svm/davit`, on the reflink disk). Then it runs `davit init
   --data-dir DATA --image … --service`. That writes a systemd **user** unit, `davit.service`
   (`WantedBy=default.target`, `KillMode=process`, so VMs outlive daemon restarts), and enables
   lingering (`loginctl enable-linger`), so the user's systemd and the daemon start at boot without a
   login. The daemon socket is `DATA/davit.sock`.
5. **Upgrade.** A rerun with a new binary or images restarts the service. A rerun with nothing changed
   touches nothing; the daemon keeps its PID.
6. **Verify.** It checks that the service is active and enabled, lingering is on, and the socket
   answers `capabilities`.

**It never takes over a daemon setup it did not create.** If the config (`~/.config/davit/config.json` by
default) belongs to another data dir, or a `davit.service` runs another config, it stops with exit 2 and
changes nothing. To install beside such a setup (for example a developer's ad-hoc daemon), pass `--config`
and `--bin-dir`; relative paths are under the host user's home:

```sh
make host-install HOST=user@host HOST_INSTALL_FLAGS='--config .config/davit-host/config.json --bin-dir .svm/davit/bin'
```

Clients reach that daemon with `--api-url unix://DATA/davit.sock` or `DAVIT_API_URL`. Two daemons on one
host each admit VMs against the whole host (see Capacity), so give each one `limits` in its config.

For example, the flags above give:
- config `~/.config/davit-host/config.json`
- binary `~/.svm/davit/bin/davit`
- data and socket under `~/.svm/davit`

How this setup was tested is in [benchmarks.md](benchmarks.md#test-host-operations-2026-10-25).

## What the host needs

`scripts/host-preflight.sh` checks all of this. It installs nothing, uses no sudo and writes nothing on the
host. It prints a PASS/WARN/FAIL table, followed by the exact command that fixes each FAIL and WARN.

| Check | Needs | Usual fix (Ubuntu 24.04) |
|---|---|---|
| arch, cpu-virt | x86_64 with `vmx`/`svm` (bare metal, or a VM with nested virtualization) | firmware VT-x/AMD-V |
| kernel | 5.10 or newer | — |
| kvm | `/dev/kvm` read-write for the SSH user | `sudo usermod -aG kvm USER`, then log in again |
| tun | `/dev/net/tun` read-write | `sudo modprobe tun` |
| cgroup | unified cgroup v2 (WARN otherwise: no per-VM limits) | `systemd.unified_cgroup_hierarchy=1` |
| userns | `unshare --user --map-root-user --net true` works | Ubuntu restricts it through AppArmor: the sysctl `kernel.apparmor_restrict_unprivileged_userns=0` on a dedicated test host, or `davit init --apparmor` ([network.md](network.md)) |
| subids | 65536 subuids/subgids and `newuidmap` (images/build.sh) | `sudo usermod --add-subuids LO-HI --add-subgids LO-HI USER` with a free range the preflight computes (above every range in /etc/subuid and /etc/subgid), `sudo apt install uidmap` |
| vsock | nothing: Firecracker's vsock is hybrid (a Unix socket on the host) | — |
| reflink | the data dir (`~/.svm/fs`, or `--fs`) on btrfs or XFS with `reflink=1` (test disks are reflinked) | `--fs /path/on/btrfs` |
| disk | 20 GiB free there by default (`--min-disk GIB` in preflight, host-tier and vm-tier lowers it). Measured: a tier run on a prepared host used about 1.3 GiB of transient space (2026-10-25, n=1); rebuilding both images used about 3.8 GB at peak (n=1). A first build also creates the builder chroot (about 0.7 GB). Old blobs are pruned after each build | — |
| memory | 4 GiB minimum, 8 GiB comfortable (three 512 MiB guests plus a Go build) | — |
| firecracker, jailer | nothing: the tier downloads the pinned Firecracker (`images/inputs.lock`, sha256-verified) into its scratch; the jailer is not used | — |
| nft | optional (WARN): without it, the userspace egress stack filters alone | `sudo apt install nftables` |
| tools | `git tar flock nsenter unshare ip debugfs sha256sum curl rsync python3 xz gpg make` | `sudo apt install …` (it prints the package list) |
| toolchain | nothing: prepare installs mise and the Go pinned in `mise.toml` into `~/.svm/bin` | — |

Other forms:

```sh
scripts/host-preflight.sh --fs /data/svm-fs user@host   # judge the reflink/disk checks at --fs
scripts/host-preflight.sh --local                       # this machine (still read-only)
scripts/host-preflight.sh --probe-only user@host > h.facts
scripts/host-preflight.sh --facts h.facts               # re-evaluate recorded facts, no SSH
```

The probe prints `key=value` facts, and the verdict is computed locally from them. `--facts` therefore works as
a dry run, and `scripts/testdata/preflight/` holds recorded facts that the hermetic tests evaluate.

Exit status: 0 no FAIL, 1 at least one FAIL, 2 usage, 255 SSH.

## What `make vm-tier` does

`scripts/host-tier.sh` runs four steps in order. The first failure stops the run.

1. **preflight**: as above. A FAIL stops the run with exit 2.
2. **images**: `scripts/vm-tier.sh --images` ships the committed `HEAD` (a git bundle, sent with rsync) and runs
   `images/build.sh` for `ubuntu-24.04-base` on the host. The blobs go to `~/.svm/fs/blobs` and the manifest to
   `~/.svm/fs/manifests`; prepare and the tests then use that manifest. The step is skipped when the host's
   image was built from the same `images/` tree. `--rebuild-images` forces a build, and `--skip-images` drops the
   step when the host already has blobs (`vm-tier.sh --prepare --blobs`). Local changes never reach the image,
   because the baked agent must match a commit. They do ride along to the tests.
3. **prepare**: `vm-tier.sh --prepare` installs mise and Go, checks every blob against its digest, and warms the
   fixture (pinned Firecracker, this checkout's agent, the derived test image).
4. **tier**: `vm-tier.sh` runs the real-VM tests serially ([vm-tier.md](vm-tier.md)). `--bench`,
   `--fresh-host`, `--timeout`, `--pkg`, `--dir` and `--fs` pass through.

Each vm-tier step cleans up after itself on the host, including on failure and Ctrl-C.

Results go to `results/vm-tier/<UTC date and time>/` (git-ignored):

| File | |
|---|---|
| `summary.md` | host, commit, test regex, each step's status, exit code and seconds, preflight FAIL/WARN lines, test counts and failing tests |
| `preflight.txt` | the full preflight table |
| `<step>.log` | each step's output |
| `<step>/logs/` | what the host sent back: `images.log` and the manifest, then `test.log`, `summary.txt`, `host.txt`, `run.txt`, `kernel-kills.txt`, `cleanup.log` |

Exit status is that of the failing step: 0 pass, 1 test failure, 2 host or usage problem, 3 image build failed,
130 interrupted, 255 SSH.

## Expected durations

Measured on the first VM host, 2026-10-25: a nested-KVM VM on an Intel i5-8400 with 6 vCPUs, 20 GB RAM and XFS reflink scratch, otherwise idle. These come from `results/vm-tier/*/summary.md`.

| Step | Time | Samples |
|---|---|---|
| preflight | about 1 s (one SSH round trip) | every run |
| images, first build on a host | about 12.5 min: the source fetches (about 25 s) and the builder chroot (about 2 min), then the build below | 1 (that run stopped at a rootfs guard, since fixed, after 732 s) |
| images, rebuild after `images/` changed | 623 s: kernel `-j4` 7.5 min, agent 3 s, rootfs 2.7 min | 1 |
| images, `images/` unchanged | 1–2 s (skipped) | 5 |
| prepare | 13–52 s (Go toolchain check, blob digests, fixture) | 6 |
| tier, everything (`TESTS` unset) | 314 s, 317 s | 2 |
| `--bench` with `TestFirecrackerBenchmark` alone | 121 s | 1 |

So a brand-new host takes about 20 minutes from `make vm-tier` to results, and a later run with unchanged `images/` takes about 6 minutes. Faster or bare-metal hosts will differ; record theirs here.

## Capacity: running vs parked boxes

These numbers are for the 2026-10-25 host: 20 GB RAM, 6 vCPUs, 58 GB XFS reflink disk (40 GB until it was grown). The box shape is the
default: 2 vCPUs, 2 GiB, 8 GiB disk. Measurements are in [benchmarks.md](benchmarks.md).

**Running boxes are bounded by memory.**
- A box restored from the golden starts small: about 30 MiB private plus its share of the golden's memory
  file (PSS about 60 MiB, RSS 140–210 MiB).
- A box that works grows towards its 2 GiB. Firecracker does not hand guest memory back.
- Planned capacity is therefore `(20 GiB − about 2 GiB for the host) / 2 GiB` ≈ **9 busy boxes**. Dozens
  of idle ones fit until they start working.
- CPUs are not reserved: 6 vCPUs run 3 boxes at full speed at once, and idle boxes cost no CPU.

**Admission checks free memory and a reservation.**
- **Free memory:** a VM must leave `limits.minFreeMemoryBytes` (default 256 MiB) of `MemAvailable`, read when
  it starts.
- **Reservation:** free memory alone keeps admitting idle boxes that can later each grow to 2 GiB and push
  the host into OOM. With `limits.memoryBytes` set, the declared memory of the live VMs
  (`capabilities.capacity.allocated.memoryBytes`) may sum to at most `memoryBytes × memoryOvercommit`
  (`memoryBudgetBytes`). The overcommit defaults to 1.5, because boxes restored from one golden share its
  pages.

`davit init` writes `memoryBytes` = host RAM − 2 GiB and `memoryOvercommit` 1.5 into a new VM-host config,
and `make host-install` runs it. An existing config is kept as it is, so set these by hand on an older host
and restart the daemon (`systemctl --user restart davit`):

```json
"limits": {"memoryBytes": 19327352832, "memoryOvercommit": 1.5, "cpus": 12}
```

On the 20 GB host that is 18 GiB × 1.5 = 27 GiB of declared memory: 13 default boxes live at once, while the
free-memory floor still stops admission early if they really use it. `memoryOvercommit: 1` gives a hard
18 GiB (9 boxes). `cpus: 12` is 2× CPU overcommit. With two daemons on one host, split the limits between
them.

**When there is no room, idle boxes go first.** Before refusing, admission stops VMs that have an idle
timeout (every project box) and have been idle at least `limits.reclaimIdleAfterSeconds` (default 120;
negative: never), oldest activity first. A VM with a running process or an open session never qualifies.
Then a request with `waitSeconds` queues FIFO ([project-boxes.md](project-boxes.md#when-the-host-is-full)).

**Parked (stopped) boxes cost only disk.**
- A stopped box has no VMM and uses no RAM.
- Its own disk is what it wrote: 14 MiB for a freshly stopped box on the dev image, plus whatever the project writes (a
  500 MiB file added 558 MiB). Everything else is shared with the image by reflink.
- Each golden costs about 2 GiB more, once per image and shape (its memory file, not shared).
- With 15 GB free, that is room for several hundred empty parked boxes. In practice, project data is the
  limit: at 1 GiB of dependencies and build output per project, about 12 projects after one golden.
  Snapshots (`davit snapshot create`, then `new --from`) share their blocks with every box made from
  them.

### Project images and their instances

A project image is a snapshot of a builder after its setup, with one instance per worktree. The first instance of
a snapshot, for each shape, captures a golden of it. Every later instance restores that golden, so it starts
like a fresh box and shares the golden's memory pages. Unit costs measured on this host (benchmarks.md,
2026-10-25):

| Unit | Disk | RAM |
|---|---|---|
| Project image (snapshot) | what its setup wrote: 3.3 GB for a 2.5 GB dependency install | none |
| Its golden, per shape used | 2 GiB memory file, plus 3 MiB of booted-disk blocks | page cache, shared by its instances |
| Instance, parked | 6 MiB, plus what it writes (90 MiB after a warm rebuild) | none |
| Instance, running | the same | about 30 MiB private at idle (PSS 60–90), up to its 2 GiB under load |
| Fresh `new` box (no project image) | 10–14 MiB | about 30 MiB at idle |
| Golden per base image and shape | 2 GiB | page cache, shared |

For **N project images, with M instances each and R running**:
- **disk** ≈ N × (image size + 2 GiB golden) + N × M × instance writes
- **RAM** ≈ R × (30 MiB idle … 2 GiB busy) + the goldens' pages in use

On this host, with 58 GB of reflink disk (about 43 GB above the 10 GiB golden floor and the base images) and
20 GB of RAM:
- **About 8 project images** of this size: 8 × 5.3 GB ≈ 42 GB. Beyond that, the golden floor evicts the least
  recently used project goldens, and those projects' next boxes cold-boot once to recapture. Instances add a little each: 3 per image is 18
  instances at about 0.1 GB, about 2 GB. Disk is the limit.
- **Running:** RAM, not starting, is the limit. Idle instances cost about 30 MiB each, so dozens fit at once. About
  9 can be busy at full size (2 GiB each). The 6 vCPUs run 3 busy instances at full speed.
- **Starting:** an instance is ready about 0.6 s after `new --from`. Parking every idle instance (`stop`) and
  starting a fresh one is cheap. `resume` of a parked instance cold-boots its own disk in about 1.9 s.

Put `limits.memoryBytes` in the daemon config (see above) so admission stops at what the host can back.

**Golden GC.** The daemon removes goldens that can never be restored again: their root is gone (a deleted
snapshot's collected commit, or a replaced image's rootfs), or they were captured by another Firecracker build,
host CPU or device layout. The sweep runs at startup, after each capture, and at most every 10 minutes during
creates, and logs `golden removed` with the reason. A golden whose snapshot was deleted keeps that snapshot's
blocks until the sweep, so removing it frees them. `make host-install` deletes a replaced image's old rootfs
blob, which lets the sweep drop that image's goldens. Kernels are kept, because VMs created from the old image
still cold-boot from them.

**Golden free-space floor.** Each golden is about one guest's memory on disk (2 GiB for the default box), so
goldens of many project images can fill the data disk. The daemon keeps the data dir's free space above
`firecracker.goldenMinFreeBytes` (default 10 GiB; negative disables it):
- **Eviction:** it evicts the least recently used goldens first. The most recently used golden of each base
  image is never evicted, so fresh boxes of every image stay fast. A golden's last use is its last capture or
  restore (the golden directory's mtime).
- **Captures:** it checks before each capture, with room for that capture's memory file. A capture that still
  doesn't fit is refused, and that create cold-boots.
- **When:** at startup, on every lifecycle sweep (every 15 s), and with the golden sweep.
- **Reporting:** each eviction is logged as `golden evicted` and emitted as a `golden.evicted` event
  ([events.md](events.md)): to the snapshot's owner for a project image, to the host owner otherwise.

An evicted golden costs nothing until it is needed. Instances already restored from it keep running. The next
create from that image or snapshot captures it again, which takes one cold boot plus the memory write (21.7 s
for the 2.5 GB-setup project image on the test host); the creates after that restore it.

## Running the host as a VM (Proxmox)

A Proxmox VM with nested KVM works as a host. Two settings matter, and both are in the VM's config, not davit's.
What they did on the test host is in [benchmarks.md](benchmarks.md#test-host-operations-2026-10-25).

**Memory: balloon floor plus free page reporting.** For a 20 GiB VM, for example, `memory: 20480` and `balloon: 8192`. With the
balloon device, Proxmox (8.4, QEMU 9.2) adds `free-page-reporting=on`, and the Ubuntu 24.04 guest kernel has
`virtio_balloon` built in.
- **Returned memory:** memory the guest frees goes back to the node, including a box's VMM memory when the box
  stops.
- **Page cache stays:** the guest's cache of images, goldens and box disks is not free memory. It stays with the
  VM, and keeps golden restores fast.
- **The floor:** it only matters when the node passes 80% memory use (another VM started). Proxmox then
  inflates the balloon toward 8 GiB. The guest driver allocates balloon pages without retrying, so inflation
  takes free memory and page cache but never pushes a running box's memory out or triggers the OOM killer.
- **Admission:** box admission reads the guest's MemAvailable, which already excludes ballooned pages, so it
  refuses boxes the guest can't back.
- **Why not lower:** a 4 GiB floor would leave the daemon and about one busy box; 8 GiB keeps about three busy
  boxes possible.
- **Applying it** needs a VM restart. Do it with no box running.

**Disk: a thin pool can be overprovisioned.** When the VM's data disk (XFS reflink) is a thin LV, for example in
the default `pve/data` pool:
- **Overprovisioning:** the virtual sizes of the disks in a pool can add up to more than the pool, so the pool
  can fill even though no single disk is full. When it does, every VM on it stops writing.
- **Trim:** keep `discard=on` on the disks and the guest's `fstrim.timer` enabled (weekly), so blocks freed in the
  guest go back to the pool.
- **Alert:** watch `lvs -o lv_name,data_percent,metadata_percent pve/data` on the node, and alert at 75% data or
  metadata. Before growing a VM disk, check that the pool would stay under 75% if the new space filled completely.
  A disk grows online with `qm resize VMID scsi1 +NG`, then `xfs_growfs` in the guest.
- LVM's thin-pool autoextend (`thin_pool_autoextend_threshold`) is a node setting, off by default.

## CI

- `.github/workflows/ci.yml` runs the hermetic gate on hosted runners, one job per stage: `check-lint`,
  `check-go` in three shards, `check-cross` and `check-sdk`. Go, tools and SDK dependencies are cached.
- `.github/workflows/vm-tier.yml` is manual (`workflow_dispatch`: tests regex, bench, fresh host, rebuild
  images). It runs `make vm-tier` on a self-hosted runner labelled `kvm`, targeting `vars.VM_TIER_HOST`
  (default `localhost`, so the runner's user needs key auth to it), and uploads `results/vm-tier/` as an
  artifact.

To register the runner, follow GitHub's self-hosted runner instructions on the host, add the extra label `kvm`,
and run it as the same non-root user the preflight checks.
