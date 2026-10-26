# Mac VZ helper protocol, version 1

The contract between the Go daemon's Mac runtime adapter (`internal/runtime/mac`) and the per-VM helper:
a signed Swift executable that owns one `VZVirtualMachine` ([MAC-BACKEND.md](MAC-BACKEND.md)). The Go
reference implementation is [`internal/runtime/mac/helperproto`](../../internal/runtime/mac/helperproto)
(codec, message types, validation, client), and [`fakehelper`](../../internal/runtime/mac/fakehelper) is a
complete Go helper with a simulated VM. The adapter passes the runtime conformance suite against that fake.
A Swift helper is correct when it behaves like the fake for every message below. **No Swift helper exists
yet, and nothing here has run on a Mac.**

## Process model

- One helper process serves one VM run: one `(vmId, generation)`.
- The adapter creates an owner-only directory (mode 0700) and starts
  `helper serve --dir DIR --vm ID --generation N`:
  - in its own session (setsid);
  - with cwd DIR;
  - with a minimal environment;
  - with stdout and stderr appended to `DIR/helper.log`.
- The helper must survive the daemon: no parent-death signal, no exit on a closed control connection. It
  exits **only** once its VM has reached a terminal state, after writing `exit.json`.
- `helper probe` prints one `ProbeResult` JSON object on stdout and exits 0. It never starts a VM. The
  daemon runs it at start and for `davit doctor`.
- Any other invocation exits 2 with a usage message on stderr.

## Files in DIR

| File | Writer | When | Content |
|---|---|---|---|
| `launch.json` | adapter | right after spawn | `{pid, birth, boot}`: the helper's OS identity (macOS `kern.proc.pid` start time in µs, `kern.bootsessionuuid`) |
| `helper.json` | helper | at start, **before** listening | `Record{protocol, incarnation, key, pid, startedAt}` |
| `ctl.sock` | helper | listening | control RPC, mode 0600 |
| `str.sock` | helper | listening | guest byte streams, mode 0600 |
| `console.log` | helper | while running | guest serial console |
| `exit.json` | helper | after the VM stopped, before exit | `Exit{incarnation, state, reason, detail, at}` |

- **Atomic writes:** JSON files are written atomically (write to a temporary name, fsync, rename).
- **Incarnation:** a random 128-bit hex ID chosen at helper start; it identifies this helper for its whole life.
- **Path length:** macOS limits socket paths to 103 bytes. The adapter names DIR `RUNTIME/vm/<12 hex>` and
  checks the length.

## Framing

```
 0    1    2      3      4                 8
+----+----+------+------+-----------------+------------------------+
| 'S'| 'H'| fmt=1| flags| length (u32 BE) | payload: JSON envelope |
|    |    |      |  =0  | 2..65536        |                        |
+----+----+------+------+-----------------+------------------------+
```

- **Envelope:** `{"v":1,"id":7,"type":"boot","seq":0,"body":{...},"error":{"code":"...","message":"...","retryable":false}}`.
  Absent members may be omitted. Unknown members are ignored.
- **Bad frames:** a payload over 65536 bytes, bad magic, a nonzero flags byte, framing version ≠ 1,
  unparseable JSON, or `v`/`type` missing is a framing error, and the connection is closed. A reader
  rejects an oversize length **before** allocating.
- **Requests and replies:** a request has a nonzero `id`. Its reply has the same `id` and `type`, and either
  a `body` or an `error`.
- **One request in flight:** a control connection carries one request at a time. A client wanting
  concurrency opens more connections, and the helper must serve them concurrently.
- **Unknown types:** answered with error `unsupported`; the connection stays open.
- **Version:** after Hello, every frame carries the negotiated `v`. A frame with another version gets error
  `version_mismatch`, and the connection is closed.

## Handshake (every connection, both sockets)

```
client -> helper  hello {"minVersion":1,"maxVersion":1,"client":"davit 0.x"}       (envelope v = 1)
helper -> client  hello {"version":1,"incarnation":"…","key":{"vmId":"…","generation":N},
                         "helperVersion":"1.0.0","build":"…","operations":["create",…]}
```

- **Version choice:** the helper picks the highest version in both ranges. With no common version it
  answers `version_mismatch` and closes.
- **Hello first:** a first frame that is not `hello` gets `invalid_state`, and the connection is closed.
- **Peer check:** the helper checks the connecting peer's UID (`LOCAL_PEERCRED`/`xucred`) equals its own, and
  closes otherwise. The client checks the helper's UID the same way.
- **Identity check:** the client compares `incarnation` and `key` with `helper.json` and refuses a mismatch,
  since a recycled socket or PID never matches.

## Control messages (`ctl.sock`)

All replies below carry a `StateResult` body: `{"state", "startedAt", "reason", "detail"}`.

| Type | Request body | Semantics |
|---|---|---|
| `create` | `VMConfig` | Validate, then build the `VZVirtualMachineConfiguration` (see below). Must be idempotent: the same configuration again → ok; a different one → `conflict`. Only valid in `created`. Does not start the VM. |
| `boot` | none | `created` → `starting` → `running`, then reply. Replying only once VZ reports running is required. Idempotent: `running`/`paused` → reply at once. A concurrent boot waits for the first one. Terminal → `invalid_state`. |
| `state` | none | Current state; never blocks on VZ work. |
| `pause` / `resume` | none | Wait for the confirmed VZ transition. Same-state → ok; otherwise from the wrong state → `invalid_state`. |
| `stop` | `{"graceMillis":N}` | Request guest shutdown (`requestStop`), state `stopping`, and wait up to N ms for VZ to report stopped. On success the state is `stopped`/`shutdown`. On timeout reply `timeout` (`retryable: true`) and return to `running`: **never force**, because forcing is the client's explicit `kill`. On terminal → reply the state. |
| `kill` | none | Force-stop VZ (`stop`) and reply `stopped`/`killed`. Idempotent. |
| `watch` | none | Turns the connection into an event stream: `event` frames with the watch request's `id`, `seq` = 1, 2, …, body `StateResult`. The first event is the current state; the stream ends (connection closed) after a terminal state. A slow watcher may miss intermediate states, never the terminal one. |

**States:** `created` → `starting` → `running` ⇄ `paused`. From `running`, a stop goes through `stopping`
to `stopped`, or back to `running` if the guest refuses. `stopped` and `error` are terminal.

**Terminal sequence:** on entering a terminal state (stop, kill, or VZ stopping the VM on its own or with
an error), the helper:
1. replies to the pending request;
2. closes its listeners and existing stream connections;
3. writes `exit.json`;
4. exits with status 0.

**Exit reasons:** `shutdown` (the guest powered off), `killed`, `crashed` (VZ error, with `detail`).
A helper that dies without `exit.json` is reported as `crashed` by the adapter.

### VMConfig

```json
{"key":{"vmId":"vm_ab12","generation":3},"cpus":2,"memoryBytes":2147483648,
 "kernel":"/abs/vmlinux","initrd":"/abs/initrd","commandLine":"console=hvc0 root=/dev/vda rw",
 "rootDisk":"/abs/root.img","network":"none","vsockPort":1024}
```

- **Key:** must equal the helper's own `--vm/--generation` (`invalid_argument` otherwise).
- **Paths:** absolute, clean, ≤ 1024 bytes. The helper opens each with `O_NOFOLLOW`, requires a regular file,
  and keeps the descriptor for the VM's life. Symlinks, directories and devices → `invalid_argument`.
- **Boot loader:** `VZLinuxBootLoader` with kernel, initrd and command line, plus a `VZGenericPlatformConfiguration`.
- **Root disk:** a `VZVirtioBlockDeviceConfiguration` over a read-write `VZDiskImageStorageDeviceAttachment`
  of `rootDisk`.
- **Console:** a virtio console to `console.log`.
- **vsock:** a `VZVirtioSocketDeviceConfiguration`. The guest agent listens on `vsockPort`, and the guest
  must see host CID 2.
- **Shape:** `cpus` and `memoryBytes` within the probe's limits; `memoryBytes` is whole MiB.
  Out of range → `invalid_argument`.
- **Network:** `"none"` means no NIC. Anything else is `unsupported` until the packet-link network contract
  ([MAC-BACKEND §3](MAC-BACKEND.md#3-networking-and-tailnet-access)) adds a mode. **Never** fall back to
  VZ NAT.
- **Validation failure:** call `validate()` on the configuration; a failure → `vz_error` with the message.

## Guest streams (`str.sock`)

```
client -> helper  hello …                       (as above)
client -> helper  dial {"port":1024,"timeoutMillis":10000}
helper -> client  dial {}                        or an error: invalid_state (not running), refused, timeout
then raw bytes in both directions
```

- **Dial:** the helper connects with `VZVirtioSocketDevice.connect(toPort:)` and proxies bytes between the
  connection and the guest stream, with backpressure.
- **Half-close:** EOF from one side half-closes toward the other.
- **Teardown:** the stream ends when either side closes or the VM stops.
- **Scope:** only this VM's device is reachable; there is no listener for guests to connect to.
- **Limits:** bound the number of concurrent streams (at least 64) and refuse more with `refused`
  (`retryable: true`).
- **Isolation:** guest bytes never travel in frames, and a busy stream never delays the control socket.

## Probe

```json
{"minVersion":1,"maxVersion":1,"helperVersion":"1.0.0","build":"…","osVersion":"14.6.1","osBuild":"23G93",
 "arch":"arm64","vzSupported":true,"entitled":true,
 "limits":{"maxCPUs":10,"minMemoryBytes":134217728,"maxMemoryBytes":68719476736,"memoryAlignBytes":1048576},
 "operations":["create","boot","state","pause","resume","stop","kill","watch","dial"]}
```

- **`vzSupported`:** `VZVirtualMachine.isSupported`.
- **`entitled`:** whether this signed process holds `com.apple.security.virtualization` (read the
  code signature's entitlements).
- **Limits:** from `VZVirtualMachineConfiguration` minimums and maximums.
- **Refusal:** the daemon refuses to start the runtime when either flag is false, or when no protocol
  version is shared.

## Errors

| Code | Meaning |
|---|---|
| `malformed_frame`, `frame_too_large` | framing; the connection is closed |
| `version_mismatch` | no common version / wrong frame version |
| `unsupported` | unknown type or unavailable feature (`network` other than `none`) |
| `invalid_argument` | a field fails validation |
| `invalid_state` | not valid in the current state |
| `conflict` | `create` with another configuration |
| `timeout` | a deadline passed; state unchanged (`stop` grace) |
| `refused` | vsock dial refused or too many streams |
| `unauthorized` | peer UID mismatch |
| `vz_error` | Virtualization.framework error (message carries it) |
| `internal` | anything else |

The adapter maps these onto domain error codes (`internal/runtime/mac`: `mapErr`).

## Compatibility rules

- **Extensions:** new message types or body members are additive within version 1; receivers ignore what
  they do not know.
- **Breaking changes:** a change to existing semantics needs version 2, negotiated by Hello. The daemon
  pins the helper release, its signing identity and the protocol range together
  ([MAC-BACKEND, signed helper boundary](MAC-BACKEND.md#signed-helper-boundary)).
