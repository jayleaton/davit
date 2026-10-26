# The real-VM test tier on a VM host

`make check` is hermetic: no KVM, no guests. The tests that boot real Firecracker guests (adapter conformance,
golden restore, live fork, egress, the CLI end to end `TestM1Acceptance`, fork identity and stop durability,
the benchmarks) are opt-in with `DAVIT_TEST_FIRECRACKER=1` and run **only on a separate VM host**, through
`scripts/vm-tier.sh`. Never on the development machine. For a new host, `make vm-tier HOST=user@host` does everything below
in one command (preflight, image build on the host, prepare, tests): see [vm-host.md](vm-host.md).

```sh
scripts/vm-tier.sh --prepare vmhost                  # once per host (and after a new base image)
scripts/vm-tier.sh vmhost                            # the whole tier
scripts/vm-tier.sh vmhost 'TestM1Acceptance$'        # tests matching a go test -run regex
scripts/vm-tier.sh --bench vmhost 'Benchmark$'       # + DAVIT_BENCH=1 (docs/benchmarks.md numbers)
scripts/vm-tier.sh --fresh-host vmhost '^$'          # scripts/fresh-host-e2e.sh on real Firecracker only
scripts/vm-tier.sh --check vmhost                    # host requirements only
scripts/vm-tier.sh --images vmhost                   # build the base image on the host from HEAD
```

`vmhost` is any SSH destination: a tailnet name, an IP, `user@host`, or a `~/.ssh/config` alias. Key auth
without prompts (the script uses `BatchMode`); one SSH connection is shared for the whole run.

## What a run does

1. Packs the committed `HEAD` plus every local change (staged, unstaged, untracked-but-not-ignored files;
   never `.build/`, `bin/`, `images/out/`) and ships it with the host half, `scripts/vm-tier-remote.sh`.
2. On the host, under a lock (one run or prepare at a time): makes `~/.svm/src` exactly that tree, checks the
   host (below; a failed check exits 2), then runs
   `go test -p 1 -count=1 -timeout 90m -v -run REGEX` over the real-VM packages
   (`./internal/runtime/firecracker/`, `./cmd/davit/`, `./internal/runtime/firecracker/fctest/`; `--pkg` to
   pick others). `-p 1` and tests without `t.Parallel` mean one package, one test, at a time.
3. Streams the output, then copies the logs back to `.build/vm-tier/<run>/logs/`:

   | File | Contents |
   |---|---|
   | `test.log` | the full `go test -v` output (benchmark JSON included with `--bench`) |
   | `summary.txt` | the `--- PASS/FAIL/SKIP`, `ok` and `FAIL` lines |
   | `host.txt`, `run.txt` | the host check, commit (and whether local changes rode along), command, Go version, CPUs, memory, load |
   | `kernel-kills.txt` | kernel OOM and kill reports during the run (evidence for a VM that died from outside) |
   | `cleanup.log` | every process the cleanup had to kill (empty on a clean run) |
   | `fresh-host.txt`, `fresh-host.log` | with `--fresh-host` |

4. Always cleans up on the host, also on failure or Ctrl-C: kills the test job's process group and every
   process whose working directory or command line is in the run's directories (test binaries, daemons, VMMs,
   namespace holders, egress helpers), removes the run directory and the per-run scratch (test disks, e2e data
   dirs). The pinned Firecracker binary, the derived test image and the blobs persist.
5. Exits with the remote status: 0 pass, 1 test failure, 2 host or usage problem, 3 image build failed
   (`--images`), 130 interrupted, 255 SSH. `--out DIR` puts the results somewhere other than
   `.build/vm-tier/<run>/`.

## Host requirements

`--check` (and every run) prints one line per requirement:

- **x86_64** with **KVM**: `/dev/kvm` read-write for the SSH user (group `kvm`).
- **Unprivileged user and network namespaces**: `unshare --user --map-root-user --net true` works. Ubuntu
  24.04 restricts them through AppArmor; see the AppArmor section of [network.md](network.md) (`davit init
  --apparmor`), or `sysctl kernel.apparmor_restrict_unprivileged_userns=0` on a dedicated test host.
- **A reflink filesystem** (btrfs or XFS) for blobs and scratch: `~/.svm/fs` by default, `--fs DIR` elsewhere.
  Disks are reflinked from the image; on ext4 the fixture fails.
- **Tools**: `git tar flock nsenter unshare ip debugfs sha256sum curl` (Debian/Ubuntu: `git tar util-linux
  iproute2 e2fsprogs coreutils curl`). `nft` is optional (without it the userspace egress stack filters alone).
- **20 GiB free** on the reflink filesystem; RAM for three 512 MiB guests at once plus the build (8 GiB is
  comfortable).

## Prepare (once per host)

```sh
scripts/vm-tier.sh --prepare vmhost --blobs /path/to/images/out/blobs
```

- Installs [mise](https://mise.jdx.dev) into `~/.svm/bin` if the host has none, and the Go version pinned in
  `mise.toml`.
- Sends the base image's blobs (only those `images/manifests/ubuntu-24.04-base.json` names, only those the host
  lacks; sparse, so the 4 GiB rootfs costs its real size) from `--blobs` (default `$DAVIT_VM_BLOBS`, else
  `images/out/blobs`; build them with `images/build.sh`), and verifies each against its digest on the host.
- Warms the fixture with `TestFixture` (`internal/runtime/firecracker/fctest`): downloads and verifies the
  pinned Firecracker release, builds this checkout's guest agent and derives the test image. No VM boots.

Rerun it after the base manifest changes; it is idempotent and only sends what is missing.

Instead of sending blobs, `--images` builds `ubuntu-24.04-base` on the host from the committed `HEAD`
(`images/build.sh`; blobs in `~/.svm/fs/blobs`, manifest in `~/.svm/fs/manifests`). Once that manifest exists,
`--prepare` sends nothing, and prepare and runs use it instead of the committed one. The build is skipped while
the host's image was built from the same `images/` tree (`--rebuild-images` forces one).

## Host layout

| Path | |
|---|---|
| `~/.svm/bin/mise` | when the host has no mise |
| `~/.svm/src` | the shipped checkout of the last run |
| `~/.svm/runs/<run>/` | payload, `out/` (logs), `t/` (TMPDIR, short for 108-byte socket paths); removed after the run |
| `~/.svm/fs/blobs` | image blobs (`sha256/<hex>`) |
| `~/.svm/fs/manifests` | the base manifest built on the host (`--images`), with the `images/` tree it came from |
| `~/.svm/fs/images-work` | `images/build.sh` scratch |
| `~/.svm/fs/scratch` | `DAVIT_TEST_SCRATCH`: Firecracker (`bin/`) and derived image (`images/`) persist |
| `~/.svm/lock` | one run at a time |

`--dir` moves `~/.svm` (keep it short); `--fs` moves the reflink part.

## Reading a failure

- Start with `summary.txt`, then the failing test in `test.log`. The CLI end-to-end tests print the daemon log on
  failure.
- A VM reported `lost while daemon was down` or exiting `killed`: every SIGKILL the Firecracker adapter sends
  is logged (debug level, which the e2e daemons use) as `SIGKILL firecracker VMM` with the VMM and the reason.
  If the dead VMM has no such line, it was killed from outside the adapter: check `kernel-kills.txt` (OOM) and
  `cleanup.log`, and whether anything else ran on the host.
- `LEAKED PROCESSES` (from `fctest.Guard`/`Sweep`) lists a process that outlived its test with its record,
  console tail and the lifecycle calls made; the sweep has killed it.
