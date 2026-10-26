# Spike: microVM backend for a self-hosted boat.dev alternative

Date: 2026-10-22. A throwaway spike, run outside this repository; its scripts are kept in [spike-scripts/](spike-scripts/).
Everything ran rootless: no sudo, and no system installs.

## Setup

| | |
|---|---|
| Host | AMD Ryzen 7 6800U laptop, Linux 7.2.5 (Arch Linux), 16 threads, 27 GiB RAM shared with other jobs (loadavg 1.7–6.2 during runs), swap in use, host THP=`always` |
| Scratch FS | **btrfs** (`compress=zstd:3`, 93% full), so **`cp --reflink=always` works** |
| Device perms | `/dev/kvm`, `/dev/net/tun`, `/dev/vhost-vsock` are all 0666; `kernel.unprivileged_userns_clone=1`; `vm.unprivileged_userfaultfd=0` |
| VMMs | Firecracker **v1.17.0**, Cloud Hypervisor **v54.0** (official static release binaries) |
| Guest kernel | Firecracker CI `vmlinux-6.1.155` (PVH, so the same image boots on both VMMs) |
| Guest rootfs | Ubuntu 24.04.5 (ubuntu-base) + systemd + openssh-server + docker.io 29.1.3 + socat, 4 GiB sparse ext4 (492 MiB allocated), built rootless in ~45 s |
| VM shape | 2 vCPU, **384 MiB** for every VM, so 10 forks stay under the 4 GiB cap (sum of configured guest RAM ≤ 3.84 GiB at all times) |
| Harness | Everything ran under a low-priority wrapper (nice 15, idle I/O, CPUWeight 20), so the numbers are **pessimistic** on a busy, shared machine |

"Banner" means the moment sshd's `SSH-2.0` banner arrives over vsock: the guest is ready to accept SSH.
"ssh" means a full `ssh root@vm true` returned. About 140 ms of that is client-side: spawning the Python vsock ProxyCommand plus the key exchange. A host SSH gateway that keeps the proxy in-process would cut most of it.

## Numbers (ms unless noted; ranges are across all samples from 2–3 independent runs)

| Metric | Firecracker 1.17 | Cloud Hypervisor 54 |
|---|---|---|
| **(a) Cold boot → SSH banner** (systemd, sshd, dockerd) | 734 – 987 (n=6) | 673 – 708 (n=3) |
| (a) Cold boot → `ssh true` via vsock | 883 – 1132 | 839 – 848 |
| (a) Cold boot → `ssh true` via tap/virtio-net | 996 – 1265 | 1956 – 2059 (\*) |
| **(b) Snapshot create** (pause + full mem file 384 MiB, fsync) | 245 – 298 (one outlier at 736) | 161 – 543 |
| (b) Pause VM | < 1 | ~2 |
| (b) Disk snapshot (reflink of 4 GiB image) | 6 – 9 | 7 – 9 |
| (b) Same snapshot with `sync_snapshot_files:false` | 524 – 1450 (no gain observed, see notes) | n/a |
| (b) Snapshot size on disk | 384 MiB mem + 26 KiB vmstate; the disk is shared via reflink | same |
| **(c) Restore → VM resumed** (FC API up + `snapshot/load`) | 23 – 35 | n/a (single CLI call) |
| **(c) Restore → SSH banner** | **44 – 73** | **59 – 80** (`copyonwrite`) / 293 – 719 (`copy`) |
| (c) Restore → `ssh true` | 176 – 235 | 191 – 218 (`copyonwrite`) / 417 – 846 (`copy`) |
| **(d) Fork N=1** (wall-clock until all forks pass `ssh true`) | 196 – 202 | 175 – 214 (cow) / 481 (copy) |
| **(d) Fork N=5** | 217 – 248 (median per-VM 202 – 235) | 224 (cow) / 1131 (copy) |
| **(d) Fork N=10** | **305 – 480** (median 265 – 366) | 448 (median 408) (cow) |
| (d) Per-fork disk cost at fork time | 0 bytes (reflink, 2 – 9 ms) | same |
| **(e) Idle forked VM, private memory** (anon dirty) | **13 – 17.5 MiB** | 17 MiB (cow) / 386 MiB (copy) |
| (e) Idle forked VM, PSS at N=10 | **23 – 28 MiB** (10 VMs = 233 – 283 MiB total) | 32 MiB (320 MiB total) |
| (e) Idle forked VM, RSS | 105 – 145 MiB (≈ 90 – 130 MiB of it is the *shared, clean* page cache of the mem file) | 161 MiB |
| (e) Idle **cold-booted** VM, RSS | 360 MiB (≈ the full 384 MiB) | 371 MiB |

(\*) CH guest virtio-net-pci came up late under systemd-networkd. vsock on CH was fine; not investigated further.

Correctness checks that passed on every run:
- Docker works in the guest (`docker run` of a locally imported image prints `docker-ok`).
- A file written in the source before the snapshot is visible in every restore.
- A file written in fork 0 is **not** visible in fork 1: disks are isolated.
- SSH over the tap/virtio-net path works in every fork (10/10), not just over vsock.

## Findings that shape the design

1. **Fork cost is restore cost.** Firecracker's `File` memory backend mmaps the snapshot memory file `MAP_PRIVATE`. N forks share the clean pages through the host page cache, and each fork pays only for the pages it dirties: about 15 MiB per idle fork. The shared part is clean file cache, so the host can reclaim it under pressure. CH v54's `memory_restore_mode=copyonwrite` gives the same result. CH's default `copy` mode is roughly 5–10× slower and costs the full RAM.
2. **Cold-booted VMs cost their full RAM.** The guest fills free RAM with page cache, and host THP=`always` turns each touch into a 2 MiB fault. Balloon free-page reporting did **not** help here (RSS 361 vs 360 MiB) because the memory isn't free, it's cache. Options for density: drop caches or inflate the balloon before snapshotting, set THP=`madvise` on hosts, or always serve "new VM" from a golden snapshot instead of a cold boot. The last option is what makes "VM in a second" true anyway.
3. **"Stop = snapshot" scales with RAM.** It took about 250–300 ms for 384 MiB, mostly writing the memory file, so expect roughly 3 s for a 4 GiB VM. Firecracker diff snapshots (`track_dirty_pages`) would shrink repeated stops. Not measured. `sync_snapshot_files:false` was *slower* in all 3 runs. It always ran second, right after a 384 MiB fsync on this 93%-full btrfs, so it is confounded and not conclusive.
4. **Snapshots carry identity, so forks need a post-restore guest agent over vsock** (measured with `scripts/fork-identity.py`):
   - The guest clock is stale by the snapshot's age (−53 s after 53 s). TLS and cron need a step: a vsock agent sets the time, or chrony with `makestep` plus `ptp_kvm`.
   - `machine-id`, SSH host keys and `boot_id` are **identical across forks**. Regenerate them per fork (host keys matter for an SSH gateway's known_hosts).
   - VMGenID is present and the kernel RNG reseeds: `/proc/sys/kernel/random/uuid` differs per fork. Good.
5. **No per-fork re-IP needed.** Every VM runs in its own network namespace with an identical `tap0` / 172.16.0.2. Restores need no `network_overrides`, which is Firecracker's documented "network for clones" pattern.
6. **Relative paths make forking trivial without root.** Drive (`rootfs.ext4`) and vsock (`v.sock`) paths are relative and resolved against each VMM process's cwd, so one snapshot restores into N directories, each with its own reflinked disk. In production the jailer's chroot does the same thing.
7. **Guest kernel needs a custom build for Docker.** The FC CI kernel lacks `CONFIG_IP_NF_RAW` and nftables. Docker 29's bridge networking failed until `DOCKER_INSECURE_NO_IPTABLES_RAW=1` was set (it's in the rootfs as a drop-in). A product kernel should enable `IP_NF_RAW`, `NF_TABLES`, `NFT_*`, `VXLAN`, `IP_VS`, `FUSE_FS` (missing), and BTRFS/overlay as needed.
8. **SSH readiness is about 50 ms after restore.** The rest is client handshake. A host-side SSH gateway (`ssh vm-name@host` → per-VM vsock UDS → guest :22) needs no guest network at all and works the instant the VM resumes.

## What needed root (and what didn't)

| Thing | Root? | Notes |
|---|---|---|
| Boot / snapshot / restore microVMs | No | `/dev/kvm` is 0666 on this host. On Ubuntu/Debian hosts the user needs the `kvm` group. |
| vsock (host↔guest SSH, exec, agent) | No | FC and CH both implement "hybrid vsock" as a host Unix socket (`CONNECT <port>\n`), so `/dev/vhost-vsock` isn't even used. |
| tap device + guest virtio-net | **No** | `unshare -rn` (user+net namespace), then `ip tuntap add tap0` works because `/dev/net/tun` is 0666 and the ns owner has CAP_NET_ADMIN. The host reaches guest :22 through a `socat UNIX-LISTEN:tcp22.sock → 172.16.0.2:22` bridge run inside the netns, because Unix sockets cross netns boundaries. Caveat: Ubuntu 24.04 hosts block unprivileged userns by default (`kernel.apparmor_restrict_unprivileged_userns=1`), so they'd need an AppArmor profile for the daemon or root. |
| Rootfs build (apt, setuid bits, root-owned files) | No | `unshare --map-auto -r` with `/etc/subuid` (100000:65536), chroot apt, then `mkfs.ext4 -d` inside the ns. Docker was *not* used: membership in the docker group is root-equivalent, and dockerd bypasses the wrapper's cgroup. |
| **Guest egress to the internet** | **Yes (not done)** | Neither `pasta`/passt nor `slirp4netns` is installed, and installing them is a system install. Rootless option: `pasta --netns <ns>` per VM (what rootless podman does). Root setup required otherwise, per the Firecracker clone-network pattern: |
| Firecracker jailer (chroot, cgroups, uid drop, seccomp) | Yes | Production should run the daemon as root (or with CAP_SYS_ADMIN/NET_ADMIN) and use the jailer for every VM. |
| UFFD lazy memory restore (FC `Uffd` backend, CH `ondemand`) | Yes | Needs `sysctl vm.unprivileged_userfaultfd=1` or access to `/dev/userfaultfd`. Not needed for these numbers: the File/COW backends are already this fast. |

Egress root setup (host, once, then per VM, with the VM in netns `$NS`):
```sh
sysctl -w net.ipv4.ip_forward=1
iptables -t nat -A POSTROUTING -s 10.200.0.0/16 -o <uplink> -j MASQUERADE
# per VM i: veth between host and the VM's netns, NAT the fixed guest IP out of it
ip link add veth$i-h type veth peer name veth$i-g netns $NS
ip addr add 10.200.$i.1/30 dev veth$i-h && ip link set veth$i-h up
ip -n $NS addr add 10.200.$i.2/30 dev veth$i-g && ip -n $NS link set veth$i-g up
ip -n $NS route add default via 10.200.$i.1
ip netns exec $NS iptables -t nat -A POSTROUTING -s 172.16.0.2 -o veth$i-g -j MASQUERADE
```

## Tailscale attach options (analysis only; nothing joined to any tailnet)

- **Host proxy (recommended default):** the control daemon embeds `tsnet` (or the host runs one tailscaled) and serves an SSH gateway that maps `vm-name` to that VM's vsock socket. No guest egress is needed, it's instant after restore, and no keys live in snapshots. `tsnet` can even present *each VM as its own tailnet node* (one `tsnet.Server` per VM, inside the host daemon), giving per-VM ACLs and MagicDNS names without touching the guest.
- **VM joins the tailnet (opt-in):** tailscaled runs in the guest and needs guest egress (above). Node state must **not** be captured in a shared snapshot: forks would clone the node key. Wipe `/var/lib/tailscale` before the golden snapshot, and have the post-restore agent run `tailscale up --authkey=<ephemeral, tagged>` per fork. That adds roughly 1–2 s of async join time after restore, so it isn't on the SSH-ready critical path.

## Recommendation for the Linux backend

**Firecracker as the primary backend. Keep a thin VMM interface so Cloud Hypervisor can be a second backend later.**

- Restore and fork are the headline, and FC is the fastest and most consistent there: resumed in 23–35 ms, SSH banner in about 50 ms, 10 parallel forks all SSH-able in 0.3–0.5 s, about 15 MiB private per idle fork. CH matches it only in the non-default `copyonwrite` mode.
- FC's snapshot API is the more mature fit for this product: the File backend (MAP_PRIVATE) is the default, the UFFD backend enables lazy or remote memory, and it has diff snapshots, `network_overrides`, `vsock_override`, VMGenID, and the jailer for multi-tenant isolation.
- CH booted cold about 0.2 s faster and has features FC deliberately lacks: PCI/VFIO passthrough (GPUs), virtio-fs, memory/CPU hotplug, and native qcow2. That's the reason to keep the abstraction, not to start there.
- **Storage:** raw ext4 images plus reflink on a reflink-capable FS (btrfs here; XFS `reflink=1` in production) cost 2–9 ms per fork and 0 bytes up front, with no dm/qcow layers. Make a reflink-capable image store a hard requirement, with dm-thin as the root-requiring fallback.
- **"New VM in a second"** should be "restore a golden snapshot" (about 50 ms), never a cold boot (about 1 s). Cold boot is only for building golden snapshots.
- **Must-build items surfaced by this spike:**
  1. A post-restore vsock agent for clock, machine-id, host keys and hostname.
  2. A custom guest kernel (netfilter raw/nft for Docker).
  3. A host SSH gateway over vsock.
  4. A network mode: rootless `pasta` per netns, or the root veth/NAT above.
  5. Diff snapshots so stop/start stays fast for big-RAM VMs.

## Reproduce

All paths are relative to the scratch dir, with the files from [spike-scripts/](spike-scripts/) under `scripts/`. Large artifacts were deleted afterwards. `fetch.sh` and `build-rootfs.sh` recreate them in about 1–2 min. `$HEAVY` is an optional low-priority command prefix (for example `nice -n 15 ionice -c 3`); leave it empty to run directly.

```sh
scripts/fetch.sh                                            # FC 1.17, CH 54, CI kernel 6.1.155, ubuntu-base 24.04.5 → dl/, bin/
$HEAVY scripts/build-rootfs.sh images/rootfs.ext4 4G   # rootless rootfs (also creates work/id_ed25519)
$HEAVY python3 -I scripts/fcspike.py all             # FC: 3 cold boots, snapshot, 3 restores, forks 1/5/10, memory
$HEAVY python3 -I scripts/fcspike.py all --balloon --boots 2 --restores 2 --forks 10 --idle 30 --out fc-results-balloon.json
$HEAVY python3 -I scripts/chspike.py --restore-modes copyonwrite --out ch-results-cow.json
$HEAVY python3 -I scripts/chspike.py --boots 1 --forks 1,5 --restore-modes copy --out ch-results-copy.json
$HEAVY python3 -I scripts/fork-identity.py          # needs run/snap from an fcspike run
# manual SSH into any running VM (vsock, no network):
ssh -i work/id_ed25519 -o ProxyCommand="python3 -I scripts/vsock-proxy.py ~/.cache/vmspike/<vm>/v.sock 22" root@vm
```

Files:
- `scripts/fetch.sh`: downloads.
- `scripts/build-rootfs.sh`: rootless image build.
- `scripts/fcspike.py`: Firecracker harness (per-VM netns + tap + vsock, timings, smaps memory).
- `scripts/chspike.py`: CH variant.
- `scripts/vsock-proxy.py`: ssh ProxyCommand for hybrid vsock.
- `scripts/fork-identity.py`: clock and identity check.
- `results/*.json` (written by the harnesses, not kept in this repository): raw output of every run, including per-VM timings and load averages.
