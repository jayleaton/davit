# Results

## Current: agent protocol in manifests, local release artifacts, arm64 kernel

Measured 2026-10-22 on the same reference dev machine as below: an AMD Ryzen 7 6800U laptop (16 threads, 27 GiB RAM, btrfs at 96% full, Linux 7.2), shared with other jobs, builds at nice 15 / idle I/O.

**Status of each item:**

| Item | State |
|---|---|
| Protocol skew guard (daemon, `init`, `doctor`) | done; unit tests for refuse (major), warn (minor and untracked) and OK |
| Manifests record `guest.agent.protocol` | done; current manifests record protocol 1–1 with `exec`, `files`, `files.tar`, `identity`, `tcp` |
| Images rebuilt with protocol recorded | done, **but the baked agent predates the final rebase** (see "Deferred") |
| `make release` | done once, before the final rebase; output below |
| `install.sh` from a local release dir, with image bundles | done; unit-tested |
| Install + boot from a real release (`TestReleaseInstallAndBoot`) | **deferred**: needs a VM host |
| arm64 kernel | cross-built and verified; **booting it is deferred** to an arm64 host |
| Release signing | **not implemented**: a marked hook; the signing key is not chosen yet |

### Images (rebuilt with the agent protocol recorded)

| Manifest | id | kernel | rootfs | agent |
|---|---|---|---|---|
| `ubuntu-24.04-base` | `sha256:861d0592d471…` | `0cd311da…` (unchanged) | `8923ad9b…` | `m1-71-gdf80ed9` |
| `ubuntu-24.04-harness` | `sha256:bef94cc4e57b…` | `0cd311da…` | `54814632…` | `m1-71-gdf80ed9` |

- The agent records protocol 1–1 and features `exec`, `files`, `files.tar`, `identity`, `tcp`.
- **The x86_64 kernel is byte-identical to the previous generation**, even though the builder gained a cross compiler. Reproducibility held.
- Smoke: 2× base and 1× harness passed every gate (runs of 2026-10-22, 08:16–08:20 UTC). That includes the new gate, run on the cold boot and every fork: the live handshake is v1, inside the manifest's 1–1 range, and the negotiated features equal manifest ∩ host.
- **Timings are not representative.** Load average was 38–80 on 16 threads. Cold boot → barrier ready took 4.8–13.6 s and fork → barrier ready 0.36–4.2 s; the SSH banner alone took 4.8–5.4 s cold. Earlier generations measured at load 5–30 are below; re-measure on a quiet host.

### Release artifacts (`make release` before the final rebase)

| File | Size |
|---|---|
| `davit-<v>-linux-amd64.tar.gz` | 13.7 MB |
| `davit-<v>-linux-arm64.tar.gz` | 12.5 MB |
| `davit-<v>-darwin-arm64.tar.gz` | 10.5 MB |
| `davit-image-ubuntu-24.04-base-x86_64.tar.zst` | 292 MB (1.0 GiB allocated rootfs, zstd -12) |
| `davit-image-ubuntu-24.04-harness-x86_64.tar.zst` | 576 MB (1.9 GiB allocated rootfs) |
| `install.sh`, `release.json`, `SHA256SUMS` | — |

Whole run: 181 s, at high load (load not sampled during the run; 45–80 in the smoke runs just before). The binaries took ≈ 2 min; the earlier attempt during a kernel build had a compile killed for memory, which is why `release.sh` now deletes incomplete releases. `release.json` noted that both images bake an agent built from an earlier commit than the release, compatible by protocol 1–1. The artifacts were deleted afterwards to save disk; `make release` recreates them.

### arm64 kernel (cross-built, not booted)

`ARCH=aarch64 images/build.sh kernel` took 17 min 49 s at load ≈ 20, including a one-time builder rebuild that added the cross compiler.
- **Output:** `sha256:1756d397…`, 19.9 MB, `Linux kernel ARM64 boot executable Image, little-endian, 4K pages`, release `6.18.58-slipway`, toolchain `aarch64-linux-gnu-gcc 13.3.0` (Ubuntu 24.04 builder).
- **Checks passed:** every fragment line survived `olddefconfig`, and moby `check-config.sh` reports all 34 "Generally Necessary" items enabled. The optional items missing are the same as on x86_64: SCTP, AppArmor, btrfs, ZFS. The i8042 Ctrl+Alt+Del fragment is x86_64-only, because Firecracker's SendCtrlAltDel is x86-only.
- **Booting it needs an arm64 host.** KVM only runs the host's arch, and there is no emulation path. The arm64 rootfs must also be built on an arm64 host.

### Deferred (VM work runs on a separate VM host, not the dev machine)

1. **Rebuild both images at the final HEAD**, then `make release` and commit the manifests. The committed manifests bake agent `m1-71-gdf80ed9`, built before the final rebase.
   - Main changed no guest-protocol code since (`internal/guestproto`, `guestagent` and `guestclient` are identical, and agentinfo at HEAD prints the same protocol 1–1 and features). So the skew guard grades these manifests CompatOK against this HEAD, and the daemon accepts them.
   - They still don't satisfy "the baked agent matches main's commit".
2. **`TestReleaseInstallAndBoot`** with `DAVIT_TEST_FIRECRACKER=1`, run against the rebuilt release. It has never been run.
3. **Boot the arm64 kernel** on an arm64 host; also build and smoke an arm64 rootfs there.
4. **Re-measure** boot and barrier timings on a quiet host.

## Previous generation: guest agent baked in, harness image, Ctrl+Alt+Del stop (superseded)

Measured on 2026-10-22, four smoke runs between 06:55 and 06:58 UTC (3× base, 1× harness).

| Manifest | id | kernel | rootfs | agent |
|---|---|---|---|---|
| `ubuntu-24.04-base` | `sha256:2c8a5be4c12d…` | `0cd311da…` | `bb263873…` (1.0 GiB allocated of 4 GiB sparse) | `366434e2…` |
| `ubuntu-24.04-harness` | `sha256:8b4042346fdd…` | `0cd311da…` | `1079239e…` (1.9 GiB allocated) | `366434e2…` |

The agent is `davit` `m1-11-ga645631` (clean tree, go1.26.8, 35 MB static).

### Reproducibility

Build B ran `build.sh` from **empty** work, out and manifest dirs. Build A ran in the normal work dir with a cached builder. Both produced byte-identical manifests for both images: kernel, agent, both rootfs images, the package lists and the harness CLIs all matched. Same host for both builds; cross-host is still untested.

| Build | Wall time | Phases |
|---|---|---|
| B (empty dirs, load ≈ 20) | 26 min 44 s | downloads + builder 3.6 min, kernel 15.7 min, agent 3 s, base rootfs 3.3 min, harness rootfs 3.8 min |
| A (cached builder; its driver was killed mid-run, so the remaining steps ran as the same `build.sh` subcommands) | — | kernel 12.9 min, agent 4 s, base rootfs 3.3 min, harness rootfs 4.7 min |

### Conditions: heavily loaded host

Same hardware as below: an AMD Ryzen 7 6800U laptop (16 threads, 27 GiB RAM, btrfs at 96% full, Linux 7.2), shared with other jobs, builds at nice 15 / idle I/O.

**The 1-minute load average was 22–30 during these runs**, versus 5–13 for the previous generation, because other jobs were running Go builds, end-to-end daemon tests and their own Firecracker VMs.

Measurements that don't involve the agent slowed down by the same factor, which shows the load is the cause:
- cold boot → SSH banner: 2.1–4.2 s, previously 1.0–1.1 s
- snapshot create: 4.0–6.6 s, previously 0.6 s

Treat every number below as an upper bound for this hardware. Guest: 2 vCPU, 1 GiB.

### Boot, barrier, restore, stop (ms)

**Barrier ready** means `guestclient.Prober.Barrier` returned: the product's own host code started with the VM, it authenticated over vsock, set the clock, assigned hostname and identity (rotating it for new VMs and forks), and readiness confirmed agent + sshd. That is the point at which the product would admit SSH or exec. **Barrier only** is the same call timed inside the probe.

| Metric | n | min | median | max |
|---|---|---|---|---|
| Cold boot → SSH banner | 4 | 2135 | 2498 | 4162 |
| **Cold boot → barrier ready** | 4 | 2267 | 3278 | 4797 |
| Restore → VM resumed | 12 | 26 | 56 | 171 |
| Restore → SSH banner (old identity, not yet admitted) | 12 | 62 | 141 | 779 |
| **Restore (fork) → barrier ready** (new machine-id + host key, clock stepped) | 12 | 285 | 557 | 1383 |
| Barrier alone, on a fork | 12 | 233 | 472 | 1265 |
| Guest clock after barrier vs host (snapshot aged 10–14 s) | 12 | −10.1 | −2.1 | +1.3 |
| **Ctrl+Alt+Del → Firecracker exited 0** | 4 | 516 | 593 | 1733 |

For context only, from one development run at load 8–11, with the same barrier and boot code but a pre-review dirty-tree agent: cold boot → barrier ready 1318 ms (n=1), and fork → barrier ready 228–343 ms (n=3). That's the order of magnitude to expect on a quiet host. Re-measure there before quoting it.

Notes:
- **Barrier vs banner:** on forks the barrier adds roughly 150–450 ms over the SSH banner at this load. It rotates the ed25519 host key, writes the sshd drop-in, restarts `ssh.service` and `systemd-journald`, then waits until sshd answers again. Which step dominates was not instrumented.
- **`ssh true` can beat barrier-ready:** it sometimes succeeds first, against the old identity. Only the barrier-ready time counts for the product, whose gateway admits SSH only after the barrier.
- **Ctrl+Alt+Del:** the stop runs after the test containers have been removed. A busybox `httpd` running as PID 1 ignores SIGTERM and would add Docker's 10 s stop timeout (seen in a dev run). Every run proved a clean shutdown: a write made without `sync` was on the disk afterwards.

### Gates passed in all four runs

Every gate in README "Smoke test", including:
- **Agent running as root in the real guest:** the baked binary equals the manifest digest, and the handshake version equals the manifest (`m1-11-ga645631`, protocol 1). The barrier completed on the cold boot and on 3 forks per run. Each fork's machine-id and host key differed from the source and from every earlier fork. The second session authenticated with the rotated secret. Exec ran as `uid 0` and as the default user `davit`. The identity stamp and the kernel hostname were set.
- **Stop:** `SendCtrlAltDel` → clean shutdown → Firecracker exit code 0.
- **Harness image:** through the agent's exec and login-shell path (internal/harness's exact argv), `claude --version` printed `2.1.308 (Claude Code)` and `codex --version` printed `codex-cli 0.163.0`. Neither CLI was run against a model, because that needs credentials.
- **Everything from the previous generation still passes:** Docker networking without the raw-table workaround, kernel features, check-config, snapshot/restore and disk state.

## Previous image generation: ubuntu-24.04-base without a baked agent (superseded)

Measured on 2026-10-22: four smoke runs. Raw per-run JSON is written to `images/results/` locally and is not committed.
Committed manifest: `sha256:c45215c1a71f1fb646e2ece72bd98af1205ef574ecfd2426507294366a721999`.
- kernel `sha256:9ddd0882…`, rootfs `sha256:affec2d6…`
- Runs 1–3 used manifest `890ab98f…`. It has the **same kernel and rootfs blobs**; only its recipe digests differ, from before the default work dir moved out of the source tree. Run 4 used the committed manifest.

### Hardware and conditions

| | |
|---|---|
| Host | AMD Ryzen 7 6800U laptop (8C/16T), 27 GiB RAM shared with other jobs, Linux 7.2.5 (Arch Linux), THP `always` |
| Storage | btrfs (`compress=zstd:3`) at 93–94% full; disk copies are reflinks |
| Load | loadavg 5–13 across the smoke runs; other jobs were building and running VMs at the same time |
| Harness | Every build and VM ran under a low-priority wrapper (nice 15, idle I/O, CPUWeight 20), so these numbers are pessimistic |
| VMM | Firecracker v1.17.0 (official static release) |
| Guest | 2 vCPU, 1024 MiB. Kernel `6.18.58-slipway`, Ubuntu 24.04.5, Docker CE 29.9.2, rootfs is a 4 GiB sparse ext4 (≈1 GB allocated) |

### Boot and restore (ms)

Banner means sshd's `SSH-2.0` banner arrived over the guest's virtio-net/TAP path. `ssh true` means a full OpenSSH login and command over the same path, which adds 123–174 ms over the banner (client process spawn plus key exchange).

| Metric | n | min | median | max |
|---|---|---|---|---|
| **Cold boot → SSH banner** | 4 | 1036 | 1085 | 2093 |
| **Cold boot → `ssh true`** | 4 | 1177 | 1226 | 2255 |
| Snapshot create (pause + 1 GiB mem file + disk reflink) | 4 | 580 | 687 | 2386 |
| Restore → Firecracker API up | 12 | 20 | 29 | 40 |
| Restore → VM resumed (`snapshot/load`) | 12 | 26 | 37 | 49 |
| **Restore → SSH banner** | 12 | 53 | 69 | 260 |
| **Restore → `ssh true`** | 12 | 178 | 207 | 405 |

Notes:
- **Cold boot includes first-boot work.** That is machine-id generation and SSH host-key generation (ed25519 + ecdsa, 60 ms on the critical chain). dockerd and containerd start in parallel.
  - The guest reports `Startup finished in` 190–262 ms (kernel) + 1.58–2.32 s (userspace). That userspace time includes dockerd, which is off the ssh path.
  - Critical chain: `ssh.service` was ready at about 0.81 s after kernel start. The slowest link on it was `systemd-resolved` (+202 ms).
- **The 2.2 s cold boot is an outlier.** It was the first run, started immediately after a 15-minute clean build. The likeliest cause is page cache and CPU contention, but it wasn't investigated. The other three runs were 1.18–1.23 s.
- **First restore of a run vs later restores.** The first restore reads the 1 GiB mem file from a cold page cache: its banner took 54, 73, 212 and 260 ms. Every later restore in a run took 53–87 ms.
- **The 2.4 s snapshot create** was one run where the 1 GiB fsync landed on a busy, 94%-full btrfs. The other three took 580–790 ms.
- **Comparison with the spike** (384 MiB guest, CI kernel 6.1): spike cold boot → banner was 734–987 ms; restore → banner was 44–73 ms; restore → `ssh true` was 176–235 ms (over vsock). These guests have 2.7× the RAM, a newer kernel and a fuller Docker stack. Restore is in the same range; cold boot is about 50–350 ms slower. The runs differ in RAM, kernel, rootfs and network path, so this isn't an A/B of any single change.
- **Docker in the guest:** offline `docker build` (BuildKit, `FROM scratch`) took 638–968 ms, and the first `docker run` took 404–657 ms (n=4).

### What the smoke proved (all four runs, every gate)

Full list in `README.md` "Smoke test". Highlights:
- **Docker networking works with no `DOCKER_INSECURE_NO_IPTABLES_RAW`**, using iptables 1.8.10 on the `nf_tables` backend:
  - default-bridge `docker run`
  - container egress through MASQUERADE (the host listener saw `172.16.0.2`)
  - a published port reached from the host over the TAP (PREROUTING DNAT and FORWARD)
  - Docker's raw-table DROP rule present (`-A PREROUTING -d 172.17.0.2/32 ! -i docker0 -j DROP`)
  - user-defined network DNS
  - `docker info` reports no warnings (`overlayfs` driver, cgroup v2)
  - after restore, the published container still served to the host
- moby `check-config.sh`, run against the **running** kernel's `/proc/config.gz`: every "Generally Necessary" item is enabled. Optional items still missing: `IP_SCTP`, `SECURITY_APPARMOR`, `BTRFS_FS` (+`_POSIX_ACL`), and ZFS (userspace).
- nft inet table with set, fib, ct and masquerade; VXLAN; IPVS; FUSE; overlayfs; guest→host vsock; virtio-balloon bound with free-page reporting negotiated; VMGenID (`VMGENCTR:00`) bound.
- Informational: Docker's native **nftables firewall backend** (`firewall-backend: nftables`) also worked on this kernel. It created `ip`/`ip6 docker-bridges` tables and the container ran.
- Informational: a restored VM shares the source's `boot_id`, machine-id and host keys, as SPIKE predicted. The guest agent has to repair them per fork.

### Build and reproducibility

| Build | Inputs | Wall time | Kernel | Rootfs |
|---|---|---|---|---|
| 1 (pre-review recipe) | warm downloads | n/a (kernel post-processing finished by hand after an interrupted run) | `9ddd0882…` | `f0286b3c…` |
| 2: `build.sh`, empty work/out dirs (pre-review recipe) | none cached | 17 min 40 s | `9ddd0882…` | `f0286b3c…` |
| A: `build.sh` (post-review recipe) | downloads cached; builder rebuilt | 13 min 44 s | `9ddd0882…` | `affec2d6…` |
| B: `build.sh`, empty work/out dirs (post-review recipe) | none cached | 14 min 44 s | `9ddd0882…` | `affec2d6…` |
| C: `build.sh`, **committed recipe**, work dir moved to `~/.cache` | downloads cached; builder rebuilt | 13 min 26 s | `9ddd0882…` | `affec2d6…` |

- **Builds 1 and 2 were byte-identical, and so were A, B and C.** A and B produced the *same manifest file*: same id, so same kernel, rootfs, config, `modules.builtin` and package list.
- C reproduced A and B's blobs from a different scratch path, so the output doesn't depend on where the work dir lives.
- The kernel didn't change between recipes. The review fixes only re-asserted config values that were already set.
- The rootfs changed because sealing now removes the build host's resolver backup (see the README).
- All four builds ran on the same host. **Cross-host reproducibility has not been tested.** The toolchain is pinned in the builder, but extended attributes from the build filesystem are a known way it could differ (see the README).
- About two-thirds of the wall time is the kernel at `-j4`. The rootfs takes about 3 min. Building the builder takes about 1.5 min, and it is cached between builds.

Disk use (measured):
- outputs 1.0 GB: rootfs 990 MiB allocated of a 4 GiB sparse file, plus a 29 MiB kernel
- downloads 191 MB, builder 542 MB, Firecracker 24 MB
- the kernel and rootfs trees are deleted after each step (peak not measured)

A smoke run takes about 30 s.
