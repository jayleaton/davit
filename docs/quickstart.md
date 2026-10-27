# Quickstart

From nothing to `davit box run -- make test` running your repository's command in a VM on your own Linux
host. Your laptop reaches the host over SSH. No Davit account, tailnet or open port is needed.

There are two machines:

- **The host**: any Linux box with KVM. It runs the davit daemon and the VMs.
- **Your laptop** (macOS or Linux): it runs the `davit` CLI, from inside your git repositories.

They can be the same machine. Then skip steps 2 and 3.

## Requirements

| Machine | Needs |
| --- | --- |
| Host | Linux x86_64 with read-write `/dev/kvm`: bare metal, or a VM with nested virtualization. The prebuilt images are x86_64 only, so arm64 machines can be clients but not hosts yet |
| Host | A user account with systemd (the daemon runs as a systemd user service) and an SSH server your laptop can reach. Step 3 helps you authorize your key |
| Host | Free disk for the image and your projects' boxes. [install-host.md](install-host.md) gives the current figure for the prebuilt install. Building images from source needs more ([vm-host.md](vm-host.md#what-the-host-needs)). On btrfs or XFS (with `reflink=1`, the default), VM disk copies are instant; other filesystems work but copy whole disks |
| Laptop | macOS or Linux, and the OpenSSH client (`ssh`) |

davit itself runs without root. Some host prerequisites need an administrator once: the `kvm` group, and on
Ubuntu 23.10 and later the AppArmor restriction on user namespaces. The host installer checks them, and for
each one it prints the exact command and why, then stops so you can run them and rerun the installer. With
`--yes-sudo` it runs them itself (sudo asks for your password as usual).
[install-host.md](install-host.md) has the full list.

## 1. Install davit on the host

On the host:

```sh
curl -fsSL https://github.com/jayleaton/davit/releases/download/v0.1.0-rc4/install.sh | sh -s -- --host
```

This installs `davit` and a prebuilt developer image (Ubuntu 24.04 with Go, Node.js, pnpm, uv, mise and
Docker), starts the daemon as a systemd user service, and checks the host. Run the check again at any time:

```sh
davit doctor
```

`davit doctor` checks the host's prerequisites and its daemon. It prints one summary line, then one line per
failure (`✗`) or warning (`!`), each followed by its fix. `davit doctor -v` lists every check, and
`davit doctor --fixes` prints only the root commands. Prerequisites, options and upgrades: [install-host.md](install-host.md).

## 2. Install the CLI on your laptop

On your laptop:

```sh
curl -fsSL https://github.com/jayleaton/davit/releases/download/v0.1.0-rc4/install.sh | sh -s -- --client
```

This installs only the `davit` CLI (into `~/.local/bin` by default). It installs no daemon and no images. On
macOS the installer installs the client even without `--client`. If your shell does not find `davit`
afterwards, add `~/.local/bin` to your PATH, as the installer says.

## 3. Connect the laptop

```sh
davit connect you@your-host
```

`you@your-host` is what you would pass to `ssh`: a user and a host name or address, with `:PORT` if sshd is
not on 22. `davit connect` prints one line per step, with ✓ or ✗:

1. **key**: it uses `~/.ssh/id_ed25519` (or `--key PATH`). It creates that key only if it is missing and the
   host refuses your existing SSH setup, and it never overwrites a key.
2. **ssh**: it tests a non-interactive login (`ssh -o BatchMode=yes`). On the first contact it accepts the host's
   key, as `ssh-copy-id` does. If the host refuses the key, it prints two ways to authorize it:

   ```sh
   ssh-copy-id -i ~/.ssh/id_ed25519.pub you@your-host
   ```

   or a line to paste on the host:

   ```sh
   mkdir -p ~/.ssh && echo 'ssh-ed25519 AAAA… davit@laptop' >> ~/.ssh/authorized_keys
   ```

   Then it waits and retries every few seconds, for up to 10 minutes in a terminal. `--wait D` changes the
   wait. With `--wait 0`, or when it is not run in a terminal, it exits at once with exit code 3.
3. **davit**: it checks that davit is installed on the host and prints its version.
4. **daemon**: it checks that the host's daemon answers through `davit dial-stdio`, the same path every later
   command uses, and prints the daemon's version and runtime.
5. **saved**: it saves `ssh://you@your-host` as this machine's default host, in `~/.config/davit/client.json`
   (macOS: `~/Library/Application Support/davit/client.json`).

It ends with:

```text
✓ connected to you@your-host (davit VERSION, runtime RUNTIME)
next: cd your-repo && davit box run -- make test
```

From then on every `davit` command on this laptop talks to that host, with no flags or environment.

The full form:

```text
davit connect [USER@]HOST[:PORT] [--via JUMP] [--key PATH] [--wait D] [--json]
davit connect --check [--json]
```

- `--via JUMP`: the host is behind a jump box. This is OpenSSH's ProxyJump, and the saved host becomes
  `ssh://you@your-host?jump=JUMP`.
- `--check`: verifies the saved host again (or the one given with `--host`). It changes nothing. Run it when
  something stops working; [troubleshooting.md](troubleshooting.md) has the fix for each failure.
- `--json`: machine-readable output, for scripts and agents.
- An `https://` URL instead of `USER@HOST` connects to a host's tailnet gateway ([network.md](network.md)). It
  skips the SSH steps.
- `davit status` is a repeatable check of this client's connection: endpoint, identity and one request.
  `davit doctor`, run on the host, checks the host's prerequisites.
- `davit client-config` shows the saved host, and changes it (`--set-host HOST`, `--clear`).
- `--host` or `DAVIT_HOST` picks another host for one command. It accepts `user@host`, `ssh://…`, a tailnet name
  or an `https://` URL.

## 4. Run your first box

```sh
cd your-repo
davit box run -- make test
```

`davit box run` runs your repository's command, whatever it is: `make test`, `go test ./...`, `pnpm test`,
`uv run pytest`. The first run in a repository does more:

1. **Dependencies.** davit makes a VM for this repository and installs its dependencies there, once. It runs
   `.davit/setup.sh` if the repository has one. Otherwise it runs every step that applies:

   | Found at the top of the repository | Setup step |
   | --- | --- |
   | `mise.toml`, `.mise.toml` or `.tool-versions` | `mise install` (later steps then run with the pinned tools) |
   | `go.mod` | `go mod download` |
   | `package.json` | `pnpm install`, `npm ci`, `yarn install` or `bun install`, by lockfile (`npm install` without one); a [Vite+](https://viteplus.dev) project gets vp and `vp install`, with the package manager `packageManager` pins |
   | `uv.lock`, `pyproject.toml` or `requirements.txt` | `uv sync`, or a `.venv` with `uv pip install -r` |
   | a Makefile with a `setup` target | `make setup` |

   The result is saved as the project image, which every later run starts from. A JS project's VMs get 4 GiB
   memory instead of 2 GiB (`--memory` sets another), and its `node_modules/.bin` is on PATH, so `vp` or
   `tsc` run as they are.
2. **Your VM.** This git worktree gets its own persistent VM, made from the project image. Another worktree of
   the same repository gets its own VM from the same image.
3. **Sync.** Before each run, the worktree's files are copied in: tracked files, and untracked files that are
   not ignored. `.git` is not copied, but the files arrive as a git index, so `git status` works inside.
4. **Run.** The command runs in the same relative directory as your shell. stdout and stderr stream back, and
   davit exits with the command's exit code.

Later runs reuse the project image and the VM, so only the sync and your command remain.

**When the detection is not enough** (private registries, services, generated code, warm build caches), commit a
`.davit/setup.sh`, or give the setup as a command:

```sh
davit box image build --setup-cmd 'sudo apt-get install -y libpq-dev && make deps'
```

`davit box image update` runs the setup again on top of the current image, and takes the same flags. A failed setup prints its exit
code and how to read its output. Details: [project-boxes.md](project-boxes.md#the-project-image-and-its-setup).

**Sync adds and updates; it never deletes.** A file you delete or rename locally stays in the box under its old
name. When that matters (a test that globs files, a stale generated file), delete it in the box as well
(`davit box run -- rm path`), or start this worktree's VM over with `davit box rm --yes`: the next run makes a
new one from the project image.

## Day to day

```sh
davit box run -- make test           # sync, run, stream, exit with the command's code
davit box sync --pull coverage.out   # copy a file or directory back (paths relative to the repo's top level)
davit box ssh                        # a shell in this worktree's VM
davit box ls                         # the host's memory, your project images and VMs
davit box down                       # stop this worktree's VM; its disk is kept
```

- **Idle stop.** A box stops itself after 15 minutes with no command, stream or `box ssh` session
  (`--idle`; `0s` turns it off). The disk is kept. `run`, `ssh` and `sync` start a stopped one again.
- **New dependencies.** After changing `.davit/setup.sh` or the lockfiles, `davit box image update` makes a new
  version of the project image, and `davit box up --refresh` moves this worktree's VM to it. `--refresh`
  discards the VM's disk, and says so. VMs made from the older version keep working.
- **Cleaning up.** `davit box rm --yes` deletes this worktree's VM and its disk. `davit box gc` stops idle
  boxes, and `davit box image rm --yes` deletes the project image (it refuses while the project has VMs;
  `--force` deletes them too).

Every box command takes `--json`. [project-boxes.md](project-boxes.md) has every command and flag, and what
happens when the host is full.

## Use it from AI agents

Paste the block from [the README](../README.md#use-it-from-ai-agents) into your repository's `AGENTS.md` or
`CLAUDE.md`. It tells Claude Code, Codex and other coding agents to run heavy commands in the box and to say so
when the box is not available.

## Shell completion

`davit completion bash|zsh|fish` prints a completion script for commands, their flags and subcommand words
(`snapshot create|delete|...`), generated from the CLI's own command table:

```sh
echo 'source <(davit completion bash)' >> ~/.bashrc                 # bash
mkdir -p ~/.zfunc && davit completion zsh > ~/.zfunc/_davit        # zsh: fpath=(~/.zfunc $fpath) before compinit in ~/.zshrc
                                                                        #   (or: source <(davit completion zsh) after compinit)
davit completion fish > ~/.config/fish/completions/davit.fish      # fish
```

Where a command takes a VM first (`stop`, `exec`, `info`, ...), the scripts offer VM IDs and names from
`davit list`, run under `timeout 2` with errors discarded; without `timeout(1)` (stock macOS) they offer none.
Only the bash script is exercised by tests here; the zsh and fish scripts are generated the same way but were not
loaded in those shells.

## Next steps

- Something failed: [troubleshooting.md](troubleshooting.md).
- Every box command, the setup detection and host capacity: [project-boxes.md](project-boxes.md).
- Plain VMs, forks, snapshots, coding agents inside VMs, sharing and the desktop: [vms-and-agents.md](vms-and-agents.md).
- Reaching the host over a tailnet instead of SSH: [network.md](network.md).
- What is verified and what is not: [status.md](status.md).
