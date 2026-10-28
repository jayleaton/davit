# @davit/sdk

TypeScript client for the davit v1 API (`api/openapi.yaml`). ESM only, zero
runtime dependencies, Node >= 20 and Bun. Over HTTPS it uses only the global
`fetch`, so it also runs in Deno and browsers; the local unix socket uses
`node:http`, imported only when a socket is used.

## Install

```sh
npm install @davit/sdk
```

From this repository: `cd sdk/typescript && npm install && npm run build`.

## Quickstart

### Local daemon (unix socket)

```ts
import { DavitClient } from "@davit/sdk";

// No options: $DAVIT_API_URL, else the default socket
// ($XDG_RUNTIME_DIR/davit/davit.sock, $XDG_DATA_HOME/davit/davit.sock,
// ~/.local/share/davit/davit.sock; macOS
// ~/Library/Application Support/davit/davit.sock).
const client = new DavitClient();
// or: new DavitClient({ socket: "/run/user/1000/davit/davit.sock" });

const vm = await client.createVM({ name: "web" }); // waits until ready
console.log(vm.state, vm.bootMode);

await client.stopVM("web");                       // commits the disk
const snap = await client.createSnapshot("web", "base");
const fork = await client.forkVM("web", { name: "web-2", snapshot: "base" });
const twin = await client.forkVM("web", { name: "web-3", live: true }); // running VM: RAM + processes + disk
await client.resumeVM("web");
await client.stopVM("web");
await client.resumeVM("web", { shape: { cpus: 4, diskBytes: 40 * 2 ** 30 } }); // resize; disk only grows
await client.setVMEgress("web", { mode: "allowlist", allow: ["github.com", "registry.npmjs.org"] });

for await (const v of client.iterVMs()) console.log(v.name, v.state);

await client.deleteVM("web-2");
```

### Profiles, TTL and renames

```ts
const caps = await client.getCapabilities();
caps.profiles;                                    // [{ name: "big", shape: { cpus, memoryBytes, diskBytes } }]
await client.createVM({ name: "build", type: "big" });          // `shape` fields override the profile
await client.forkVM("build", { name: "build-2", type: "big" });

await client.extendVM("build");                     // like `davit extend`: restart the auto-stop countdown
await client.extendVM("build", { ttlSeconds: 7200 });  // ...with a new TTL of 2h
await client.renameVM("build", "ci");
await client.updateVM("ci", { ttlSeconds: 600, autoStop: true }, { ifMatch: vm.stateRevision });
```

`updateVM` (PATCH) is synchronous and returns the VM; only the fields given
change. It is naturally idempotent (no `Idempotency-Key`) and supports
`ifMatch`. `extendVM` always re-enables auto-stop (exactly what the CLI sends).
`vm.expiresAt` is when the VM auto-stops; `vm.autoStopWarningAt` is when the
host starts warning (`expiresAt` minus `capabilities.ttlWarningSeconds`).

### Remote host (HTTPS + token)

```ts
const client = new DavitClient({
  baseUrl: "https://davit-host.tailnet.ts.net",
  token: process.env.DAVIT_TOKEN,  // a token's secret (slw_...), sent as Authorization: Bearer
});
```

`baseUrl` must be `https://`; plain `http://` is accepted only for loopback
hosts (`localhost`, `127.0.0.0/8`, `::1`), so a token is never sent in clear
text over a network. `DAVIT_API_URL` accepts `unix:///abs/path.sock` or an
http(s) URL. All routes live under `/api/v1`.

A custom `Transport` (`{ request(req): Promise<TransportResponse> }`) can be
passed as `transport`; the built-ins are `FetchTransport`, `HttpsTransport`
(extra trusted CAs) and `UnixSocketTransport`.

### Remote access over the tailnet

The daemon's tailnet gateway serves the same API over HTTPS. The CLI, SSH and
both SDKs share one client configuration, three environment variables:

| Variable | Meaning | Client option (wins) |
| --- | --- | --- |
| `DAVIT_API_URL` | `https://<host>[:port]` | `baseUrl` (or `socket`) |
| `DAVIT_TOKEN` | optional bearer token (`slw_…`, `davit token create`) | `token` |
| `DAVIT_CA_FILE` | optional PEM bundle of extra trusted CAs | `caFile` |

- Without a token the gateway authenticates the caller's tailnet identity
  (nothing to configure). `$DAVIT_TOKEN` is used only when the endpoint is
  an http(s) base URL, never for the local unix socket (owner-only; it never
  gets a bearer) and not with a custom `transport`. `token: ""` sends no token
  even when the variable is set.
- With Tailscale's certificate for the host's MagicDNS name (publicly trusted)
  no CA file is needed. For an operator certificate signed by a private CA, set
  `DAVIT_CA_FILE` (or `caFile`): the bundle is trusted **in addition to**
  Node's built-in roots for requests and for the WebSocket streams (process
  attach, tunnels). `fetch` cannot take a CA without a dependency, so such a
  client uses `HttpsTransport`: node:https and node:tls with
  `ca: [...tls.rootCertificates, pem]` (Bun: its fetch `tls.ca` option). The
  file is read and parsed at construction (`process.getBuiltinModule`, Node ≥
  20.16 or Bun); a missing, unreadable or invalid file throws a `TypeError`.
  The variable is ignored for the local socket; `caFile` with `socket` (or a
  custom `transport`) throws.

```ts
const client = new DavitClient();  // all three from the environment
const pinned = new DavitClient({ baseUrl: "https://box.example.internal:8443", token: "slw_...", caFile: "/etc/davit/ca.pem" });
```

[`examples/remote-https.ts`](examples/remote-https.ts) reads only those three
variables, prints the runtime engine and the VMs, creates a VM (name from
`argv[2]`, default `sdk-https`) and waits until it is ready, runs `uname -a`,
prints its stdout and deletes the VM. It exits 0 on success, 1 on an API
error (printing `<code>: <message>`) or a failed command, 2 on bad
configuration. It runs from source with Node ≥ 22.18 (or `bun`), from the
repository root:

```sh
DAVIT_API_URL=https://davit-host.tailnet.ts.net \
DAVIT_TOKEN=slw_... DAVIT_CA_FILE=/path/to/ca.pem \
  node sdk/typescript/examples/remote-https.ts my-vm
```

## Resize

`resumeVM(vm, { type, shape })` resizes a stopped VM as it boots (feature
`resize`): CPUs and memory change freely within the runtime's limits, the disk
can only grow (`InvalidArgument` with `details.reason === "disk_shrink"`
otherwise), and a running VM fails with `StopRequired`. The grown block device
is not grown inside the guest: run `sudo resize2fs /dev/vda` in the VM to use
the new space.

## Waiting

Lifecycle calls (`createVM`, `forkVM`, `stopVM`, `resumeVM`, `recoverVM`, `deleteVM`,
`createSnapshot`) wait by default, like the CLI: they long-poll the operation's
events and return the final `VM` (or `Snapshot`). Pass `wait: false` to get the
202 `OperationAccepted` body instead and follow it yourself:

```ts
const { operation } = await client.createVM({ name: "job" }, { wait: false });
const done = await client.wait(operation.id, {
  timeoutMs: 60_000,                             // default 10 minutes
  onProgress: (e) => console.log(e.phase, e.progress),
});
const vm = await client.waitReady("job");        // ready or running, else throws
```

A `WaitTimeout` only stops the client from waiting; the server operation keeps
running (`err.operationId`). An `AbortSignal` (`signal`) behaves the same.

`createVMs(count, template, opts)` creates up to 100 VMs from one template in
one request (feature `batchCreate`, like `davit new web --count 3`); a
template `name` N names them N-1 ... N-count. It returns one item per VM, in
order, with its `accepted` operation or its `error` (e.g. `resource_exhausted`
when the host is full): items are admitted on their own, so check every item.
It does not wait unless `wait: true`, which follows every accepted operation
and puts the terminal operation and VM in its item (failures are reported, not
thrown). Calling again with the same `idempotencyKey` returns the same
operations and creates only the items refused before.

```ts
const { items } = await client.createVMs(3, { name: "worker" }, { idempotencyKey: "workers-1", wait: true });
for (const it of items) console.log(it.index, it.name, it.error?.code ?? it.accepted?.operation.state);
```

When the host is full (feature `capacityReclaim`), `createVM` and `resumeVM`
first stop idle VMs that have an idle timeout; `capacityWaitSeconds` (integer
0..3600) then queues the request that long for room, first come first served,
instead of failing with `ResourceExhausted` at once (`waitSeconds`). It is not
`wait`, which follows the operation. `reclaimVMs(idleSeconds, { labels, dryRun })`
stops your VMs idle that long, like `davit box gc`, and returns them (oldest
activity first) with their stop's `operationId`, without waiting for the stops;
`dryRun: true` only lists them. It sends no idempotency key: a repeat stops
nothing twice.

```ts
const vm = await client.createVM({ name: "web" }, { capacityWaitSeconds: 120 }); // queue up to 2 minutes for room
for (const it of (await client.reclaimVMs(3600, { labels: { "davit.box": "myproject" } })).items) {
  await client.wait(it.operationId!);
}
```

Every lifecycle mutation (and `execProcess`, `createPrompt`, `steerPrompt`, `createToken`,
`createCredentialProfile`, `createCredentialGrant`) sends an
`Idempotency-Key` (pass `idempotencyKey`, or a UUIDv4 is generated once per
call and reused on retry); lifecycle mutations also accept `ifMatch` (the VM's
`stateRevision`). `updateVM` (with `extendVM`/`renameVM`), `deleteSnapshot`,
`killProcess`, `writeFile`, `cancelPrompt`, `interruptPrompts`, `revokeToken`,
`deleteCredentialProfile`, `putCredential`, `deleteCredential` and
`revokeCredentialGrant` are naturally idempotent and send no key. The client retries only when the request certainly never reached
the server (connection refused, socket file missing): at most 3 attempts with
a short backoff.

## Egress

Each VM has an egress policy, `vm.egress` (`{ mode, allow? }`). The mode is
`off` (nothing leaves the VM, DNS included), `filtered` (the public internet
only; private, host, metadata and tailnet ranges stay blocked) or `allowlist`
(only the `allow` entries, on top of `filtered`).

```ts
const vm = await client.createVM({ name: "job", egress: { mode: "allowlist", allow: ["GitHub.com", "10.9.1.2/16"] } });
vm.egress.allow;                                         // ["10.9.0.0/16", "github.com"]: normalized by the server
await client.forkVM("job", { name: "job-2" });           // after a stop: inherits the source's policy (or pass egress)
await client.setVMEgress("job", { mode: "off" });        // resolves the VM
```

- `createVM`'s `egress` defaults to the host's `network.egress` (else
  `filtered`); `forkVM`'s `egress` option defaults to the source's policy.
- `setVMEgress(vm, egress)` is `updateVM(vm, { egress })`: synchronous and naturally idempotent. It is
  live on a running VM (new connections and DNS lookups; flows the new policy
  no longer allows are closed) and kept for later boots of a stopped one.
- `allow` entries are domains (`github.com` also matches its subdomains,
  `*.example.com` only subdomains) and IPv4 addresses or CIDRs, at most 256,
  for `allowlist` only. A bad entry throws `InvalidArgument`. A running
  lifecycle operation throws `OperationInProgress` (retryable), and a running
  VM that cannot be updated throws `Unavailable` (the old policy stays).

## Privacy and ephemeral VMs

`vm.privacy` is the VM's `PrivacyPolicy` (feature `privacyPolicy`). Every flag
defaults to off; forks inherit the source's policy.

```ts
await client.createVM({ name: "scratch", privacy: { ephemeral: true } });       // deleted when it stops
await client.createVMs(5, { name: "job", privacy: { noOutputRetention: true } });
const vm = await client.getVM("agent");
await client.updateVM("agent", { privacy: { ...vm.privacy, noProcessSamples: true } });
```

- `ephemeral`: deleted on any stop (`stopVM`, the TTL auto-stop, a forced
  stop), nothing committed. A VM that crashes into `error` is not stopped and
  stays until deleted or recovered.
- `noSnapshot`: a stop commits nothing; the stopped VM cannot be resumed or
  forked and explicit snapshots are refused.
- `noOutputRetention`: the host stores no process output (history lists the
  processes, argv included, without output). The guest agent still holds recent
  output in guest memory while the VM runs.
- `noProcessSamples`: no stored whole-guest process samples; listing guest
  processes still works on a running VM.
- `updateVM(vm, { privacy })` replaces the whole policy; flags left out are
  off. A server without the feature throws `ServerNotImplemented`.
- Not secure erasure: deleted data can survive in host backups, filesystem free
  space and SSD wear levelling, and the guest can always write to its own disk
  and send data within its egress policy. See
  [docs/vms-and-agents.md](../../docs/vms-and-agents.md#ephemeral-and-private-vms).

## Environments

An environment is a named, versioned workspace manifest (repos to clone,
variables, secret files that name host secrets, setup commands). Every change
mints a new immutable revision; a VM pins one revision.

```ts
const env = await client.createEnvironment("app", {
  repos: [{ url: "https://github.com/acme/app.git", path: "/work/app" }],
  variables: { CI: "1" },
  setup: [{ name: "deps", argv: ["make", "deps"], cwd: "/work/app" }],
});                                                          // revision 1
await client.createEnvironmentRevision("app", { variables: { CI: "2" } }, { ifMatch: env.latestRevision });
for await (const r of client.iterEnvironmentRevisions("app")) console.log(r.revision);  // oldest first
await client.createVM({ name: "dev", environment: "app@2" }); // or "app" (latest), or { environment, revision }
await client.deleteEnvironment("app");                       // pinned VMs keep their copy
```

- `createEnvironment(name, manifest, { idempotencyKey? })` resolves the
  `Environment`. Omitted `repos`, `variables`, `secretFiles` and `setup` are
  sent empty; an unknown manifest key throws `TypeError`. It always sends an
  `Idempotency-Key`; the server also returns the existing environment for the
  same name and an identical first manifest, and throws `InvalidArgument`
  when the name is taken.
- `createEnvironmentRevision(env, manifest, { ifMatch?, idempotencyKey? })`
  mints the next revision from a complete manifest. `ifMatch` is the expected
  `latestRevision`; a stale one throws `RevisionMismatch` with
  `details.latestRevision`. A retry with the same key returns the revision it
  minted.
- `listEnvironments({ limit, cursor })` / `iterEnvironments()`,
  `getEnvironment(idOrName)`, `deleteEnvironment(env)` (no key; a repeat
  throws `NotFound`), `listEnvironmentRevisions(env, { limit, cursor })` /
  `iterEnvironmentRevisions(env)`, `getEnvironmentRevision(env, n)`.
- `createVM`, `forkVM` and `resumeVM` take `environment`: an
  `EnvironmentRef`, `"NAME"` (the latest revision) or `"NAME@REVISION"`
  (parsed by `parseEnvironmentRef`, which throws `TypeError` on anything
  else). A daemon without a workspace preparer throws `ServerNotImplemented`.

## Processes

```ts
// Run to completion: argv runs directly (no shell unless you ask for one).
const r = await client.run("web", ["sh", "-c", "echo hi; echo oops >&2; exit 3"]);
r.exitCode;     // 3   (124 = deadline, 128+n = signal n, 255 = unknown)
r.stdout;       // Uint8Array; r.stdoutText / r.stderrText decode UTF-8
r.process;      // the final Process (status, exitCode, signal, timedOut)

await client.run("web", ["cat"], { input: "piped\n", env: { A: "1" }, cwd: "/tmp", deadlineSeconds: 30 });

// Background work: start detached, follow later, signal it.
const p = await client.execProcess("web", ["./server"], { detached: true });
for await (const proc of client.iterProcesses("web")) console.log(proc.id, proc.status);  // oldest first
await client.getProcess("web", p.id);
await client.killProcess("web", p.id, "TERM");   // default SIGTERM

// Streams: output chunks with increasing seq, then one exit event.
const stream = await client.attachProcess("web", p.id, { outputCursor: 0 });
for await (const ev of stream) {
  if (ev.type === "exit") console.log("exit", ev.process.exitCode);
  else process.stdout.write(ev.data);          // ev.type is "stdout" | "stderr"
}
```

`listProcesses` pages oldest first (`limit`, `cursor`, `nextCursor`);
`iterProcesses` follows the cursor. A start interrupted by a daemon crash is
reported with status `lost` and is never re-run.

`execProcess` always sends an `Idempotency-Key` (pass `idempotencyKey` to
deduplicate separate calls). A process that is not `detached` is killed by
the server when its first stream closes before it exits (or when no stream
attaches within 60 seconds); `run` attaches immediately.

A `ProcessStream` also has `sendStdin(bytes | string)` and `closeStdin()`
(only for processes started with `stdin: true`, on their first stream),
`signal(name)` and `close()`. `stream.cursor` is the seq of the last output
chunk delivered: if the connection drops before the exit, the stream
re-attaches from it automatically (3 attempts) and drops duplicates; stdin is
not replayed after a reconnect. To resume later yourself, pass it as
`outputCursor`. A server `error` message throws the typed error.

Streams use a built-in RFC 6455 WebSocket client (no dependencies, 1 MiB
message limit) over the same endpoint as the HTTP calls: the unix socket,
TLS for `https://` base URLs (with the bearer token), or TCP for loopback
`http://`. It needs `node:net`/`node:tls`, so process streams work on Node and
Bun but not in browsers (whose `WebSocket` cannot send an `Authorization`
header); the other process calls work everywhere.

## Job history

The daemon keeps the output of managed (detached) jobs on the host, so a job
can be listed and its output read after it ended, while the VM is stopped and
across daemon restarts.

```ts
for await (const e of client.iterProcessHistory("web")) {          // newest first, across generations
  console.log(e.process.id, e.process.status, e.stdoutBytes, e.stderrBytes);
}
const out = await client.collectProcessOutput("web", procId);       // { stdout, stderr, complete, cursor }
for await (const chunk of client.iterProcessOutput("web", procId, { stream: "stderr" })) {
  process.stderr.write(chunk.data);                                 // { type, seq, data: Uint8Array }
}
```

- `listProcessHistory(vm, { limit, cursor })` resolves a `HistoryList`;
  `iterProcessHistory(vm)` follows `nextCursor`. `outputDropped` is true when
  the retention limit cut the oldest output.
- `readProcessOutput(vm, id, { after?, stream?, limit? })` resolves one raw
  `ProcessOutput` page (base64 `data`, `nextCursor`, `complete`).
- `iterProcessOutput(...)` yields decoded chunks and `collectProcessOutput(...)`
  resolves the bytes. Both follow `nextCursor` until a page is `complete` (the
  process ended and everything retained was read) or brings nothing new (it
  still runs: read again later with `after: out.cursor`). Malformed base64
  throws a `DavitError` with code `internal`.
- A daemon without guest transport throws `ServerNotImplemented`.

## Runs and push

A run creates a VM, runs one command in it, keeps the exit code and deletes
the VM (unless `keep`), like `davit run`. `pushTree` applies a tar of files
under a guest directory, like `davit push`.

```ts
const out = await client.runToCompletion({ argv: ["make", "test"] }, { files: tarBytes });
out.exitCode; out.stdout; out.stderr; out.run.state;          // RunOutcome

let run = await client.createRun({ argv: ["sh", "-c", "exit 3"], timeoutSeconds: 600 });
run = await client.waitRun(run);                              // polls getRun until endedAt
run.state; run.exitCode;                                      // "failed", 3

const held = await client.createRun({ argv: ["make"], hold: true, cwd: DEFAULT_PUSH_PATH });
// ...once (await client.getRun(held)).state === "waiting":
await client.pushTree(vmId, DEFAULT_PUSH_PATH, tarBytes);     // { files, bytes, sha256, generation }
await client.startRun(held);
```

- `createRun(req, { idempotencyKey? })` resolves the `Run` at once (state
  `creating`). It always sends an `Idempotency-Key`: the same key and body
  return the same run, another body throws `IdempotencyConflict`.
  `req.environment` takes an `EnvironmentRef` or `"NAME[@REV]"` as on `createVM`.
- `listRuns({ limit, cursor })` resolves one `RunList` page, oldest first;
  `iterRuns()` follows `nextCursor`.
- `getRun(run)`, `startRun(run)` (only a `waiting` run; else
  `InvalidState`), `cancelRun(run)`, `waitRun(run, { timeoutMs? })` (polls
  every 200 ms; a timeout throws `WaitTimeout` with `details.runId` and never
  cancels the run).
- `outcome` and `decidedAt` give the result as soon as it is known; `state`
  becomes `outcome` only after cleanup (output grace, then the VM is deleted
  unless `keep`), when `endedAt` is set. `failed` covers a non-zero exit
  (`exitCode`), a signal kill (`signal`, with no `error`), the timeout
  (`error.code` `timeout`) and setup failures (`error`). A cancel sets
  `outcome` to `cancelled`.
- After the command exits, the run keeps its VM for an output grace so its
  output can still be read through `processId` (`attachProcess`,
  `collectProcessOutput`): `req.outputGraceSeconds` (0..600, 0 = none;
  omitted = the host default, daemon config `runs.outputGrace`, 5 s). Once
  `outcome` is set, `cancelRun` only ends that grace and keeps the outcome;
  before that it kills the command and the run ends `cancelled`.
- `runToCompletion(req, { files?, filesPath?, idempotencyKey?, timeoutMs? })`
  does it all: create (with `hold` when `files` is given), push and start,
  attach and collect the output until the exit (if the VM is already gone, the
  rest comes from the host-retained output), `cancelRun` once
  `outcome`/`decidedAt` is set (not with `outputGraceSeconds: 0`), then
  `waitRun`. `exitCode` follows `davit run` (130 cancelled, 124 timeout, the
  exit code, 128+n for a signal); `signal` names the signal. A run that failed
  before its command started throws its typed error.
- `pushTree(vm, path, tar, { mode? })` sends a **ready tar** (bytes or a
  stream) as `application/x-tar`. The TypeScript SDK has no tar writer and does
  not select files; build the tar yourself (`git ls-files -z --cached --others
  --exclude-standard` gives the CLI's selection), or use the Python SDK's
  `push_dir` or `davit push`. `merge` (default) adds and replaces files and
  keeps the others; `mode: "mirror"` is sent as is and this server answers
  `ServerNotImplemented`. No `Idempotency-Key` (naturally idempotent).

## Files

```ts
// Write (atomic replace; the agent verifies size and SHA-256), then read back.
const info = await client.writeFile("web", "/home/dev/app.conf", "port=8080\n", { mode: 0o640 });
info.sha256;                                     // FileInfo: path, sizeBytes, mode, sha256, generation

const { data, mode } = await client.readFile("web", "/home/dev/app.conf");  // Uint8Array, "0640"

// Large files: stream both ways without buffering.
await client.writeFile("web", "/data/big.bin", someReadableStream);         // or any AsyncIterable<Uint8Array>
const file = await client.readFileStream("web", "/data/big.bin");
for await (const chunk of file) sink.write(chunk);                          // or file.toReadableStream()

// Directories travel as tar (`archive: true`): bytes you build or receive.
await client.writeFile("web", "/srv/site", tarBytes, { archive: true, mode: 0o755 });
const tree = await client.readFile("web", "/srv/site", { archive: true });
```

- `path` must be absolute. `user` picks the guest user whose permissions
  scope the access (default: the image's user).
- `generation` guards against landing in a different boot: a mismatch with
  the VM's `instanceGeneration` throws `InvalidState`. Omitted, the SDK reads
  the VM first and uses its current generation (and ID).
- `mode` (writes) is a number (`0o640`) or an octal string (`"0640"`); reads
  report the server's `X-Davit-File-Mode` string.
- Truncation is never silent: a body shorter than its `Content-Length`, or a
  tar without its end marker (two zero blocks after complete entries), throws
  `TransportError` (for streams, from the iteration).
- `Uint8Array` and string bodies (UTF-8) are retried when the daemon was
  unreachable; streamed bodies are sent once, chunked.
- The SDK has no tar writer/extractor (no dependencies); use your own tar
  tool or library for `archive: true` payloads. The Python SDK adds
  `upload_dir`/`download_dir` on top of its stdlib `tarfile`.

## Snapshot files

A snapshot (ID or name) can be read on the host without booting it. Paths
resolve inside the snapshot: `..` stops at `/` and symlinks stay within it.

```ts
for await (const e of client.iterSnapshotTree("base", { path: "/etc" })) {  // SnapshotEntry, paged
  console.log(e.type, e.mode, e.sizeBytes, e.path, e.linkTarget);
}
const { data, mode, sha256 } = await client.readSnapshotFile("base", "/etc/hostname");
const site = await client.readSnapshotFile("base", "/srv/site", { archive: true });  // tar bytes
const disk = await client.exportSnapshot("base", { format: "raw" });  // FileReadStream; default "tar"
for await (const chunk of disk) sink.write(chunk);
```

- `listSnapshotTree(snapshot, { path = "/", limit, cursor })` returns one
  `SnapshotTree` page, and `iterSnapshotTree` follows `nextCursor`.
- `readSnapshotFile` / `readSnapshotFileStream(snapshot, path, { archive })`
  are checked like VM reads. A regular file must also hash to its
  `X-Davit-Content-Sha256` header, or the read throws `TransportError`. A
  missing header throws `TransportError` too. Archives and exports carry their
  digest as an HTTP trailer, which the SDK does not read.
- `exportSnapshot(snapshot, { format: "tar" | "raw" })` only streams; exports
  are disk-sized.

## Tunnels

A tunnel is one TCP connection to a port on the guest's loopback, carried over
a WebSocket (as `davit forward` and `davit ssh` use it). Node and Bun only.

```ts
const t = await client.connectTunnel("web", 8080); // openTunnel + attachTunnel
try {
  await t.write("GET / HTTP/1.0\r\nHost: x\r\n\r\n");
  t.closeWrite(); // the guest port reads EOF
  for await (const chunk of t) process.stdout.write(chunk); // until the guest side ends
} finally {
  t.close();
}
```

- `openTunnel(vm, port)` resolves a `Tunnel`; nothing listening on the port
  throws `Unavailable`. Attach within 30 s (`expiresAt`), else the server
  closes it. `attachTunnel(tunnelOrId)` resolves a `TunnelStream` (single
  use); `closeTunnel(tunnelOrId)` releases an unattached tunnel.
- `TunnelStream`: `write(data)` (messages of at most 32 KiB, waits for the
  socket to drain), `read()` (`null` at EOF), `closeWrite()`, `close()`, and
  `for await` over `Uint8Array` chunks. A normal server close is EOF; any
  other end throws `TransportError`.

## Reverse forwards

A reverse forward makes a service reachable from inside the VM: the guest agent
listens on `127.0.0.1:guestPort`, and each connection a guest process makes
there is bridged to a far end fixed when the forward is created (the guest
never chooses it). Owner, or a token with `admin` on the VM. Node and Bun only.

```ts
// Like `davit forward --reverse web 5432:127.0.0.1:5432`.
const s = await client.reverseForward("web", 5432, "127.0.0.1", 5432);
await client.run("web", ["psql", "-h", "127.0.0.1", "-c", "select 1"]);
s.close(); // revokes the forward and cuts live connections
console.log(await s.closed); // "closed", or the server's revoked reason

await client.forwardHostService("web", 8080, "10.0.0.5:8080"); // operator-listed host service
```

- `reverseForward(vm, guestPort, localHost, localPort)` creates a `session`
  forward (address label `localHost:localPort`), attaches its session stream
  and, for each guest connection, claims it and dials the local service
  (10 s bound), splicing both with half-close in both directions. A local
  service that cannot be reached closes that one connection. If the attach
  fails, the forward is revoked. `ReverseSession`: `forward`, `active`,
  `close()`, and `closed`, which resolves (never rejects) with `reason` once
  the session ended: the server's `revoked` reason, `"closed"` after
  `close()`, or a dropped stream (then `error` is a `TransportError`).
- `forwardHostService(vm, guestPort, address)` is a `host` forward: the
  daemon dials `address`, which must be listed exactly in the operator's
  `network.reverseHostServices` (else `PermissionDenied`).
- `listReverseForwards(vm)`, `createReverseForward(vm, guestPort, target)`
  and `revokeReverseForward(vm, forwardOrId)` are the plain calls. One forward
  per guest port (`Conflict`); a guest agent that cannot listen is
  `UnsupportedCapability` (capabilities feature `reverseForward`). Stopping or
  deleting the VM revokes its forwards.

## App endpoints

An app endpoint gives an app listening on a guest loopback port a stable,
tailnet-private URL, `https://<gateway>:<gatewayPort>/`, on the shared tailnet
gateway. Callers need a tailnet identity: the owner, or
`Authorization: Bearer <token>` for a token with `exec` on the VM (the bearer
is not forwarded to the app). The endpoint belongs to the VM: stopping the VM
keeps the URL, which answers 503 until the VM runs again; deleting the endpoint
removes it; forks inherit none.

```ts
const ep = await client.expose("web", 3000); // createAppEndpoint; name defaults to "port-3000"
console.log(ep.url); // https://<gateway>:<gatewayPort>/
await client.renameAppEndpoint("web", ep, "site"); // the URL stays the same
await client.hideAppEndpoint("web", "site"); // closes the port; showAppEndpoint reopens it
await client.deleteAppEndpoint("web", "site");
```

- `listAppEndpoints(vm)`, `createAppEndpoint(vm, port, name?)`,
  `updateAppEndpoint(vm, endpoint, { name?, hidden? })` and
  `deleteAppEndpoint(vm, endpoint)` are the plain calls; `endpoint` is the ID,
  the name or the `AppEndpoint`. `expose`, `hideAppEndpoint`,
  `showAppEndpoint` and `renameAppEndpoint` are shorthands.
- Each guest port and each name has at most one endpoint (`Conflict`). A
  daemon without a tailnet gateway throws `UnsupportedCapability` (the
  capabilities feature `appEndpoints` is `unsupported`).

## Desktop

The desktop is the VM's app endpoint of kind `desktop` (named `desktop`), for
the guest VNC server on loopback `port` (default 5900; the desktop image
profile runs x11vnc there). Its `url` opens the built-in viewer, and
`<url>websockify` is a websockify-compatible RFB stream, so noVNC or any
websockify client works too. `view` rights are view-only (the gateway drops
key, pointer and clipboard input); `exec` adds input. Stopping the VM cuts the
stream (503 at the same URL until it runs again); `closeDesktop` removes it.

```ts
const desk = await client.openDesktop("web"); // or openDesktop("web", 5901); returns the open one if any
console.log(desk.url, desk.kind); // https://<gateway>:<gatewayPort>/ desktop
await client.getDesktop("web"); // NotFound when no desktop is open
await client.closeDesktop("web"); // cuts every viewer
```

- Like app endpoints, no `Idempotency-Key` is sent. A daemon without a tailnet
  gateway throws `UnsupportedCapability` (the capabilities feature `desktop` is
  `unsupported`).

## SSH credentials

`issueSSHCredential(vm, { publicKey, guestUser?, ttlSeconds? })` signs your
own OpenSSH public key (an authorized_keys line; the SDK never generates keys)
and resolves an `SSHCredential`: `certificate` (valid for the VM's current
generation, at most 600 s), `guestUser`, `generation`, `hostKeys` to pin
(they match `vm.endpoint.sshHostKeyFingerprints`), `sshTarget` and
`expiresAt`. An invalid key throws `InvalidArgument`; a runtime without guest
transport `UnsupportedCapability`.

## Tokens

Service tokens let another tailnet host (a CI job, an agent) use this daemon
with limited rights. Only the owner identity (the local socket, or the
tailnet owner) may manage them; a client that itself uses a token gets
`PermissionDenied` on these calls.

```ts
const tok = await client.createToken("ci", { actions: ["exec"], vm: "web", ttlSeconds: 30 * 86400 });
tok.secret;  // "slw_...": shown only once, store it now
const remote = new DavitClient({ baseUrl: "https://davit-host.tailnet.ts.net", token: tok.secret });

for await (const t of client.iterTokens()) console.log(t.id, t.name, t.scopes, t.expiresAt);  // never secrets
await client.revokeToken(tok);  // or the ID; a second revoke throws NotFound
```

- `createToken(name, { actions?, vm?, scopes?, ttlSeconds, idempotencyKey? })`
  resolves a `CreatedToken`. Give `actions` (`view`, `exec` implies view,
  `admin` implies both) with an optional `vm` (a name or ID, or an array;
  one scope each; omitted means every VM), or explicit
  `scopes: [{ vmId?, actions }]`. `ttlSeconds` is 1..31536000 (one year). It
  always sends an `Idempotency-Key`; a replay within 10 minutes returns the
  same token. An unknown action throws `InvalidArgument`, an unknown VM
  `NotFound`.
- The SDK never logs the secret. `CreatedToken` is a plain object like every
  wire type, so `console.log(tok)` prints it: log `tok.id` instead.
- `listTokens({ limit, cursor })` returns one page; `iterTokens()` follows
  `nextCursor`. `revokeToken(tokenOrId)` resolves `undefined`; the token
  stops working on its next request.
- The bearer works only on the daemon's tailnet listener (`https://`),
  where the request still needs a tailnet identity and runs as the token's
  issuer limited to its scopes. The local socket ignores tokens.

## CI runners

When the daemon runs GitHub Actions jobs (`ci.github`, one fresh box per job;
see [docs/ci-runners.md](../../docs/ci-runners.md)), the host owner can read
their state, like `davit ci status`. Other callers get `PermissionDenied`.

```ts
const st = await client.getCIStatus({ limit: 50 });  // CIStatus
if (st.enabled) {
  for (const job of [...st.queued, ...st.running, ...st.recent]) console.log(job.repo, job.job, job.state, job.result, job.vmId, job.processId);
}
```

- `getCIStatus({ limit? })` resolves a `CIStatus`: `enabled` (false when
  `ci.github` is off; the lists are then empty), `maxConcurrentJobs`,
  `queued` (FIFO order), `running` (starting, running, stopping) and `recent`
  (done and refused, newest first, at most `limit`: 1..200, server default 20).
- Each `CIJob` names the GitHub job (`repo`, `jobId`, `runId`, `job`,
  `workflow`, `labels`) and its runner box (`shape`, `runner`, `runnerId`,
  `vmId`, `processId`: the runner's output is in the VM's process history).
  `state` and `result` (`succeeded`, `failed`, `cancelled`, `timedOut`,
  `refused`) are open unions; `conclusion` is GitHub's own result once known,
  `error` why a job failed or was refused, and `assignedJobId` the job GitHub
  gave the runner when it is not `jobId`. Timestamps: `queuedAt`,
  `boxReadyAt`, `startedAt`, `completedAt`, `endedAt` (the box is gone).

## Events and webhooks

The host event stream (`vm.created`, `vm.state`, `operation.*`, `run.*`,
`prompt.*`; tolerate unknown types) is read with a cursor; `data` is an
allowlisted summary (IDs, states, error codes), never env values, prompt
text or secrets. Webhooks receive the same events as signed POSTs; managing
them is owner-only.

```ts
for await (const ev of client.iterEvents({ types: ["vm.", "operation.state"], start: "latest", follow: true })) {
  console.log(ev.seq, ev.type, ev.subject, ev.data);  // runs until you break (or abort `signal`)
}

const hook = await client.createWebhook("https://ci.example/davit", { events: ["vm."] });
hook.secret;  // shown only once: give it to the receiver, never log it
for await (const d of client.iterWebhookDeliveries(hook, { state: "dead" })) await client.retryWebhookDelivery(hook, d);
const next = await client.rotateWebhookSecret(hook, { overlapSeconds: 3600 });  // old secret signs for 1 h more
```

- `listEvents({ limit, cursor, types, vmId, start: "oldest" | "latest", waitMs })`
  returns one `EventList`; `nextCursor` is always set. `iterEvents(...)`
  follows it and ends at the first empty page; with `follow: true` it keeps
  long-polling (`waitMs`, default 25000) forever.
- `createWebhook(url, { events?, description?, enabled?, idempotencyKey? })`
  and `rotateWebhookSecret(webhook, { overlapSeconds?, idempotencyKey? })`
  resolve a `WebhookSecret` and always send an `Idempotency-Key`.
  `getWebhook`, `updateWebhook(webhook, { url?, events?, description?, enabled? })`
  (PATCH; only given fields change, no key), `deleteWebhook`,
  `listWebhooks`/`iterWebhooks`, `listWebhookDeliveries(webhook, { state? })`/
  `iterWebhookDeliveries` and `retryWebhookDelivery(webhook, delivery)` take
  IDs or the returned objects.
- Receivers verify each delivery with the raw body:

  ```ts
  import { WebhookVerificationError, verifyWebhook } from "@davit/sdk";

  try {
    verifyWebhook(rawBody, request.headers.get("x-davit-signature"), [newSecret, oldSecret]);  // fetch-style Request
  } catch (err) {
    if (err instanceof WebhookVerificationError) { /* reject; err.reason: malformed | timestamp | signature */ }
    throw err;
  }
  ```

  The header is `t=<unix seconds>,v1=<hex>[,v1=...]`, each `v1` the
  HMAC-SHA256 of `"<t>." + body` under one live secret (several during a
  rotation overlap); any match of any given secret passes. Timestamps more
  than `toleranceSeconds` (default 300) old or ahead are rejected; the
  comparison is constant-time.

## Members and shares

Members are other tailnet users with rights only on what the owner shares.
A member is added by login and pinned to its stable tailnet user ID on its
first connection; from then on it is that user whatever its login becomes,
and a recycled login is refused. All of these calls are owner-only (local
socket or the owner's tailnet identity; tokens and members get
`PermissionDenied`); without the tailnet gateway they throw
`UnsupportedCapability` (capabilities feature `members`).

```ts
const alice = await client.addMember("alice@example.com", "viewer");  // default role
await client.shareVM("web", alice, "exec");      // or the ID, the login, or "*"
await client.shareSnapshot("base", "*");         // every member, each at its own role
for (const s of (await client.listShares("vm", "web")).items) console.log(s.member, s.role, s.credentials);
await client.unshare("vm", "web", "alice@example.com");  // NotFound if there is no such share
await client.removeMember(alice);                // with its shares
```

- Roles nest as `viewer` < `exec` < `admin` (as token actions: view, then
  exec code, files, tunnels and SSH, then manage the VM). A share's `role`
  defaults to the member's own role.
- A member sees only what is shared with it: any other VM, snapshot or
  environment is `NotFound` (404), as if it did not exist.
- An `exec` or `admin` share without `credentials: true` pauses the VM's
  credential grants for everyone while it exists (processes in one guest
  cannot be told apart).
- `createShare(kind, resource, member, role?, opts?)` is an upsert: sharing
  the same resource with the same member again replaces the role and
  `credentials`. `role` is a role or `{ role?, credentials? }`. `shareVM`,
  `shareSnapshot` and `shareEnvironment` fix the kind.
- `updateMember(member, role)`, `deleteShare(share)`, `unshare(kind,
  resource, member)` and `removeMember(member)` cut the member's live
  sessions (API streams, SSH, app endpoint connections, its reverse
  forwards). `unshare` looks the share up with `listShares(kind, resource)`
  (resolving a login through `listMembers`) and deletes it.
- Members, shares, VMs, snapshots and environments are taken by ID (logins
  and names where the API accepts them) or the returned object; path
  segments are escaped (`alice%40example.com`). No `Idempotency-Key` is sent.

## Credentials

Credential profiles hold API keys on the host; a VM gets a per-VM placeholder
instead of the key, and the host credential proxy swaps it for the secret on
requests to the credential's upstream. Profile calls are owner-only; grant
calls follow the VM's token scope (`view` to list, `admin` to create/revoke).

```ts
const prof = await client.createCredentialProfile("work", { default: true });
await client.putCredential(prof, "anthropic", {
  kind: "api_key", provider: "anthropic", envVar: "ANTHROPIC_API_KEY", secret: process.env.ANTHROPIC_API_KEY!,
});
const grant = await client.createCredentialGrant("web", "work", "anthropic");
// In the guest: ANTHROPIC_API_KEY=<grant.placeholder>, base URL http://172.16.0.1:15080
await client.revokeCredentialGrant("web", grant);  // the placeholder stops working at once
```

- The `secret` is write-only: sent once in the PUT body, stored on the host
  only, never returned (`Credential.hasSecret` tells whether one is set) and
  never logged by the SDK. An omitted (empty) `secret` keeps the stored one; a
  new credential needs one.
- `putCredential(profile, name, { kind, envVar, secret?, provider?, baseUrl? })`
  creates or replaces a credential and resolves the `CredentialProfile`.
  `api_key` credentials of providers `anthropic` and `openai` default their
  upstream to the provider's API; `custom_api` needs `baseUrl`. Existing
  grants use a new secret from their next request.
- `createCredentialProfile(name, { default?, idempotencyKey? })` and
  `createCredentialGrant(vm, profile, credential, { idempotencyKey? })` always
  send an `Idempotency-Key`; the rest send none.
- `listCredentialProfiles({ limit, cursor })` returns one page,
  `iterCredentialProfiles()` follows `nextCursor`; `getCredentialProfile`,
  `deleteCredentialProfile` (revokes its grants, `profile_deleted`) and
  `deleteCredential(profile, name)` (`credential_removed`) take an ID, a name
  or the profile.
- `listCredentialGrants(vm)` returns the VM's grants, active and revoked,
  oldest first; `revokeCredentialGrant(vm, grantOrId)` resolves the revoked
  grant. Stop keeps a grant dormant, resume reuses it, a fork child inherits
  none, and deleting the VM revokes it (`vm_deleted`).

## Prompts

Run coding-agent turns (Claude Code, Codex, OpenCode, pi) inside a VM. The
harness must be installed in the image; credentials are passed explicitly (`env`
values or named daemon `secrets`) and never stored. A harness whose
`getCapabilities().harnesses[].experimental` is true follows its CLI's documentation
but has not yet run against a real model (OpenCode, pi).

```ts
// One turn, start to finish. A failed or cancelled turn is returned, not thrown.
const { prompt: p, text } = await client.runPrompt("web", "add a test for parser.ts", {
  onEvent: (e) => { if (e.type === "message.delta") process.stdout.write(e.text ?? ""); },
});
text;             // the assistant's reply (concatenated message.delta texts)
p.state;          // "succeeded" | "failed" | "cancelled"; p.error?.code e.g. "harness_missing"
p.usage;          // { inputTokens, outputTokens, ... }

// Continue the conversation (the harness's native session is resumed).
await client.runPrompt("web", "now run it", { conversationId: p.conversationId, env: { ANTHROPIC_API_KEY: key } });

// Or drive it yourself.
const turn = await client.createPrompt("web", "refactor", { harness: "codex", new: true });
for await (const e of client.followPrompt("web", turn.id, { timeoutMs: 60_000 })) console.log(e.seq, e.type);
await client.cancelPrompt("web", turn.id);     // SIGTERM to the harness; the turn ends "cancelled"

// Attachments and harness controls (see capabilities.harnesses[]).
await client.runPrompt("web", "what is wrong here?", {
  attachments: [{ name: "shot.png", mediaType: "image/png", data: png.toString("base64") }],
  reasoningEffort: "high",
  fast: true,
});

// Steer a running turn, interrupt, and follow every turn of the VM.
const { delivery } = await client.steerPrompt("web", turn.id, "also update the docs");  // "native" | "restart"
await client.interruptPrompts("web", { all: true });  // or { conversationId }; returns { cancelled }
for await (const item of client.followVMPromptEvents("web", { after: savedCursor })) savedCursor = item.cursor;

for await (const conv of client.iterConversations("web")) console.log(conv.id, conv.turns);  // oldest first
const { items, nextCursor } = await client.listConversations("web", { limit: 20 });       // one page
await client.getConversation("web", items[0]!.id);
```

- One turn at a time per conversation: another `createPrompt` meanwhile
  throws `OperationInProgress` (retryable).
- `followPrompt` yields events in `seq` order (`conversation.started`,
  `message.delta`, `tool.call`, `tool.result`, `turn.completed`, `raw`, and
  unknown future types) until the turn is over; `waitPrompt` returns the final
  `Prompt`. Their budget (`timeoutMs`, default 10 minutes) throws
  `WaitTimeout` with `details: {promptId, cursor}`; resume with
  `followPrompt(vm, promptId, { after: cursor })`. A client timeout never
  cancels the turn.
- Event fields `input` and `raw` are arbitrary JSON (`unknown`).
- `createPrompt` options `attachments` (`{name, mediaType, path}` for a guest
  file or `{name, mediaType, data}` with inline base64; exactly one of
  `path`/`data`), `reasoningEffort` and `fast` (sent as the wire `options`),
  and `turnTimeoutSeconds` (default: the daemon's `harness.defaultTurnTimeout`, 1h; at most 86400; the prompt's
  `turnDeadlineAt`; the turn then fails with `turn_timeout`).
- `steerPrompt` returns `{delivery, prompt}`: `native` (the same turn took the
  message) or `restart` (the turn was cancelled; `prompt` is the new turn of
  the same conversation). A finished turn throws `InvalidState`, one still
  starting `OperationInProgress` (retryable). `interruptPrompts` takes
  exactly one of `conversationId` and `all: true` and returns the cancelled
  turns; a repeat returns an empty list. `steerPrompt` sends an
  `Idempotency-Key` (`idempotencyKey`, or generated once per call), so the
  message is delivered once; `interruptPrompts` sends none.
- `listVMPromptEvents` returns the VM-wide feed (`{items: [{cursor, event}],
  nextCursor}`) across turns; `after` is an opaque cursor (omitted replays
  from the oldest) and `nextCursor` is set on every page, so pass it back to
  continue, also after reconnecting. `followVMPromptEvents` long-polls it
  without end (break or abort `signal`); resume with the last item's `cursor`.
- `createPrompt` (and `runPrompt`) always sends an `Idempotency-Key`: pass
  `idempotencyKey`, or a UUIDv4 is generated once per call and reused on
  retries. The same key and body return the existing turn (no second run);
  a different body throws `IdempotencyConflict`.
- Conversations page like every list (`limit`, `cursor`, `nextCursor`) in
  `(createdAt, id)` order, oldest first; `iterConversations` follows the
  cursor for you.
- Prompts need a guest transport: on a runtime without one the calls throw
  `UnsupportedCapability`.

## Errors

Every error is a `DavitError` with `status` (HTTP status, or `null` for
client-side errors), `code`, `message`, `retryable`, `details`, `requestId` and
`operationId`. Server codes map to subclasses: `InvalidArgument`,
`Unauthenticated`, `PermissionDenied`, `NotFound`, `Conflict` (parent of
`OperationInProgress`, `IdempotencyConflict`, `InvalidState`, `StopRequired`),
`RevisionMismatch`, `PayloadTooLarge`, `UnsupportedCapability`,
`ResourceExhausted`, `Unavailable`, `ServerNotImplemented`, `Timeout`,
`Internal`. Unknown codes from newer servers stay a plain `DavitError`.
Client-side: `TransportError` (daemon unreachable), `WaitTimeout`, and
`OperationFailed` (`err.operation`, and `err.error`: the typed error built from
`operation.error`).

```ts
import { StopRequired, NotFound } from "@davit/sdk";

try {
  await client.forkVM("web");
} catch (err) {
  if (err instanceof StopRequired) await client.stopVM("web");
  else throw err;
}
```

## Types

Wire types (`VM`, `Operation`, `Snapshot`, `HostCapabilities`, ...) are
generated from `api/openapi.yaml` into `src/schema.generated.ts`. Field names
are the camelCase JSON wire names. Timestamps stay ISO 8601 / RFC 3339
strings (`new Date(vm.createdAt)`). Enum unions are open (`VMState` includes
`string & {}`) because newer servers may add values; unknown fields are
ignored. The SDK has no runtime model classes.

## Development

```sh
npm install          # dev dependencies only (typescript, @types/node, yaml)
npm run gen          # regenerate src/schema.generated.ts from api/openapi.yaml
npm run typecheck    # tsc --noEmit over src, scripts, tests and examples
npm test             # node --test: unit, contract and real-daemon tests
npm run test:bun     # the same suite under Bun
npm run build        # emit dist/ (.js + .d.ts)
```

Node 22.18+ runs the TypeScript sources directly (erasable syntax only); the
built `dist/` runs on Node 20.

The contract test fails when `api/openapi.yaml` gains an operation the SDK
neither wraps (`OPERATIONS`) nor lists in `NOT_YET_SUPPORTED`, when the
generated types are stale (run `npm run gen`), or when an error code has no
class. The integration test starts `bin/davit daemon` with the fake runtime
in a temporary directory (`DAVIT_BIN` overrides the path) and is skipped if
the binary is missing (`make build`). `DAVIT_TEST_DAEMON_LOG=1` prints each test daemon's debug log.
`test/remote.test.ts` makes a throwaway CA and server certificate with `openssl`
(skipped when it is not installed), checks that HTTPS and WebSocket connections
succeed with `caFile`/`DAVIT_CA_FILE` and fail without it, and runs
`examples/remote-https.ts` against the fake daemon behind a local TLS proxy.
