# Project boxes

An agent on your dev machine (never inside the VM) gets a VM for its project, pushes its working tree in,
runs builds and tests there, pulls results back and puts the VM down. Two layers keep that fast:

- **The project image**, one per project: a builder VM where a setup step installs the project's
  dependencies (toolchains, `go mod download`, `pnpm install`, apt packages, warm caches), saved as numbered
  disk snapshots, its *generations*. Setup runs once per generation, not on every instance.
- **Instances**, many per project, for example one per git worktree: VMs created from the latest
  generation. Each keeps its own disk between `down` and `up`.

## Setup, once per machine

Install davit on the VM host ([install-host.md](install-host.md)) and the CLI on the dev machine, then connect
the dev machine to the host ([quickstart.md](quickstart.md) has the install lines):

```sh
davit connect user@host     # key, ssh check, davit and its daemon on the host, then the default host
```

`connect` writes `~/.config/davit/client.json`, so every `davit` command on this machine talks to that host
over ssh:// with no flags or environment. `davit connect --check` verifies it again, and `davit client-config`
shows the file; `--set-host H` and `--clear` change it. `--host`, `--api-url`, `$DAVIT_HOST` and
`$DAVIT_API_URL` still win.

## The agent recipe

From inside any git worktree of any repository:

```sh
davit box run -- make test          # zero config: builds the project image the first time, then this
                                      # worktree's instance; syncs the worktree; streams output; exits with
                                      # the command's code
davit box sync --pull coverage.out  # bring results back (paths relative to the repo's top level)
davit box down                      # optional: the instance stops itself after 15 min unused
```

Unused boxes older than 3 days (project images with no instances: 1 day) can be auto-deleted when the host is
low on disk; `davit box pin` keeps one ([below](#when-the-host-is-low-on-disk)).

Sync adds and updates files but never deletes them: a file deleted or renamed in the worktree stays in the box
until it is deleted there (`davit box run -- rm PATH`) or the instance is recreated (`davit box rm --yes`,
then the next `box run`).

With no arguments the project is the repository (its `origin` remote's name, else the main worktree's
directory name), and the instance is the worktree's branch. A second worktree of the same repository gets
its own instance from the same image. `box ssh` opens a shell in the same instance.

## Names

`PROJECT[/INSTANCE]` is optional on every command. `/INSTANCE` alone means another instance of this
repository's project. Without INSTANCE the command uses the current git worktree's branch, or its directory
name on a detached HEAD, or `default` outside a work tree. git runs without any inherited `GIT_DIR`-like
variable. Names are lowercased and other characters become `-`, so `feature/login` becomes `feature-login`.
The VMs are `PROJECT--INSTANCE` and `PROJECT--image` (the builder), labelled `davit.box=PROJECT` plus
`davit.box.instance=INSTANCE` (and `davit.box.generation=N`) or `davit.box.role=image`. They are ordinary
VMs: `davit list`, `info`, `ssh`, `snapshot` and the rest work on them. A VM of the same name without the
labels is refused, not taken over. `image` is reserved as an instance name. Every box command takes `--json`.

## The project image and its setup

The setup runs in the builder, in the synced tree, under a login shell, so the image's toolchains are on
PATH. The host's default image is `ubuntu-24.04-dev`, with Go, Node.js, pnpm, uv, mise and Docker CE. The
setup is, in order of preference:

1. `--setup-cmd CMD`, or `--setup FILE` (run by its `#!` interpreter);
2. `.davit/setup.sh` in the repository (run the same way);
3. otherwise one detected from the repository's top level:

| Found | Setup step |
|---|---|
| `mise.toml`, `.mise.toml`, `.tool-versions` | `mise trust`, `mise install`; the later steps then run under `mise exec --` |
| `go.mod` | `go mod download` |
| `package.json` of a [Vite+](https://viteplus.dev) project (`vite-plus` in its dependencies or devDependencies, or a `vite.config.*` importing it) | vp's installer (`https://vite.plus`) unless the image or mise has `vp`, then `vp install` (`--frozen-lockfile` with a lockfile), which uses the package manager and version `packageManager` pins. The installer puts vp on PATH in `~/.profile` and `~/.bashrc`; it installs Node.js too when neither the image nor mise has it |
| `package.json` + `pnpm-lock.yaml` / `package-lock.json` / `yarn.lock` / `bun.lock` / none | `pnpm install --frozen-lockfile` / `npm ci` / `yarn install` (through corepack) / `bun install` / `npm install` |
| `uv.lock` / `pyproject.toml` / `requirements.txt` | `uv sync --frozen` / `uv sync` / a `.venv` with `uv pip install -r` |
| a Makefile with a `setup` target | `make setup` |

For a JS project (a `package.json` at the top of the tree, whatever its setup), the image also puts the
synced tree's `node_modules/.bin` on PATH in login shells (`/etc/profile.d/davit-node-bin.sh`, or
`~/.profile` when the image user has no sudo; POSIX, so a `/bin/sh` that is dash reads it too), so `box ssh`
finds the project's own `vp`, `tsc` or `eslint`. Directories that `~/.profile` adds (vp's installer adds its
own) come before it there. `box run` also puts every `node_modules/.bin` from its
directory up first, nearest first, as `npm run` does. An image built before this gets the profile line at its
next `box image update`.

Outside a git work tree nothing is synced, and the setup runs in an empty work dir. Commit a
`.davit/setup.sh` when the detection is not enough (services, generated code, warm build caches);
`box image update` reruns it on top of the current image.

## Commands

| Command | What it does |
|---|---|
| `box image build [PROJECT] [--from IMAGE] [--setup FILE \| --setup-cmd CMD] [--dir DIR[:GUEST]] [--cpus/--memory/--disk]` | Creates the builder from IMAGE (default the host's), syncs the worktree (or `--dir`) into it with a git index, runs the setup there, stops it and snapshots generation 1. A failed setup leaves the builder running for a look and says how to read the setup's output; the builder stops itself when idle. |
| `box image update [PROJECT] [--setup ...]` | Resumes the builder (generation N's disk), syncs, runs the setup again on top and snapshots generation N+1. Older generations stay, and instances made from them keep working. |
| `box image rm [PROJECT] [--force] --yes` | Deletes the project image: the builder and every generation snapshot, whose disks and goldens the host then frees. It refuses while the project has instances; `--force` deletes them too, with their disks. |
| `box up [PROJECT[/INSTANCE]] [--refresh] [--image I] [--cpus/--memory/--disk] [--idle 15m] [--wait 3m]` | Resumes the instance, waits out an operation in progress, or creates it from the latest generation. A project without an image gets one built first (from `--image`, else the host's default image). `--refresh` recreates an instance that is older than the latest generation, keeping its CPUs and memory unless given; it warns that the instance's disk is discarded. `--cpus/--memory/--disk` on an existing instance resize it ([below](#resizing-a-box)). When the host is full it waits up to `--wait` for room ([below](#when-the-host-is-full)); `run`, `ssh` and `sync` take `--wait` too. |
| `box down [PROJECT[/INSTANCE]] [--force]` | Runs `sync` in the guest, then a clean stop that commits the disk. |
| `box run [PROJECT[/INSTANCE]] [--cwd DIR] [-e K=V] [--timeout D] [--stdin] [--no-sync] [--raw] -- CMD...` | Brings the instance up and syncs the worktree first (`--no-sync` skips that). Then it runs CMD in a login shell with the project's mise pins (`--raw`: exactly CMD), in the current directory's place in the synced tree (`--cwd` overrides it). It streams stdout and stderr separately and exits with CMD's code (124 on `--timeout`, 128+N on a signal). `--json` streams events with the exit as the last line. |
| `box ssh [PROJECT[/INSTANCE]] [-- CMD...]` | Real OpenSSH into the instance: a short-lived certificate and the pinned host key, through the API tunnel. |
| `box sync [PROJECT[/INSTANCE]] [DIR[:GUEST_PATH]] [--git index\|full\|none]` | Pushes the worktree (or DIR) to `/home/davit/work`: tracked files and untracked files that are not ignored (`git ls-files`), checked-out submodules, never `.git`. It merges, so files deleted locally stay in the box. Then it sets up git there: `index` (the default) makes the files a git index, so `git ls-files` and `git status` work; `full` also brings the current branch's history (a bundle of HEAD, with the index on it, so `git diff HEAD` is this worktree's changes); `none` does nothing. |
| `box sync [PROJECT[/INSTANCE]] --pull GUEST_PATH [LOCAL]` | Copies a file or directory back. A relative GUEST_PATH is relative to the work dir. |
| `box ls [PROJECT]` | A host line first (memory used and free, the memory reserved by live VMs against the budget, the data disk's free space against its floor and what is reserved, the cold storage tier if the host has one, boxes running, how many are idle enough to stop for room, requests waiting), with the boxes the host auto-deleted in the last week under it. Then images (latest generation) and instances: state, tier (`hot`, or `cold (N to restore)`), the generation each started from (marked when older than the latest), shape, age, idle time and `pinned` or `pinned hot`. `--json` has the host line as `host` (`freeDiskBytes`, `diskFloorBytes`, `diskReservedBytes`, `saveReserveBytes`, `cold`, `autoDeleted`), and on items `tier` (`hot` or `cold`), `coldBytes`, and `keep` and `keepHot` on pinned items. |
| `box ls [PROJECT] --deleted` | The boxes the host auto-deleted for disk space in the last week, each with why (how long it was unused, the disk's state). |
| `box pin [PROJECT[/INSTANCE] \| PROJECT/image] [--hot]` | Pins an instance, or the project image: the host never auto-deletes it. It can still move to cold storage; `--hot` also keeps it on hot storage ([below](#cold-storage)). `box unpin` undoes both; `box up --keep` pins as it brings the box up. |
| `box image prune [--dry-run]` | Removes the host's image blobs that no image uses (host owner only; [below](#when-the-host-is-low-on-disk)). |
| `box rm [PROJECT[/INSTANCE]] --yes` | Deletes an instance and its disk. |
| `box gc [PROJECT] [--idle 10m] [--rm-older-than 7d] [--dry-run]` | Stops your boxes idle at least `--idle` (default 10m), oldest first, and with `--rm-older-than` deletes instances stopped at least that long (never a project image). A box with a running process, an open `box ssh` or recent activity is never stopped. `--dry-run` only lists. |

**When something fails, the error says what to try.** These hints are written for agents:

- Out of host memory or disk (`resource_exhausted`, exit 8), after the host stopped what was idle and the
  `--wait` ran out: `box ls`, then `box down` or `box rm` idle instances (`box gc`), or ask for less with
  `--cpus/--memory/--disk`. A disk refusal starts `host disk low: X free, need Y`: `box rm` what you no
  longer need (stopping frees memory, not disk).
- Host unreachable (exit 7): the ssh error, plus the check to run (`ssh HOST true` without a prompt) and the
  install command.
- A failed setup: its exit code and `davit history` to read its output.

`$DAVIT_BOX_WORKDIR` moves the synced tree for images with another layout.

## Box shape

A new box (the builder at `box image build`, an instance when `box up`, `run`, `ssh` or `sync` creates it)
gets the host's default shape: 2 CPUs, 2 GiB memory, 8 GiB disk. A JS project's boxes (a `package.json` at
the top of the worktree, or of `--dir`) get 4 GiB memory instead, because package installs, type checks and
linters of a JS workspace are killed for memory (exit 137) in 2 GiB. `--memory` or `--type` replaces either
default. The 4 GiB counts against the host's memory reservation like any `--memory`: when the host is full the
box waits in the queue ([below](#when-the-host-is-full)), and a host that can never hold 4 GiB in one VM
(the engine's largest VM, `limits.memoryBytes` or the reservation budget below it) keeps its default and says
so. The default applies only when a box is created: an existing box keeps its shape, and `box up` without
flags never resets it to a default. `--cpus`, `--memory` or `--disk` on an existing box resize it
([below](#resizing-a-box)).

## Resizing a box

`--cpus`, `--memory` and `--disk` (or `--type`) on `box up`, `run`, `ssh` or `sync` also apply to an instance
that already exists, and the host keeps the new shape: every later `box up`, flags or not, boots it.
`box ls` (and `--json`: `cpus`, `memoryBytes`, `diskBytes`) shows it. The disk only grows.

- **Stopped:** it starts with the new shape.
- **Running:** a VM's shape changes only when it boots, so the box is stopped (its disk committed, as by
  `box down`) and started again with the new shape. stderr says so: `box P/I: resizing from 1 cpu, 2.0 GiB
  memory, 8.0 GiB disk to --memory 4GiB: stopping it ...`. Open `box ssh` sessions end. If a process started
  through davit (a `box run`, attached or detached, or a `davit exec`) still runs in it, nothing is stopped:
  the error (`stop_required`, exit 6) names the processes and the commands to run once they are done,
  `davit box down P/I` then `davit box up P/I --memory 4GiB`.
- The same shape again changes nothing (also while others queue for capacity), and a shape the host refuses
  (above its limits, a smaller disk) fails before the box stops.

A resize always boots the box's committed disk (a resume never restores a memory snapshot, which is tied to
the shape it was taken with), so the disk's contents carry over unchanged. The new shape is admitted like any
resume: through the reservation and the `--wait` queue below. If it gets no room, the box stays stopped with
its disk and old shape, and the error says so; `box up` without the flags starts it as it was.

Known limits: the check for running work sees only processes started through davit. Open `box ssh` sessions
(and anything started from them) do not stop a resize; they end with the restart. The host keeps the newest
1024 process records per VM, so a process started before the 1024 most recent ones is not seen either.

## When the host is full

A box that cannot be admitted (host memory, the memory reservation, the VM count or CPUs) does not fail
straight away:

1. **The host makes room.** It stops idle boxes, oldest activity first, and retries after each stop. A box
   counts as idle when it is ready, runs no process (attached or detached), has no stream or `box ssh` session
   open, and has had no exec or ssh activity for 2 minutes (`limits.reclaimIdleAfterSeconds`). Only VMs with
   an idle timeout qualify, so every box does; plain VMs, ephemeral and no-snapshot VMs never do. The stop is
   an ordinary clean stop (disk kept; `box up` resumes it). Each one is logged by the daemon and shows in
   `davit events` as an `operation.created` stop with `reason: reclaim`.
2. **Then it waits.** `box up`, `run`, `ssh` and `sync` queue for up to `--wait` (default 3 min; `0s` fails
   at once). The queue is first in, first out, and its head retries as boxes stop or go idle. stderr says
   `waiting for host capacity (N ahead, X GiB needed, Y free)` whenever that changes.
3. **Only then does it fail**, with `resource_exhausted` (exit 8) and the hints above.

Admission checks two things:

- **Free memory.** A new box must leave 256 MiB of `MemAvailable` (`limits.minFreeMemoryBytes`).
- **The reservation.** A box restored from the golden starts at about 30 MiB but can grow to its whole shape,
  so free memory alone would admit more boxes than can work at once. With `limits.memoryBytes` set, the
  declared memory of the live VMs may sum to at most `limits.memoryBytes × limits.memoryOvercommit`. The
  overcommit defaults to 1.5, because boxes restored from one golden share its pages. `davit init` (and
  `make host-install`) sets `memoryBytes` to the host's RAM less 2 GiB on a new VM host.
  [vm-host.md](vm-host.md#capacity-running-vs-parked-boxes) has the numbers.

Over the API, `createVM` and `resumeVM` take `?waitSeconds=` (0 by default). While someone waits, a request
without a wait fails at once rather than overtaking (a `resumeVM` of a ready VM needs no room and answers at
once). `capabilities.capacity` reports `waiting`,
`reclaimable` and `memoryBudgetBytes`. `POST /vms/reclaim` is `box gc` (feature `capacityReclaim`).

## When the host is low on disk

Every box's disk lives on the host's data filesystem. A new box costs almost nothing there at first (its disk
is a reflink of the image) and grows as it writes, so the host keeps a **floor** of free space: the larger of
10% of the filesystem and 5 GiB (`limits.minFreeDiskPercent`, `limits.minFreeDiskBytes`).

1. **Admission reserves room to grow.** A create or resume (and `box image build`, which creates its builder)
   is admitted only if free space, less a quarter of its disk size (2 GiB for the default 8 GiB disk;
   `limits.diskHeadroomPercent`) and less what is reserved for boxes still starting, stays at the floor.
   Otherwise it queues like a box that does not fit in memory (`--wait`), and stderr says why:

   ```text
   waiting for host disk space (0 ahead): host disk low: 6.0 GiB free, need 7.0 GiB; `davit box gc` stops idle boxes, `davit box rm` deletes ones you no longer need
   ```

   When the wait runs out it fails with `resource_exhausted` (exit 8) and the same `host disk low: …`
   message.
2. **Saving never fails for space.** The host sets aside a save reserve (2 GiB, `limits.saveReserveBytes`)
   that only stops use: `box down`, idle stops and `box gc` commit the disk even when running boxes have
   filled it. Admission refuses new work long before that.
3. **Below the floor the host cleans up**, and stops as soon as free space is back at the floor plus a margin
   (2 GiB), cheapest loss first:
   - davit's own garbage: disks of deleted boxes, goldens that can never be used again, and image blobs no
     image uses (`box image prune` does this on demand);
   - idle boxes are stopped (as when making room for memory);
   - **unused boxes are deleted**, least recently used first: project images with no instances that have not
     been used for a day, then stopped instances unused for 3 days. A box that is running, has a session or
     process, is queued for, was used within that age or is **pinned** (`box pin`, `box up --keep`) is never
     deleted, and neither is any VM that is not a box, nor the image a queued `box up` boots. A queued box
     create or resume runs the same cleanup for the room it needs, but while the host is above the floor
     it stops and deletes only your own boxes.

   Each deletion is logged by the daemon, recorded on the box (`box ls --deleted`, the VM's `deleteReason`)
   and in `davit events` as `vm.autodeleted`, and named under the `box ls` host line and by `davit doctor`
   for a week. The host administrator can change the ages, the margin, or turn auto-delete off
   ([vm-host.md](vm-host.md#disk-space-floor-reservation-and-auto-delete)).

   On a host with a cold storage tier, stopped boxes move there before any is deleted
   ([below](#cold-storage)).

## Cold storage

A host can have a second, larger and slower disk for stopped boxes ([storage-tiers.md](storage-tiers.md)).
A box stopped and unused for a while (6 hours by default) moves there. Its next `box up`, `run`, `ssh` or
`sync` copies its disk back first, so that first resume is slower; the ones after it are not. stderr says so:

```text
box myproj/main: restoring from cold storage (5.1 GiB)…
box myproj/main: resuming (operation op_…)
  storage.restore       …% restoring from cold storage: 2.0 GiB of 5.1 GiB (39%)
```

How long the copy takes depends on the host's cold disk and the box's size.

- **`box ls`** shows each box's tier: `hot`, or `cold (5.1 GiB to restore)`.
- **`box pin`** (and `box up --keep`) keeps a box from being auto-deleted, but it can still go cold.
  **`box pin --hot`** also keeps it on hot storage, so it always resumes at once; on a cold box it brings it
  back in the background. Pin hot only the boxes you resume often: pinned-hot boxes stay on the host's fast
  disk for good.
- If the host's disk is low, the resume reserves room for the copy too, and can wait or fail with
  `restoring from cold storage needs N GiB: host disk low: …`
  ([troubleshooting.md](troubleshooting.md#restoring-from-cold-storage-needs-n-gib)).

## Idle stop

Boxes never TTL-stop. A box stops itself (a clean stop, disk kept) after `--idle` with no activity: the
default is 15 min, `$DAVIT_BOX_IDLE` overrides it, and `0s` disables it. A forgotten box therefore does not
hold host RAM. Activity is an exec, an attached process stream (a running `box run`) or an open tunnel (a
`box ssh` session, a forward). The countdown restarts when the last one ends, and a box with one open is never
stopped. `davit info` shows `idleTimeoutSeconds` and `lastActivityAt`.

This is the VM's `idleTimeoutSeconds` (`createVM`, `updateVM`), available on any VM. Activity is tracked in
the daemon's memory: after a daemon restart a VM counts from its last state change.

## Remote hosts over SSH

By default the CLI reaches a daemon on another machine over OpenSSH. `davit connect user@host` sets this up
and saves the URL; by hand:

```sh
export DAVIT_HOST=ssh://user@host        # or --host, or `davit connect user@host`; ssh://USER@HOST[:PORT][/DAEMON_SOCKET]
davit box up myproj
```

How it works:

- Each API connection is one ssh session running `davit dial-stdio` on the host. That command splices the
  session to the daemon's unix socket. Without a path in the URL it uses the first socket that exists of
  `$DAVIT_API_URL`'s, the default config's, `make host-install`'s (`~/.svm/davit/davit.sock`, whatever
  config its unit runs) and the built-in default.
- The daemon sees a local owner connection, so the host needs no listener, token or tailnet. It needs only
  sshd, key authentication (the CLI uses `BatchMode`) and `davit` in `~/.local/bin`, `~/.svm/davit/bin`
  or the system PATH (`$DAVIT_REMOTE_BIN` names another binary). A non-interactive ssh command sees only
  the system PATH, so those two install locations are searched first.
- A shared OpenSSH control master makes every call after the first cheap. It lives in `~/.cache/davit/ssh/`
  and is kept for 2 min.
- `box ssh` goes through the same path: its ProxyCommand runs the CLI with the same ssh:// URL.
- `?jump=JUMP` in the URL goes through a jump box (OpenSSH ProxyJump; `davit connect --via`), and `?key=PATH`
  picks the private key (`davit connect --key`).
- `$DAVIT_SSH` names the ssh client.

With a tailnet configured, `--host NAME` keeps using the tailnet gateway.

## Measured

Timings of `box run`, `up`, `down`, `sync` and image builds, with their hardware and sample counts, are in
[benchmarks.md](benchmarks.md#project-boxes-from-a-dev-machine-over-ssh-2026-10-25).

## Needs work

- **Sync merges.** Deletions are not mirrored, because `pushTree` `mode=mirror` is not implemented.
- **No generation pruning yet.** Every generation stays a named snapshot of `PROJECT--image` until
  `box image rm`; remove single old ones with `davit snapshot delete`.
