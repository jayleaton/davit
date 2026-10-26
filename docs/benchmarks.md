# Benchmarks

Measured numbers only. Each entry says what was timed, on what hardware, and how many samples. These are not release gates on reference hardware.

Entries are dated, newest first. What an entry says is missing or "not yet" describes the tree it measured; later entries supersede it (for example, the guest agent, its barrier and guest egress without pasta/slirp4netns all exist now).

## Test host operations (2026-10-25)

Notes from running the first test host, a Proxmox 8.4 VM (QEMU 9.2) with nested KVM on an i5-8400: 6 vCPUs,
20 GiB (`memory: 20480`, `balloon: 8192`), an XFS reflink data disk on a thin LV in `pve/data`. The settings
are explained in [vm-host.md](vm-host.md#running-the-host-as-a-vm-proxmox). One observation each.

- **Install beside another daemon.** davit ran with `--config .config/davit-host/config.json --bin-dir
  .svm/davit/bin` beside another user's daemon. Restarts through the systemd unit were tested. A reboot was not,
  because the other daemon and its VMs were live on the host.
- **Balloon with free page reporting.** After a VM restart with every box stopped, the node went from 22.8 GB
  used to 3.1 GB; the VM held 0.5 GB instead of 18.2 GB.
- **Thin pool.** The virtual disk sizes in the pool added up to 223 GiB against a 137 GiB pool. One `fstrim` in
  the guest took the data disk from 99.98% to 93% allocated and the pool from 63.07% to 61.12%. The data disk
  was grown from 40 to 58 GiB online (`qm resize`, then `xfs_growfs`), because +40 GiB would have let the pool
  reach about 90%. Thin-pool autoextend was off.

## Project boxes from a dev machine over SSH (2026-10-25)

These two runs were in [project-boxes.md](project-boxes.md) until the docs were reorganised. Both drove the
host from a dev machine over the ssh:// transport, before `davit connect` existed.

### Two repositories, zero config

Measured 2026-10-25 on the test host, a nested-KVM VM on an i5-8400 (6 vCPUs, 19 GiB, XFS
data). The daemon is the one `make host-install` installs, with the `ubuntu-24.04-dev` image. Driven from the
development machine with a saved default host, so no flags and no environment. The two repositories are real projects,
each a local clone with a second worktree on another branch, and neither has a `.davit/setup.sh`. Instances
use the default shape (2 vCPUs, 2 GiB). One sample per line.

| Step | A Go web service (mise; `make test`: Postgres in Docker, `-race`, coverage floors) | A pnpm monorepo (`pnpm test`) |
|---|---|---|
| first `davit box run -- TEST` in worktree 1, total | 160.9 s, passes | 93.4 s, passes |
| … of which the image (detected setup) | 25.3 s (mise install, go mod download: 21.2 s) | 67.4 s (pnpm install: 57.4 s) |
| … instance ready (created from the image) | at 28.5 s | at 69.5 s |
| … sync of the worktree | 0.85 s (430 files, 6.0 MiB) | 0.59 s (170 files, 1.3 MiB) |
| both worktrees' `box run -- TEST` in parallel | worktree 1 9.3 s (warm); worktree 2 134.4 s (new instance 2.1 s; cold build cache, new test DB) | worktree 1 19.0 s; worktree 2 27.3 s (new instance 2.2 s) |
| `box down` of each | 0.8 s, 1.5 s | 1.1 s, 1.4 s |
| `box up` of both in parallel; to the first command's exit | 2.2 s, 2.1 s; 3.3 s, 3.2 s | 2.4 s, 2.4 s; 3.8 s, 3.8 s |
| TEST again after the resume | 17.5 s, passes | 26.8 s, passes |
| `box run --no-sync -- true` on a running instance (steady state) | | 0.12 s (`--raw` 0.13 s) |

The first command just after a resume takes about 1 s more than steady state (a guest that has just booted).

### davit's own repository

Measured 2026-10-25 on the same host: 6 vCPUs, 19 GiB, XFS data. Firecracker runtime, the ubuntu-24.04-base
image carrying this build's agent. Driven from the development machine over `DAVIT_HOST=ssh://user@host` on the LAN. The
project is davit's own repository with its `.davit/setup.sh`. Builder: 4 vCPUs, 6 GiB, 30 GiB disk. Instances:
3 vCPUs, 4 GiB, one per worktree; two worktrees (a feature branch and a throwaway second one). One sample per
line unless noted.

| Step | Time |
|---|---|
| API call over ssh://: the first (control master setup), then the next two | 506 ms; 44 ms, 31 ms |
| `box image build davit` (generation 1: mise, Go, modules, staticcheck, SDK deps) | 147.8 s total (setup 83.7 s) |
| `box image update davit` (generation 2: the setup also warms the vet caches) | 334.6 s (setup 325.7 s) |
| two instances `box up` from generation 1, in parallel | 4.2 s, 4.4 s (4.4 s for both) |
| first command after up | 0.12 s |
| `box sync` of the worktree | < 1 s (683 files, 6.8 MiB in an earlier run: 1.4 s) |
| `make check-lint` in both instances in parallel, generation 1 (cold build caches) | 616 s, 615 s, both pass |
| `make check-lint` on an instance from generation 2 (warm caches) | 107 s, passes |
| `box down` (2 instances; then 2 more) | 1.5 s, 2.6 s; 0.4 s, 1.2 s |
| `box up` resume, both in parallel, then the first command | up 2.76 s, 2.75 s; to first command 2.89 s, 2.87 s |
| `box up --refresh` (delete + a new instance from generation 2) | 2.9 s |
| `box ssh … -- echo` (real OpenSSH, ProxyCommand over ssh://) | 0.3 s |
| idle stop with `--idle 1m` (sweep every 15 s) | stopped 76 s after the last command |
| memory per idle instance after a cold boot (Firecracker process, `smaps_rollup`) | Pss 476, 477 MiB (Private_Dirty 475, 476 MiB) |

**Instances cold-boot from the image's disk (history).** This was true for this run. Later the same day,
instances started restoring a memory snapshot of their project image instead (see
[the next entry](#project-image-instances-restore-a-golden-of-their-snapshot-2026-10-25):
about 0.6 s to a first command and about 30 MiB private memory per idle instance). In this run an instance was
a new VM created from a disk snapshot: a reflinked disk and a cold boot. Up took about 4 s, and each instance
held its own guest memory (about 476 MiB idle). A RAM fork of a running VM (`davit fork --live`, boot mode `ram_restore`) shares pages
with its source instead. That is where a figure like 25 MiB private per instance comes from. But it needs the
source running, and a 6 GiB box's live fork took 30.5 s end to end in an earlier run. Persistent per-worktree
instances therefore use disk snapshots.

**Down is a disk stop.** `box down` commits the disk, and `box up` boots it again in under 3 s to a first
command here.

**Toolchains that need a git checkout.** At the time of this run, `box sync` set up no git metadata, so the
runs above used `sh -lc 'git init -q; git add -A; make check-lint'`. Sync now does that itself (`--git index`,
the default).

## Project-image instances restore a golden of their snapshot (2026-10-25)

**Setup:** same host as below (a nested-KVM VM on an Intel i5-8400; 6 vCPUs, 20 GB, XFS
reflink), load 0.5–1.3, nothing else running. Instances use the default shape (2 vCPUs, 2 GiB, 8 GiB).
- The harness is `scripts/box-image-bench.sh`, n=1 setup, with the same heavy setup as the entry below (a Docker
  image, k8s client-go, a next/MUI pnpm project and a numpy/pandas/scipy/scikit-learn venv, about 2.5 GB).
- The project image is that builder, stopped and snapshotted.

**What changed:**
- A create from a snapshot captures a golden of that snapshot's disk, once per snapshot and shape. Its
  fingerprint covers the committed disk, so each project-image generation gets its own. Later creates from it
  restore that golden instead of cold-booting.

| Measurement | Before (cold boot of the snapshot) | Now (golden of the snapshot) |
|---|---|---|
| `new --from` the project image → first command | 1935–2910 ms (n=5, p50 2093) | **583–611 ms** (n=5: 605, 611, 603, 602, 583) |
| The first `new --from` (captures the golden) | same as the others | 21.7 s once, then golden-restored |
| Private dirty per running instance, 30 s idle, 3 running | 350 MiB | **30–31 MiB** (RSS 206–208, PSS 90–91) |
| The same with 6 running | (not measured) | **30–31 MiB** (RSS 207–209, PSS 60–62: shared six ways) |
| Instance's own disk, idle / after a warm offline rebuild | 7 / 90 MiB | 6 / 90 MiB (rebuild 15.3–16.0 s) |
| Golden on disk | none | 2048 MiB memory file + 3 MiB of booted-disk blocks (the rest is shared with the snapshot) |
| `stop`, then `resume` → first command (the instance's own disk, cold) | — | 1905 ms (n=1) |

**Persistence checks:**
- A file written in an instance survived `stop`/`resume`.
- It also survived a daemon restart while the instance ran, and a `resume` after that restart.
- After an unclean host reboot (`systemctl reboot` with the instance running):
  - the service came back on its own (lingering);
  - a stopped instance resumed with its files;
  - the running one came back in `error`. `davit recover` kept its writes, while `resume` boots the last
    committed disk and drops them, as designed.

**Golden sweep on the host:** after the benchmark's snapshot was deleted, the next daemon start removed
exactly its golden (reason "root disk … is gone"; allocated 7.47 GB, which includes the snapshot blocks it
still held) and kept the two image goldens. The filesystem freed about 5 GB.

The builder's own disk in this run counted other projects' VMs on the same daemon (7.7 GB), so the 3.3 GB from
the entry below stands for this setup.

## Project boxes on the dev image: nested KVM on an i5-8400 (2026-10-25)

**Host:** the host from the entry below (6 vCPUs, 20 GB, XFS reflink).
- davit was installed with `make host-install` as a systemd user service, from a tree that
  includes the golden-budget fix below.
- The image was `ubuntu-24.04-dev`. Its rootfs is about 1.7 GiB allocated, against about 1.0 for base.
- Default box shape: 2 vCPUs, 2 GiB, 8 GiB disk. The root filesystem grows to fill the disk (7.8 GiB).

**Not a quiet host.** Another daemon on the host ran 2 VMs throughout, about 3.1 GiB RSS, and load was 5.3–8.7. The
spread below is mostly that.

**Harness:** `scripts/box-bench.sh -n 4`, run twice. It drives the installed daemon over SSH with the real CLI.
"→ first command" means the CLI call plus `davit exec BOX -- true` returning.

| Measurement (ms) | n | p50 | p95 | min |
|---|---|---|---|---|
| `new` (golden restore) → first command | 8 | **788** | 1405 | 655 |
| `exec BOX -- true` on a running box | 40 | **45** | 100 | 31 |
| `stop`, then `resume` (cold boot, disk kept) → first command | 8 | 4695 | 14164 | 2451 |
| `snapshot create`, then `new --from` (cold boot of the snapshot) → first command | 8 | 4183 | 6665 | 2345 |

**First box of an image and shape** (golden capture, n=1 each):
- **2 GiB:** the golden took 31.0 s to prepare (daemon log).
- **3 GiB:** the first `new` took 42.9 s. It cold-booted while the capture (21.5 s) ran. The next box, which waited
  for the capture, took 23.5 s; after that 1.4 s.

**Memory** (each VMM 30 s after `new`, MiB, 5 boxes per run, 2 runs):

| | RSS | PSS | Private dirty |
|---|---|---|---|
| Running box (golden restore) | 143–212 | 57–71 | 28–35 |
| Stopped box | 0 (no VMM; 0 Firecracker processes for 5 stopped boxes) | | |

**Disk per box:**
- The box's own blocks are those outside the image's and the golden's physical extents (`xfs_io fiemap`), counted
  over its disk and its committed copy.
- That is 10 MiB running idle and 14 MiB stopped. Writing a 500 MiB file brought it to 572 MiB.
- A golden adds its 2 GiB memory file (unshared) and 10 MiB of disk, once per image and shape.

Reading:
- A box is ready for commands about 0.8 s after `new`, and a command round trip takes 45 ms.
- **Parked boxes are cheap:** no RAM, and tens of MiB of disk until the project writes data.
- **Waking a parked box is a cold boot: 2.5–5 s** with the dev image, against 2.4 s for the base image (entry
  below). The image starts more services at boot, and the host was loaded. Restoring from a snapshot costs the
  same.

### Project images: a snapshot after a heavy setup, and instances forked from it (2026-10-25)

Same host and install. Load was 1.2–1.7, with the other daemon's VMs stopped by then. The harness is `scripts/box-image-bench.sh
--instances 3 --samples 5`, n=1 setup.
- **Builder:** a default-shape box on the dev image, with the setup run through `bash -l`.
- **Setup (about 2.5 GB of dependencies):** `docker pull node:24-bookworm`; a Go module with `k8s.io/client-go`
  (`go get`, `go build`); a pnpm project (next, react, typescript, @mui/material, esbuild); a uv venv (numpy,
  pandas, scipy, scikit-learn).
- **Project image:** the builder was then stopped and snapshotted, and deleted.
- **Instances:** created with `new --from SNAPSHOT`. Three ran at once.

| Step | Result |
|---|---|
| Setup in the builder | 140.7 s |
| Caches it filled (MiB) | go-build 542, go modules 258, pnpm store 388, uv 255, the projects 13 (`node_modules` hard-links into the store); Docker's layers are root-owned and not in this `du` |
| Builder's own disk after setup | 3349 MiB (outside the image's and golden's blocks) |
| `stop` builder | 4939 ms |
| `snapshot create` | 14 ms (the stop already committed the disk; the snapshot names it) |
| Project image on disk after deleting the builder | 3349 MiB |
| `new --from` the project image → first command | **1935, 1989, 2093, 2510, 2910 ms** (n=5, p50 2093) |
| Each of 3 running instances, 30 s idle: VMM RSS / PSS / private dirty | 353 / 351 / 350 MiB (all three the same ±1) |
| Each instance's own disk, idle | 7 MiB |
| Warm rebuild in each (go build, `pnpm install --offline`, `uv pip sync --offline`, `docker run node:24`) | 14.2, 13.9, 15.7 s |
| Each instance's own disk after it | 90 MiB |

Reading:
- **An instance costs a few MiB of disk and boots in about 2 s with the project's dependencies warm.** A
  project image costs what its setup wrote (3.3 GB here), once, whatever the number of instances. The caches are
  at the tools' defaults (docs/images.md), so the rebuild in each instance ran offline.
- **A running instance costs about 350 MiB of private memory at idle, 10× a golden-restored box.** A disk
  snapshot always cold-boots (lifecycle: "golden restore applies only to a fresh image"), so each instance
  boots its own kernel, systemd and dockerd and shares no memory with its siblings. A golden captured per
  project image (snapshot plus shape) would bring instances down to the 30 MiB / under-1 s numbers above. That
  is a lifecycle change, not part of this work.

**Fixed along the way:**
- **Golden capture budget.** The capture had a flat 60 s budget for boot plus the memory write. A 3 GiB guest ran
  out of time under this load (`snapshot/create: context deadline exceeded`), and the failure was cached for the
  daemon's lifetime, so every box of that shape cold-booted (5.2–8.4 s instead of about 1.4 s). The budget is now
  60 s plus 20 s per GiB of guest memory. A capture that runs out of time is retried by the next `new`.

## First real VM host: nested KVM on an i5-8400 (2026-10-25)

**Host:** a nested-KVM VM on an Intel i5-8400. It has 6 vCPUs, 20 GB RAM, no swap and Ubuntu 24.04.5 (kernel 6.8.0-147). Blobs and scratch are on a dedicated 40 GB XFS disk with `reflink=1`. Firecracker is v1.17.0. The guest is `ubuntu-24.04-base` built on that host by `make vm-tier`, plus this checkout's agent, with 1 vCPU / 512 MiB. The host was otherwise idle (load average 0.4–1.1).

**Harness:** `DAVIT_BENCH=1` through `make vm-tier` (`--bench --pkg ./cmd/davit/`), with `TestFirecrackerBenchmark` run twice back to back (A and B) and `TestDensityBenchmark` run once. "First command" means CLI start until `davit exec VM -- true` has returned. It includes the restore barrier and SSH readiness.

| Measurement (ms) | n | A p50 / p95 | B p50 / p95 |
|---|---|---|---|
| `new` from the golden → first command | 20 | **667 / 720** | **663 / 892** |
| `new` → CLI returned (ready) | 20 | 614 / 667 | — |
| Live fork (`fork --live`) → first command | 10 | 748 / 850 | 806 / 945 |
| Live fork, source paused | 10 | 73 / 106 (RAM 55–89, disk 13–34) | — |
| Disk fork (cold boot of the committed disk) → first command | 10 | 2435 / 3580 | 2404 / 3466 |
| `resume` after stop (cold boot) → first command | 10 | 2353 / 3252 | 2377 / 3102 |
| Exec round trip on a running VM (`davit exec warm -- true`) | 20 | — | **51 / 151** |
| First `new` on a fresh data dir, golden capture included | 1 | 7778 | 10501 |

Density (`TestDensityBenchmark`, 5 VMs of each kind, 30 s idle, MiB per VM, p50 with min–max):

| VM | PSS | Private dirty | RSS |
|---|---|---|---|
| Golden `new` | 60 (59–60) | 25 (25–25) | 196 |
| Disk fork | 309 (308–311) | 309 (308–311) | 311 |
| Live-fork child | 240 (197–273) | 64 (38–180) | 242 |
| Live-fork source after 5 forks | 309 | 308 | 311 |

Full-disk clone, 5 samples each (ms): reflink 3.4–5.8; copy (the non-reflink fallback) 1543–8078. Guest egress (`TestFirecrackerEgress`, n=1) was 15.4 MB/s in the guest against 15.1 MB/s on the host, with the egress stack at 20% of one core.

Reading:
- **Golden `new` meets the SPEC target of p50 < 1 s on nested virtualization:** 663–667 ms to the first authenticated command. The quiet number on the reference dev machine (285–328 ms, bare metal) is about half that. The gap is the cost of nesting; these two hosts have not been compared on equal hardware.
- **Cold boots (disk fork, resume) take about 2.4 s.** That is the guest's own boot plus the barrier, the same shape as on the reference dev machine.
- **A live fork pauses its source for 73 ms (p50),** mostly the RAM snapshot.
- **Density matches the reference dev machine's entry.** Golden restores share their memory file (PSS 60 MiB, private 25 MiB), and a cold-booted VM costs about 310 MiB.
- An exec round trip on a running VM costs 51 ms at p50, including the CLI process start.

Raw logs: `results/vm-tier/2026-10-25T035518Z/` (run A plus density) and `results/vm-tier/2026-10-25T040136Z/` (run B). They are git-ignored and not published.

## Rootless guest egress through the built-in userspace stack (2026-10-22)

Host as in the other entries (Ryzen 7 6800U, shared with other jobs), **with no pasta or slirp4netns installed**. `nft` is present, so the netns ruleset filters too. Guest: `ubuntu-24.04-base`, 1 vCPU / 512 MiB. Egress backend: `davit netstack-egress`, a gVisor netstack (`gvisor.dev/gvisor`, Apache-2.0, the version tsnet already pinned) serving an uplink TAP in the VM netns. Test: `DAVIT_TEST_FIRECRACKER=1 go test -run TestFirecrackerEgress ./cmd/davit/` at low priority (nice 15, idle I/O class).

Functional checks (real VM, 3 runs):
- `curl -sI https://example.com` succeeds.
- `git ls-remote https://github.com/git/git HEAD` succeeds.
- `docker pull hello-world && docker run hello-world` succeeds.
- These are refused: a live HTTP server on the host's LAN address (reachable from the host itself), `169.254.169.254`, and the tailnet address `100.100.100.100`.
- Deleting the VM ends its egress process.

| Metric | Run 2 | Run 3 |
|---|---|---|
| 100 MiB download (`proof.ovh.net/files/100Mb.dat`), guest | 18.8 MB/s | 18.4 MB/s |
| Same file, same run, directly on the host | 16.9 MB/s | 14.9 MB/s |
| Egress stack CPU for the guest download | n/a (see note) | 1.84 s over 5.74 s wall (about 32% of one core, about 17 ms CPU per MB) |
| Load average | 16.5 | 17.4 |

Stack ceiling without the internet in the way (n=1, throwaway measurement): 1 GiB from a host-local server through `up0` and the stack, sent from inside a rootless netns. That gives **70 MB/s, at about 1.4 cores**, at low priority (nice 15) at load about 25. It does not include the VMM's `tap0` path.

Reading:
- The guest download matches the host's own, so it was bound by internet bandwidth, not by the stack.
- Run 2's CPU sample measured the wrong process (a stale stack from an earlier run, before the holder-exit fix), so it is omitted.
- Only IPv4 TCP and UDP are carried. ICMP (`ping`) is not. DNS on port 53 to any address is answered through the host's resolvers.

## Per-VM host memory and full-copy clone time (2026-10-22)

`TestDensityBenchmark` (`cmd/davit`, opt-in with `DAVIT_TEST_FIRECRACKER=1 DAVIT_BENCH=1`) reads `/proc/<vmm>/smaps_rollup` of each VM's Firecracker process after 30 s idle. Five VMs of each kind ran at once (1 vCPU, 512 MiB, image `ubuntu-24.04-base` plus this checkout's agent). Same host and harness as the entries below; two runs, load average 12–28. The host was under memory pressure (about 13 GB of swap in use by other work), so `Swap` is reported too: PSS alone undercounts a VM whose pages were swapped out.

MiB per VM, run 2 (n=5 each); run 1 in parentheses where it differed:

| VM (idle 30 s) | PSS p50 (min–max) | Private dirty p50 (min–max) | RSS p50 | Swap |
|---|---|---|---|---|
| Golden `new` (5 restores of one golden) | **64** (64–66) | **25** (25–27) | 219 | 0 |
| Disk fork (cold boot of a committed disk) | 306 (267–307) (run 1: 217, 95–307) | 306 (266–306) | 308 | 0–42 |
| Live-fork child (each from its own checkpoint) | 235 (154–262) (run 1: 153, 107–202) | 58 (24–180) | 237 | 0 |
| Live-fork source after 5 forks | 40 PSS + **264 swapped** (run 1: 20 PSS) | 40 | 43 | 264 |

Reading:
- **Golden restores are dense.** Five VMs map one golden memory file. The clean pages are shared page cache, so PSS splits them five ways (RSS 219 → PSS 64) and only about 25 MiB per VM is private dirty.
- **A disk fork costs what a cold boot costs:** about 300 MiB private dirty after 30 s for a 512 MiB VM. The guest fills its RAM as it boots (kernel, systemd, page cache).
- **A live-fork child shares nothing with its siblings**, because each fork writes its own checkpoint. Its file-backed pages (PSS 150–260 MiB) are reclaimable page cache from that checkpoint, not anonymous memory. Its private dirty (24–180 MiB) grows with how long the child has been running: the first child, which ran longest, was highest. Sharing one checkpoint across several children (`fork --live --count N`, not built) would make them as dense as golden restores.
- **The source's low PSS is swap, not savings.** 264 MiB of its 512 MiB was swapped out on this memory-pressured host.

### Full-copy clone (`storage: "copy"`, the non-reflink fallback) vs reflink

This is storage `CreateWritable` of the image rootfs (4 GiB apparent, about 0.8 GiB of data) into an 8 GiB VM disk on the same btrfs, timed directly. n=5 per run, two runs.

| Mode | Run 1 (ms, load 28) | Run 2 (ms, load 14) |
|---|---|---|
| copy | 3383, 1831, 2692, 5199, 7736 | 3236, 2121, 1676, 1472, 953 |
| reflink | 45, 269, 35, 36, 34 | 19, 19, 28, 21, 18 |

A full copy is 50–200× slower: 1–8 s on this busy host, depending on writeback load. It is also the cost every `new`, fork and stop commit pays per disk without a reflink filesystem. Reflink stays at 18–45 ms, with one 269 ms outlier.


## Fast fork: live fork vs disk fork and resume (2026-10-22)

`fork --live` forks a running VM. Lifecycle pauses the source, the runtime writes a Firecracker RAM/device checkpoint, and storage commits the source disk while it is still paused. The source then resumes. The child restores that checkpoint onto a reflink of that exact commit (`bootMode: ram_restore`), and the restore barrier gives it a new identity. The default `fork` is unchanged: a disk fork that cold boots the last committed disk.

All runs used the same host and harness as the entries below. The benchmark is `TestFirecrackerBenchmark` (real `davit` binary; first command = `davit exec <vm> -- true` after the restore barrier). Shape: 1 vCPU, 512 MiB. At most 2 VMs ran at once. Other jobs shared the host, which made it heavily loaded and noisy, so each run lists its load average. Comparisons are only fair within one run: in the "interleaved" runs, disk-fork and live-fork samples alternate, so both see the same load.

| Run | Load avg | Disk fork → first cmd p50 / p95 | **Live fork → first cmd p50 / p95** | Source paused p50 / p95 | Resume → first cmd p50 / p95 |
|---|---|---|---|---|---|
| Before (`main`, n=12) | 15–20 | 2158 / 2884 | n/a | n/a | 2701 / 5877 |
| After, checkpoints on tmpfs, interleaved (n=12) | 14–27 | 2304 / 2633 | **791 / 1054** | 327 / 629 | 2736 / 3305 |
| After, checkpoints on disk (default), interleaved (n=12) | 13–60 | 6762 / 10491 | **3944 / 8186** | 2015 / 4880 | 3429 / 5210 |
| After, checkpoints on disk, not interleaved (n=12) | 17–20 | 2761 / 3692 | 1331 / 6219 | 470 / 3755 | 3156 / 5666 |

Reading:
- **Live fork reaches its first command 2–3× sooner than a disk fork in the same run.** With checkpoints on tmpfs: p50 0.79 s vs 2.3 s, p95 1.05 s vs 2.6 s. The child skips kernel and userspace boot. What remains is the restore (about 50 ms) plus the barrier's identity repair (about 0.7 s, guest-agent work).
- **The cost is a pause of the source.** Its length is the time to write the source's RAM (512 MiB here) into the checkpoint file plus the disk commit. On disk (the default) that write competes with every other writer for dirty page cache, so on this busy host it ranged from 0.4 s to 4.9 s. On tmpfs it held at 0.3–0.6 s. Expect it to scale with guest RAM.
- `firecracker.checkpointDir` chooses where checkpoints go:
  - tmpfs: short, predictable pauses, but a live-forked child's RAM image stays pinned in host memory (unreclaimable) for the child's whole life;
  - disk (the default): those pages remain reclaimable page cache.
- **Resume and disk fork are unchanged.** Run-to-run differences come from host load. Their cold-boot time is set by the guest (see below).

Correctness (`TestLiveFork`, real VMs):
- The child sees the parent's unsynced page-cache file and its running systemd service.
- The child gets a new hostname and machine-id.
- Writes are isolated in both directions; the parent keeps running.
- Live fork of a stopped VM is refused with `invalid_state`.
- No checkpoint files are left behind.
- The conformance suite gained `CheckpointRestore`, which Firecracker and the fake both pass.

Caveats:
- **Firecracker resets the source's vsock connections when it snapshots.** Open exec streams and tailnet SSH/tunnel sessions to the source drop and must reconnect. The daemon now pings a cached guest-agent session before reusing it, so new commands just work.
- The child keeps the source's processes. Anything that must not run twice (for example, holding an external lock) is the user's concern, as SPEC §6 says for live fork.
- A live fork interrupted by a daemon restart fails cleanly and can simply be retried:
  - the restart deletes unconsumed checkpoints;
  - a source left paused is unpaused during recovery.

### Disk fork / resume cold-boot timeline (notes for images and guest-agent)

Captured from forked VMs (two samples, load average ~18; `systemd-analyze blame`, guest journal, operation journal):
- **Host side:** storage, reconcile and the Firecracker start take 0.3–0.5 s. Readiness (guest boot plus barrier) takes 2.3–2.9 s.
- **When `ssh.service` starts:** 2.2–2.6 s guest uptime. The barrier waits for its banner, and the agent restarts it once after rotating host keys.
- **Biggest guest-side costs:**
  - `dev-vda.device`: 0.9–1.1 s waiting on udev for the root disk;
  - `e2scrub_reap.service`: 0.45–0.6 s;
  - several `modprobe@*.service` runs at 0.2–0.25 s each (the kernel has no modules);
  - `containerd.service` (0.4–0.6 s) and `systemd-resolved` (0.4 s) are also in early boot.
- **Suggestions for images:**
  - mask `e2scrub_reap` and the `modprobe@` instances for built-ins;
  - stop waiting on udev for `/dev/vda` (static root);
  - keep Docker/containerd off the critical path.
- **Suggestions for the guest agent:** set up host keys before `ssh.service` first starts instead of restarting it.

## Fast restore barrier: golden `new` → first authenticated command (2026-10-22)

Same host, harness (a low-priority wrapper), store, engine, image (`ubuntu-24.04-base` plus this checkout's agent via `fctest`), shape (1 vCPU, 512 MiB) and benchmark (`DAVIT_TEST_FIRECRACKER=1 DAVIT_BENCH=1 go test -run TestFirecrackerBenchmark ./cmd/davit/`, n=20 `new`, n=10 `fork`) as the entries below. "Ready" still includes the full barrier **and** waits for SSH: `readiness.ssh` only flips once sshd has restarted with this VM's new host keys.

What changed:
- **Host keys** (ed25519 and ECDSA P-256) are generated in-process. The image's keygen unit no longer runs `ssh-keygen` inside the sshd restart.
- **Unit restarts run in the background.** The journald restart (it only refreshes `_MACHINE_ID` stamps) is never waited for. SSH readiness waits for sshd's restart.
- **Disk sync and secrets:** one `syncfs` before the identity stamp replaces about 16 per-file fsyncs, and the host makes one secret write per barrier instead of three.
- **Goldens are captured after boot has settled** (`guestclient.SettledGate` as the Firecracker `GuestReady`). Captured mid-boot, a restore's sshd restart queued behind the guest's remaining boot jobs: 500–720 ms instead of about 110 ms.

**Load caveat.** Other jobs were compiling and running VMs throughout, and the load average ranged from 11 to 45. That makes the absolute numbers noisy, so runs were alternated back-to-back. Compare rows in the same window, not across windows.

| Window | Tree | `new` → first command p50 / p95 (ms) | `fork` → first command p50 / p95 (ms) | Load avg |
|---|---|---|---|---|
| A | main | 1008 / 1334 | 3467 / 7583 | 18 |
| A | barrier-fast, agent changes only (golden still captured mid-boot) | 845 / 1691 | 2726 / 17858 | 21 |
| B | main | 1849 / 2319 | 2856 / 3480 | 22 |
| B | barrier-fast + settled golden | **606 / 816** | 1539 / 2047 | 15 |
| B | barrier-fast, agent changes only | 1441 / 1918 | 1849 / 2629 | 13 |
| C | main | 1750 / 7270 | 1970 / 2460 | 17 |
| C | barrier-fast + settled golden | **435 / 739** | 3136 / 4551 | 15 |
| Final | barrier-fast rebased on a later main | **547 / 1253** | 4627 / 7205 | 30 at start, 45 at end |
| Quiet (earlier, same code on the pre-integration tree) | barrier-fast + settled golden | **285 / 320** and **328 / 401** | 1505 / 2194 and 1384 / 1771 | 8–10 |

Run C raw samples (barrier-fast + settled golden): 619.8, 553.3, 510.6, 434.6, 399.9, 531.4, 383.7, 1012.9, 447.2, 463.8, 406.7, 397.5, 445.9, 498.3, 396.6, 738.6, 391.3, 401.7, 370.3, 383.8.

Per-step view (5 restores at the quiet load, from the agent's and prober's timing logs):

| Phase (ms) | Before | After |
|---|---|---|
| Identity work in the guest (clock, hostname, machine-id, host keys, config, stamp, secret) | about 40 | about 40, fewer fsyncs |
| Unit restarts on the barrier's critical path | 530 – 700 | 0 (background) |
| sshd restart until it serves the new keys (counted only through SSH readiness) | 500 – 720 (mid-boot golden) | about 110 |
| Prober total: connect, identity, SSH readiness | 710 – 970 | 200 – 230 |

Reading:
- **The SPEC p50 < 1 s target is met with a wide margin, and p50 < 400 ms is met on a quiet host.** Under load the p50 is 435–606 ms, against 1.0–1.8 s for main in the same windows.
- Most of the gain needs the settled golden. With the agent changes alone, sshd still waits behind boot jobs.
- **Identity checks (real VMs):** right after `ready`, the sshd on port 22 serves the VM's fresh ed25519 key, the same one as on disk. The ed25519 key, ECDSA key and machine-id all differed across VMs. `TestM1Acceptance` and the Firecracker CLI tests pass.
- **Golden capture now waits for a fully booted guest (Docker included).** The first `new` on a fresh data dir, golden capture included, took **3.5 – 10 s** across these runs (n=1 each, load-dependent), against 3.6 – 5.9 s before. That is paid once per image/shape/host fingerprint.
- Not waiting for SSH at all (a daemon `bootPlan` option, not adopted) measured 248 / 290 at load 16. That would need a live SSH-readiness check in the gateway, because lifecycle records readiness only once.

## Acceptance on real VMs over the tailnet gateway (2026-10-22)

`TestM1Acceptance` (`cmd/davit`, opt-in with `DAVIT_TEST_FIRECRACKER=1`) runs the real `davit daemon` binary with the Firecracker runtime, rootless per-VM network namespaces from `internal/network`, and the tailnet gateway. The tailnet is Tailscale's in-process `testcontrol` plus DERP on loopback, never a real tailnet. SSH is plain `golang.org/x/crypto/ssh` from an in-process tsnet peer to `<vm>@gateway`. The front door authenticates the peer by WhoIs, logs in to the guest's sshd with a 5-minute CA certificate, pins the guest host key the restore barrier reported, and reaches the guest over the agent's `tcp.dial` over vsock.

Setup: same host, harness, engine and image as the next entry. The image is base rootfs `sha256:affec2d6…` plus this checkout's agent. Shape: 1 vCPU, 512 MiB. At most 4 VMs ran at once (2 GiB of guest RAM). Guest egress was off: no pasta or slirp4netns on this host, which `doctor` reports.

Per-step wall time in ms over 3 runs (min – max). Host load average was 11–18.

| Step | ms |
|---|---|
| `new` on a fresh data dir, golden capture included | 4304 – 5286 |
| First `ssh web@gateway` command after that `new` (barrier already passed) | 262 – 643 |
| **Warm `new` (golden restore + barrier) → first tailnet SSH command** | **1125 – 2102** |
| `cp` a small file in through the agent | 16 – 26 |
| `stop` (agent power-off, VMM reaped, disk commit) | 523 – 852 |
| `resume` (cold boot + barrier) → tailnet SSH command | 2139 – 3972 |
| `fork` (reflink + cold boot + barrier) → tailnet SSH command | 2225 – 3885 (n=6) |
| Daemon SIGKILLed during a fork's boot → restart → fork replayed and ready → SSH | 5311 – 7408 |
| `delete` of 4 VMs (VMMs, namespaces, disks) | 438 – 486 |

All three runs also confirmed:
- another tailnet user is refused, over SSH and over the API;
- a revoked owner is refused on the next connection;
- fork writes are isolated from each other and from the source;
- the killed fork's journal shows `recovered`;
- `delete` leaves no VMM, no namespace holder and no instance directory.

## Firecracker + guest agent: golden `new` → first authenticated command (2026-10-22)

Same host, harness, store, engine and shape as the entry below. The image is `ubuntu-24.04-base` (base rootfs `sha256:affec2d6…`, rebuilt upstream since the entry below) with the **real guest agent** from this checkout installed by `fctest` (the static `davit` binary as `/usr/local/bin/davit-guest-agent`). The daemon is the real `davit daemon` process with no test hooks.

"Ready" now means the daemon's prober has run the full restore barrier: authenticated session, clock step, hostname, machine-id and SSH host-key rotation, restart of `ssh`/`journald`, and an SSH banner. "First command" is `davit exec <vm> -- sh -c true` succeeding. This is the SPEC §1 "first authenticated command, including identity repair", minus the tailnet hop.

| Metric (ms) | Run 3 p50 / p95 | Run 4 p50 / p95 | n per run |
|---|---|---|---|
| **`new` (golden restore + barrier) → first command** | **1454 / 1965** | **1047 / 1126** | 20 |
| `new`: CLI returned `ready` (barrier passed) | 1430 / 1910 | 1024 / 1103 | 20 |
| `fork` (reflink + cold boot + barrier) → first command | 2070 / 2148 | 1998 / 2225 | 10 |
| First `new` on a fresh data dir, golden capture included | 5893 (n=1) | 4024 (n=1) | 1 |

Load average was 11–13 during both runs.

Run 4 raw samples for `new` → first command: 1071, 1051, 1009, 992, 1126, 1045, 1027, 1123, 1020, 1047, 1049, 1021, 1077, 995, 1073, 1000, 1014, 1105, 1304, 1073.

Reading:
- **The SPEC target of p50 < 1 s is not met yet (1.05–1.45 s). The p95 < 2 s target is met.**
- The VM is not the bottleneck. The entry below measured restore to a raw command at about 110 ms. The rest is the barrier's identity repair: unit restarts and the SSH wait.
- The obvious levers belong to the guest agent: rotate host keys before `ssh.service` first starts in the golden (gate sshd on the barrier instead of restarting it), skip the journald restart, and report readiness before the banner when SSH isn't requested.
- Fork improved from about 2.9 s to 2.0 s. The real agent starts before `sysinit.target`, whereas the old stand-in waited for full boot.


## Firecracker runtime, VM only (stand-in shell, no barrier): golden `new` and fork (2026-10-22)

| | |
|---|---|
| Host | AMD Ryzen 7 6800U (8C/16T), 27 GiB RAM shared with other jobs, Linux 7.2.5 (Arch Linux). Load average 7–14 during runs |
| Harness | a low-priority wrapper: nice 15, idle I/O class, CPUWeight 20. Numbers are pessimistic for an idle host |
| Store | btrfs (`compress=zstd:3`, 94% full); storage mode reflink |
| Engine | Firecracker v1.17.0 (pinned, sha256-verified), rootless: per-VM user+net namespace with `tap0`, hybrid vsock |
| Image | `ubuntu-24.04-base` from `images/` (base rootfs `sha256:f0286b3c…`) (kernel 6.18.58-slipway, Ubuntu 24.04 + systemd + Docker CE), plus the **test-only** stand-in agent from `internal/runtime/firecracker/fctest`: a root shell on vsock 1024 that starts listening once `systemctl is-system-running --wait` returns |
| Shape | 1 vCPU, 512 MiB RAM, 8 GiB disk (default) |
| Test | `DAVIT_TEST_FIRECRACKER=1 DAVIT_BENCH=1 go test -run TestFirecrackerBenchmark ./cmd/davit/`. The daemon runs in-process and every verb is the real `davit` binary with `--json` |

"First command" is measured from just before `davit new` (or `fork`) is spawned until `true` has run in the guest over hybrid vsock and its exit status has come back. It **excludes** the restore barrier and identity repair (clock, machine-id, host keys), which belong to the guest agent and don't exist yet. It also excludes SSH and tailnet, so it is **not** yet the SPEC's "first authenticated command". Samples run one after another, with each VM deleted before the next one starts.

| Metric (ms) | Run 1 p50 / p95 | Run 2 p50 / p95 | n per run |
|---|---|---|---|
| **`new` (golden restore) → first command** | **108.8 / 122.1** | **111.9 / 120.9** | 20 |
| `new` (golden restore): CLI returned `ready` | 87.7 / 97.9 | 98.2 / 107.1 | 20 |
| `fork` (reflink + cold boot) → first command | 2941 / 3166 | 2782 / 4672 | 10 |
| `fork`: CLI returned `ready` | 92.9 / 112.1 | 103.7 / 123.1 | 10 |
| First `new` on a fresh data dir, golden capture included | 3217 (n=1) | 5254 (n=1) | 1 |

Run 2 raw first-command samples:
- new: 114.8, 116.4, 129.7, 109.6, 116.6, 115.7, 116.5, 109.7, 106.5, 109.5, 114.0, 105.1, 105.2, 120.9, 112.7, 111.9, 111.2, 111.1, 111.2, 113.5
- fork: 3591.8, 4671.6, 3234.8, 3107.1, 2736.0, 2782.9, 2627.3, 2668.0, 2782.1, 2590.3

Notes:
- The SPEC target for warm `new` is a first command in under 1 s at p50. Golden restore lands at about 110 ms here, before the guest agent's barrier is added.
- `fork` and disk `resume` cold boot the committed disk (SPEC §6: golden RAM is never restored onto a modified disk). Their first command waits for full userspace boot, including `docker.service`, because the stand-in only listens once boot completes. The real agent starts before `sysinit.target`, so expect it to answer sooner. Measure again when it lands.
- The CLI returns `ready` before the guest answers. At this stage readiness was "VMM running" (`lifecycle.RuntimeProber`) until the guest-agent prober exists. The VM's `readiness.agent` stays `false`, which is honest.
- In the first version of the stand-in, socat's default 0.5 s half-close linger added a flat ~500 ms to every command. The stand-in now uses `-t 0.01`. That latency belonged to the shim, not the VM.
