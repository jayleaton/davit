# Storage tiers: hot and cold storage for boxes

A host keeps every box's disk on its data filesystem, usually a fast SSD. Stopped boxes cost only disk, but
agents leave many of them behind, and a small SSD fills up. A **cold storage tier** is a second, larger and
slower local disk (a hard disk, or a big SATA SSD). The host moves stopped boxes that nobody used for a while
to it, and copies a box back when it is resumed. Nothing is lost; the first resume after a box went cold is
slower.

The cold tier is optional and off by default. Without it, a host works as described in
[vm-host.md](vm-host.md#disk-space-floor-reservation-and-auto-delete).

## Concepts

### Hot and cold

- **Hot** is the data dir's filesystem (`dataDir`). Everything runs from it.
- **Cold** is a directory on another local filesystem (`coldStorage.dir`). Only stopped VMs' committed disks
  go there. Nothing ever runs from it.

These always stay hot:

- images and their blobs, project images (their generations and goldens), and image builders;
- running, starting and stopping VMs;
- VMs used within `coldStorage.after` (default 6 hours);
- VMs with an open session, process stream, tunnel or prompt lease;
- VMs a request is queued for (a resume, or a create from their image);
- VMs whose disk another VM still boots from (a snapshot or fork made from them);
- ephemeral and no-snapshot VMs;
- VMs pinned hot (`box pin --hot`);
- VMs with less than `coldStorage.minBytes` (default 512 MiB) of their own data.

Any other stopped VM can go cold, box or not. Only boxes are ever auto-deleted.

### Going cold (demotion)

The lifecycle sweeper runs every 15 seconds. It moves stopped VMs unused for `coldStorage.after`, least
recently used first, **one at a time**, and only while the host is not busy:

- no VM is starting or stopping;
- nothing is queued for capacity;
- I/O pressure (Linux PSI, `/proc/pressure/io` "some avg10") is at or below `coldStorage.ioBusyPercent`
  (default 20).

A move reads at most `coldStorage.bytesPerSecond` (default 100 MiB/s), for the copy and again for the
read-back that verifies it. It runs in the idle I/O class, which only has an effect with the BFQ I/O
scheduler (`cat /sys/block/sdX/queue/scheduler`).

What moves: the VM's last committed disk, plus its named snapshots that no other VM needs. The stopped VM's
working disk is dropped, because its commit holds the same data.

A move is crash-safe. In order:

1. Copy the data into `<cold>/tmp`, fsync it and drop it from the page cache.
2. Read the copy back from the disk and compare its content digest with the source's.
3. Publish it: a rename into `<cold>/commits` and an fsync of the directory.
4. One journal transaction checks that the VM did not change meanwhile and records it as cold. A resume,
   delete, pin or new snapshot during the move aborts it.
5. Storage GC removes the hot copy.

A crash at any point leaves the hot copy, or both copies, and the daemon finishes or undoes the move when it
starts again. It never leaves neither. A `box up` of a box that is being moved cancels the move.

### Coming back (promotion)

`box up`, `box run`, `box ssh`, `box sync` or a plain `davit resume` of a cold box first copies its disk back
to hot storage, then boots it. stderr says so:

```text
box myproj/main: restoring from cold storage (5.1 GiB)…
box myproj/main: resuming (operation op_…)
  storage.restore       …% restoring from cold storage: 2.0 GiB of 5.1 GiB (39%)
```

- The restore reads the whole cold copy at the cold disk's read rate. It is not rate-limited. How long it
  takes depends on that disk and on the box's size.
  One measurement (2026-10-27, `TestFirecrackerColdTier`, one sample): a box with 1 GiB of its own random
  data, 1.9 GiB in its cold copy, restored in 9.7 s (about 200 MiB/s, 5.1 s per GiB, rebased onto its image);
  its `box up` took 12.2 s. Both tiers were loop-file XFS filesystems on the same virtual disk of a Proxmox
  VM (6 vCPUs, 19 GiB), not a hard disk, so expect a real HDD to be slower: roughly its sequential read rate.
- It checks the cold copy's digest while it reads. A damaged copy fails the resume
  (`cold copy of commit … is damaged`), and the box stays cold.
- Resume admission reserves the bytes to restore on hot, on top of the usual growth headroom. On a host low on
  disk the resume is refused, or queued with `--wait` like a create:
  `restoring from cold storage needs 5.1 GiB: host disk low: …`.
- After the resume the box is hot again. It goes cold again once unused for `after`.

`box pin --hot` on a cold box brings it back in the background, without booting it. So does draining the tier
(`coldStorage.drain`, [below](#removing-it)).

### What reflink gives, and what cold storage costs

On one XFS (`reflink=1`) or btrfs filesystem, a box's disk is a reflink clone of its image or snapshot. It
shares every unchanged block, so it costs only what the guest wrote. A copy to another filesystem cannot
share anything: it takes every allocated byte, image data included.

- **Moving a box frees on hot** about the box's own data: the blocks it does not share with the image or
  snapshot it started from. davit measures that with FIEMAP before it moves anything.
- **Moving a box costs on cold** its full allocated data: image data plus own data, and the same again for
  each named snapshot that moves with it.
- **Restoring writes on hot** only the box's own data, if its base (the project image generation it started
  from, or the image's root filesystem) is still on hot: the restore clones the base and writes only the
  blocks that differ. The resumed box shares with its image again.

This is why a box with little data of its own (`coldStorage.minBytes`) stays hot: moving it would free almost
nothing and only slow its next resume. Moves never clone on the cold filesystem (every cold copy is a full
copy), so reflink there is not required; doctor still recommends XFS `reflink=1` or btrfs, the same as hot,
so the disk can serve as a data dir later.

### The disk floor, the save reserve and auto-delete

The hot disk keeps its floor, growth reservation and save reserve as before
([vm-host.md](vm-host.md#disk-space-floor-reservation-and-auto-delete)). The save reserve is on hot only.
Stops always commit to hot, so they keep working whatever the cold disk does.

When the hot disk is below its floor, the cleanup goes in this order, cheapest loss first:

1. davit's own garbage;
2. idle boxes are stopped (reflink storage only);
3. **stopped VMs move to cold storage**, least recently used first. Under pressure the age is 10 minutes
   instead of `after`, and the host's busy-ness is ignored;
4. only then are unused hot boxes auto-deleted (`autoDelete.instanceMinAge`, `imageMinAge`). Cold boxes are
   skipped here: deleting them frees nothing on hot;
5. **under pressure**, when steps 1 to 4 left the hot disk short, the age limits are waived down to **1 hour**:
   step 3 again, its cold cleanup now deleting cold boxes least recently used first whatever their age, then
   step 4 for hot boxes unused for at least an hour. "Under pressure" means the hot disk is below its floor; a
   queued request's own cleanup above the floor never gets here.

While the tier drains (`coldStorage.drain`), step 3 is skipped.

The cold disk has its own floor (`coldStorage.minFreeBytes`, `coldStorage.minFreePercent`: by default the
larger of 5 GiB and 5%). A move never takes it below. A move that would first runs the **cold cleanup**: it
auto-deletes cold boxes, least recently used first, unused for at least `autoDelete.coldInstanceMinAge` (12
hours), until the move fits, and then the move goes ahead. In step 5 the cold cleanup goes on past that age, down
to boxes unused for an hour. The sweeper also runs it when the cold disk is below its floor. Their
`deleteReason` says `cold storage low` (and, from step 5, `under pressure` with the age limit it waived), and each
is a `vm.autodeleted` event. So the cold disk fills before anything is deleted, and the hot cleanup deletes only
what cannot go cold.

Neither cleanup ever deletes a box that is pinned, running or starting, has an operation in progress (a
resume restoring it from cold included), is being moved between the tiers, holds a session, process or
prompt lease, is the target of a queued request, or was used within the last hour. With
`autoDelete.enabled: false`, neither deletes anything.

### When the cleanup is stuck

A cleanup run that frees nothing is not repeated until a box changes (stopped, unpinned, deleted) or 10 minutes
pass, and it logs one warning, at most every 10 minutes while it stays stuck, naming what blocks it:

```text
WARN host disk low: cleanup freed nothing; retrying when a box changes, or in 10m freeBytes=… blockedBy="cold storage full: 7.9 GiB to move, 13.0 GiB free, floor 12.5 GiB, and no cold box may be deleted; 3 pinned (proj/a, proj/b, proj/c); 2 running (…); 1 used in the last 1h (…)"
```

Unpin (`box unpin`), stop or remove what it names, or add room.

### Pin and pin hot

| Command | VM field | Auto-deleted for disk space | Can go cold |
| --- | --- | --- | --- |
| (none) | | yes, once unused for days | yes |
| `box pin`, `box up --keep` | `keep` | never | yes |
| `box pin --hot` | `keep`, `keepHot` | never | never |
| `box unpin` | clears both | yes | yes |

A pinned box can still go cold: cold storage is where long-kept boxes belong. Agents pin liberally with
`box up --keep`. If a pin also kept a box hot, pinned boxes would fill the SSD for good, which is the problem
the cold tier solves. Use `box pin --hot` for the few boxes you resume often and want to start at once.

## Configuration

The tier is the `coldStorage` block of the daemon's config (`~/.config/davit/config.json` by default). The
config is strict: an unknown key stops the daemon. Any `coldStorage` setting without `dir` is an error. After
a change, restart the daemon: `systemctl --user restart davit`.

### coldStorage

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `dir` | absolute path | none: no cold tier | The cold directory. It must exist and be outside `dataDir`. Point it at a subdirectory of the cold disk's mount point, not the mount point itself ([why](#the-davit-directory)). |
| `after` | duration (`"6h"`, `"90m"`) | `"6h"` | How long a stopped VM must be unused before it moves. |
| `bytesPerSecond` | bytes per second | `104857600` (100 MiB/s) | Caps a move's reads: the copy, then the read-back that verifies it. Negative: unlimited. Restores are not capped. |
| `minBytes` | bytes | `536870912` (512 MiB) | The least data of its own a VM's disk must hold to move. Negative: any. |
| `minFreeBytes` | bytes | `5368709120` (5 GiB) | With `minFreePercent`, the cold disk's floor: the larger of the two. Negative: no floor. |
| `minFreePercent` | percent of the cold filesystem | `5` | At most 90. Negative: bytes only. |
| `ioBusyPercent` | percent | `20` | Moves wait while PSI I/O "some avg10" is above this. Negative: never wait. |
| `drain` | boolean | `false` | Move nothing more to cold storage, and bring every cold VM back to hot, one at a time, without booting it ([below](#removing-it)). |

Why the defaults:

- **`after` 6h.** A box stopped over lunch or between meetings stays hot and resumes at once. One stopped for
  the night or longer goes cold, and the next morning's first resume pays one copy back.
- **`bytesPerSecond` 100 MiB/s.** Below the sequential rate of most current hard disks, so the guests' own
  I/O keeps the disks' attention.
- **`minBytes` 512 MiB.** A smaller box would free almost nothing on hot.
- **Floor 5 GiB or 5%.** The hot disk's rule (larger of bytes and a percentage, the same two keys) at half the
  percentage: a cold disk is many times larger, and 10% of a 250 GiB disk (25 GiB) is three moves' worth left
  idle.

### Related keys

These existing keys guard the hot disk ([vm-host.md](vm-host.md#disk-space-floor-reservation-and-auto-delete)).

| Key | Default | Meaning |
| --- | --- | --- |
| `limits.minFreeDiskBytes` | `5368709120` (5 GiB) | The hot floor, with `minFreeDiskPercent`: the larger of the two. Negative turns disk admission and the cleanup off. |
| `limits.minFreeDiskPercent` | `10` | At most 90. |
| `limits.diskHeadroomPercent` | `25` | What admission reserves for a starting VM's growth, as a share of its disk size: 2 GiB for an 8 GiB disk. |
| `limits.saveReserveBytes` | `2147483648` (2 GiB) | The preallocated file in the data dir that stops can use. Negative: none. |
| `autoDelete.enabled` | `true` | Off: neither cleanup deletes boxes. |
| `autoDelete.instanceMinAge` | `"72h"` | How long a stopped instance on hot must be unused before it may be auto-deleted. |
| `autoDelete.coldInstanceMinAge` | `"12h"` | The same for an instance on cold, which the cold cleanup deletes to make room for the next move. |
| `autoDelete.imageMinAge` | `"24h"` | The same for a project image with no instances. |
| `autoDelete.marginBytes` | `2147483648` (2 GiB) | How far above a floor a cleanup frees before it stops. |
| `autoDelete.blobMinAge` | `"1h"` | How long an unreferenced image blob must be unchanged before it is removed. |
| `firecracker.goldenMinFreeBytes` | `10737418240` (10 GiB) | Goldens are evicted to keep this much free on hot. |

### A full host config

```json
{
  "dataDir": "/home/you/.local/share/davit",
  "socket": "/run/user/1000/davit/davit.sock",
  "runtime": "firecracker",
  "storage": "reflink",
  "limits": {
    "memoryBytes": 19327352832,
    "memoryOvercommit": 1.5,
    "minFreeDiskBytes": 5368709120,
    "minFreeDiskPercent": 10,
    "diskHeadroomPercent": 25,
    "saveReserveBytes": 2147483648
  },
  "autoDelete": {
    "enabled": true,
    "instanceMinAge": "72h",
    "coldInstanceMinAge": "12h",
    "imageMinAge": "24h",
    "marginBytes": 2147483648,
    "blobMinAge": "1h"
  },
  "coldStorage": {
    "dir": "/mnt/davit-cold/davit",
    "after": "6h",
    "bytesPerSecond": 104857600,
    "minBytes": 536870912,
    "minFreeBytes": 5368709120,
    "minFreePercent": 5,
    "ioBusyPercent": 20,
    "drain": false
  },
  "firecracker": {
    "goldenMinFreeBytes": 10737418240
  }
}
```

JSON has no comments, so line by line:

- `dataDir`, `socket`, `runtime`: keep what `davit init` wrote. The data dir is the hot tier.
- `storage`: `reflink` requires XFS `reflink=1` or btrfs on the data dir; `""` (the default) probes.
- `limits.memoryBytes`, `memoryOvercommit`: memory admission, unrelated to storage
  ([vm-host.md](vm-host.md#capacity-running-vs-parked-boxes)).
- `limits.minFreeDiskBytes`, `minFreeDiskPercent`: the hot floor, here the defaults.
- `limits.diskHeadroomPercent`: growth reserved per starting box.
- `limits.saveReserveBytes`: room kept for stops, on hot.
- `autoDelete`: the defaults. Both the hot and the cold cleanup use them.
- `coldStorage.dir`: a subdirectory on the cold disk, owned by the daemon's user.
- `coldStorage.after` … `ioBusyPercent`: the defaults, written out.
- `coldStorage.drain`: `false`; set `true` to empty the tier.
- `firecracker.goldenMinFreeBytes`: the golden floor on hot, the default.

Every value above except `dataDir`, `socket`, `runtime`, `memoryBytes`, `memoryOvercommit` and
`coldStorage.dir` is the default, so a real config needs only the `dir` line to turn the tier on.

## Example setups

Each example is a whole config. Keep your own `dataDir`, `socket`, `runtime` and `limits` and add only the
`coldStorage` block.

### A single SSD (no cold tier)

Today's default. Every box stays on the data disk, and the low-disk cleanup auto-deletes unused boxes when it
runs out.

```json
{
  "dataDir": "/home/you/.local/share/davit",
  "socket": "/run/user/1000/davit/davit.sock",
  "runtime": "firecracker"
}
```

- **Sizing:** [the sizing section](#sizing) without the cold part. Everything you keep must fit on the SSD.
- **Trade-off:** every resume is fast. Old boxes are deleted, not moved, when the disk is low.

### SSD hot and HDD cold, in a Proxmox VM

Our own host: a Proxmox VM whose data dir (`~/.svm/davit`, from `make host-install`) is on an SSD-backed
virtual disk of 80 GiB, with a second virtual disk of 250 GiB on HDD storage, mounted at `/mnt/davit-cold`.

```json
{
  "dataDir": "/home/you/.svm/davit",
  "socket": "/home/you/.svm/davit/davit.sock",
  "runtime": "firecracker",
  "limits": {"memoryBytes": 19327352832, "memoryOvercommit": 1.5},
  "coldStorage": {"dir": "/mnt/davit-cold/davit"}
}
```

- **Sizing:** worked out [below](#worked-example-80-gib-ssd-and-250-gib-hdd).
- **Trade-off:** a box resumed the morning after costs one copy back from the HDD.
- **Preparing the disk:** [Proxmox](#proxmox).

### Bare metal: NVMe hot and SATA HDD cold

The same shape on real hardware: the data dir on the NVMe, the hard disk formatted XFS and mounted at
`/mnt/davit-cold`.

```json
{
  "dataDir": "/home/you/.local/share/davit",
  "socket": "/run/user/1000/davit/davit.sock",
  "runtime": "firecracker",
  "coldStorage": {"dir": "/mnt/davit-cold/davit", "after": "6h", "bytesPerSecond": 104857600}
}
```

- **Sizing:** the cold disk is usually many times the NVMe. With the default cold floor (5%), a 2 TB disk keeps
  about 93 GiB free.
- **Trade-off:** moves are slow but paced and in the idle I/O class. Set the HDD's scheduler to BFQ for the
  idle class to work: `echo bfq | sudo tee /sys/block/sdX/queue/scheduler` (persist it with a udev rule).
- With nothing else on the HDD, `ioBusyPercent` can stay at the default. If other services use it, lower
  `bytesPerSecond`.

### NVMe hot and a big SATA SSD cold

The cold disk is fast to read, so a cold resume costs much less, and boxes can go cold sooner.

```json
{
  "dataDir": "/home/you/.local/share/davit",
  "socket": "/run/user/1000/davit/davit.sock",
  "runtime": "firecracker",
  "coldStorage": {"dir": "/mnt/davit-cold/davit", "after": "2h", "bytesPerSecond": 262144000}
}
```

- `after` `2h`: a box unused for two hours goes cold.
- `bytesPerSecond` 250 MiB/s: an SSD takes a faster move without slowing the guests as much as an HDD would.
  Keep it below what the SATA SSD writes sustained.
- **Trade-off:** more moves, so more writes to the cold SSD. Enable `fstrim.timer` ([below](#trim)).

### Cold storage on a NAS or network share

Not supported: the daemon refuses an NFS, SMB/CIFS, 9p, Ceph or FUSE directory at start
([Network storage](#network-storage)). A network block device formatted XFS on the host works.

## Network storage

`coldStorage.dir` must be a local filesystem. The daemon checks the filesystem type at start and refuses NFS,
SMB/CIFS, 9p, CephFS, FUSE and AFS, with `cold storage DIR is on a network filesystem (…): not supported`. The
daemon still starts; the tier is reported unavailable ([below](#when-the-cold-disk-is-missing)).

Why:

- **Durability.** A move is safe only if a completed fsync and rename are on the disk. Network filesystems
  acknowledge them under their own rules (close-to-open consistency, server write-back caches, reconnects
  that replay or drop requests). A crash or a network drop could then lose the only copy.
- **Server-side caching.** The read-back that verifies a copy must read the disk, not a cache. davit drops
  the local page cache; it cannot drop a server's.
- **No local reflink and no FIEMAP.** davit measures shared blocks and clones bases with local filesystem
  calls that network filesystems do not offer or report differently.
- **Free space.** The floor and the cold cleanup trust `statfs`. On a share with quotas, thin pools or other
  users, it does not tell the truth about what davit may use.

The supported alternative is a **block device over the network, formatted locally**: an iSCSI LUN, a Ceph RBD
image, or a Proxmox virtual disk on network-backed storage. The host sees a disk, formats it XFS and mounts
it; to davit it is a local filesystem. Its speed is that of the network and the storage behind it.

## Preparing a cold disk

The commands below need root once. davit itself never runs them.

### Find the disk

```sh
lsblk -o NAME,SIZE,TYPE,ROTA,MOUNTPOINTS,MODEL
```

`ROTA` 1 is a rotating disk. Pick the empty disk (`/dev/sdX` below). Formatting erases it; check twice.

### Format it

XFS with reflink (the default in xfsprogs 5.1 and later; `-m reflink=1` makes sure):

```sh
sudo mkfs.xfs -m reflink=1 -L davit-cold /dev/sdX      # the whole disk
# or a partition: sudo mkfs.xfs -m reflink=1 -L davit-cold /dev/sdX1
```

btrfs works too: `sudo mkfs.btrfs -L davit-cold /dev/sdX`.

### Mount it at boot

Add one line to `/etc/fstab`, by label (or `UUID=…`, from `sudo blkid /dev/sdX`):

```text
LABEL=davit-cold  /mnt/davit-cold  xfs  defaults,nofail,x-systemd.device-timeout=30s  0  0
```

- `nofail`: the host boots without the disk. davit then starts without its cold tier and says so
  ([below](#when-the-cold-disk-is-missing)).
- `x-systemd.device-timeout=30s` (optional): how long boot waits for the disk to appear.
- No `discard` option ([Trim](#trim)).

```sh
sudo mkdir -p /mnt/davit-cold
sudo systemctl daemon-reload
sudo mount /mnt/davit-cold
xfs_info /mnt/davit-cold | grep -o 'reflink=[01]'      # reflink=1
```

### The davit directory

Create a directory on the disk, owned by the user that runs `davit.service`, and point `coldStorage.dir` at
it:

```sh
sudo install -d -o "$USER" -g "$USER" -m 0700 /mnt/davit-cold/davit
```

Use a subdirectory, not the mount point. When the disk is not mounted, the mount point is still there: an
empty directory on the root filesystem. A subdirectory is then absent, and the daemon refuses it. With the
mount point itself, a daemon whose tier is new would adopt that empty directory and fill the root disk. The
mount point is also owned by root.

On its first start with the tier, the daemon writes `davit-cold.json` (a random ID) into the directory and
records the same ID in `<dataDir>/storage/cold.json`. From then on it accepts only a directory with that
marker.

### Trim

Freed blocks go back to the storage below only when the filesystem tells it, with TRIM. Use the weekly timer,
not the `discard` mount option, which trims on every delete and slows deletes down:

```sh
sudo systemctl enable --now fstrim.timer
```

On a hard disk it changes nothing. On an SSD it keeps write speed up, and on thin storage (an LVM-thin pool, a
thin virtual disk) it is how deleted boxes give the space back.

### Proxmox

On the Proxmox node, add a new virtual disk to the VM. This makes a 250 GiB volume on the storage named
`general`:

```sh
qm config <vmid> | grep -E '^(scsi|virtio|sata)[0-9]|^scsihw'   # used disk slots, and the controller
qm set <vmid> --scsi2 general:250,discard=on,iothread=1
```

- **Slot.** Pick a free one: `scsi0` is usually the boot disk, and on our host `scsi1` is the data disk.
- **`iothread=1`** gives the disk its own I/O thread. It needs the `virtio-scsi-single` controller
  (`scsihw: virtio-scsi-single`).
- **`discard=on`** passes the guest's TRIM to the storage. Keep it on thin storage, so `fstrim.timer` returns
  space to the pool.
- **`ssd=1`** only when the backing storage is an SSD. It tells the guest the disk does not rotate; on an HDD
  it would be a lie.
- **Format.** LVM-thin, ZFS and RBD storages make raw volumes. On directory storage choose
  `general:250,format=raw`: qcow2 adds a copy-on-write layer under XFS's own, which costs I/O and makes
  space harder to follow. Prefer raw.
- **Thin pools** can be over-provisioned: the virtual sizes add up to more than the pool, and a full pool
  stops every VM on it. Watch it as [vm-host.md](vm-host.md#running-the-host-as-a-vm-proxmox) describes.
  Directory storage of raw files has the same risk with sparse files.

In the guest the disk usually appears at once. If `lsblk` does not show it, rescan:

```sh
echo '- - -' | sudo tee /sys/class/scsi_host/host*/scan
lsblk
```

Then format and mount it as above.

## Sizing

A plan, not a promise: what a box writes decides it.

**Hot** must hold:

- the hot floor: the larger of `limits.minFreeDiskBytes` and `limits.minFreeDiskPercent` of the disk;
- the save reserve (`limits.saveReserveBytes`);
- the images, project images and their goldens (about 2 GiB per golden of the default shape);
- the own data of every box used within `after`, running or not, and of boxes below `minBytes`;
- the growth reservation of boxes starting at once: `limits.diskHeadroomPercent` of each disk size;
- the cleanup's margin (`autoDelete.marginBytes`), so it does not run for every create;
- room to restore: a cold box's resume needs its whole cold size free above the floor and headroom.

**Cold** must hold:

- the cold floor: the larger of `coldStorage.minFreeBytes` and `coldStorage.minFreePercent` of the disk;
- per cold box: its image data plus its own data, and the same for each named snapshot that went with it.
  A cold copy shares nothing.

"Image data" is what the image's or generation's root filesystem allocates (`du -h` on it), not the box's
disk size.

### Worked example: 80 GiB SSD and 250 GiB HDD

Our host, with the default floors. Boxes have the default 8 GiB disk, start from a project image of about
3 GiB, and write 1–3 GiB each. Estimates, not measurements.

Hot, 80 GiB:

| | GiB |
| --- | --- |
| floor: max(5 GiB, 10% of 80 GiB) | 8 |
| save reserve | 2 |
| dev image (1.7 GiB) and two project images of about 3 GiB | about 8 |
| three goldens of the default shape, 2 GiB each | 6 |
| headroom for one box starting: 25% of 8 GiB | 2 |
| cleanup margin | 2 |
| **left for boxes' own data** | 80 − 28 = **about 52** |

At 1–3 GiB of own data each, that is 17–52 boxes on hot, about 26 at 2 GiB. Those are the boxes used within
the last 6 hours. Everything older moves to cold.

Cold, 250 GiB:

| | GiB |
| --- | --- |
| floor: max(5 GiB, 5% of 250 GiB) | 12.5 |
| **left for cold boxes** | **237.5** |
| one cold box: 3 GiB image data plus 1–3 GiB own data | 4–6 |

237.5 / 6 ≈ 39 and 237.5 / 4 ≈ 59: about 39–59 cold boxes, fewer if they also have named snapshots.

Without the cold tier the same 52 GiB would hold every box the host keeps: about 26. With it, about 26 recent
boxes stay hot and about 40 more wait on the HDD. When the HDD fills, its oldest unpinned boxes (unused for
12 hours or more; with the hot disk below its floor and nothing else to free, for an hour or more) are
auto-deleted to make room for the next move.

## Operations

### Adding a cold tier to a host

1. Prepare the disk ([above](#preparing-a-cold-disk)): format, fstab, mount, the davit directory.
2. Add the `coldStorage` block to the config. `dir` alone is enough.
3. Restart the daemon: `systemctl --user restart davit`. Running VMs keep running across it.
4. Check:

   ```sh
   davit doctor -v          # "Cold storage": free, floor, and the reflink probe
   davit box ls             # the host line ends with "; cold … free of …"
   ```

Stopped boxes already on the host move once unused for `after`, one at a time.

### Changing it

- **The same disk at a new path.** Stop using the old path (unmount, change the mount point in fstab, mount
  again), change `coldStorage.dir`, restart. The marker's ID matches, and the record follows the new path.
- **The settings.** Change them and restart. A new `after` applies to the next sweep.
- **A new disk.** davit refuses a directory without its tier's marker, so empty the old tier first:
  1. drain it ([below](#removing-it)) and remove the `coldStorage` block;
  2. restart: the daemon forgets the old tier once no VM is on it;
  3. prepare the new disk, add a `coldStorage` block for it, and restart again.

### Removing it

1. Set `"drain": true` in `coldStorage` and restart. Nothing more goes cold, and the sweeper brings cold VMs
   back, one at a time, while the hot disk keeps its floor plus the margin. The `box ls` host line says
   `(draining: boxes move back, none go cold)`.
2. Wait until `box ls` shows no cold box, or `box rm` the cold boxes you do not need. If the hot disk has no
   room for the rest, the drain waits; free hot space first.
3. Remove the `coldStorage` block and restart. The daemon forgets the tier (`<dataDir>/storage/cold.json`)
   once no VM is on it.

Removing the block while boxes are still cold leaves them unavailable, and doctor and `box ls` say so, until
you put the block back.

The old directory keeps its `davit-cold.json`. Before you use that disk for a tier again, remove the
directory (`rm -rf /mnt/davit-cold/davit`) and create it empty.

### When the cold disk is missing

At start, the daemon refuses a cold directory that:

- does not exist (the disk is not mounted, and `dir` is a subdirectory of the mount point);
- has no `davit-cold.json` although this host had a tier (an empty mount point);
- has the marker of another davit store;
- is on a network filesystem ([Network storage](#network-storage)).

The daemon **still starts**. Everything on hot works. It logs an error, and the boxes on cold storage cannot
resume:

```text
host: …; hot disk 31.0 GiB free of 80.0 GiB, floor 8.0 GiB; cold storage UNAVAILABLE: 12 boxes on it cannot resume (cold storage /mnt/davit-cold/davit is not a directory (is the cold disk mounted?): …); …
```

`davit doctor` fails its "Cold storage" check with the same reason. Resuming a cold box fails:

```text
error: vm myproj--main is on cold storage, which is unavailable: … (docs/storage-tiers.md#when-the-cold-disk-is-missing) (unavailable)
```

**Fix:** mount the disk (`sudo mount /mnt/davit-cold`), then restart the daemon
(`systemctl --user restart davit`). With `nofail`, systemd does not hold the user's services back for the
disk, so a disk that appears late at boot also needs that restart.

### Monitoring

```sh
davit box ls                                                    # tier per box; the hot and cold lines
davit box ls --json | jq 'select(.event=="result") | .data.host.cold'
davit box ls --json | jq -r 'select(.event=="result") | .data.items[] | select(.tier=="cold") | "\(.project)/\(.instance) \(.coldBytes)"'
davit doctor -v                                                 # "Cold storage" and "Disk admission"
davit events --types vm.autodeleted                             # auto-deletions, hot and cold
journalctl --user -u davit -g 'cold storage'                    # the daemon's moves and restores
```

`box ls` shows each box's tier, `hot` or `cold (5.1 GiB to restore)`, and `pinned` or `pinned hot`. The host
line has both disks:

```text
host: …; hot disk 31.0 GiB free of 80.0 GiB, floor 8.0 GiB, reserved 2.0 GiB; cold 151.2 GiB free of 250.0 GiB, floor 25.0 GiB, 14 boxes (73.4 GiB) moved after 6h0m unused; …
```

`(LOW: unused cold boxes may be auto-deleted)` follows when the cold disk is below its floor. Over the API,
`capabilities.capacity.cold` has `freeBytes`, `totalBytes`, `floorBytes`, `afterSeconds`, `vms`, `bytes`,
`fastClone`, `detail`, `sameFilesystem`, `draining` and `unavailable`, and each VM has `tier`, `coldBytes`,
`coldAt` and `keepHot`.

Doctor's "Cold storage" check warns when the cold directory is on the data dir's own filesystem (moving
frees nothing), when it cannot reflink, and below twice its floor. It fails below the floor and when the tier
is unavailable.

The daemon logs, at warn level, `moved vm to cold storage` (`coldBytes`, `ownBytes`, `hotFreedBytes`,
`seconds`) and `brought vm back from cold storage`, and at info level `restored vm from cold storage`
(`bytes`, `seconds`, `mibPerSecond`, `rebased`). A move has no event of its own. Cold auto-deletions are
`vm.autodeleted` events like hot ones.

### Slow first resumes

A slow first resume after a box went cold is expected: it copies the box back first, at the cold disk's
read rate. Look at the `storage.restore` lines `box up` prints, and at `mibPerSecond` in the daemon's
`restored vm from cold storage` line.

- Pin the boxes you resume often: `davit box pin --hot PROJECT/INSTANCE`.
- Raise `after`, so boxes stay hot longer (more hot disk used).
- Use a faster cold disk: a SATA SSD reads many times faster than a hard disk.
- `rebased=false` in the log means the box's base was no longer on hot, so the restore wrote the whole copy.

Slow or missing moves to cold storage:

- The host is busy: VMs starting or stopping, requests queued, or I/O pressure above `ioBusyPercent`
  (`cat /proc/pressure/io`).
- `bytesPerSecond` caps them: at the default 100 MiB/s, a 5 GiB box reads 10 GiB (copy and read-back), so it
  takes at least about 100 seconds.
- The idle I/O class only works with the BFQ scheduler (`cat /sys/block/sdX/queue/scheduler`). Without it,
  moves compete with the guests at normal priority, paced only by `bytesPerSecond`.
- A box that never goes cold: [troubleshooting.md](troubleshooting.md#a-box-never-goes-cold).
