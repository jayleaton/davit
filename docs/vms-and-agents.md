# VMs and agents: the full tour

Project boxes ([quickstart.md](quickstart.md)) are the main way to use davit. Under them, davit is a general
host for fast, persistent Ubuntu VMs: `new`, `exec`, `fork` and `stop` them, on your own machine, with an
optional tailnet gateway (Tailscale or Headscale) for remote access, sharing and the desktop. VMs are
Firecracker microVMs, run rootless; a fork is a copy-on-write clone of a VM's disk. This page goes from an
empty host to a coding agent running inside a VM.

Every command output below is copied from a real run of [`scripts/fresh-host-e2e.sh`](../scripts/fresh-host-e2e.sh)
on the reference dev machine (see [Measured](#measured)); progress lines are trimmed and IDs differ per run.
Steps that box could not run (tailnet enrollment, prompts against a real model, members, desktop) are marked
**not captured**: they are covered by hermetic tests against a simulated tailnet and fake harnesses, not by a
real run yet.

## Requirements

This table covers every feature on this page, as `davit doctor` checks them. Hosts are x86_64: the prebuilt
images are x86_64 only (an arm64 host would need images built from source, which has not been tried). The disk figures are for the images this
page uses (`ubuntu-24.04-base` and `ubuntu-24.04-harness`); the host installer's requirements are in
[install-host.md](install-host.md).

| Need | Why | Check |
| --- | --- | --- |
| Linux x86_64 with read-write `/dev/kvm` (usually: be in the `kvm` group) | Firecracker needs KVM | `davit doctor` → `kvm` |
| Unprivileged user + network namespaces | every VM gets its own rootless netns + tap | `doctor` → `userns`. Ubuntu ≥ 23.10 restricts these by default (AppArmor): `davit init --apparmor` prints a scoped profile and the one-time root commands ([network.md](network.md#ubuntu-apparmor)) |
| Firecracker | the VMM | nothing to install: the daemon downloads the pinned release into the data dir and checks its sha256 (`doctor` → `firecracker`) |
| btrfs or XFS (`reflink=1`) for the data dir | instant disk clones for new/fork/resume; other filesystems work but copy whole disks | `doctor` → `storage` |
| ~10 GiB free, plus ~1 GiB per image (~2 GiB for the harness image) | images, snapshots, VM disks; new/fork/resume are refused below 1 GiB free | `doctor` → `disk_space` |
| A guest image with the guest agent baked in | VMs boot from it | `doctor` → `images` |
| A Tailscale account (or a Headscale server) and an auth key | the tailnet gateway (sections 6, 8 and 9); optional, since the CLI reaches the host over SSH without it | `doctor` → `tailnet_*` |
| Optional: `pasta` (passt) or `slirp4netns`, plus `nft` | used for guest internet when present; otherwise the built-in userspace stack serves it | `doctor` → `egress` |

davit runs without root, and nothing changes host networking. The `kvm` group and the Ubuntu AppArmor profile
need an administrator once. A Mac cannot host VMs yet (see
[Not done yet](#not-done-yet)); a Mac or any Linux machine works as a client.

## 1. Install

Install the host with the host installer ([install-host.md](install-host.md)). It runs `davit init` and starts
the daemon as a systemd user service, so steps 2 to 4 below show what that did, and how to add a tailnet or
change the config later. After a config change, restart the service: `systemctl --user restart davit`.

The captured run below installed from release artifacts built from a checkout instead
([developing.md](developing.md#building-a-release-from-a-checkout)), and ran the daemon in the foreground.
The guest agent is the `davit` binary itself, baked into each image, so install the images of the same release
when you upgrade.

## 2. `davit init` and the tailnet

Create an auth key in the Tailscale admin console (Settings > Keys; reusable is not needed) or with
`headscale preauthkeys create`, then:

```sh
davit init --auth-key-stdin --owner-login you@example.com < ts-authkey.txt
```

- `--auth-key-stdin` stores the key 0600 in `~/.config/davit/tailnet-authkey` (the config's
  `tailnet.authKeyFile`) and never prints it. Instead, the daemon also reads `DAVIT_TS_AUTHKEY` from its
  environment. The key is needed only for the first enrollment; without one, the daemon logs a login URL.
- `--owner-login` (repeatable; `--owner-tag tag:NAME` for tagged nodes) names who owns this host over the tailnet.
  With an owner and a key, init enables the tailnet gateway. Owners pin to their stable tailnet user ID on first
  contact; every other tailnet identity is refused unless you add it as a [member](#8-share-with-other-people--not-captured).
- **Headscale:** add `--control-url https://headscale.example.com` (the config's `tailnet.controlURL`; empty means
  Tailscale). `--hostname NAME` picks the node name (default `davit-<os hostname>`).
- HTTPS: set `"https": true` under `tailnet` in the config to serve the tailnet's own certificate (enable HTTPS
  certificates in the admin console); the SDKs need HTTPS for remote hosts. Headscale, or tailnets without HTTPS
  certificates, use `tlsCertFile` / `tlsKeyFile` ([network.md](network.md#remote-access-one-configuration-for-cli-ssh-and-sdks)).

Without `--runtime`, init picks Firecracker when `/dev/kvm` works and says why if it falls back to the fake
runtime. Re-running init is safe; `--force` regenerates the config (the old one is backed up) and is needed to
change `--runtime`, `--hostname` or `--control-url` later; `--service` installs a systemd user unit.

The captured run (no tailnet on this box, so no key or owner; it passed `--image` because it did not install
images through `install.sh`):

```text
$ davit init --data-dir <work>/d --runtime firecracker --image <work>/image/manifests/ubuntu-24.04-base-local.json --image-blobs <work>/image/blobs
wrote ~/.config/davit/config.json
runtime   firecracker (requested with --runtime)
data      <work>/d
socket    <work>/r/davit/davit.sock
image     ubuntu-24.04-base-local (blobs verified)

Next steps:
  1. Create a tailnet auth key (Tailscale admin console > Settings > Keys) and store it: davit init --auth-key-stdin < key.txt (or set DAVIT_TS_AUTHKEY for the daemon)
  2. Name the tailnet owner to enable the gateway: davit init --owner-login you@example.com
  3. Start the daemon: davit daemon   (or install it as a user service: davit init --service)
  4. Check the host: davit doctor
  5. Create your first VM: davit new
```

## 3. `davit doctor`

Every check is pass/warn/fail with a one-line fix. By default `doctor` prints a summary and only the problems;
`-v` prints every check, as captured below (an older run, before the tailnet checks became optional skips);
the daemon runs the same checks at startup and refuses to start on a failure. `--json` gives one JSON line per
check.

```text
$ davit doctor -v
PASS  config         ~/.config/davit/config.json (runtime firecracker)
PASS  platform       linux/amd64
PASS  cpu_virt       AMD-V available on bare metal; kvm nested=1 for guests of this host
PASS  kvm            /dev/kvm is read-write, KVM API 12
PASS  userns         unprivileged user+network namespaces work
PASS  tun            /dev/net/tun is usable
PASS  cgroup_v2      controllers: cpuset cpu io memory hugetlb pids rdma misc dmem
PASS  vsock          /dev/vhost-vsock present
PASS  memory         27.2GiB total, 11.3GiB available
PASS  firecracker    Firecracker v1.17.0 (<work>/d/bin/firecracker-v1.17.0-x86_64)
PASS  storage        FICLONE ok on btrfs
PASS  disk_space     51.2GiB free
PASS  images         ubuntu-24.04-base-local (blobs present, guest agent baked in)
PASS  network        rootless per-VM user+net namespace with tap0 (guest 172.16.0.2)
PASS  egress         userspace (built-in gVisor stack), LAN/host/metadata deny list enforced by the stack and by nft
PASS  tailnet_state  <work>/d/tailnet is private and writable
WARN  tailnet_auth   configured auth key file ~/.config/davit/tailnet-authkey is missing
                     fix: davit init --auth-key-stdin < key-file
SKIP  tailnet_owner  tailnet gateway disabled
                     fix: davit init --owner-login you@example.com (with an auth key stored) enables it; or set "enabled" and "ownerLogins" under "tailnet"
WARN  daemon         not reachable: cannot reach the daemon: Get "http://local/api/v1/capabilities": dial unix <work>/r/davit/davit.sock: connect: no such file or directory
                     fix: davit daemon   (or: systemctl --user start davit)

15 passed, 3 warnings, 1 skipped
```

## 4. Start the daemon

```sh
davit daemon            # foreground; or: davit init --service (systemd --user), then loginctl enable-linger $USER
```

The pinned Firecracker release is downloaded on first start into the data dir and verified by sha256. The
gateway enrolls in the background (the local socket works meanwhile); run `davit doctor` again once the daemon is
up: `tailnet_gateway` then reports the enrollment (`needs-login` with the URL, `needs-approval`, `up` with the
URL clients use) or the fix for what went wrong.

## 5. Use it

```text
$ davit new box --cpus 2 --memory 1GiB
create succeeded in 4013ms
box (vm_wg6iwpbolvgchjwz)
  state      ready (rev 2, gen 1)
  boot       golden_restore
  shape      2 cpus, 1GiB memory, 8GiB disk
  storage    reflink (fastClone=true)
  auto-stop  2026-10-22T14:02:29+07:00
```

The first `new` of an image and shape boots it once and captures a golden snapshot; later ones restore it:

```text
$ davit new quick --cpus 2 --memory 1GiB
create succeeded in 730ms
```

Run commands (argv is passed as-is; use `sh -c` for shell syntax) and copy files:

```text
$ davit exec box -- uname -sr
Linux 6.18.58-davit

$ davit exec box -- sh -c 'grep PRETTY /etc/os-release; nproc; free -m | sed -n 2p'
PRETTY_NAME="Ubuntu 24.04.5 LTS"
2
Mem:             983         203         751           0         156         779

$ davit cp ~/hello.txt box:/tmp/hello.txt
copied ~/hello.txt to vm_wg6iwpbolvgchjwz:/tmp/hello.txt (20 B in 14ms)

$ davit exec box -- cat /tmp/hello.txt
hello from the host
```

**Which guest user.** `exec`, `cp`, `ssh`/`scp` and `ssh box@<gateway>` run as the image's default user
(`guest.user.name` in its manifest; `davit` in the stock images), who has passwordless `sudo`, so
`sudo apt install …` works. Ask for root explicitly with `--user root` (`davit exec box --user root -- …`,
`davit cp --user root …`). Over SSH use `sudo`: the stock images' sshd refuses root logins.

`stop` shuts the guest down cleanly and commits its disk; `fork` makes an independent VM from that commit;
`resume` boots the committed disk again. Processes do not survive stop/resume; files do:

```text
$ davit exec box -- sh -c 'echo fork-me >/home/davit/mark'
$ davit stop box
stop succeeded in 1774ms
$ davit fork box box2
fork succeeded in 1877ms
$ davit exec box2 -- cat /home/davit/mark
fork-me
$ davit exec box2 -- sh -c 'echo only-in-box2 >/home/davit/mark; cat /home/davit/mark'
only-in-box2
$ davit resume box
resume succeeded in 1645ms
$ davit exec box -- cat /home/davit/mark
fork-me
$ davit delete box2 --yes
delete succeeded in 183ms
```

A guest that ignores the shutdown request gets `stopGrace` (default 30 s) per attempt, `stopAttempts` attempts
(default 3, at most 10) in the daemon config; each timed-out attempt shows up as an operation event. After the
last one the VMM is killed and the snapshot is recorded crash-consistent, as with `stop --force`; set
`"stopForceAfterRetries": false` to have the stop fail instead (the VM goes to `error` and keeps its last good
snapshot).

A VM is resized when it boots: `davit resize box --cpus 4 --memory 8GiB --disk 40GiB` resumes a stopped VM with
the new shape (`--type NAME` picks an administrator profile, `--stop` stops a running VM first; `davit resume`
takes the same shape flags). CPUs and memory may change either way within the runtime's limits; the disk can only
grow. The grown disk is a larger clone of the committed one, but the guest filesystem is not grown for you: run
`davit exec box -- sudo resize2fs /dev/vda` once to use the new space.

If a guest wedges (no `exec`, no SSH, a clean `stop` times out), `davit recover box` power-cycles it
without replacing its disk: the VMM is killed with no RAM or disk commit, and the VM's current disk cold
boots as is in a new generation. Files the guest wrote survive the way they survive a power loss
(crash-consistent; data still in the guest's page cache is lost), processes do not, nothing rolls back to a
snapshot and no snapshot is taken. It asks first (`--yes` skips the question; scripts and `--json` must
pass it) and waits like `resume` (`--no-wait` returns the operation). It works on `ready`, `running` and
`error` VMs that still have a working disk; a stopped VM is resumed instead, and an `error` VM whose disk
is gone resumes from its last snapshot.

`davit new box --count 5` creates `box-1` ... `box-5` in one request (`createVMs`, at most 100); each VM
is admitted and reported on its own, and a retry with the same key creates only the missing ones. At most
`limits.concurrentBoots` boots (create, fork, resume, recover; default 8, negative for no limit) run at once
in the daemon; the others wait queued, with an operation event saying so. Stop and delete never wait.

Agents inside VMs are untrusted, so you can narrow what they reach. Egress is per VM: `filtered` (the
default: public internet, never your LAN, the host, cloud metadata or the tailnet), `allowlist`, or `off`.
Domains in an allowlist cover their subdomains; DNS for anything else gets NXDOMAIN. Changes apply to a running
VM at once, and forks inherit their source's policy:

```console
$ davit new agent --egress allowlist --allow github.com,registry.npmjs.org
$ davit egress agent off          # cut it off now; existing connections close
$ davit egress agent filtered
```

Every `stop` commits the disk as a snapshot (`davit snapshots box`). Its files can be read on the host
without booting it, root or a mount; paths resolve inside the snapshot (`..` and symlinks never leave it), and
every transfer is checked against the daemon's SHA-256:

```console
$ davit snapshot tree SNAPSHOT /home/davit --all     # list a directory
$ davit snapshot pull SNAPSHOT /home/davit/proj ./   # a file, or a directory (modes and symlinks kept)
$ davit snapshot export SNAPSHOT -o fs.tar             # the whole filesystem; --format raw: the disk image (sparse)
```

A crash-consistent snapshot (the VM was killed, not stopped) is read as it is on disk, without journal replay.
Reading needs an `exec` token scope, as `cp` does; a read in progress completes even if the snapshot is deleted.

### Environments

An environment is a named, versioned workspace template: repositories to clone, non-secret variables, secret files
(named host secrets, never values) and setup commands, plus a credential profile whose grants new VMs receive.
Every change is a new immutable revision; a VM pins one, and its workspace is prepared after the VM is ready:

```sh
cat > web.json <<'JSON'
{"repos": [{"url": "https://github.com/you/web.git", "path": "/home/davit/web"}],
 "variables": {"NODE_ENV": "development"}, "secretFiles": [],
 "setup": [{"name": "deps", "argv": ["npm", "ci"], "cwd": "/home/davit/web"}]}
JSON
davit env create web --file web.json
davit new w1 --environment web            # the latest revision; web@2 pins revision 2
davit env revise web --file web.json      # a new revision; existing VMs keep theirs
davit env revisions web
```

### Ephemeral and private VMs

By default davit keeps a lot on the host: every stop commits the disk, and it stores your managed
processes' output (`history`) and samples of every guest process (`ps` on a stopped VM). A per-VM privacy
policy turns that off. Set it at `new` (also with `--count`) or later with `davit privacy`; forks inherit it:

```console
$ davit new scratch --ephemeral                      # deleted when it stops
$ davit new agent --no-snapshot --no-output-retention --no-process-samples
$ davit privacy agent                                # show the policy
$ davit privacy agent --no-output-retention=false    # change one flag, keep the others
```

- `--ephemeral`: the VM is deleted when it stops, whatever stops it: `davit stop`, the TTL auto-stop or a
  forced stop. Nothing is committed; its automatic snapshots and working disk are removed. If the daemon
  crashes in the middle of that stop, it finishes the delete when it recovers. A VM that crashes into `error`
  has not stopped: it stays, disk included, until you `delete` or `recover` it.
- `--no-snapshot`: a stop commits nothing and throws the working disk away, so the stopped VM cannot be
  resumed or forked, and `snapshot create` is refused. The VM record stays until you delete it.
- `--no-output-retention`: the host stores no process output; `history` lists the processes (argv, exit
  status, times; argv is still kept, so do not put secrets on the command line) without their output. You
  can still follow output live while the process runs. Inside the guest, the guest agent keeps each
  process's recent output in its memory (up to 8 MiB per process, for the last 256 exited processes) while
  the VM runs, so `exec --attach` works; it is gone when the VM stops. A live fork (`fork --live`) copies
  the guest's memory, that buffer included, into the new VM.
- `--no-process-samples`: no whole-guest process samples are stored; `ps` still answers live from a
  running VM, and a stopped one has no last sample to show.

Limits, plainly:

- These flags limit what davit keeps. They are **not secure erasure**: deleted disks and records can
  survive in backups or snapshots of the host filesystem, in filesystem free space and in SSD wear
  levelling. Use full-disk encryption on the host if that matters.
- The guest is not limited: it can always write anything to its own disk (kept until the VM's disk is
  discarded) and send data anywhere its egress policy allows (`--egress off` cuts that).
- With the Firecracker runtime, the VMM log on the host (`vmm.log` in the VM's instance directory, replaced
  at each boot and removed with the instance) captures the guest's serial console: kernel messages and
  anything a guest process writes to `/dev/console`. The privacy flags do not change that.
- A daemon that does not have the feature yet (`davit capabilities`: `privacyPolicy`) refuses these flags
  with `not_implemented` rather than ignoring them.

VMs stop themselves after their TTL (default 1 h from start; `--ttl`, `--no-auto-stop`). Every command takes
`--json` (one JSON object per line) for scripts and agents; the Python and TypeScript SDKs in [`sdk/`](../sdk)
wrap the same API.

### Who am I, tokens and updates

- `davit whoami` shows who you are to the daemon: the principal, how you authenticated (`local` over the Unix
  socket, `tailnet` as an owner identity, or `token`), the token's scopes and expiry, the endpoint and the daemon's
  version. It never prints a secret; for a token it names only the variable it came from (`DAVIT_TOKEN`).
- Token presets: `davit token create ci-bot --preset ci` (exec on every VM, or `--vm` ones: run commands, move
  files), `--preset readonly` (view: state, history, desktop status) or `--preset admin` (also lifecycle changes
  and opening or closing a desktop). No token can manage tokens, members or shares, whatever its preset.
- A member sees its access in `whoami` too: each shared VM, snapshot and environment with its role.
- `davit token rotate tok_... --overlap 10m` prints a new secret with the same name and scopes; the old one keeps
  working for the overlap (default: revoked at once) and never longer than before. `davit token usage` lists
  each token's use count and last use. Counts are written in batches (at most once a minute per token, and at
  shutdown), so a daemon crash can lose the last minute of counts.
- `davit version --check` compares this build with the latest published release (or the release directory in
  `DAVIT_RELEASE_URL` / `releaseBaseUrl`); offline it reports `unknown`. It never installs anything: to update,
  rerun `install.sh`.

Shell completion for every command: [quickstart.md](quickstart.md#shell-completion).

## 6. From your laptop (over the tailnet) — not captured

Not run on a real tailnet yet: the gateway, `ssh`, `forward` and the SDKs are proven end to end against an
in-process simulated tailnet (`TestRemoteHTTPSOverTailnet`, [network.md](network.md#enrollment-diagnostics-doctor-and-status)),
and the real laptop → host checklist is [acceptance-g2.md](acceptance-g2.md). Install the same `davit` binary on
the laptop (`install.sh --client`; macOS and Linux builds are in the release), log the
laptop into the tailnet as an owner, and point it at the host's gateway: the URL `doctor` prints on the host
under `tailnet_gateway`. Without a tailnet, every command below also works over SSH once `davit connect` has
saved the host ([quickstart.md](quickstart.md#3-connect-the-laptop)).

```sh
export DAVIT_API_URL=https://davit-myhost.tail1234.ts.net   # or pass --host davit-myhost.tail1234.ts.net to each command
davit status                          # checks the endpoint, TLS, identity and one request, with a fix for each failure
davit list
davit exec box -- make test
davit ssh box                         # OpenSSH through an API tunnel: short-lived certificate, pinned host key
davit ssh-config >> ~/.ssh/config     # then ssh / scp / rsync / sftp davit-box work directly
davit scp -r ./src box:/home/davit/src
davit forward box 8080:3000           # localhost:8080 -> the VM's port 3000, until Ctrl-C
davit forward --reverse box 5432:localhost:5432   # the VM's 127.0.0.1:5432 -> this laptop's Postgres, until Ctrl-C
davit expose box 3000 --name web      # a stable tailnet URL for the app on port 3000 (endpoints lists them)
ssh box@davit-myhost                  # plain OpenSSH also works: the gateway's SSH front door; box+root@... picks another user
```

Other tailnet identities get 403 from the API and an auth failure from SSH. For CI, mint a scoped service token on
the host (`davit token create ci-bot --preset ci`) and set `DAVIT_TOKEN` on the runner; the runner must still be
on the tailnet. The Python and TypeScript SDKs read the same `DAVIT_API_URL`, `DAVIT_TOKEN` and
`DAVIT_CA_FILE` ([sdk/README.md](../sdk/README.md)).

## 7. Run a coding agent — not captured

Covered by tests with fake harnesses; not run against a real model yet ([G5-ACCEPTANCE.md](research/G5-ACCEPTANCE.md)).
Use an image that has the harness: `ubuntu-24.04-harness` has Claude Code and Codex. OpenCode and pi adapters
exist (`--harness opencode`, `--harness pi`) but their images are declared profiles that nothing builds yet. Give
the VM a key through the host's credential proxy, so the real key never enters the VM:

```sh
davit new agent --image ubuntu-24.04-harness
davit credentials profile create work
davit credentials set work anthropic --provider anthropic --env ANTHROPIC_API_KEY < anthropic-key.txt
davit credentials grant agent work anthropic     # the VM gets a placeholder; the proxy swaps in the key
davit prompt agent "make the failing test pass"   # Claude Code by default; streams the turn's events
davit prompt agent "now add a regression test"    # the next turn resumes the same session
davit prompt agent --harness codex --new "review the diff"   # Codex needs an openai credential granted the same way
```

`davit conversations agent` lists conversations; `steer`, `interrupt` and `prompt-events --follow` drive a running
turn; `--attach FILE`, `--model`, `--effort` and `--turn-timeout` shape it. Every turn has a deadline (default
1 h) and holds the VM's auto-stop while it runs. Details: [Prompts and credentials](#prompts-and-credentials). Without a
grant, `--env NAME` passes a key from your shell into the VM instead (it then enters the guest).
**Behaviour change:** a credential `baseUrl` or webhook URL on the host, LAN or tailnet (e.g. a local model server) is
refused unless `network.allowPrivateUpstreams` is set ([network.md](network.md#credential-proxy-internalcredproxy)).

### Prompts and credentials

`prompt` runs a coding-agent CLI inside the VM as one turn of a conversation: Claude Code (`claude`, the default),
Codex (`codex`), OpenCode (`opencode`) or pi (`pi`), picked with `--harness`. Later turns resume the harness's own
session, one turn at a time per conversation. Harnesses must already be in the image (a missing one fails with
`harness_missing`, capability `harness.<name>`); the stock `ubuntu-24.04-harness` image has Claude Code and Codex.
They run with their permission prompts / sandbox bypassed because the VM is the sandbox.

- **Controls:** `--attach FILE` / `--attach-guest PATH` (images go to the harness natively, other files are placed in
  the VM and named by path), `--effort LEVEL` and `--fast` (the harness's own flags; `capabilities.harnesses[]`
  lists what each takes). `davit steer VM PROMPT TEXT` messages a running turn: Claude Code takes it in the turn
  (`native`), others restart the turn on their session and say so (`restart`). `davit interrupt VM
  --conversation C|--all` cancels; `davit prompt-events VM [--follow] [--after CURSOR]` is the VM-wide event feed,
  resumable by cursor.
- **Turn deadline:** every turn has one (`--turn-timeout D`, API `turnTimeoutSeconds`, at most 24h). Without it the
  daemon's `harness.defaultTurnTimeout` applies (default `1h`, at most `24h`). At the deadline the guest kills the
  harness and the turn fails with `turn_timeout`; until then the running turn holds the VM's auto-stop, so a TTL
  never stops a VM under a working agent.
- **Keys:** model API keys never enter a VM with a credential grant: `davit credentials grant VM PROFILE NAME`
  gives it a placeholder that the host credential proxy (`http://172.16.0.1:15080` inside the VM) swaps for the
  real key, only toward that credential's API. Every turn gets each grant's placeholder plus `ANTHROPIC_BASE_URL` /
  `OPENAI_BASE_URL` pointing at the proxy, so the harness never sees a key. Grants take precedence over `--env` /
  `--secret` of the same name, and the turn's first event reports the override. Without grants, **keys are never
  copied implicitly**, and any key you pass **enters the guest** as an environment variable of the harness process
  (not stored by davit): `--env NAME` forwards `NAME` from your shell, and `--secret NAME` reads it from the
  daemon's `harness.secretsFile` (a 0600 JSON object `{"NAME":"value"}`).

## 8. Share with other people — not captured

Other tailnet users can be **members** with their own rights; they see only what you share ([network.md](network.md#members-other-tailnet-users-with-separate-rights-internalmembers)):

```sh
davit members add bob@example.com --role exec
davit share box bob@example.com --role viewer    # viewer | exec | admin; MEMBER "all" = every member
davit share --snapshot SNAPSHOT bob@example.com  # a snapshot without its VM
davit shares box
davit unshare box bob@example.com                # cuts bob's live sessions on box at once
davit members remove bob@example.com
```

Members never create or fork VMs, never see credential grants or tokens, and never manage members or shares. An `exec` or `admin` share
pauses the VM's credential grants for everyone while it exists, unless you pass `--credentials`. Sharing does not
scrub the guest: share a fresh VM or a fork made for the purpose.

## 9. Desktop — not captured

A VM running a VNC server on guest loopback (port 5900 by default) shows its display in the browser through the
gateway, at a tailnet URL; view-only for `viewer` members and `view` tokens, enforced by the gateway:

```sh
davit desktop open box --browser   # prints the viewer URL and opens it
davit desktop url box
davit desktop close box            # cuts every viewer
```

The desktop image (`ubuntu-24.04-desktop`: Xvfb, Openbox, x11vnc, Chromium) is a declared profile that nothing
builds yet, so for now bring your own VNC server (`x11vnc -localhost -nopw`) in a VM. Proven with a fake VNC
server on the simulated tailnet ([network.md](network.md#desktop-the-vms-display-in-a-browser-gatewaydesktopgo-gatewayrfbgo)).

## Not done yet

- **Real-hardware runs of every change**: the Firecracker tier ([vm-tier.md](vm-tier.md)) runs by hand on a
  separate KVM host, not in `make check` or CI; the real-tailnet run ([acceptance-g2.md](acceptance-g2.md)) and a real-model prompt
  run ([G5-ACCEPTANCE.md](research/G5-ACCEPTANCE.md)) are pending.
- **Releases**: no published binaries or images; `install.sh` checks sha256 integrity, signatures are a TODO.
- **Images**: only `ubuntu-24.04-base` and `ubuntu-24.04-harness` are built; the OpenCode, pi and desktop images
  are declared profiles.
- **Guest ICMP**: the built-in userspace egress carries IPv4 TCP and UDP only, so `ping` from a guest fails on
  hosts without pasta/slirp4netns.
- **Mac backend**: the Go side exists (`"runtime": "mac"` on Apple silicon: the helper protocol, the
  adapter and APFS clone storage), but the signed Swift VZ helper does not yet, so `doctor` reports the
  missing helper and the daemon refuses to start that runtime. Use the fake runtime on a Mac for development.
- RAM `suspend`, `image` commands, a web console.
- Ubuntu ≥ 23.10 hosts need the AppArmor profile from `davit init --apparmor` (loaded once with two `sudo`
  commands) for unprivileged user namespaces; it has not been run on an Ubuntu host yet.

## Measured

Reference dev machine: AMD Ryzen 7 6800U (16 threads), 27 GiB RAM, btrfs, Linux 7.2.5, shared with other
jobs (load average around 25 observed during this session), Firecracker 1.17.0, guests with 2 vCPUs and
1 GiB RAM, daemon run at low priority.

| Step | Run 1 | Run 2 |
| --- | --- | --- |
| first `new` of the image (prepares the golden) | 6852 ms | 4013 ms |
| next `new` (golden restore) | — | 730 ms |
| `stop` (clean shutdown + disk commit) | 565 ms | 1774 ms |
| `fork` from the stopped disk (cold boot) | 1488 ms | 1877 ms |
| `resume` (cold boot of the committed disk) | 1644 ms | 1645 ms |

Two runs; times are the daemon's operation durations. More numbers in [benchmarks.md](benchmarks.md).

## Check a host end to end

```sh
scripts/fresh-host-e2e.sh                       # fake runtime: install, init, doctor, daemon, lifecycle
scripts/vm-tier.sh --fresh-host user@host '^$'  # the same on real Firecracker, on a VM host (docs/vm-tier.md)
```

It works in a throwaway `HOME`, installs through `install.sh` from a local file:// release, derives an image
carrying this build's agent (standing in for a release image), runs the tour above on real VMs, and removes
everything afterwards, also on failure.
