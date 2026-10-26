# davit Python SDK

A Python client for the davit HTTP API v1 (`api/openapi.yaml`). It is a typed,
synchronous client for Python 3.10 or newer, and httpx is its only runtime
dependency. It behaves like the Go CLI: waits are on by default, every mutation that
is not naturally idempotent carries an idempotency key, and retries happen only when it is safe to retry.

An `AsyncClient` is **not provided** yet.

## Install

```sh
pip install ./sdk/python            # from a repository checkout
pip install -e './sdk/python[dev]'  # editable, with test/lint tools
```

## Quick start: local socket

With no arguments, the client uses `$DAVIT_API_URL` if it is set
(`unix:///abs/path.sock` or an https URL). Otherwise it uses the daemon's
default socket:

- Linux: `$XDG_RUNTIME_DIR/davit/davit.sock`, or
  `$XDG_DATA_HOME/davit/davit.sock`, or
  `~/.local/share/davit/davit.sock`.
- macOS: `~/Library/Application Support/davit/davit.sock`.

```python
from davit import Client
from davit.models import ShapeOverride

with Client() as sw:  # or Client(socket="/run/user/1000/davit/davit.sock")
    print(sw.get_capabilities().runtime.engine)
    vm = sw.create_vm("web", ttl=3600)  # waits; returns the ready VM
    sw.stop_vm("web")  # commits the disk
    base = sw.create_snapshot("web", "base")  # names the committed disk
    copy = sw.fork_vm("web", "web-2", snapshot="base")
    twin = sw.fork_vm("web", "web-3", live=True)  # running VM: RAM + processes + disk
    sw.resume_vm("web")
    sw.stop_vm("web")
    sw.resume_vm("web", shape=ShapeOverride(cpus=4, disk_bytes=40 << 30))  # resize; disk only grows
    sw.set_vm_egress("web", "allowlist", allow=["github.com", "pypi.org"])
    for v in sw.iter_vms():  # follows nextCursor
        print(v.name, v.state)
    sw.delete_vm("web-2")
```

## Quick start: remote host (https and a token)

```python
from davit import Client

sw = Client(base_url="https://davit-host.tailnet.ts.net", token="slw_...")
```

The token is the `secret` of a service token (see [Tokens](#tokens)) and is
sent as `Authorization: Bearer <token>`. Plain `http://` is allowed
only for loopback hosts (`localhost`, `127.0.0.0/8`, `::1`). All routes live
under `/api/v1`; give `base_url` without that suffix.

## Remote access over the tailnet

The daemon's tailnet gateway serves the same API over HTTPS. The CLI, SSH and
both SDKs share one client configuration, three environment variables:

| Variable | Meaning | Client argument (wins) |
| --- | --- | --- |
| `DAVIT_API_URL` | `https://<host>[:port]` | `base_url=` (or `socket=`) |
| `DAVIT_TOKEN` | optional bearer token (`slw_…`, `davit token create`) | `token=` |
| `DAVIT_CA_FILE` | optional PEM bundle of extra trusted CAs | `ca_file=` |

- Without a token the gateway authenticates the caller's tailnet identity
  (nothing to configure). `$DAVIT_TOKEN` is used only for http(s) base URLs,
  never for the local unix socket, which is owner-only and never gets a
  bearer. `token=""` sends no token even when the variable is set.
- With Tailscale's certificate for the host's MagicDNS name (publicly trusted)
  no CA file is needed. For an operator certificate signed by a private CA, set
  `DAVIT_CA_FILE` (or `ca_file=`): the bundle is trusted **in addition to**
  the system roots, for HTTP requests and for the WebSocket streams (process
  attach, tunnels) alike. A missing, unreadable or certificate-free file raises
  `ValueError` when the `Client` is constructed. The variable is ignored for
  the local socket; `ca_file=` together with `socket=` is a `ValueError`.

```python
from davit import Client

with Client() as sw:  # all three from the environment
    print(sw.get_capabilities().runtime.engine)

sw = Client(
    base_url="https://box.example.internal:8443", token="slw_...", ca_file="/etc/davit/ca.pem"
)
```

[`examples/remote_https.py`](examples/remote_https.py) reads only those
three variables, prints the runtime engine and the VMs, creates a VM (name from
`argv[1]`, default `sdk-https`) and waits until it is ready, runs `uname -a`,
prints its stdout and deletes the VM. It exits 0 on success, 1 on an API
error (printing `<code>: <message>`) or a failed command, 2 on bad
configuration. From the repository root:

```sh
DAVIT_API_URL=https://davit-host.tailnet.ts.net \
DAVIT_TOKEN=slw_... DAVIT_CA_FILE=/path/to/ca.pem \
  .build/py-venv/bin/python sdk/python/examples/remote_https.py my-vm
```

## Resize

`resume_vm(vm, type=..., shape=...)` resizes a stopped VM as it boots (feature
`resize`): CPUs and memory change freely within the runtime's limits, the disk
can only grow (`InvalidArgument` with `details["reason"] == "disk_shrink"`
otherwise), and a running VM raises `StopRequired`. The grown block device is
not grown inside the guest: run `sudo resize2fs /dev/vda` in the VM to use the
new space.

## Waits, operations and idempotency

`create_vm`, `fork_vm`, `stop_vm`, `resume_vm`, `recover_vm` and `delete_vm` all
return the final `VM` by default. For `delete_vm` that is the `deleted` tombstone.
`create_snapshot` returns the new `Snapshot`.

`recover_vm` power-cycles a wedged VM on its current disk: the VMM is killed
without a RAM or disk commit and the disk cold boots as is (files survive
crash-consistent, processes do not, no snapshot is taken). It works on ready,
running and error VMs that still have a working disk; a stopped VM raises
`InvalidState` (`invalid_state`): resume it instead.

Pass `wait=False` to get the 202 `OperationAccepted` back immediately. You can
follow the operation later:

```python
acc = sw.create_vm("job", wait=False, idempotency_key="job-42")
op = sw.wait(acc.operation.id, timeout=120, on_progress=lambda ev: print(ev.phase, ev.progress))
vm = sw.wait_ready("job")  # waits for any active operation, then checks ready/running
```

- `wait` long-polls `/operations/{id}/events`, with each poll waiting at most
  25s. The default wait budget is 10 minutes (`Client(wait_timeout=...)`).
- When the budget runs out, `wait` raises `WaitTimeout`, which carries
  `operation_id`. The operation keeps running on the server; call `wait()`
  again to resume.
- If the operation fails, `wait` raises `OperationFailed`. Its `.operation` is
  the operation, and its `.error` is the typed error (for example
  `ResourceExhausted`).
- Lifecycle mutations (and `exec_process`, `create_prompt`, `steer_prompt`, `create_token`,
  `create_credential_profile`, `create_credential_grant`) send an
  `Idempotency-Key`: yours, or a UUIDv4 generated once per call.
  If you reuse a key with the same body, you get the same operation; reusing
  it with a different body raises `IdempotencyConflict`. `update_vm`,
  `delete_snapshot`, `kill_process`, `write_file`, `cancel_prompt`,
  `interrupt_prompts`, `revoke_token`, `delete_credential_profile`,
  `put_credential`, `delete_credential` and `revoke_credential_grant` are
  naturally idempotent and send no key.
- `if_match=<state_revision>` sends `If-Match`. A stale revision raises
  `RevisionMismatch`.
- A request is retried only when it certainly never reached the server
  (connection refused, or the socket file is missing). It is tried at most 3
  times, with a short backoff and the same idempotency key.

`create_vms(count, name, ...)` creates up to 100 VMs from one template in one
request (feature `batchCreate`, like `davit new web --count 3`), taking
`create_vm`'s template arguments; `name` N names them N-1 ... N-count. It
returns a `CreateVMsResult` with one `BatchItem` per VM, in order, holding its
`accepted` operation or its `error` (e.g. `resource_exhausted` when the host is
full): items are admitted on their own, so check every item. It does not wait
unless `wait=True`, which follows every accepted operation and puts the
terminal operation and VM in its item (failures are reported, not raised).
Calling again with the same `idempotency_key` returns the same operations and
creates only the items refused before.

```python
res = sw.create_vms(3, "worker", idempotency_key="workers-1", wait=True)
for it in res.items:
    print(it.index, it.name, it.error.code if it.error else it.accepted.operation.state)
```

When the host is full (feature `capacityReclaim`), `create_vm` and `resume_vm`
first stop idle VMs that have an idle timeout; `capacity_wait=<seconds or
timedelta>` (0..3600) then queues the request that long for room, first come
first served, instead of raising `ResourceExhausted` at once (`waitSeconds`;
the HTTP timeout grows by the wait). It is not `wait`, which follows the
operation. `reclaim_vms(idle_seconds, labels=..., dry_run=...)` stops your VMs
idle that long, like `davit box gc`, and returns them (oldest activity first)
with their stop's `operation_id`, without waiting for the stops; `dry_run=True`
only lists them. It sends no idempotency key: a repeat stops nothing twice.

```python
vm = sw.create_vm("web", capacity_wait=120)  # queue up to 2 minutes for room
for it in sw.reclaim_vms(timedelta(hours=1), labels={"davit.box": "myproject"}).items:
    sw.wait(it.operation_id)
```

## Renaming, extending and profiles

```python
sw.extend_vm("web")  # restart the auto-stop countdown with the current TTL
sw.extend_vm("web", timedelta(hours=3))  # new TTL, countdown restarts now
sw.rename_vm("web", "api", if_match=vm.state_revision)
sw.update_vm("api", ttl=1800, auto_stop=False)  # any subset of name/ttl/auto_stop

caps = sw.get_capabilities()
[p.name for p in caps.profiles]  # administrator-defined shapes
big = sw.create_vm("big", type="big")  # `shape=` fields override the profile
```

- `update_vm(vm, name=, ttl=, auto_stop=, if_match=)` is a synchronous `PATCH`
  that returns the VM.
  - Only the given fields change. `ttl` is in seconds or a `timedelta`.
  - Setting a TTL on a ready or running VM restarts its auto-stop countdown
    from now.
  - It sends no `Idempotency-Key`.
- `extend_vm(vm, ttl=None)` mirrors `davit extend`. It always sends
  `autoStop: true`, and adds the TTL when one is given; a TTL must be at least
  one second. `rename_vm(vm, new_name)` mirrors `davit rename`.
- `VM.auto_stop_warning_at` is when the host starts warning about the auto-stop:
  `expires_at` minus `capabilities.ttl_warning_seconds`.
- `create_vm(..., type=)` and `fork_vm(..., type=)` select a profile from
  `capabilities.profiles`. An unknown profile raises `InvalidArgument`.

## Egress

Each VM has an egress policy, `vm.egress` (`Egress(mode, allow)`). The mode is
`off` (nothing leaves the VM, DNS included), `filtered` (the public internet
only; private, host, metadata and tailnet ranges stay blocked) or `allowlist`
(only the `allow` entries, on top of `filtered`).

```python
from davit import Egress, EgressMode

vm = sw.create_vm(
    "job", egress=Egress(mode=EgressMode.ALLOWLIST, allow=["GitHub.com", "10.9.1.2/16"])
)
vm.egress.allow  # ["10.9.0.0/16", "github.com"]: normalized by the server
sw.fork_vm("job", "job-2")  # after a stop: inherits the source's policy (or pass egress=)
sw.set_vm_egress("job", "off")  # returns the VM
```

- `create_vm(..., egress=)` defaults to the host's `network.egress` (else
  `filtered`); `fork_vm(..., egress=)` defaults to the source's policy.
- `set_vm_egress(vm, mode, *, allow=None, if_match=None)` is `update_vm(vm,
  egress=...)`: synchronous and naturally idempotent. It is live on a running VM (new connections and DNS lookups;
  flows the new policy no longer allows are closed) and kept for later boots of
  a stopped one.
- `allow` entries are domains (`github.com` also matches its subdomains,
  `*.example.com` only subdomains) and IPv4 addresses or CIDRs, at most 256,
  for `allowlist` only. A bad entry raises `InvalidArgument`. A running
  lifecycle operation raises `OperationInProgress` (retryable), and a running
  VM that cannot be updated raises `Unavailable` (the old policy stays).

## Privacy and ephemeral VMs

`vm.privacy` is the VM's `PrivacyPolicy` (feature `privacyPolicy`). Every flag
defaults to off; forks inherit the source's policy.

```python
import dataclasses

from davit import PrivacyPolicy

sw.create_vm("scratch", privacy=PrivacyPolicy(ephemeral=True))  # deleted when it stops
sw.create_vms(5, "job", privacy=PrivacyPolicy(no_output_retention=True))
vm = sw.get_vm("agent")
sw.update_vm("agent", privacy=dataclasses.replace(vm.privacy, no_process_samples=True))
```

- `ephemeral`: deleted on any stop (`stop_vm`, the TTL auto-stop, a forced
  stop), nothing committed. A VM that crashes into `error` is not stopped and
  stays until deleted or recovered.
- `no_snapshot`: a stop commits nothing; the stopped VM cannot be resumed or
  forked and `create_snapshot` is refused.
- `no_output_retention`: the host stores no process output (history lists the
  processes, argv included, without output). The guest agent still holds recent
  output in guest memory while the VM runs.
- `no_process_samples`: no stored whole-guest process samples; listing guest
  processes still works on a running VM.
- `update_vm(..., privacy=)` replaces the whole policy; flags left `None` are
  off. A server without the feature raises `ServerNotImplemented`.
- Not secure erasure: deleted data can survive in host backups, filesystem free
  space and SSD wear levelling, and the guest can always write to its own disk
  and send data within its egress policy. See
  [docs/vms-and-agents.md](../../docs/vms-and-agents.md#ephemeral-and-private-vms).

## Environments

An environment is a named, versioned workspace manifest (repos to clone,
variables, secret files that name host secrets, setup commands). Every change
mints a new immutable revision; a VM pins one revision.

```python
from davit import EnvManifest
from davit.models import RepoSpec, SetupAction

manifest = EnvManifest(
    repos=[RepoSpec(url="https://github.com/acme/app.git", path="/work/app")],
    variables={"CI": "1"},
    secret_files=[],
    setup=[SetupAction(name="deps", argv=["make", "deps"], cwd="/work/app")],
)
env = sw.create_environment("app", manifest)  # revision 1
latest = env.latest_revision
rev = sw.create_environment_revision("app", {"variables": {"CI": "2"}}, if_match=latest)
for r in sw.iter_environment_revisions("app"):  # oldest first
    print(r.revision, r.manifest.variables)
vm = sw.create_vm("dev", environment="app@2")  # or "app" (latest), or EnvironmentRef(...)
sw.delete_environment("app")  # pinned VMs keep their copy
```

- `create_environment(name, manifest, *, idempotency_key=None)` returns the
  `Environment`. `manifest` is an `EnvManifest` or its wire JSON object
  (camelCase keys; omitted lists and maps are empty; an unknown key raises
  `ValueError`). It always sends an `Idempotency-Key`; the server also returns
  the existing environment for the same name and an identical first manifest,
  and raises `InvalidArgument` when the name is taken.
- `create_environment_revision(env, manifest, *, if_match=None,
  idempotency_key=None)` mints the next revision from a complete manifest.
  `if_match` is the expected `latest_revision`; a stale one raises
  `RevisionMismatch` with `details["latestRevision"]`. A retry with the same
  key returns the revision it minted.
- `list_environments(limit=, cursor=)` / `iter_environments()`,
  `get_environment(id_or_name)`, `delete_environment(env)` (no key; a repeat
  raises `NotFound`), `list_environment_revisions(env, limit=, cursor=)` /
  `iter_environment_revisions(env)`, `get_environment_revision(env, n)`.
- `create_vm`, `fork_vm` and `resume_vm` take `environment=`: an
  `EnvironmentRef`, `"NAME"` (the latest revision) or `"NAME@REVISION"`
  (parsed by `parse_environment_ref`, which raises `ValueError` on anything
  else). A daemon without a workspace preparer raises `ServerNotImplemented`.

## Processes

Commands run with an explicit `argv` list, and a shell is never inferred. To use
a shell, run `["sh", "-c", "..."]`. The VM must be `ready` or `running`.

```python
r = sw.run("web", ["sh", "-c", "echo hi; exit 3"], deadline_seconds=60)
r.exit_code, r.stdout, r.stderr  # 3, b"hi\n", b""
sw.run("web", ["cat"], input=b"data").stdout  # b"data"
```

`run()` returns a `RunResult(exit_code, stdout, stderr, process)`. A non-zero
exit is not an exception. `exit_code` follows `davit exec`:

- the guest's code (masked to 8 bits);
- `124` when `deadline_seconds` killed the process (`timed_out` is true);
- `128 + n` for a signal;
- `255` when the outcome is unknown.

If the stream is lost and cannot be re-attached, `run()` raises
`TransportError`.

The lower-level calls are:

- `exec_process(vm, argv, cwd=, env=, user=, deadline_seconds=, detached=, stdin=, tty=, idempotency_key=)`
  returns a `Process`. It always sends an `Idempotency-Key`.
- `list_processes(vm, limit=, cursor=)` / `iter_processes(vm)` list every
  recorded process, oldest first in `(createdAt, id)` order. A start that a
  daemon crash interrupted shows status `lost`. Also: `get_process(vm, id)`,
  `kill_process(vm, id, signal=None)`. With no signal, the server sends
  SIGTERM; `"KILL"` and `"SIGKILL"` both work.
- `attach_process(vm, id, output_cursor=0)` returns a `ProcessStream`.

```python
p = sw.exec_process("web", ["sh", "-c", "make test"], detached=True)
with sw.attach_process("web", p.id) as stream:
    for ev in stream:  # StreamEvent(type, seq, data, process)
        if ev.type in ("stdout", "stderr"):
            print(ev.data.decode(), end="")
    print("exit:", stream.process.exit_code)
```

How the stream behaves:

- It runs over a small, dependency-free RFC 6455 WebSocket client. That client
  uses the same endpoint as the HTTP calls: the unix socket, or `wss` for an
  `https` `base_url` with the bearer token. A custom httpx `transport` does not
  apply to it.
- Output chunks with `seq <= stream.cursor` are dropped. If the connection
  drops before `exit`, the stream re-attaches automatically from
  `outputCursor = cursor`, making up to 3 attempts. After that it raises
  `TransportError`.
- A server `error` message raises its typed error and ends the stream.
- `send_stdin(bytes | str)`, `close_stdin()` and `signal(name)` send input and
  signals. Only the first stream of a process started with `stdin=True` owns
  stdin, and stdin is never replayed after a reconnect.
- A process that is not `detached` is killed when its first stream closes
  before it exits. A `detached` process keeps running, and you can attach to
  it again later with `output_cursor=` to resume.

## Job history

The daemon keeps the output of managed (detached) jobs on the host, so a job
can be listed and its output read after it ended, while the VM is stopped and
across daemon restarts.

```python
for entry in sw.iter_process_history("web"):  # newest first, across generations
    print(entry.process.id, entry.process.status, entry.stdout_bytes, entry.stderr_bytes)

out = sw.collect_process_output("web", proc_id)  # RetainedOutput(stdout, stderr, complete, cursor)
print(out.stdout.decode(), out.complete)
for chunk in sw.iter_process_output("web", proc_id, stream="stderr"):
    print(chunk.type, chunk.seq, chunk.data)  # StreamEvent
```

- `list_process_history(vm, limit=, cursor=)` returns a `HistoryList`;
  `iter_process_history(vm)` follows `nextCursor`. `output_dropped` is true
  when the retention limit cut the oldest output.
- `read_process_output(vm, id, after=None, stream=None, limit=None)` returns
  one raw `ProcessOutput` page (base64 `data`, `next_cursor`, `complete`).
- `iter_process_output(...)` yields decoded `StreamEvent`s and
  `collect_process_output(...)` returns the bytes. Both follow `next_cursor`
  until a page is `complete` (the process ended and everything retained was
  read) or brings nothing new (it still runs: read again later with
  `after=out.cursor`). Malformed base64 raises `Internal`.
- A daemon without guest transport raises `ServerNotImplemented`.

## Runs and push

A run creates a VM, runs one command in it, keeps the exit code and deletes
the VM (unless `keep`), like `davit run`. `push_tree` applies a tar of files
under a guest directory, like `davit push`.

```python
out = sw.run_to_completion(["make", "test"], files="./app")  # pushes ./app's git selection
out.exit_code, out.stdout, out.stderr, out.run.state  # RunOutcome

run = sw.create_run(["sh", "-c", "exit 3"], timeout_seconds=600)
run = sw.wait_run(run)  # polls get_run until ended_at
run.state, run.exit_code  # "failed", 3

held = sw.create_run(["make"], hold=True, cwd="/home/davit/work")  # stops in "waiting"
# ...once get_run(held).state == "waiting":
sw.push_dir(held.vm_id, "./app")  # or push_tree(vm, path, tar_bytes_or_file)
sw.start_run(held)
```

- `create_run(argv, *, image, snapshot, type, shape, environment, cwd, env,
  timeout_seconds, keep, hold, output_grace_seconds, idempotency_key)` returns the `Run` at once
  (state `creating`). It always sends an `Idempotency-Key`: the same key and
  body return the same run, another body raises `IdempotencyConflict`.
  `environment` takes an `EnvironmentRef` or `"NAME[@REV]"` as on `create_vm`.
- `list_runs(limit=, cursor=)` returns one `RunList` page, oldest first;
  `iter_runs()` follows `nextCursor`.
- `get_run(run)`, `start_run(run)` (only a `waiting` run; else
  `InvalidState`), `cancel_run(run)`, `wait_run(run, timeout=None)` (polls
  every 0.2 s; a timeout raises `WaitTimeout` with `details["runId"]` and
  never cancels the run).
- `outcome` and `decided_at` give the result as soon as it is known; `state`
  becomes `outcome` only after cleanup (output grace, then the VM is deleted
  unless `keep`), when `ended_at` is set. `failed` covers a non-zero exit
  (`exit_code`), a signal kill (`signal`, with no `error`), the timeout
  (`error.code == "timeout"`) and setup failures (`error`). A cancel sets
  `outcome` to `cancelled`.
- After the command exits, the run keeps its VM for an output grace so its
  output can still be read through `process_id` (`attach_process`,
  `collect_process_output`): `output_grace_seconds` (0..600, 0 = none;
  omitted = the host default, daemon config `runs.outputGrace`, 5 s). Once
  `outcome` is set, `cancel_run` only ends that grace and keeps the outcome;
  before that it kills the command and the run ends `cancelled`.
- `run_to_completion(argv, *, files=None, files_path=DEFAULT_PUSH_PATH, ...)`
  does it all: create (with `hold` when `files` is a local directory or tar
  `bytes`), push and start, attach and collect the output until the exit
  (if the VM is already gone, the rest comes from the host-retained output),
  `cancel_run` once `outcome`/`decided_at` is set (not with
  `output_grace_seconds=0`), then `wait_run`. `RunOutcome.exit_code` follows
  `davit run` (130 cancelled, 124 timeout, the exit code, 128+n for a signal);
  `RunOutcome.signal` names the signal. A run that failed before its command
  started raises its typed error.
- `push_tree(vm, path, data, *, mode="merge")` sends `data` (tar `bytes`, a
  binary file or an iterable of bytes) as `application/x-tar` and returns a
  `PushResult` (`files`, `bytes`, `sha256`, `generation`). `merge` adds and
  replaces files and keeps the others; `mode="mirror"` is sent as is and this
  server answers `ServerNotImplemented`. No `Idempotency-Key` (naturally
  idempotent).
- `push_dir(vm, local_dir, path=DEFAULT_PUSH_PATH)` builds that tar from a git
  work tree like the CLI: `git ls-files -z --cached --others
  --exclude-standard` (tracked plus untracked-not-ignored, checked-out
  submodules included, never `.git`), regular files and symlinks with their
  modes and mtimes, no owners. `davit.git_selection` and
  `davit.write_tree_tar` are the two halves. A directory outside a work tree
  raises `ValueError`.

## Files

Guest paths must be absolute. `generation` is the VM's `instance_generation`.
Passing it guarantees the transfer never lands in a different boot: a mismatch
raises `InvalidState`. If you omit it, the client fetches the VM first and uses
its current generation and ID, as `davit cp` does.

```python
info = sw.write_file("web", "/home/user/app.py", b"print('hi')\n", mode=0o644)
got = sw.read_file("web", "/home/user/app.py")  # FileContent(data, mode="0644")

with sw.read_file_stream("web", "/var/log/big.log") as stream, open("big.log", "wb") as out:
    stream.copy_to(out)  # streamed, never buffered whole

with open("dataset.bin", "rb") as f:
    sw.write_file("web", "/data/dataset.bin", f)  # streamed upload (chunked)

sw.upload_dir("web", "./src", "/home/user/src")  # tar of the directory's contents
sw.download_dir("web", "/home/user/out", "./out")
```

Each call works as follows:

- `read_file` / `read_file_stream(vm, path, user=, generation=, archive=False)`.
  With `archive=True`, the result is a tar stream of a directory's contents.
  `FileStream` is a single-use context manager. Iterating it yields chunks as
  they arrive; `read()` and `copy_to(fileobj)` are also available.
- A transfer is verified once it has been fully consumed. A regular file must
  deliver exactly its `Content-Length`, and a tar must end with its two zero
  512-byte blocks. Otherwise the call raises `TransportError`.
- `write_file(vm, path, data, user=, generation=, mode=, archive=False)`
  returns a `FileInfo` with `size_bytes`, `mode`, `sha256` and `generation`.
  `data` can be `bytes`/`str` (UTF-8), a binary file object, or an iterable of
  `bytes`. Only `bytes`/`str` bodies are retried when the daemon was not
  reached; streamed bodies are sent once.
- `mode` takes `0o644` or `"0644"`. With `archive=True`, the tar is extracted
  into `path`.
- `upload_dir` / `download_dir` use the stdlib `tarfile`. Extraction uses the
  `data` filter, which rejects absolute or escaping paths and device files.
  These two helpers are Python-only.

## Snapshot files

A snapshot (ID or name) can be read on the host without booting it. Paths
resolve inside the snapshot: `..` stops at `/` and symlinks stay within it.

```python
for entry in sw.iter_snapshot_tree("base", path="/etc"):  # SnapshotEntry, paged
    print(entry.type, entry.mode, entry.size_bytes, entry.path, entry.link_target)

got = sw.read_snapshot_file("base", "/etc/hostname")  # FileContent(data, mode, sha256)
site = sw.read_snapshot_file("base", "/srv/site", archive=True)  # a directory as tar

with sw.export_snapshot("base", format="raw") as stream, open("disk.img", "wb") as out:
    stream.copy_to(out)  # format="tar" (default): the whole filesystem
```

- `list_snapshot_tree(snapshot, path="/", limit=, cursor=)` returns one
  `SnapshotTree` page, and `iter_snapshot_tree(snapshot, path="/", limit=)`
  follows `nextCursor`.
- `read_snapshot_file` / `read_snapshot_file_stream(snapshot, path, archive=False)`
  are checked like VM reads. A regular file must also hash to its
  `X-Davit-Content-Sha256` header, or the read raises `TransportError`. A
  missing header raises `TransportError` too. Archives and exports carry their
  digest as an HTTP trailer, which the SDK does not read.
- `export_snapshot(snapshot, format="tar" | "raw")` only streams; exports are
  disk-sized.

## Tunnels

A tunnel is one TCP connection to a port on the guest's loopback, carried over
a WebSocket (as `davit forward` and `davit ssh` use it).

```python
with sw.connect_tunnel("web", 8080) as conn:  # open_tunnel + attach_tunnel
    conn.send(b"GET / HTTP/1.0\r\nHost: x\r\n\r\n")
    conn.close_write()  # the guest port reads EOF
    reply = b"".join(conn)  # chunks until the guest side ends
```

- `open_tunnel(vm, port)` returns a `Tunnel` (`id`, `vm_id`, `generation`,
  `port`, `created_at`, `expires_at`). Nothing listening on the port raises
  `Unavailable`. Attach within 30 s (`expires_at`), else the server closes it.
- `attach_tunnel(tunnel_or_id)` returns a `TunnelStream`; a tunnel attaches
  once. `close_tunnel(tunnel_or_id)` releases an unattached one.
- `TunnelStream`: `send(data)` (messages of at most 32 KiB), `recv()` (`b""`
  at EOF), `close_write()`, `close()`, iteration over chunks. A normal server
  close is EOF; any other end raises `TransportError`. One thread may read
  while another writes.

## Reverse forwards

A reverse forward makes a service reachable from inside the VM: the guest agent
listens on `127.0.0.1:guest_port`, and each connection a guest process makes
there is bridged to a far end fixed when the forward is created (the guest
never chooses it). Owner, or a token with `admin` on the VM.

```python
# Like `davit forward --reverse web 5432:127.0.0.1:5432`.
with sw.reverse_forward("web", 5432, "127.0.0.1", 5432) as session:
    sw.run("web", ["psql", "-h", "127.0.0.1", "-c", "select 1"])
session.reason  # "closed": leaving the block revoked the forward

sw.forward_host_service("web", 8080, "10.0.0.5:8080")  # operator-listed host service
```

- `reverse_forward(vm, guest_port, local_host, local_port)` creates a
  `session` forward (address label `local_host:local_port`), attaches its
  session stream and, on background threads, claims each guest connection
  and dials the local service (10 s bound), splicing both with half-close in
  both directions. A local service that cannot be reached closes that one
  connection. If the attach fails, the forward is revoked.
- `ReverseSession`: `forward`, `active`, `closed`, `close()` (revokes and
  cuts live connections), `wait(timeout=None)` (blocks until the session
  ended and returns `reason`). `reason` is the server's `revoked` reason,
  `"closed"` after `close()`, or a dropped stream (then `error` is a
  `TransportError`).
- `forward_host_service(vm, guest_port, address)` is a `host` forward: the
  daemon dials `address`, which must be listed exactly in the operator's
  `network.reverseHostServices` (else `PermissionDenied`).
- `list_reverse_forwards(vm)`, `create_reverse_forward(vm, guest_port,
  target)` and `revoke_reverse_forward(vm, forward_or_id)` are the plain
  calls. One forward per guest port (`Conflict`); a guest agent that cannot
  listen raises `UnsupportedCapability` (capabilities feature
  `reverseForward`). Stopping or deleting the VM revokes its forwards.

## App endpoints

An app endpoint gives an app listening on a guest loopback port a stable,
tailnet-private URL, `https://<gateway>:<gateway_port>/`, on the shared tailnet
gateway. Callers need a tailnet identity: the owner, or
`Authorization: Bearer <token>` for a token with `exec` on the VM (the bearer
is not forwarded to the app). The endpoint belongs to the VM: stopping the VM
keeps the URL, which answers 503 until the VM runs again; deleting the endpoint
removes it; forks inherit none.

```python
ep = sw.expose("web", 3000)  # create_app_endpoint; name defaults to "port-3000"
print(ep.url)  # https://<gateway>:<gateway_port>/
sw.rename_app_endpoint("web", ep, "site")  # the URL stays the same
sw.hide_app_endpoint("web", "site")  # closes the port; show_app_endpoint reopens it
sw.delete_app_endpoint("web", "site")
```

- `list_app_endpoints(vm)`, `create_app_endpoint(vm, port, name=None)`,
  `update_app_endpoint(vm, endpoint, *, name=None, hidden=None)` and
  `delete_app_endpoint(vm, endpoint)` are the plain calls; `endpoint` is the
  ID, the name or the `AppEndpoint`. `expose`, `hide_app_endpoint`,
  `show_app_endpoint` and `rename_app_endpoint` are shorthands.
- Each guest port and each name has at most one endpoint (`Conflict`). A
  daemon without a tailnet gateway raises `UnsupportedCapability` (the
  capabilities feature `appEndpoints` is `unsupported`).

## Desktop

The desktop is the VM's app endpoint of kind `desktop` (named `desktop`), for
the guest VNC server on loopback `port` (default 5900; the desktop image
profile runs x11vnc there). Its `url` opens the built-in viewer, and
`<url>websockify` is a websockify-compatible RFB stream, so noVNC or any
websockify client works too. `view` rights are view-only (the gateway drops
key, pointer and clipboard input); `exec` adds input. Stopping the VM cuts the
stream (503 at the same URL until it runs again); `close_desktop` removes it.

```python
desk = sw.open_desktop("web")  # or open_desktop("web", 5901); returns the open one if any
print(desk.url, desk.kind)  # https://<gateway>:<gateway_port>/ desktop
sw.get_desktop("web")  # NotFound when no desktop is open
sw.close_desktop("web")  # cuts every viewer
```

- Like app endpoints, no `Idempotency-Key` is sent. A daemon without a tailnet
  gateway raises `UnsupportedCapability` (the capabilities feature `desktop` is
  `unsupported`).

## SSH credentials

`issue_ssh_credential(vm, public_key, *, guest_user=None, ttl_seconds=None)`
signs your own OpenSSH public key (an `authorized_keys` line; the SDK never
generates keys) and returns an `SSHCredential`: `certificate` (valid for the
VM's current generation, at most 600 s), `guest_user`, `generation`,
`host_keys` to pin (they match `vm.endpoint.ssh_host_key_fingerprints`),
`ssh_target` and `expires_at`. An invalid key raises `InvalidArgument`; a
runtime without guest transport `UnsupportedCapability`.

## Tokens

Service tokens let another tailnet host (a CI job, an agent) use this daemon
with limited rights. Only the owner identity (the local socket, or the
tailnet owner) may manage them; a client that itself uses a token gets
`PermissionDenied` on these calls.

```python
tok = sw.create_token("ci", actions=["exec"], vm="web", ttl_seconds=30 * 86400)
tok.secret  # "slw_...": shown only once, store it now
remote = Client(base_url="https://davit-host.tailnet.ts.net", token=tok.secret)

for t in sw.iter_tokens():  # active tokens, never their secrets
    print(t.id, t.name, [(s.vm_id or "*", s.actions) for s in t.scopes], t.expires_at)
sw.revoke_token(tok)  # or the ID; a second revoke raises NotFound
```

- `create_token(name, *, actions=None, vm=None, scopes=None, ttl_seconds,
  idempotency_key=None)` returns a `CreatedToken`. Give `actions` (`view`,
  `exec` implies view, `admin` implies both) with an optional `vm` (a name or
  ID, or a list of them; one scope each; omitted means every VM), or
  explicit `scopes=[TokenScope(vm_id=..., actions=[...])]`. `ttl_seconds` is
  1..31536000 (one year). It always sends an `Idempotency-Key`; a replay
  within 10 minutes returns the same token. An unknown action raises
  `InvalidArgument`, an unknown VM `NotFound`.
- `CreatedToken.secret` is left out of the model's `repr` (and so out of
  logs and tracebacks that print it); the attribute itself is unchanged.
  The SDK never logs it.
- `list_tokens(limit=, cursor=)` returns one page (`TokenList`);
  `iter_tokens()` follows `nextCursor`. `revoke_token(token_or_id)` returns
  `None`; the token stops working on its next request.
- The bearer works only on the daemon's tailnet listener (`https`), where
  the request still needs a tailnet identity and runs as the token's issuer
  limited to its scopes. The local socket ignores tokens.

## Events and webhooks

The host event stream (`vm.created`, `vm.state`, `operation.*`, `run.*`,
`prompt.*`; tolerate unknown types) is read with a cursor; `data` is an
allowlisted summary (IDs, states, error codes), never env values, prompt
text or secrets. Webhooks receive the same events as signed POSTs; managing
them is owner-only.

```python
for ev in sw.iter_events(types=["vm.", "operation.state"], start="latest", follow=True):
    print(ev.seq, ev.type, ev.subject, ev.data)  # runs until you break

hook = sw.create_webhook("https://ci.example/davit", events=["vm."])
hook.secret  # shown only once (kept out of repr): give it to the receiver
for d in sw.iter_webhook_deliveries(hook, state="dead"):
    sw.retry_webhook_delivery(hook, d)
new = sw.rotate_webhook_secret(hook, overlap_seconds=3600)  # old secret signs for 1 h more
```

- `list_events(limit=, cursor=, types=, vm=, start="oldest"|"latest",
  wait_ms=0..30000)` returns one `EventList`; `next_cursor` is always set.
  `iter_events(...)` follows it and ends at the first empty page; with
  `follow=True` it keeps long-polling (`wait_ms`, default 25000) forever.
- `create_webhook(url, *, events=, description=, enabled=, idempotency_key=)`
  and `rotate_webhook_secret(webhook, *, overlap_seconds=, idempotency_key=)`
  return a `WebhookSecret` and always send an `Idempotency-Key`.
  `get_webhook`, `update_webhook(webhook, *, url=, events=, description=,
  enabled=)` (PATCH; only given fields change, no key), `delete_webhook`,
  `list_webhooks`/`iter_webhooks`, `list_webhook_deliveries(webhook, *,
  state=)`/`iter_webhook_deliveries` and `retry_webhook_delivery(webhook,
  delivery)` take IDs or the returned objects.
- Receivers verify each delivery with the raw body:

  ```python
  from davit import WebhookVerificationError, verify_webhook

  try:
      verify_webhook(raw_body, headers.get("X-Davit-Signature"), [new_secret, old_secret])
  except WebhookVerificationError as err:
      ...  # reject; err.reason is "malformed", "timestamp" or "signature"
  ```

  The header is `t=<unix seconds>,v1=<hex>[,v1=...]`, each `v1` the
  HMAC-SHA256 of `"<t>." + body` under one live secret (several during a
  rotation overlap); any match of any given secret passes. Timestamps more
  than `tolerance` (default 300) seconds old or ahead are rejected; the
  comparison is constant-time.

## Members and shares

Members are other tailnet users with rights only on what the owner shares.
A member is added by login and pinned to its stable tailnet user ID on its
first connection; from then on it is that user whatever its login becomes,
and a recycled login is refused. All of these calls are owner-only (local
socket or the owner's tailnet identity; tokens and members get
`PermissionDenied`); without the tailnet gateway they raise
`UnsupportedCapability` (capabilities feature `members`).

```python
alice = sw.add_member("alice@example.com", "viewer")  # default role
sw.share_vm("web", alice, "exec")  # or the ID, the login, or "*"
sw.share_snapshot("base", "*")  # every member, each at its own role
for s in sw.list_shares("vm", "web").items:
    print(s.member, s.role, s.credentials)
sw.unshare("vm", "web", "alice@example.com")  # NotFound if there is no such share
sw.remove_member(alice)  # with its shares
```

- Roles nest as `viewer` < `exec` < `admin` (as token actions: view, then
  exec code, files, tunnels and SSH, then manage the VM). A share's `role`
  defaults to the member's own role.
- A member sees only what is shared with it: any other VM, snapshot or
  environment is `NotFound` (404), as if it did not exist.
- An `exec` or `admin` share without `credentials=True` pauses the VM's
  credential grants for everyone while it exists (processes in one guest
  cannot be told apart).
- `create_share(kind, resource, member, role=None, *, credentials=False)` is
  an upsert: sharing the same resource with the same member again replaces
  the role and `credentials`. `share_vm`, `share_snapshot` and
  `share_environment` fix the kind.
- `update_member(member, role)`, `delete_share(share)`, `unshare(kind,
  resource, member)` and `remove_member(member)` cut the member's live
  sessions (API streams, SSH, app endpoint connections, its reverse
  forwards). `unshare` looks the share up with `list_shares(kind, resource)`
  (resolving a login through `list_members`) and deletes it.
- Members, shares, VMs, snapshots and environments are taken by ID (logins
  and names where the API accepts them) or the returned object; path
  segments are escaped (`alice%40example.com`). No `Idempotency-Key` is sent.

## Credentials

Credential profiles hold API keys on the host; a VM gets a per-VM placeholder
instead of the key, and the host credential proxy swaps it for the secret on
requests to the credential's upstream. Profile calls are owner-only; grant
calls follow the VM's token scope (`view` to list, `admin` to create/revoke).

```python
prof = sw.create_credential_profile("work", default=True)
sw.put_credential(
    prof,
    "anthropic",
    kind="api_key",
    provider="anthropic",
    env_var="ANTHROPIC_API_KEY",
    secret=os.environ["ANTHROPIC_API_KEY"],
)
grant = sw.create_credential_grant("web", "work", "anthropic")
# In the guest: ANTHROPIC_API_KEY=<grant.placeholder>, base URL http://172.16.0.1:15080
sw.revoke_credential_grant("web", grant)  # the placeholder stops working at once
```

- The `secret` is write-only: sent once in the PUT body, stored on the host
  only, never returned (`Credential.has_secret` tells whether one is set) and
  never logged by the SDK; `PutCredentialRequest` leaves it out of its `repr`.
  An omitted (empty) `secret` keeps the stored one; a new credential needs one.
- `put_credential(profile, name, *, kind, env_var, secret="", provider=None,
  base_url=None)` creates or replaces a credential and returns the
  `CredentialProfile`. `api_key` credentials of providers `anthropic` and
  `openai` default their upstream to the provider's API; `custom_api` needs
  `base_url`. Existing grants use a new secret from their next request.
- `create_credential_profile(name, *, default=None, idempotency_key=None)` and
  `create_credential_grant(vm, profile, credential, *, idempotency_key=None)`
  always send an `Idempotency-Key`; the rest send none.
- `list_credential_profiles(limit=, cursor=)` returns one page,
  `iter_credential_profiles()` follows `nextCursor`; `get_credential_profile`,
  `delete_credential_profile` (revokes its grants, `profile_deleted`) and
  `delete_credential(profile, name)` (`credential_removed`) take an ID, a name
  or the profile.
- `list_credential_grants(vm)` returns the VM's grants, active and revoked,
  oldest first; `revoke_credential_grant(vm, grant_or_id)` returns the revoked
  grant. Stop keeps a grant dormant, resume reuses it, a fork child inherits
  none, and deleting the VM revokes it (`vm_deleted`).

## Prompts

A prompt is one turn of a coding-agent harness conversation (`claude`, the
default, `codex`, `opencode` or `pi`) that runs inside the VM. The VM needs a
guest transport, and the harness must be installed in the image. A missing
harness fails the turn with error code `harness_missing`. A harness whose
`get_capabilities().harnesses[].experimental` is true follows its CLI's
documentation but has not yet run against a real model (OpenCode, pi).

```python
res = sw.run_prompt("web", "add a test for parse()", on_event=lambda e: print(e.type))
res.text  # assistant text; res.events: every event
res.prompt.state, res.prompt.usage, res.prompt.error  # "succeeded" | "failed" | "cancelled" ...
again = sw.run_prompt("web", "now run it", conversation_id=res.prompt.conversation_id)

p = sw.create_prompt("web", "refactor", new=True, secrets=["ANTHROPIC_API_KEY"])
for ev in sw.follow_prompt("web", p.id, timeout=600):  # PromptEvent, in seq order
    if ev.type == "message.delta":
        print(ev.text, end="")
sw.cancel_prompt("web", p.id)  # SIGTERM to the harness; the turn ends "cancelled"

# Attachments and harness controls (see capabilities.harnesses[]).
from davit import PromptAttachment

img = PromptAttachment(name="shot.png", media_type="image/png", data=base64.b64encode(png).decode())
sw.run_prompt("web", "what is wrong here?", attachments=[img], reasoning_effort="high", fast=True)

# Steer a running turn, interrupt, and follow every turn of the VM.
s = sw.steer_prompt("web", p.id, "also update the docs")  # s.delivery: "native" | "restart"
sw.interrupt_prompts("web", all=True)  # or conversation_id=...; returns the cancelled turns
for item in sw.follow_vm_prompt_events("web", after=saved_cursor):  # runs until you break
    saved_cursor = item.cursor
```

How it works:

- `create_prompt(vm, text, harness=, conversation_id=, new=, model=, cwd=, env=, secrets=,
  attachments=, reasoning_effort=, fast=, turn_timeout_seconds=)` returns the accepted `Prompt`
  at once.
  - `attachments` are `PromptAttachment(name=, media_type=, path=)` (a guest file) or `data=`
    (inline base64); exactly one of `path`/`data`. `reasoning_effort` and `fast` are sent as
    `options`. `turn_timeout_seconds` bounds the turn (server default 3600, at most 86400;
    `turn_deadline_at` on the prompt); it then fails with `turn_timeout`.
  - A conversation runs one turn at a time. Starting a turn while another is
    active raises `OperationInProgress` (retryable).
  - `env` values and named daemon `secrets` enter the guest as the harness's
    environment and are not stored.
  - It always sends an `Idempotency-Key`: `idempotency_key=` or a UUIDv4
    generated once per call, reused on retries. The same key and body return
    the existing turn; the same key with a different body raises
    `IdempotencyConflict`. `run_prompt` passes `idempotency_key=` through.
- `follow_prompt(vm, id, after=0, timeout=None)` yields every event once, and
  stops after the turn finishes (its last event is `turn.completed`).
  - Event types are `conversation.started`, `message.delta`, `tool.call`,
    `tool.result`, `turn.completed`, `raw`, and possibly new ones from a newer
    server. `input` and `raw` are arbitrary JSON values.
  - If the budget runs out, it raises `WaitTimeout` (no `operation_id`: a prompt is not an operation; `details["promptId"]`,
    `details["cursor"]`). Running out of budget, or stopping early, never
    cancels the turn.
- `wait_prompt(vm, id, ...)` returns the final `Prompt`. `run_prompt(vm, text, ...)`
  (create + wait) returns a `PromptResult(prompt, text, events)`, where `text`
  is the assistant's `message.delta` texts joined together. A failed or cancelled turn is returned, not
  raised; check `state` and `error.code` (`harness_missing`, `harness_failed`,
  `harness_exit`, `cancelled`, `lost`, ...).
- `get_prompt`, `cancel_prompt`, and `list_prompt_events(vm, id, after=, wait_ms=)`
  for the raw long-poll.
- `steer_prompt(vm, id, text)` sends a message to a running turn and returns a `SteerResult`:
  `delivery` `native` (the same turn took it) or `restart` (the turn was cancelled; `prompt` is
  the new turn of the same conversation). It sends an `Idempotency-Key` (`idempotency_key=` or
  generated once per call), so the message is delivered once. A finished turn raises
  `InvalidState`, one still starting `OperationInProgress` (retryable).
- `interrupt_prompts(vm, conversation_id=)` or `interrupt_prompts(vm, all=True)` cancels active
  turns and returns them (`cancelled`); a repeat returns an empty list (no `Idempotency-Key`).
- `list_vm_prompt_events(vm, after=, conversation_id=, wait_ms=, limit=)` returns the VM-wide
  feed (`PromptFeed`: items of `cursor` + `event`) across turns. `after` is an opaque cursor
  (`None` replays from the oldest); `next_cursor` is set on every page, so pass it back to
  continue, also after reconnecting. `follow_vm_prompt_events(vm, after=)` long-polls it
  without end and yields `PromptFeedItem`s; resume with the last item's `cursor`.
- `list_conversations(vm, limit=, cursor=)` returns one page, in stable
  `(createdAt, id)` order (oldest first). `iter_conversations(vm)` follows
  `nextCursor` through every page, and `get_conversation(vm, id)` returns one.

## Errors

Every error derives from `davit.DavitError`. Each one has `status` (the
HTTP status, or `None` for client-side errors), `code`, `message`, `retryable`,
`details`, `request_id` and `operation_id`.

| Server `code`                 | Class                                             |
| ----------------------------- | ------------------------------------------------- |
| invalid_argument              | `InvalidArgument`                                 |
| unauthenticated               | `Unauthenticated`                                 |
| permission_denied             | `PermissionDenied`                                |
| not_found                     | `NotFound`                                        |
| operation_in_progress         | `OperationInProgress` (a `Conflict`)              |
| idempotency_conflict          | `IdempotencyConflict` (a `Conflict`)              |
| invalid_state                 | `InvalidState` (a `Conflict`)                     |
| stop_required                 | `StopRequired` (a `Conflict`)                     |
| revision_mismatch             | `RevisionMismatch`                                |
| payload_too_large             | `PayloadTooLarge`                                 |
| unsupported_capability        | `UnsupportedCapability`                           |
| resource_exhausted            | `ResourceExhausted`                               |
| unavailable                   | `Unavailable`                                     |
| not_implemented               | `ServerNotImplemented`                            |
| timeout                       | `Timeout`                                         |
| internal                      | `Internal`                                        |
| (unknown, from a newer server)| `DavitError`                                    |

These errors come from the client itself:

- `TransportError`: the daemon is unreachable, the connection failed
  mid-request, or a process stream was lost (code `unavailable`).
- `WaitTimeout`: the wait budget ran out (code `timeout`).
- `OperationFailed`: an awaited operation failed.

```python
from davit import NotFound, StopRequired

try:
    sw.fork_vm("web")
except StopRequired:
    sw.stop_vm("web")
```

## Models

The wire types in `davit.models` are frozen dataclasses generated from
`api/openapi.yaml`. They use snake_case attributes, for example `state_revision`,
`vm_fqdn` and `source_vm_id`. Unknown JSON fields are ignored. Enums are open:
a value the client does not know (for example a new `VMState`) still parses and
still compares equal to its string. Timestamps are timezone-aware `datetime`s.

## Development

Run these commands from the repository root. They use the repo-local venv at
`.build/py-venv`.

```sh
# regenerate models from api/openapi.yaml (commit the result)
.build/py-venv/bin/python sdk/python/scripts/gen_models.py
# check that models.py is up to date (the test suite also does this)
.build/py-venv/bin/python sdk/python/scripts/gen_models.py --check
# lint and format check
.build/py-venv/bin/ruff check sdk/python && .build/py-venv/bin/ruff format --check sdk/python
# type check (strict; the config also pins python_version = 3.10)
.build/py-venv/bin/mypy --strict sdk/python/src
.build/py-venv/bin/mypy --config-file sdk/python/pyproject.toml sdk/python/src
# tests: unit, contract, and the real-daemon integration test
.build/py-venv/bin/python -m pytest sdk/python/tests
```

The integration tests start `bin/davit daemon` with the fake runtime, in a
private `$TMPDIR/swpy-*` directory (`/tmp` if `$TMPDIR` is too long for a unix
socket path). That directory and the daemon are removed after
each test, even when the test fails. The process, file and prompt tests enable the fake runtime's
in-process guest agent (`"fake": {"guestAgent": true}`), so their commands run
on the host as your user; the prompt tests point the `claude`/`codex` harness commands
at the repo's fake scripts in `internal/harness/testdata`. `tests/test_remote.py` makes a
throwaway CA and server certificate with `openssl` (skipped when it is not installed), checks
that HTTPS and WebSocket connections succeed with `ca_file`/`DAVIT_CA_FILE` and fail
without it, and runs `examples/remote_https.py` against the fake daemon behind a local TLS proxy. The WebSocket unit tests use a scripted server on a
unix socket. Set `DAVIT_BIN` to use another binary. If the binary is missing,
the tests are skipped; run `make build` first.

The contract tests fail when `api/openapi.yaml` changes in a way this SDK does
not cover:

- an operation that is not in `davit.OPERATIONS` or `davit.NOT_YET_SUPPORTED`;
- generated models that are stale;
- an error code with no matching exception class.
