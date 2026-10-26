# Architecture

How the code is laid out, how the daemon stays consistent across crashes, and notes on the Firecracker runtime.
The product and architecture contract is [research/SPEC.md](research/SPEC.md).

## Layout

| Path | Owns |
| --- | --- |
| `cmd/davit` | single binary: CLI verbs, `daemon`, `guest-agent`; end-to-end tests |
| `api/` | `openapi.yaml` (v1) + thin Go wire types, drift-checked by reflection |
| `internal/domain` | VM/snapshot/operation/image types, state machine + invariants, typed errors, capability contract |
| `internal/state` | SQLite (pure Go, WAL, synchronous=FULL), migrations, operation journal, idempotency, store lock |
| `internal/lifecycle` | the only lifecycle state machine: planning, admission, execution, crash replay, TTL |
| `internal/runtime` | engine adapter interface; `fake/` adapter; `runtimetest/` conformance suite |
| `internal/runtime/firecracker` | Linux adapter: pinned binary, rootless netns+tap launcher, VMM supervision, golden capture/restore, hybrid-vsock dialer; `fctest/` real-VM fixture |
| `internal/runtime/mac` | Apple-silicon adapter over the (not yet written) Swift VZ helper; APFS clone storage |
| `internal/gateway` | tailnet gateway (tsnet): API, SSH front door, app endpoints, desktop (RFB), member sessions |
| `internal/members` | members, shares and their identity pins |
| `internal/credproxy` | per-VM credential proxy |
| `internal/network` | rootless and privileged per-VM networking, egress policy, in-netns DNS, reverse connections |
| `internal/guestssh` | gateway guest SSH: daemon user CA, short-lived per-VM/generation certificates, barrier-recorded host keys |
| `internal/storage` | disk store interface; reflink (FICLONE) and sparse-copy implementation with honest probing |
| `internal/guestproto` | versioned framed host↔guest RPC types and codec |
| `internal/guestagent` | in-guest agent (Linux): vsock :1024, handshake, restore barrier, exec, files, tunnels, readiness |
| `internal/guestclient` | host side: injected dialer, `Prober` (lifecycle ReadinessProber running the barrier), exec, files, secret store |
| `internal/guestexec` | process API backend: per-VM agent sessions, process registry, exec idempotency, attach |
| `internal/harness` | `prompt`: Claude Code, Codex, OpenCode and pi adapters, normalized events, conversations, turn runner |
| `internal/tarx` | safe tar write/extract for directory transfer (agent and `cp`) |
| `internal/wsock` | minimal bounded RFC 6455 WebSocket (attach and tunnel streams) |
| `internal/httpapi` | HTTP transport, Unix-socket peer-credential auth; listener/Authenticator seam for tsnet |
| `internal/cli`, `internal/config`, `internal/daemon` | CLI (JSONL, exit codes, waits), config file, daemon wiring |
| `internal/preflight`, `internal/onboarding` | host checks shared by `doctor` and daemon startup; `init` |
| `internal/tools/linkcheck` | checks relative links and heading anchors in every Markdown file (part of `make check-lint`) |
| `images/` | rootless, deterministic guest image pipeline (kernel, rootfs, manifests) |
| `packaging/` | `make release`, checksum-verifying `install.sh`, systemd user unit template, AppArmor profile |
| `sdk/python`, `sdk/typescript` | client SDKs (`davit`, `@davit/sdk`): unix socket or HTTPS+token, idempotent mutations, waits, typed errors; models generated from `api/openapi.yaml` with drift and coverage contract tests |

## How durability works

Every mutation reserves its idempotency key, writes the VM change and a queued operation in one SQLite
transaction, then runs outside any transaction: journal the phase, call runtime/storage, commit the
observation. Every step is idempotent on IDs chosen at planning time (disk `vm-gN`, commit `c-<op>`,
snapshot ID, runtime key `(vm, generation)`), so after a crash the daemon reconciles runtime instances
against the journal (orphans destroyed, lost instances marked `error`) and simply replays unfinished
operations.

## Runtime notes

The pinned Firecracker release is downloaded into `dataDir/bin` and verified by sha256 (or set
`firecracker.binary`). Keep `dataDir` and the image blobs on one reflink filesystem (btrfs, XFS) for fast
clones. Images must contain the guest agent: the static `davit` binary installed as
`/usr/local/bin/davit-guest-agent`. Golden capture waits for the agent's vsock port, gives up after 60 s and
falls back to an honestly reported cold boot. A clean `stop` asks the agent to power off; without an agent only
`stop --force` works. With no `images` configured the daemon loads every manifest in
`<dataDir>/images/manifests` with blobs in `<dataDir>/images/blobs`. Each VM gets a rootless network namespace
with a `tap0` NIC (`network.mode`: `rootless`, `none`, or auto); `davit doctor` says when egress is off.
`davit fork VM NAME --live` forks the running VM itself (RAM, processes and disk at one instant; the source
pauses briefly, shortest with `firecracker.checkpointDir` on a tmpfs); plain `fork` cold boots the last committed
disk. Numbers and caveats: [benchmarks.md](benchmarks.md).

Guests are untrusted, so egress is a per-VM policy: `off`, `filtered` (default: public internet, no
LAN/host/metadata/tailnet) or `allowlist` (`davit new --egress allowlist --allow github.com,registry.npmjs.org`),
changeable live with `davit egress <vm> <mode>`. The LAN stays denied unless the operator permits single services
(`network.lanServices`, e.g. `tcp/10.0.0.5:5432`); see [network.md](network.md).
A per-VM privacy policy limits what the host keeps: `davit new --ephemeral` (deleted on any stop),
`--no-snapshot`, `--no-output-retention`, `--no-process-samples`, changeable with `davit privacy <vm>`. It is not
secure erasure; see [Ephemeral and private VMs](vms-and-agents.md#ephemeral-and-private-vms) for the limits.
Model API keys never enter a VM: `davit credentials grant VM PROFILE NAME` gives it a placeholder that the host
credential proxy (`http://172.16.0.1:15080` inside the VM) swaps for the real key, only toward that credential's API.
**Behaviour change:** credential upstreams and webhook URLs on the host, LAN or tailnet are refused unless
`network.allowPrivateUpstreams` is set ([network.md](network.md#credential-proxy-internalcredproxy)).
