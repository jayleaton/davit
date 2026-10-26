# Troubleshooting

The errors you are most likely to meet on a first run, with what each means and how to fix it. `USER@HOST`
stands for your host, as you gave it to `davit connect`. davit prints an error as `error: MESSAGE (CODE)`.

Three commands help with all of them:

- `davit connect --check`, on your laptop, verifies the saved host again: the key, the SSH login, davit on the
  host and its daemon. It prints ✓ or ✗ for each step and changes nothing. `--host H` checks another host.
- `davit status`, on your laptop, is a quick repeatable check of the connection: the endpoint, who you are to
  the daemon, and one request.
- `davit doctor`, on the host, checks the host's prerequisites (KVM, user namespaces, disk, images) and its
  daemon, with a fix for each problem.

`--json` gives the result as JSON, for scripts and agents. Every davit command takes it.

## No host configured

```text
error: no davit host configured: run `davit connect user@host` (on the host itself: start the daemon with `systemctl --user start davit`, or `davit doctor`) (unavailable)
```

**What it means.** This machine has no saved default host, and no davit daemon runs here either. The CLI does
not know where to send the command.

**Fix.** On your laptop, connect it to the host:

```sh
davit connect you@your-host
```

On the host itself, start the daemon and check the host:

```sh
systemctl --user start davit
davit doctor
```

`davit client-config` shows the saved host. `--host` or `DAVIT_HOST` picks a host for one command.

## Host unreachable

```text
error: cannot reach USER@HOST over ssh: <ssh's reason> (check the address, that the host is on, and that `ssh USER@HOST true` works; behind a jump box: `davit connect --via JUMP USER@HOST`) (unavailable)
```

**What it means.** ssh could not open a connection to the host. The reason after the colon is ssh's own, for
example `Connection timed out`, `Connection refused` or `Could not resolve hostname`. Box commands exit with
code 7.

**Fix.**

1. Check the address: the host name or IP, the user, and `:PORT` if sshd is not on port 22.
2. Check that the host is on and that sshd runs there.
3. Check that ssh itself works without a prompt:

   ```sh
   ssh USER@HOST true
   ```

4. If the host is only reachable through a jump box, connect through it:

   ```sh
   davit connect --via JUMP USER@HOST
   ```

Then `davit connect --check`.

Any other ssh failure prints ssh's own message and how to diagnose it:

```text
error: ssh to USER@HOST: <ssh's message> (`davit connect --check` diagnoses the connection) (unavailable)
```

## Key not authorized

```text
error: USER@HOST does not accept this machine's SSH key: run `davit connect USER@HOST`, which prints the line that authorizes it (unauthenticated)
```

**What it means.** ssh reached the host, but the host refused this machine's key. davit logs in with
`BatchMode`, so it never asks for a password.

**Fix.** Run `davit connect`:

```sh
davit connect USER@HOST
```

It prints the one-liner that authorizes the key, then waits and retries every few seconds (up to 10 minutes in
a terminal). Run one of them:

```sh
ssh-copy-id -i ~/.ssh/id_ed25519.pub USER@HOST
```

or, logged in on the host as that user, paste the line it printed. It has this form, with this machine's
public key:

```sh
mkdir -p ~/.ssh && echo 'ssh-ed25519 AAAA… davit@laptop' >> ~/.ssh/authorized_keys
```

`ssh-copy-id` needs password login to work on the host; the pasted line does not. With `--key PATH`, davit
uses that key instead of `~/.ssh/id_ed25519`. With `--wait 0`, or when not run in a terminal, `davit connect`
does not wait: it prints the lines and exits with code 3. Run it again once the key is authorized.

## Host key changed or unknown

```text
error: the host key of USER@HOST changed or is unknown: if the host was reinstalled, verify its new key, then `ssh-keygen -R HOST` and `davit connect USER@HOST` (unauthenticated)
```

**What it means.** ssh refused the host's key. Either the host presents a different SSH host key from the one in
`~/.ssh/known_hosts`, or the host is not in `~/.ssh/known_hosts` yet and the command is not `davit connect`
(only `davit connect` accepts a new host key, on first contact). A changed key is expected after the host was
reinstalled or its sshd keys were regenerated. Otherwise it can mean you are talking to another machine.

**Fix.** For an unknown host, run `davit connect USER@HOST`. For a changed key, first verify the new key. On
the host's console (not over this connection), print its fingerprint:

```sh
ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
```

If it matches what ssh reports, remove the old key and connect again:

```sh
ssh-keygen -R HOST          # for a port other than 22: ssh-keygen -R '[HOST]:PORT'
davit connect USER@HOST
```

## davit missing on the host

```text
error: davit is not installed on USER@HOST (or not on its PATH): install it there (docs/install-host.md), then `davit connect --check` (unsupported_capability)
```

**What it means.** The SSH login works, but the host has no `davit` command where davit looks. Over SSH davit
runs `davit dial-stdio` on the host. It looks in `~/.local/bin`, then in `~/.svm/davit/bin`, then in the
system PATH. A non-interactive ssh session does not read your shell profile, so a PATH set there is not seen.

**Fix.** Install davit on the host ([install-host.md](install-host.md)), then check again from the laptop:

```sh
davit connect --check
```

If davit is installed somewhere else on the host, set `DAVIT_REMOTE_BIN` on the laptop to its path.

## Daemon not running on the host

```text
error: davit is installed on USER@HOST but its daemon is not running: on the host run `systemctl --user start davit` (or `davit doctor`), then `davit connect --check` (unavailable)
```

**What it means.** davit is on the host, but nothing answers on the daemon's socket. The service is stopped,
failed to start, or was never installed.

**Fix.** On the host:

```sh
systemctl --user start davit
davit doctor
```

`davit doctor` prints a fix for every check that fails, including the daemon. If the service does not stay up,
`systemctl --user status davit` and `journalctl --user -u davit` show why. Then, from the laptop,
`davit connect --check`.

A box command that loses the host later adds the same pointer to its error:
``; `davit connect --check` diagnoses the default host (`davit client-config` shows it), then retry``.

## Project setup failed

```text
error: image setup exited 1 (its output: `davit history PROJECT--image PROCESS`); the builder PROJECT--image is left running for a look (`davit ssh PROJECT--image`; it stops itself when idle). Fix the setup (.davit/setup.sh, or --setup-cmd) and run `davit box image build` again
```

**What it means.** The step that installs the project's dependencies failed on the first run (or on
`box image build` / `update`). The command after `--` did not run.

**Fix.** Read the setup's output with the `davit history` command it printed, or look around in the builder with
`davit ssh PROJECT--image`. Then fix the setup: commit a `.davit/setup.sh`, or pass the command with
`davit box image build --setup-cmd '…'`, and run `davit box image build` again. The setup runs in the synced
worktree, as the image's default user (who has passwordless `sudo`), in a login shell.
[project-boxes.md](project-boxes.md#the-project-image-and-its-setup) lists what is detected.

An exit of 137 is the guest killing the setup (or a `box run` command) for memory. A JS project's boxes get
4 GiB, other projects' 2 GiB ([Box shape](project-boxes.md#box-shape)); ask for more with `--memory 8GiB`, which
also resizes an existing box ([below](#box-out-of-memory)).

## Box out of memory

**What it means.** `box run` exits with 137 (128 + SIGKILL) and nothing else failed: the guest kernel's OOM
killer usually ended the command because the box has too little memory. `davit box run -- grep MemTotal
/proc/meminfo` shows what the box has. A JS project's boxes start with 4 GiB, others with 2 GiB
([Box shape](project-boxes.md#box-shape)).

**Fix.** Give the box more: `davit box up --memory 4GiB` (or `box run --memory 4GiB -- …`). An existing box
is resized and keeps the new size ([project-boxes.md](project-boxes.md#resizing-a-box)). A running box is
stopped and started again, its disk kept. If processes started through davit still run in it, the command
refuses (`stop_required`), names them and says what to run once they are done:

```text
error: box PROJECT/INSTANCE runs with 2 cpu, 2.0 GiB memory, 8.0 GiB disk; the new shape needs a restart, which would end the 1 process(es) started through davit still running in it (PROCESS). When they are done, run `davit box down PROJECT/INSTANCE` and then `davit box up PROJECT/INSTANCE --memory 4GiB` (stop_required)
```

## Host full

A box that does not fit does not fail at once. The host first stops idle boxes to make room, then the command
waits in a queue for up to `--wait` (default 3 minutes; `--wait 0s` fails at once). While it waits, stderr
says:

```text
waiting for host capacity (N ahead, X needed, Y free)
```

When the wait runs out, the command fails with `resource_exhausted` and exit code 8. The message names what ran
out, and box commands add what to try:

```text
error: host has 900 MiB of memory available; this VM needs 2048 MiB and 256 MiB must stay free (limits.minFreeMemoryBytes); stop or delete VMs, or retry later; the host is full: free capacity with `davit box ls` then `davit box down PROJECT/INSTANCE` (or `box rm`), ask for less with --cpus/--memory/--disk, or queue longer with --wait 10m (resource_exhausted)
```

The first part depends on what ran out:

| Message starts with | Out of |
| --- | --- |
| `host has N MiB of memory available; this VM needs …` | free memory on the host |
| `live VMs reserve N MiB of the N MiB memory budget …` | the memory reservation (`limits.memoryBytes` × `limits.memoryOvercommit`) |
| `host has N live VMs, the limit (limits.vms) is N` | the VM count limit |
| `host cpu limit exceeded`, `host disk limit exceeded` | `limits.cpus`, `limits.diskBytes` |
| `host data dir has N bytes free, below the N-byte floor` or `host disk is full` | disk space on the host |

**Fix.** Free memory by stopping boxes, or disk by removing them. From your laptop:

```sh
davit box ls                          # the host's memory and reservation, then your project images and VMs
davit box down PROJECT/INSTANCE       # stop a running VM; its disk is kept
davit box gc                          # stop every box of yours idle for at least 10 minutes
davit box rm PROJECT/INSTANCE --yes   # delete a VM and its disk
davit box image rm PROJECT --yes      # delete a project image and its versions (--force: its VMs too)
```

`box gc --rm-older-than 7d` also deletes VMs stopped for at least that long, and `--dry-run` lists what it would
do. Other options:

- Ask for less: `davit box up --memory 1GiB` (or `--cpus`); on a box that exists this resizes it
  ([project-boxes.md](project-boxes.md#resizing-a-box)). The disk only grows.
- Wait longer: `--wait 10m` on `box up`, `run`, `ssh` or `sync`.
- Raise the limits, if the host has the room: set `limits` in the daemon's config on the host, then
  `systemctl --user restart davit`. [vm-host.md](vm-host.md#capacity-running-vs-parked-boxes) explains
  `memoryBytes`, `memoryOvercommit`, `vms` and `cpus`, with numbers.

[project-boxes.md](project-boxes.md#when-the-host-is-full) describes how the host makes room and queues
requests.
