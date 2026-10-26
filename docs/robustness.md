# Robustness: fuzzing, crash consistency, resource bounds

What the daemon core is tested against without real VMs (fake runtime, in-process
guest agent), and what still needs a VM host (`scripts/vm-tier.sh`, docs/vm-tier.md).

## Fuzzing

Go native fuzz tests. `go test` (and so `make check`) runs only their committed
seeds; fuzz one with `go test -run '^$' -fuzz '^NAME$' -fuzztime 45s
-fuzzminimizetime 20x ./PKG`. The daemon-backed fuzzers are slow per exec, and
Go's default minimization (60 s per new interesting input, not counted in
`execs`) otherwise looks like a stall.

| Fuzzer | Package | Input | Invariant |
|---|---|---|---|
| `FuzzAPIBodies` | internal/httpapi | any body, Idempotency-Key and If-Match on each of 33 create/update operations, against the real daemon in-process | never a panic, a hang or a 500; every error is a well-formed `ErrorResponse` |
| `FuzzIdempotencyKey` | internal/httpapi | any key and VM name on createVM | a replay returns the same operation; the key with another body is `idempotency_conflict` |
| `FuzzMigrateOldDB` | internal/state | a store left at each schema version 0..current, holding arbitrary VM / operation / process documents | migrates to the current schema, passes `integrity_check` and `foreign_key_check`, and every reader and startup scan returns records or errors (never panics) |
| `FuzzReadFrame` | internal/guestproto | raw bytes on the agent connection | invalid envelopes are rejected; accepted frames re-encode |
| `FuzzDecodeMessage` | internal/guestproto | every registered message type with any body | decoding never panics; an accepted message survives encode / decode still valid |
| `FuzzVerifyHelloAck` | internal/guestproto | any Hello / HelloAck pair | an ack is accepted only with the proof derived from the shared secret |

Runs on the reference dev machine, 2026-10-23, one worker each (`-parallel 1`), 45 s, at
low priority (idle I/O class) on a loaded machine:

| Fuzzer | Execs | Result |
|---|---|---|
| FuzzAPIBodies | 13,424 | pass |
| FuzzIdempotencyKey | 1,076 (each exec creates, replays and deletes a VM) | pass |
| FuzzMigrateOldDB | 400 (on tmpfs; 49 on disk: each exec opens a store with fsyncs) | pass |
| FuzzDecodeMessage | 736,232 | pass |
| FuzzVerifyHelloAck | 881,914 | pass |
| FuzzReadFrame | 954,039 | pass |

Found and fixed: `state.Open` built its SQLite URI from the raw path, so a data
directory containing `?`, `#` or `%` opened a different file and dropped every
pragma (WAL, `synchronous=FULL`, foreign keys). The path is now escaped
(`TestOpenPathWithURIChars`). The migration fuzzer found it: its seed temp
directories contain `#`.

Not fuzzed here: the tailnet gateway and auth middleware, tokens and members
(hardened and tested separately), and request bodies of streaming endpoints
(tunnels, attach, uploads), which are byte streams, not JSON.

## Crash consistency

`TestCrashConsistency` (internal/lifecycle) kills the daemon at every durable step
of create, stop, resume, snapshot, fork, delete and a privacy purge (turning on
`noOutputRetention`). A kill at step N uses the store's write-fault hook: journal
writes 1..N-1 commit, write N and every later write fail (a killed process
makes nothing durable after its last commit), then the service is dropped and
a new one starts on the same files. N runs from 1 until the mutation finishes
first, so every commit point of every phase is a crash point. After each
restart:

- every operation finishes (recovery replays or fails it), and no VM keeps an
  active operation or a transitional state;
- the store passes `integrity_check` and `foreign_key_check`, and every VM
  record holds its invariants;
- runtime instances match the journal: each ready VM has exactly its current
  generation running, stopped / error / deleted VMs have none, no orphans;
- a VM left in `error` recovers (`recoverVM`) to ready;
- a VM with `noOutputRetention` keeps no output.

Result of the full matrix (`DAVIT_CRASH_MATRIX=full`, 2026-10-23, reference dev machine,
one run, 89 s): create 19, stop 16, resume 19, snapshot 5, fork 20, delete 13 and
privacy purge 4 crash points; all converge. `make check` kills at the first
three writes and every third after (about 40% of the points) to stay within
its time budget; run the full matrix after changing lifecycle or state.

Limits of the model: side effects outside the journal (runtime, storage) happen
in-process, so the test covers a crash between journal commits, not a torn
write inside SQLite or a half-written disk image. SQLite's own atomicity
(WAL, `synchronous=FULL`) and the storage layer's rename-based commits cover
those; testing them needs power-cut tests on real hardware.
`guestexec` has its own crash points around exec (`TestExecCrashNeverRunsTwice`:
after reserving the key, after the agent started the process).

Not covered here: member unshare (internal/members keeps its own store and
its own tests).

## Resource bounds

| Resource | Bound | Beyond it | Test |
|---|---|---|---|
| Live guest processes (each holds a collector and an agent session) | 256 per VM, 2048 per host (`guestexec.Config.MaxLivePerVM`, `MaxLive`) | new exec refused `resource_exhausted` (429, retryable); replays still answer. Exits nobody observed (no collector under `noOutputRetention`) are asked of the agent before refusing | `TestLiveProcessLimit`, `TestLiveProcessLimitSeesUnobservedExits` |
| Retained output | newest 8 MiB per process (`RetainOutput`), oldest dropped and marked `outputDropped` | | `TestOutputRetentionKeepsNewest` |
| Attach streams | synchronous: a slow consumer blocks its stream's sender, never buffers on the host; the agent client queues at most 64 frames per stream (`guestclient` `streamQueue`) | backpressure to the guest (the agent blocks the writer) | by construction (no dedicated test) |
| Event long-polls | 64 per caller (each token, each member, and the owner's own session), 256 for all of an owner's tokens and members together (the owner's own session is outside this cap), 1024 per host (`events.Service.MaxWaitersPerCaller`, `MaxWaitersPerOwner`, `MaxWaiters`); each waits at most 30 s and reads at most 200 events | a waiting list refused `resource_exhausted` (429, retryable); a list without a wait is unaffected | `TestLongPollLimit`, `TestLongPollCapPerGrant`, `TestLongPollSlotsPerGrant` |
| Webhook retries | 12 attempts per delivery, backoff capped at 1 h (existing) | dead | webhooks tests |
| Webhook outbox | 10,000 pending per webhook (`webhooks.Service.MaxPending`); 1,000 delivered / dead kept (`KeepFinished`) | oldest pending dead-lettered with `lastError` "outbox full" (retryable); oldest finished deleted | `TestBoundDeliveries` |
| Request bodies | 1 MiB JSON, larger for uploads (`httpapi.MaxBodyBytes`, `MaxUploadBytes`) | `payload_too_large` | httpapi tests |

Before this pass, finished webhook deliveries were never pruned, so the outbox
table grew forever even with a healthy endpoint, and an endpoint that stayed
down grew it by every matching event. Live processes were unbounded.

## Needs real VMs

- Crash consistency with a real VMM: killing the daemon while Firecracker
  boots, snapshots or restores, and a host power cut during a disk commit.
- Exec and output limits under real guest load (thousands of processes, fast
  writers) and the agent's own buffers in a real guest kernel.
- Guest protocol fuzzing against the agent running as PID 1 inside a VM (here
  it runs in-process on the host).
