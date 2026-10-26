# Davit product specification

Status: v0 contract, 2026-10-22. “davit” is a working name (binary `davit`, env `DAVIT_*`); renaming it touches
one constant in `internal/buildinfo` plus the module path.
Scope: open-source, self-hosted agent VMs on one user's Linux server or Apple-silicon Mac.
Normative words MUST/SHOULD describe proposed requirements, not existing implementation.

## 1. Product contract and evidence

Davit creates persistent Ubuntu machines, reaches them over the user's tailnet, and cheaply forks saved state.
The runtime is replaceable; CLI, API, durability and security semantics remain stable across engines.
License: MIT (LICENSE), with separately audited runtime/image dependencies.
V0 is Linux-first; Mac, SDKs, harness adapters and streaming follow behind explicit capabilities.
No cloud account with Davit, hosted control plane, billing or public ingress is required.
Tailscale coordination is external by default; configurable control URL permits later tested Headscale support.

Input: the [Firecracker spike](./SPIKE.md).
The spike measured FC restore→SSH banner 44–73ms; ten forks completing SSH commands in 305–480ms; 13–17.5MiB private idle memory.
Those results used 384MiB guests, btrfs and a busy, shared development machine; they exclude tailnet enrollment and production identity repair.
SSH banner, full SSH command, guest-agent readiness and application readiness are separate measurements.
The spike's RAM snapshot creation is **not** the disk-only `stop` specified here.
Its suggested 1–2s tailnet join allowance was analysis, not a measured tailnet result.

V0 release goals on declared reference hardware, with cached compatible images and persistent gateway:

- Fresh golden `new`: first authenticated command <1s p50, <2s p95, including identity repair.
- Ten RAM-checkpoint forks: <1s p95 to all first commands, only if the optional capability ships.
- Disk fork/resume: report separately; correct cold-boot fallback is allowed and disclosed.
- No promise of 15MiB per 4–8GiB developer VM; measure PSS, private dirty and working-set growth.
- Missing image downloads, full-copy storage, endpoint enrollment, Docker and desktop readiness have separate timings.

## 2. Domain model and lifecycle

- **Image:** immutable architecture-specific kernel/rootfs + build manifest; identified by digest.
- **Golden:** trusted booted RAM/device checkpoint and its exact disk generation, shape and compatibility fingerprint.
- **VM:** durable owner/id/name/config plus private writable disk and monotonically increasing instance generation.
- **Disk snapshot:** immutable committed disk generation; contains no resumable processes or RAM.
- **RAM checkpoint:** matching RAM/device state and immutable disk generation; runtime/CPU/device dependent.
- **Operation:** durable asynchronous intent, progress and terminal result; survives client disconnect/daemon restart.
- **Endpoint:** host gateway or optional host-resident per-VM tailnet node; independent of guest lifecycle.
- **Ready:** identity barrier passed and authenticated guest-agent command succeeded; SSH probe also required for SSH-enabled images.
- **Running:** same readiness as ready, with at least one Davit-managed exec/process active; not a CPU-usage heuristic.

State transitions:

```text
new                  -> provisioning -> ready <-> running
ready | running      -> stopping -> stopped
stopped              -> provisioning -> ready          # disk resume
ready | running      -> error                          # unexpected exit/unrecoverable health loss
provisioning|stopping -> error                          # retain last committed disk, expose cause
error                -> provisioning                   # resume from last committed disk, explicit request
ready|running|error -> provisioning -> ready        # recover (G9): power cycle on the current disk, no commit
any nondeleted state -> deletion operation -> tombstone # deleted is terminal, never resume
later: ready|running -> suspending -> suspended -> provisioning -> ready
```
`ready`/`running` do not imply Docker or any user service is healthy; `readiness.services` reports them separately.
VM carries `stateRevision`, `instanceGeneration`, `activeOperationId`, `lastError`, `lastSnapshotId` and actual `bootMode`.
One mutating lifecycle operation per VM; conflicting requests return 409 `operation_in_progress` with operation ID.
Host allocation is bounded by configured CPU/RAM/disk limits; reservation is transactional before spawn.
Later suspend states are advertised only when the runtime supports them; clients MUST tolerate new enum values.
Deletion immediately closes new access; async cleanup completes before operation success; bounded tombstone retained.

## 3. CLI contract

`davit` contains CLI, `daemon`, guest-agent subcommand and image tooling entrypoints in one host executable.
Host artifacts are OS/architecture-specific; Linux guest agent is a separate build artifact from the same source.
Commands use the local Unix socket by default; `--host <FQDN|URL>` selects a remote host.
Global flags: `--host`, `--api-url`, `--json`, `--no-update`, `--timeout <duration>`, `--help`, `--version`.
Global timeout is the client's operation wait budget; exec's timeout is the remote process deadline.
Where flags collide, command-position `exec --timeout` means process timeout; use `--wait-timeout` for lifecycle waits.
No automatic upgrades during commands; `--no-update` suppresses update notices only. Explicit `self-update` is later.

| Command | V0 arguments / flags | Behavior |
| --- | --- | --- |
| `init` | `--hostname`, `--storage`, `--runtime`, `--auth-key-stdin`, `--control-url`, `--non-interactive`, `--config`, `--data-dir`, `--force`, `--service` | Preflight, write config/dirs (idempotent; `--force` backs up), store auth key 0600, optional systemd user unit; tailnet join happens at daemon start |
| `doctor` | `--config` | Same preflight checks the daemon runs at startup, plus config and daemon reachability; pass/warn/fail with one-line fixes |
| `daemon` | `--config <file>` | Run API, reconciliation and gateways |
| `new [NAME]` | `--image <ref>`, `--from <snapshot>`, `--type <profile>`, `--cpus`, `--memory`, `--disk`, `-e/--env KEY=VALUE`, `--ttl <duration>`, `--no-auto-stop`, `--idempotency-key`, `--no-wait`, `--wait-timeout` | Create VM; default golden path when compatible |
| `list` | `--all`, `--filter <states>`, `--limit`, `--cursor` | Default provisioning/ready/running; filters use full state names |
| `info [ID]` | `--watch [--until STATE]` | State, operations, image, capabilities, endpoints, storage and readiness; `--watch` prints each change |
| `ssh [ID] [-- COMMAND...]` | `--user <guest-user>` | Local OpenSSH over the tunnel API with a short-lived per-VM-generation certificate and pinned host keys; default image user `davit` with guest sudo. `ssh-config` writes matching OpenSSH config |
| `scp SRC DST` | `-r/--recursive`, `--preserve` | One remote side `ID:/path`; SSH/SFTP transport, no shell-interpolated paths |
| `cp SRC DST` | `-r`, `--user` | One remote side `ID:/path`; the guest agent's file API (no SSH needed); directories as verified tar |
| `exec [ID] -- COMMAND...` | `--cwd`, `-e/--env`, `--timeout <seconds>`, `--stream`, `--detach`, `--stdin`, `--tty`, `--idempotency-key` | Preserve argv; shell only via explicit `sh -lc ...` |
| `exec ID --status PID` | `--attach PID [--after SEQ]`, `--kill PID` are mutually exclusive alternatives | Status, reconnectable output, process-group termination |
| `stop [ID]` | `--force`, `--no-wait`, `--wait-timeout`, `--idempotency-key` | Disk snapshot and stop; processes end |
| `resume [ID]` | `--ttl`, `--no-auto-stop`, `--no-wait`, `--wait-timeout`, `--idempotency-key` | Boot last committed disk; later RAM resume only from suspended |
| `fork ID [NAME]` | `--snapshot <id>`, `--type`, `--cpus`, `--memory`, `--ttl`, `--no-auto-stop`, `--no-wait`, `--wait-timeout`, `--idempotency-key` | Independent VM from committed disk snapshot; new owner-authorized identity |
| `snapshot create [ID] NAME` | `--idempotency-key` | Name committed state of a stopped VM (no data copy); running source returns 409 `stop_required` in v0 |
| `snapshots [ID]` | `--limit`, `--cursor` | List immutable disk snapshots |
| `snapshot delete SNAPSHOT` | `--yes` | Synchronous; remove the reference, GC only unreferenced data; in-use snapshots return 409 `invalid_state` |
| `delete ID` | `--yes`, `--no-wait`, `--wait-timeout`, `--idempotency-key` | Delete VM and its unreferenced data; explicitly saved snapshots survive |
| `extend [ID] [DURATION]` | — | Restart the auto-stop countdown from now; a DURATION also becomes the VM's TTL (`PATCH /vms/{id}`) |
| `rename ID NEW-NAME` | — | Rename a VM (`PATCH /vms/{id}`); names are labels, never identities |
| `forward ID` | `--remote PORT`, `--local PORT`, `--bind ADDR`, `--reverse` | Foreground TCP tunnel; loopback bind default; reconnect only within same generation |
| `image list/pull/inspect/build` | digest/ref; build `--file <manifest>` | M2 user image tooling; bundled verified default image works in M1 |
| `capabilities` / `operations ID` | `--json` / `--watch` | Host capabilities or durable operation result |
| `context use ID` / `show` / `clear` | VM ID scoped to host | Saved context for interactive terminals; `show` reports which source applies |
| `prompt [ID] TEXT` | `--harness claude\|codex` (`--provider`), `--new`, `--resume CONV`, `--model`, `--cwd`, `--env NAME`, `--secret NAME`, `--no-wait`, `--idempotency-key` | One turn of a coding-agent harness inside the VM (shipped early, ahead of M4); later: `--reasoning-effort`, `--attach` |
| `conversations [ID]` | `--limit`, `--cursor` | The VM's harness conversations, `(createdAt, id)` order |
| `desktop` | later: `--vnc`; negotiated stream mode | M4 optional desktop image and authenticated streaming |
| `suspend` | later: same lifecycle wait/idempotency flags | RAM+disk checkpoint, capability gated |

Profiles are administrator-defined shapes (config `profiles`, listed in capabilities); `--type` picks one and `--cpus/--memory/--disk`
override its fields; byte quantities accept explicit MiB/GiB, APIs use integer bytes.
`--disk` cannot shrink a disk; shape overrides incompatible with RAM templates select cold boot or fail explicit `--require-golden`.
Add `--require-golden` to `new`; never silently claim a cold path used RAM restore.
Default TTL is 1h from successful start/resume, not idle time; `--no-auto-stop` disables it.
Active exec/SSH traffic does not silently renew TTL; `extend` does. Inside the warning window (config `ttlWarning`, default 10m;
`VM.autoStopWarningAt`) the host broadcasts to the guest's terminals (`wall`) and the CLI prints a stderr notice; the stop then
follows normal durability rules.
`fork` from running VM requires an existing committed snapshot; response states its ID/time and excludes later writes.
`new --from` and `fork` share snapshot restore semantics; they do not automatically inject source host-managed secrets or inherit runtime sockets/tailnet credentials.
Fork copies disk content, which may contain user credentials: v0 permits it only within the same owner boundary.
Live snapshots, `fork --live`, batched `new --count`, setup scripts, environment profiles and public `host` URLs are later.
No v0 raw engine-specific flags; backend selection is administrator policy, with actual runtime visible in `info`.

### JSONL, errors, exit status and context

Every `--json` stdout line is one JSON object; stdout contains no banners, progress bars or human diagnostics.
One-result reads emit one `result`; lists emit `item` records then `result` with `nextCursor`.
Lifecycle waits emit `operation`, ordered `progress`, then exactly one `result` or `error`.
Exec streaming emits `process.started`, `stdout`/`stderr`, then `process.exited`; chunks carry sequence and text or base64 encoding.
Envelope: `{schemaVersion:1,event,time,requestId,operationId?,seq?,data?,error?}`.
`seq` is monotonic within the operation/process stream; reconnect with `after`, deduplicate by stream ID+sequence.
JSON parse/argument errors also emit a terminal JSON `error`; diagnostic logging stays on stderr.
A stream gap/expired retention returns an explicit error; no claim of exactly-once network delivery.
`--json` is explicit; v0 does not change output format automatically when piped.
`ssh`/`scp`/TTY modes reject `--json` before connecting: use `exec --stream --json` for structured process output.

CLI-owned errors: 2 invalid usage; 3 authentication; 4 authorization; 5 not found; 6 conflict/unsupported capability;
7 transport/client wait timeout; 8 resource exhaustion; 1 internal/general failure. Success is 0.
`exec`/`ssh` return the guest exit status when known (0–255); 124 denotes Davit process deadline, 130 interrupt,
255 denotes lost transport with unknown result. JSON identifies error origin; exit numbers alone may overlap guest codes.
Client wait timeout never cancels an accepted lifecycle operation; print its ID and how to query it.
Ctrl-C stops following lifecycle progress; foreground exec sends SIGINT then configured escalation; detached exec persists.

`current` is explicit-ID override, then `DAVIT_CURRENT_ID`, then saved per-host/per-user context for interactive terminals.
Noninteractive commands without explicit ID/env fail; scripts never depend on another shell's mutable context.
Successful waited `new`/`fork` select context only in interactive use; `resume` does not change it.
Saved context lives in the user config dir (`<config>/<name>/context.json`, 0600), keyed by API URL/host; it stores the VM ID.
`context use` verifies the VM exists; deleting the context VM through the CLI clears it.
`snapshot create` was `snapshot ID NAME`: a subcommand form avoids ambiguity with `snapshot delete` for a VM named `delete`.
VM and snapshot arguments accept an ID or the caller's name; deleted VMs resolve by ID only.
No shell-startup mutation is required; optional completion/wrapper integrations can export shell-local context.
`self` is authenticated guest metadata identity; daemon verifies its credential against VM ID/generation.
A guest's bootstrap token permits agent operations only, not owner lifecycle control; no privileged host API key in the image.

## 4. HTTP API v1 outline

Base `/api/v1`; JSON request/response except defined byte/WebSocket streams. UTC RFC3339 timestamps, opaque IDs.
No cloud billing/org objects; owner is an authorization principal, not an upstream harness account.
Below is an OpenAPI-style design outline; M0 supplies validated `api/openapi.yaml` and generated types.

```yaml
openapi: 3.1.0
info: {title: Davit API, version: 1.0.0}
servers: [{url: 'https://{host}/api/v1', variables: {host: {default: davit-host}}}]
security: [{bearerToken: []}, {}]
x-davit-tailnet-required: true
# Empty HTTP security alternative means direct WhoIs-authenticated transport, not anonymous access.
components:
  securitySchemes:
    bearerToken: {type: http, scheme: bearer}
# Responses/schema references are abbreviated below; this is an outline, not a complete OpenAPI document.
paths:
  /capabilities: {get: {operationId: getCapabilities}}
  /images: {get: {operationId: listImages}}
  /images/{imageId}: {get: {operationId: getImage}}
  /images/pull: {post: {operationId: pullImage}} # M2 asynchronous operation
  /images/build: {post: {operationId: buildImage}} # M2 asynchronous operation
  /vms: {get: {operationId: listVMs}, post: {operationId: createVM}}
  /vms/{vmId}: {get: {operationId: getVM}, patch: {operationId: updateVM}, delete: {operationId: deleteVM}}
  /vms/{vmId}/stop: {post: {operationId: stopVM}}
  /vms/{vmId}/resume: {post: {operationId: resumeVM}}
  /vms/{vmId}/fork: {post: {operationId: forkVM}}
  /vms/{vmId}/snapshots: {get: {operationId: listVMSnapshots}, post: {operationId: snapshotVM}}
  /snapshots: {get: {operationId: listSnapshots}}
  /snapshots/{snapshotId}: {get: {operationId: getSnapshot}, delete: {operationId: deleteSnapshot}}
  /operations/{operationId}: {get: {operationId: getOperation}}
  /operations/{operationId}/events: {get: {operationId: listOperationEvents}}
  /vms/{vmId}/processes: {get: {operationId: listProcesses}, post: {operationId: execProcess}}
  /vms/{vmId}/processes/{processId}: {get: {operationId: getProcess}, delete: {operationId: killProcess}}
  /vms/{vmId}/processes/{processId}/stream: {get: {operationId: attachProcess}}
  /vms/{vmId}/files: {get: {operationId: readFile}, put: {operationId: writeFile}}
  /vms/{vmId}/tunnels: {post: {operationId: openTunnel}}
  /tunnels/{tunnelId}: {delete: {operationId: closeTunnel}}
  /tunnels/{tunnelId}/stream: {get: {operationId: attachTunnel}}
  /vms/{vmId}/ssh-credentials: {post: {operationId: issueSSHCredential}}
  /vms/{vmId}/prompts: {post: {operationId: createPrompt}}
  /vms/{vmId}/prompts/{promptId}: {get: {operationId: getPrompt}, delete: {operationId: cancelPrompt}}
  /vms/{vmId}/prompts/{promptId}/events: {get: {operationId: listPromptEvents}}
  /vms/{vmId}/conversations: {get: {operationId: listConversations}}
  /vms/{vmId}/conversations/{conversationId}: {get: {operationId: getConversation}}
  /tokens: {get: {operationId: listTokens}, post: {operationId: createToken}}
  /tokens/{tokenId}: {delete: {operationId: revokeToken}}
```

Schema contracts:

- `CreateVM`: name?, image digest/ref?, snapshotId?, shape?, env?, ttlSeconds?, autoStop?, requireGolden?; image and snapshot are exclusive.
- `ForkVM`: snapshotId?, name?, shape?, ttlSeconds?, autoStop?; source owner policy enforced.
- `VM`: ID/owner/name/state/revision/generation; shape/image; bootMode; timestamps; TTL; operation; lastSnapshotId/sourceSnapshotId;
  lastError; readiness `{agent,ssh,services}`; endpoint `{mode,state,gatewayFQDN?,vmFQDN?,sshTarget?}`; storage `{mode,fastClone}`.
  No per-VM capability set: one runtime serves a host, so `GET /capabilities` plus the VM's actual bootMode/storage/readiness
  answer "what can this VM do"; a per-VM set returns only if hosts gain mixed runtimes.

- `Operation`: ID/kind/VM/state `{queued,running,succeeded,failed}`/phase/progress/result/error; state transitions atomically journaled.
- `Snapshot`: ID/name?/owner/sourceVmId/generation/image/arch/kind/consistency/sizeBytes/digest/createdAt; kind `disk` or later `ram`.
  The storage manifest stays host-internal; `digest` is `sha256:` of that manifest, `generation` the committed instance generation,
  `sizeBytes` the logical disk size (forks cannot shrink below it), `createdAt` the commit time. Several snapshots may name one commit.
  Unnamed snapshots are automatic stop commits: kept while they are their VM's last committed disk or something depends on them
  (a fork's only resume source, an unfinished operation), pruned when superseded, collected with the VM. Named snapshots survive VM deletion.
- `ExecRequest`: argv array, cwd, env, deadlineSeconds?, detached, stdin, tty; never infer shell from argument count.
- `Process`: opaque ID/generation/status/exitCode?/signal?/outputCursor; persisted metadata and bounded output retention.
- `Error`: `{code,message,retryable,details,requestId,operationId?}`; secrets and host paths redacted.
- `Capability`: supported architectures, boot modes, RAM checkpoint formats (`runtime.ramCheckpointFormats`), shapes, storage clone modes,
  networking and services. Lists are always arrays, never null.

Lifecycle mutation returns 202 `{operation,vm?}` with Location `/operations/{id}`; CLI waits by default.
v0 `snapshotVM` copies no data, so its 202 operation is already `succeeded`; `deleteSnapshot` is synchronous 204 (repeat: 404).
Read returns 200; file PUT is atomic guest-side replace on success; no automatic archive extraction.
File API requires VM generation, absolute guest path and explicit guest-user scope; block host traversal and symlink races.
Process/tunnel creation returns resource ID; streaming attaches separately with authenticated bounded WebSocket frames.
No replay of arbitrary stdin after reconnect; output can replay from cursor. Lost process outcome becomes `unknown`/`lost`.

`Idempotency-Key` is required for create/fork/snapshot, process creation and prompt creation, and supported on stop/resume/delete.
`deleteSnapshot`, `killProcess`, `writeFile` and `cancelPrompt` take no key: they are naturally idempotent (a repeat has no second
effect and returns the current state or an explicit 404/409). A daemon-wide test replays every mutating endpoint and fails when a
new mutation lacks either a key or a natural-idempotency case.
CLI generates a UUID before submission and reuses it on transport retry; users supply one to deduplicate separate invocations.
Scope is host + authorization owner/principal + method + canonical path + key, where the canonical path names the VM by ID (a name
is resolved first; the raw path is recorded too, so a replay by name still finds the operation after the VM is deleted); hash the
normalized body, excluding no semantic fields. Process and prompt creation scope keys by (owner, VM ID, key).
Same key/body returns the same operation/resource; different body returns 409 `idempotency_conflict`.
Persist reservation and operation in the same SQLite transaction before external effects; retain keys for at least 24h after terminal state.
Always reauthorize a replay. A matching key cannot bypass revoked permissions; explicit reruns need a new key.
Stop on stopped and resume on ready are safe no-ops; repeating exec with a new key executes again.
`If-Match: <stateRevision>` supports explicit preconditions; stale revision gives 412. No force override of another operation.

Lists use `limit` (default 50, max 200), opaque cursor, stable `(createdAt,id)` ordering and owner-scoped filtering.
Cursor binds filters/order/authorization scope; result `{items,nextCursor}`. Concurrent inserts need not appear mid-pagination.
Events use `(streamId,seq)` cursor with documented retention; no timestamp-only deduplication.
HTTP errors: 400 malformed; 401 unauthenticated; 403 denied; 404 absent/invisible; 409 state/idempotency conflict;
412 revision mismatch; 413 oversized; 422 unsupported shape/architecture/capability; 429 quota; 500 internal (message redacted);
501 operation specified but not implemented in this build; 503 host/runtime unavailable; 504 host-side deadline exceeded.
Error codes, not English messages, are stable; 429/503 carry `Retry-After: 1` as a minimum hint (clients add their own backoff).

### Authentication and authorization

Unix socket is owner/admin restricted; local peers are authenticated by OS credentials.
Remote listeners bind to tsnet only; TLS where available, never an accidental public-interface fallback.
Resolve identity using tsnet LocalClient.WhoIs on the **actual accepted peer address**; failure is closed.
Never trust user-supplied identity headers, forwarded IP headers, VM names, SSH usernames or source IP alone.
Map stable tailnet user/node IDs and explicitly allowed tagged-node principals; tagged workloads are not human users.
Tailnet ACL/grants govern reachability; Davit owner/admin/viewer/exec permissions govern VM actions.
Bootstrap init pins the initiating authorized owner; tailnet membership alone grants no VM control.
V0 supports a single configured owner plus explicit service-token grants; multi-owner collaboration is later.

Optional random API tokens are hashed at rest, shown once, scoped by VM/action, expiring and revocable.
Over tsnet, tokens authenticate automation; an accompanying valid WhoIs identity and token grants are intersected, not unioned.
Tokens cannot exceed issuing principal rights; generic shared reverse proxies are unsupported unless explicitly authenticated.
Tokens do not expose a public API listener. Local automation can use the socket instead.
Browser handoffs use short-lived single-use grants bound to principal/action/VM/generation/audience, never query-string tokens.
Validate Origin and CSRF protections for browser mutations; stop/delete/generation changes invalidate session grants.
SSH gateway validates the transport principal and VM access before issuing/accepting a short-lived scoped SSH credential.
V0 has two SSH paths. (1) The tunnel API (`openTunnel`/`attachTunnel`/`closeTunnel`): an authorized WebSocket byte tunnel to the
guest's sshd, used by `davit ssh/scp/forward` with ordinary local OpenSSH/SFTP and `issueSSHCredential`. (2) A terminating
front door on the gateway, `ssh <vm>@<host>`, for zero-config use: it authorizes the tailnet identity, then opens its own
session to guest sshd with a per-VM-generation certificate and the barrier-recorded host key (the gateway, not the client,
verifies the guest host key on this path).
Use a temporary client key + short-lived host-CA certificate scoped to VM/generation/guest user; agent configures allowed principals.
Verify guest host keys against the authenticated API identity record; never disable host-key checking to make clones work.
Host gateway TLS identity and guest SSH host identity are separate; scp uses the same authorized transport.
Do not splice tsnet.ListenSSH session objects into guest sshd: raw SSH tunneling and Tailscale SSH termination are different protocols.
This is Davit SSH over Tailscale; full Tailscale SSH policy integration is optional, not implied by using tsnet.

## 5. Host daemon and adapter boundaries

**Go** is the default implementation language: direct tsnet integration, static CLI/daemon distribution, strong network tooling,
portable concurrency and uncomplicated process supervision. Use a pinned pure-Go SQLite implementation for CGO-free host builds.
“Single static binary” describes Davit; VMMs, jailer and image artifacts remain separately versioned verified dependencies.
Mac's VZ backend may require a signed Swift helper/framework bridge; package it explicitly instead of claiming Go can replace VZ.
No plugin system is necessary in v0: compile adapters against versioned internal interfaces.

SQLite WAL + foreign keys hold VMs, operations, principals, snapshot manifests/references, endpoints and idempotency records.
Bulk disks/RAM live in object storage directories, not SQLite blobs. Use filesystem locks to prevent two daemons sharing a store; durability mode is WAL with synchronous FULL.
No SQLite transaction stays open across VMM/network calls; journal intent, execute, verify, then commit observation.
Reconcile on start: discover supervised process identities/cgroups, compare generations, reattach agents or mark error.
Never identify a process by PID alone; never claim a VM stopped merely because the API process restarted.
Error recovery must prove the prior runtime is gone or explicitly reconcile it before starting another generation.
Retain the private working disk after failed commit for recovery; do not overwrite it with an older snapshot silently.

Runtime adapter semantic interface (not engine API passthrough):

```text
Capabilities() -> arch, coldBoot, goldenRestore, diskFork, ramSnapshot, liveFork,
                  guestTransport, shapeConstraints, fingerprintRules
Prepare(image, shape) -> prepared runtime artifacts
ColdBoot(instance, diskSet, networkHandle) -> RuntimeHandle
RestoreGolden(instance, compatibleGolden, matchingDiskSet, networkHandle) -> RuntimeHandle
Checkpoint(handle, quiescence) -> optional RAM/device checkpoint artifacts
RestoreCheckpoint(instance, exactCheckpointDiskSet, networkHandle) -> RuntimeHandle
Pause/Unpause(handle); Stop(handle, grace); Kill(handle); Inspect(handle)
GuestTransport(handle) -> authenticated-capable dialer, never a raw global guest address
Destroy(handle) -> idempotent cleanup
```
Lifecycle service composes runtime + storage; adapters do not own user policy, SQLite or snapshot names.
`diskFork` means the adapter can boot a private cloned disk; it does not imply live RAM duplication.
Runtime handles include generation and expected process identity; stale handles cannot mutate a replacement VM.
Engine/API-version/CPU/kernel/device/shape fingerprints determine checkpoint compatibility; no cross-engine RAM conversion.
Compare alternatives: shared lifecycle service + narrow adapters is chosen over duplicate per-engine controllers or a generic hypervisor API.
Tests use adapter contract suites; feature negotiation stays concentrated in lifecycle planning.

Storage interface: `Probe`, `CreateWritable`, `CloneCommitted`, `Commit`, `OpenForRuntime`, `Release`, `Collect`.
A commit publishes immutable manifests only after data + directories are durably synced; partial work stays unreferenced.
Snapshot references pin base disks/RAM while any descendant or running mmap depends on them; GC respects active leases.
Use same-filesystem reflink raw ext4 images on btrfs/XFS where supported; actual fast-clone probe required.
ZFS adapter uses dataset/zvol snapshots and clones, tracks origin dependencies, and obeys the same ownership/lease contract.
Full-copy fallback is supported with explicit `storage.mode=copy`, space reservation and visible progress; never label it fast fork.
Write amplification, available physical space and quota are monitored; shallow clones do not reserve unlimited future capacity.
Disk full: ENOSPC/EDQUOT and SQLite "disk is full" map to retryable 429 `resource_exhausted` (CLI exit 8). The journal keeps an
8 MiB incompressible reserve file beside the database and releases it on the first full write, so terminal results and failure
records still commit; the sweeper re-arms it once space returns. Stop and delete are never refused (they reclaim space);
new/fork/resume are refused below `limits.minFreeDiskBytes` (default 1 GiB, negative disables), the same floor `doctor` checks.
Low space never blocks daemon start. A second full event before re-arm can leave an operation unrecorded until restart replay.
Storage is host-owned; guest paths never become unchecked host filesystem paths.

### Guest agent and restore barrier

Use versioned length-framed RPC over vsock; Firecracker/CH hybrid UDS and Mac native vsock are transport implementations.
Handshake negotiates protocol/features and binds a host-created per-generation challenge/secret to this instance.
No reusable secret in golden images; guest cannot impersonate a different VM through self-reported IDs.
RPC covers readiness, exec argv/env/cwd, PTY/signals, files, streams, time/identity setup, shutdown and service probes.
Before admitting SSH, HTTP forwarding or exec after clone/restore:

1. Host establishes a fresh authenticated control session; old vsock/TCP streams are discarded.
2. Set wall clock from trusted host time; reconcile timers and random-seed/VMGenID behavior before TLS/user workloads.
3. Assign logical VM ID + generation + hostname; new VM/fork gets distinct machine-id and SSH host keys.
4. Refresh agent credentials/network identity; restart identity-dependent services or use an early bootstrap barrier.
5. Verify agent command, expected identity and SSH host-key registration; then publish ready.

Do not claim that writing `/etc/machine-id` resets systemd/D-Bus identities already cached in memory.
Golden preparation must gate identity-dependent services or provide a tested restart sequence; template credentials are forbidden.
Linux `/proc/sys/kernel/random/boot_id` is not a writable per-fork identity API; RAM clones may share it.
Expose `instanceGeneration` as the control-plane run identity; workloads requiring a distinct kernel boot ID use cold boot.
Resume of the same disk VM keeps its persistent machine-id/SSH identity; a fork rotates them; each instance rotates control secrets.
System uptime/process timers after optional RAM suspend require runtime-specific testing; wall-clock repair alone is insufficient.

### Images and networking

Image manifest pins OCI source digest, architecture, packages/toolchain, kernel/config digest, rootfs size and guest-agent protocol.
Pipeline: verify/pull OCI layers -> apply OCI whiteouts safely -> isolated unpack/build -> provision Ubuntu/systemd/agent/SSH -> ext4.
Arbitrary OCI ENTRYPOINT is not a host build command; arbitrary OCI images are not assumed valid systemd VM images.
Build-time kernel/rootfs tools run in an isolated builder; never require a user's Docker socket or execute image code on the host.
Reproducible manifests/SBOM/provenance; signed official downloads; verify digests before use; no guest secrets in layers/goldens.
Custom kernel includes virtio block/net/vsock, ext4, namespaces/cgroups, seccomp, overlay, netfilter raw, nftables/required NFT modules,
bridge/veth/conntrack/NAT, VXLAN, IPVS as supported, and FUSE. Ship modules or built-ins consistently with rootfs.
Docker bridge and container networking MUST work without the spike's insecure no-iptables-raw workaround.
M1 kernel carries the required configuration; full OCI/user-image tooling and Docker networking acceptance land in M2.

Linux default: one VM per netns + TAP, identical guest IP inside isolated namespaces, unique host veth uplinks.
Use scoped nftables forwarding/NAT between guest subnet, per-netns uplink and host egress; namespaces prevent clone address collisions.
Provision/cleanup through a narrow privileged helper; privileged code never accepts arbitrary shell commands.
Guest egress uses NAT; default denies host-management addresses, LAN/private/link-local/metadata ranges and other VM subnets.
Explicit admin policy can permit LAN/tailnet services; Docker socket/host mounts are never implicit.
Keep shared gateway/agent reachable over vsock independently of guest egress. Rootless passt/pasta is a later network adapter.
`forward` uses authenticated gateway + guest-agent TCP dial/listen; reverse forwarding requires explicit guest listener policy.
Ordinary forward supports guest localhost services; no public bind without explicit `--bind` and authorization.
Normal forwarding requires `--remote`, defaults local port to remote; reverse requires `--local`, defaults remote port to local.
Both bind loopback at the listening side by default; tunnels have byte limits/backpressure and cannot dial arbitrary host targets.

### Tailnet gateway and per-VM names

Default: one persistent tsnet node, `davit-<host>.<tailnet>.ts.net`, with host API and SSH gateway.
CLI/SSH config maps logical VM names to this gateway; routing IDs are authenticated parameters, never authorization claims.
HTTP forwarding uses `/vms/{id}/...` or allocated gateway ports; arbitrary Host headers do not select an authorized VM.
**One tsnet node cannot create arbitrary per-VM MagicDNS records.** Do not advertise synthetic `<vm>.<host>.ts.net` names.
Optional `endpoint.mode=node`: one host-resident tsnet.Server per VM, separate persistent Dir/identity and actual registered name.
It proxies authorized SSH tunnels/HTTP to that VM over vsock; no tailscaled or node key is needed inside the guest.
Publish actual registered FQDN/IP after enrollment, not requested hostname; handle naming conflicts explicitly.
Any warm slot rebind closes old sessions, rotates grants and detaches prior VM authorization before reuse.
Enrollment is asynchronous; report endpoint_pending/ready/error separately from VM readiness.
Stop retains endpoint identity; ordinary VM auto-stop TTL never deletes that identity. Explicit endpoint retention/delete policy controls cleanup.
Fork gets a new endpoint identity.
Warm pre-enrolled endpoint slots may reduce join latency; node quotas/naming cleanup require tests before enabling a pool.
Per-VM names are not on the v0 first-command critical path; shared gateway always provides stable access.
[WhoIs/tsnet](https://tailscale.com/docs/reference/tsnet-server-api), [DNS limits](https://tailscale.com/docs/reference/dns-in-tailscale).

## 6. Golden snapshots, stop and suspend

Image preparation cold boots once per compatible image/shape/host fingerprint and produces a trusted golden checkpoint.
Default fresh `new` clones the golden's matching immutable disk, restores RAM with private writes, then passes the restore barrier.
File-backed private memory mappings are the initial Linux fast path; custom userfaultfd paging is not required by the spike.
Template files remain pinned for the runtime lifetime; host resource admission does not assume pages will stay shared forever.
No user workload, owner API key, node state or reusable login session may exist in a golden.

Critical invariant: **never restore golden RAM onto an arbitrarily modified persistent root filesystem.**
Kernel filesystem/page caches, journal state and user processes must agree with the captured disk generation.
V0 disk-only `resume`, `fork` and `new --from <disk snapshot>` therefore cold boot their saved rootfs unless a certified adapter
implements a neutral bootstrap checkpoint captured before mutable filesystems are mounted/opened.
A neutral bootstrap may still restore the kernel/early userspace, then mount the selected disk and start services; benchmark it separately.
This optimization requires exact device layout/compatibility checks; “same image” alone does not prove safety.
Expose bootMode `golden_restore`, `bootstrap_restore`, `cold_boot` or `ram_restore` and timings in result/info.
On engines without RAM restore, `new` uses cold boot transparently in capability terms, but never misreports the mode.
`--require-golden` fails 422 on incompatible shape/platform/missing prepared golden rather than falling back.

`stop`: reject new work -> signal/stop processes/services -> clean guest shutdown -> verify VMM exit -> commit disk -> publish stopped.
If shutdown cannot complete, return error and retain old committed snapshot; `--force` explicitly opts into crash-consistent disk state.
A forced snapshot records `consistency=crash`; do not imply that databases or outstanding guest writes were flushed.
Disk/RAM copy/publication failure MUST NOT replace the last good committed generation or report success.
Normal stop does not write RAM, and booted processes do not survive resume.
`snapshot create` in v0 names the stopped VM's committed disk; running quiesced checkpoints are later.

Optional later `suspend`: freeze applications/filesystems as needed, pause VMM, commit matching disks + RAM/device state, release runtime.
Suspended restore resumes processes, but old network sessions/SSH/tunnels may break and must reconnect after the barrier.
Capability/compatibility failure is explicit; user can choose a disk-only resume that discards suspended execution state.
Live fork is a distinct later capability; clone RAM+disk atomically, detach source safely, scrub identity and expose cloned-process caveats.
[Runtime snapshot invariants](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md).

## 7. Mac backend, installation and security

Mac uses a separate VZ/Apple Containerization or microsandbox adapter; native arm64 Linux images, shared guest-agent protocol.
Mac capabilities advertise actual cold/golden/checkpoint support; Linux RAM save/restore is not assumed from VZ API availability.
Disk-only stop/resume remains portable; APFS cloning can implement fast disk copies, otherwise full-copy fallback applies.
No Linux netns/TAP assumption crosses the interface: use host virtualization networking and vsock gateway.
Docker runs inside Linux without nested virtualization; Firecracker inside a Mac guest requires supported hardware/OS nesting.
x86-64 guest kernels on arm64 are not a native virtualization feature; emulation/Rosetta userspace is optional and separately labeled.
Validate distro/systemd compatibility before treating Apple container-oriented images as full Ubuntu developer VMs.
Mac backend design and constraints: [MAC-BACKEND.md](./MAC-BACKEND.md).

Planned one-line installation: `curl -fsSL https://<release-domain>/install | sh`; domain is deliberately undecided.
Installer downloads a signed/checksummed versioned host binary, verifies trust, installs to a user directory, prints next steps.
No automatic root setup, shell-file editing, tailnet join or service enablement from the installer.
`davit init` selects runtime/storage, runs preflight, presents exact system setup, then installs/enables the host service when authorized.
Runtime choice is honest: firecracker only when `/dev/kvm` works and the build includes it, else fake with an explicit warning.
`davit doctor` and daemon startup share one preflight (`internal/preflight`); failures are graded by the configured runtime
(missing KVM fails for firecracker, warns for fake) and the daemon refuses to start on any failure.
Ubuntu 23.10+ `kernel.apparmor_restrict_unprivileged_userns=1` is reported as a failure for VM runtimes with an AppArmor-profile fix,
not a recommendation to disable the host-wide restriction.
`packaging/install.sh` verifies SHA256SUMS (and optional out-of-band `DAVIT_SHA256`); signature verification is required before a public release.
Interactive init uses device/browser tailnet auth; automation uses auth-key stdin; never echo/store enrollment keys in logs.
Allow persistent tsnet state with strict permissions; preauthorized tags/node approval remain tailnet administrator policy.
Support config/control URL without assuming Headscale matches all Tailscale features; test before advertising compatibility.
Preflight checks hardware acceleration, arch, kernel/cgroup/vsock/TUN, CoW/space, runtime integrity, permissions, forwarding and image signature.
Report required Linux privilege and macOS signing/OS requirements. Do not disable host security controls to pass preflight.
No silent full-copy or cold-boot performance downgrade: display actual mode and remediation.

Threat model: guest code is untrusted; host OS/admin, verified runtime, official kernel and daemon are trusted.
Use jailer/sandbox, dedicated runtime UID, cgroup v2 CPU/RAM/PID limits, seccomp and restricted runtime sockets on Linux.
Separate narrow privileged network/storage supervisor from API/gateway; no guest access to host KVM/API socket/store.
Encrypt tailnet traffic, authenticate guest channels, restrict Unix sockets, authorize every HTTP/SSH/tunnel target.
MAC/DNS/VM names and peer-supplied headers are not identities; reject path traversal/symlink escapes and bound message/file sizes.
Snapshots contain secrets from guest RAM/disks: owner-only access, hashes/signatures at trust boundaries, no cross-owner golden promotion.
Fork does not remove arbitrary user secrets from disk; warn in docs and preserve owner scope, not speculative secret-scrubbing promises.
Audit lifecycle/authorization events without env/token/command-secret logging; rotate/revoke API and endpoint credentials.
Disk encryption uses host facilities initially; network isolation and VM boundaries do not imply protection from host administrator access.

## 8. Repository and parallel work plan

Planned layout:

```text
cmd/davit/                 CLI/daemon/guest-agent dispatch
api/openapi.yaml            public HTTP contract; generated bindings in api/gen/
internal/domain/            VM/operation/snapshot types, errors, invariants, capability contracts
internal/lifecycle/         orchestration, planning, TTL, admission, reconciliation
internal/state/             SQLite migrations, transactions, idempotency, leases
internal/runtime/           interface + conformance suite
internal/runtime/{fc,ch,mac}/ engine implementations; no API ownership
internal/storage/           interface + reflink/copy/zfs implementations and GC
internal/guestproto/         framed RPC types/versioning; generated code
internal/guestagent/         exec/files/readiness/identity/shutdown
internal/network/           netns/NAT/helper + portable network contracts
internal/gateway/           tsnet, identity authorization, SSH, HTTP/tunnels, endpoint registry
internal/httpapi/           transport handlers; no independent lifecycle state machine
internal/cli/               rendering, command parsing, context, waits
images/                     manifests, kernel configs, deterministic builder and golden preparation
packaging/                  verified downloads, service units, Mac helper packaging
sdk/{python,typescript}/     generated client + small ergonomic wrappers (M4)
docs/                       design decisions, operator/security/CLI docs
integration/                real-host lifecycle and adapter contract scenarios
```

M0 freezes domain/API/protocol contracts before parallel implementations depend on them; one owner regenerates shared bindings.
No broad generic plugin framework, Kubernetes requirement or premature distributed scheduler.

| Workstream | Owns | Depends on / conflict boundary |
| --- | --- | --- |
| A: domain/state/lifecycle | domain, state, lifecycle | Publishes contracts/migrations; only owner changes shared lifecycle invariants |
| B: runtime/storage/images | runtime, storage, images | Implements A's contracts; owns golden compatibility and snapshot durability |
| C: guest/protocol | guestproto, guestagent | Protocol reviewed once with B/D; host interfaces use dialers, not engine details |
| D: network/gateway/security | network, gateway, privileged helper | Consumes A/C; owns auth/endpoint policy, never adds duplicate lifecycle logic |
| E: product/API/CLI | httpapi, cli, API schema, SDKs/docs | Uses A; proposes contract changes to A, does not modify runtime internals |

| Milestone | Deliverable | Required artifact-level acceptance |
| --- | --- | --- |
| M0 skeleton | Go build, OpenAPI, domain/protocol types, SQLite migrations, config, fake adapters | Generate/validate API; durable operation/idempotency/recovery tests; selected repo lint/type/build commands documented |
| M1 Linux core | Verified bundled image/kernel; golden new, ssh, stop/resume, disk fork over persistent tailnet gateway | Two clients reach real VM; distinct fork writes/keys; actual stop durability; restart daemon mid-operation; revoked peer denied; benchmark full barrier |
| M2 images/Docker/forward | OCI image pipeline, Docker-ready profiles, NAT, forward/reverse, copy fallback; optional ZFS/named endpoints | Container actually reaches allowed internet; forbidden LAN/host blocked; localhost app/tunnel works; disk-full/copy mode/GC correct; per-node DNS measured separately |
| M3 Mac | Native arm64 adapter/helper, image builds, disk stop/fork, capability negotiation | Same lifecycle contract suite on target Mac; Docker works; no false RAM/nesting claims; real command/space timings |
| M4 SDKs/prompt/desktop | Python/TS SDKs, harness adapters/conversations, optional desktop, optional suspend/live fork | SDK roundtrip; harness native session resumes; secret isolation; authenticated first video frame and latency/CPU measurements; RAM compatibility tests |

M1 may use a limited development-only egress setup; production exposure still requires sandbox/auth isolation gates.
V0 release includes M1+M2 security/networking gates; M1 alone is an internal headless preview.
M2 completes the general NAT/egress policy before declaring arbitrary downloaded agent workloads supported.
Keep long-term tests at domain state/durability/auth boundaries and meaningful runtime integration seams, not command-parser snapshots.
Clean large VM/image/test artifacts after validation; retain compact benchmark manifests/results with hardware and sample counts.

## 9. Defaults and decisions

Defaults: Linux-first, Go, MIT-licensed Davit code, 1h TTL, disk-only stop, reflink preferred with honest copy fallback,
persistent shared tsnet gateway, arm-native Ubuntu, optional per-VM endpoint nodes, capability-gated RAM features.
Engine selection remains open; raw Firecracker is a proven spike candidate, not a dependency in the product API.
Naming, simultaneous Mac priority and desktop priority remain open.
