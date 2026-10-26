# Client SDKs

| SDK | Package | Runtime deps | Docs |
| --- | --- | --- | --- |
| Python ≥ 3.10 | `davit` | `httpx` | [python/README.md](python/README.md) |
| TypeScript (Node ≥ 20 for `dist/`, ≥ 22.18 from source; Bun) | `@davit/sdk` | none | [typescript/README.md](typescript/README.md) |

Both SDKs implement one contract, so behaviour is identical apart from naming style. Change
either SDK only together with the other, and record any new shared rule here.

## Shared contract

- **Endpoint.** Explicit unix `socket`, or `https://` base URL + bearer token (plain `http` only
  for loopback). Default: `DAVIT_API_URL`, else the daemon's default socket
  (`$XDG_RUNTIME_DIR/davit/davit.sock`, `~/.local/share/davit/…`, macOS
  `~/Library/Application Support/davit/…`).
- **Remote configuration.** One configuration shared with the CLI and SSH: `DAVIT_API_URL`
  (`https://<host>[:port]`), `DAVIT_TOKEN` (optional bearer) and `DAVIT_CA_FILE` (optional PEM
  bundle of extra trusted CAs, for operator certificates; Tailscale certificates need none).
  Explicit arguments (`token`, `ca_file`/`caFile`) win; an empty token sends none. The token
  variable applies only to http(s) base URLs, never to the unix socket (owner-only, never a
  bearer); the CA variable is ignored for the socket and an explicit CA file with a socket is an
  error. The CA bundle is trusted in addition to the default roots, for HTTP requests and the
  WebSocket streams alike, and is read and checked at client construction (missing, unreadable or
  certificate-free → error). Each SDK ships a `remote_https` / `remote-https` example that reads
  only these variables.
- **Requests.** `X-Request-Id` per call. Lifecycle mutations, `execProcess` and `createPrompt` send an
  `Idempotency-Key` (caller's or a UUIDv4 generated once per call); `If-Match` on request.
  `createToken`, `createCredentialProfile`, `createCredentialGrant`, `createEnvironment`,
  `createEnvironmentRevision`, `createRun` and `steerPrompt` send one too. `updateVM`,
  `deleteSnapshot`, `killProcess`, `writeFile`, `pushTree`, `cancelPrompt`, `interruptPrompts`,
  `revokeToken`, `deleteCredentialProfile`, `putCredential`, `deleteCredential`,
  `revokeCredentialGrant`, `deleteEnvironment`, `startRun` and `cancelRun` are naturally idempotent
  and send no key.
  Retries happen only when the request certainly never reached the daemon (connection refused /
  socket missing), at most 3 attempts, same key; streamed bodies are sent exactly once.
- **Waits.** Lifecycle calls wait by default (`wait=false` returns the 202 `OperationAccepted`).
  `wait` long-polls operation events (≤ 25 s per poll) and returns the operation, raises
  `OperationFailed` with the typed error, or `WaitTimeout` with the operation id. A client timeout
  never cancels server work. `waitReady` returns a ready/running VM or raises its `lastError`.
- **Capacity.** `createVM` and `resumeVM` take an optional capacity wait (Python
  `capacity_wait`, seconds or `timedelta`; TypeScript `capacityWaitSeconds`; integer 0..3600,
  validated client-side; 0 or omitted sends nothing) sent as the `waitSeconds` query: on a full
  host the request queues that long for room (feature `capacityReclaim`). It is unrelated to
  `wait`, which follows the operation; Python extends that request's HTTP timeout by the wait.
  `reclaimVMs(idleSeconds, labels?, dryRun?)` stops the caller's idle VMs (`davit box gc`) and
  returns them with their stop `operationId` without waiting; it sends no `Idempotency-Key`.
- **VM metadata.** `updateVM` is a synchronous PATCH of `{name?, ttlSeconds?, autoStop?, egress?, privacy?}`
  (If-Match supported). `extendVM`/`extend_vm` mirrors `davit extend` (always `autoStop: true`,
  plus `ttlSeconds` when given, which must be > 0); `renameVM`/`rename_vm` mirrors `davit rename`.
  `type` on create/fork selects an administrator profile from `capabilities.profiles`.
- **Errors.** `DavitError` → one subclass per API error code (`Conflict` parents the four 409
  codes); unknown codes stay `DavitError`. Client-side: `TransportError`, `WaitTimeout`,
  `OperationFailed`.
- **Processes.** `run` = exec + attach from cursor 0 + collect until exit; exit codes as
  `davit exec` (124 deadline, `code & 0xff`, 128+n signals, else 255). Attach is a
  dependency-free RFC 6455 client over the same endpoint; output is deduplicated by `seq`
  cursor; a dropped stream re-attaches from its cursor (≤ 3 attempts); stdin is chunked ≤ 256 KiB;
  incoming messages ≤ 1 MiB. `listProcesses` pages oldest first in `(createdAt, id)` order;
  `iterProcesses` follows the cursor.
- **Files.** `generation` is looked up when omitted (as `davit cp`); paths must be absolute;
  truncation is detected (Content-Length for files, structural tar check for archives) and raises
  `TransportError`. Python adds `upload_dir`/`download_dir` (stdlib `tarfile`); TypeScript has no
  stdlib tar, so it has none.
- **Tunnels.** `openTunnel` + `attachTunnel` (= `connectTunnel`, which closes the tunnel if
  the attach fails) reuse the attach WebSocket client: binary messages carry the bytes both ways,
  writes are chunked ≤ 32 KiB, an empty binary message is the half-close (`closeWrite`), text
  messages are ignored, and a normal (1000/1005) server close is EOF while any other end is a
  `TransportError`. Tunnels are single use and not re-attached; errors before the upgrade are
  the typed API errors (a port with no listener is `Unavailable`).
- **Reverse forwards.** `reverseForward` / `reverse_forward` mirrors `davit forward --reverse`:
  create a `session` forward (address label `host:port`), attach `attachReverseSession`, and per
  `connection` event claim `attachReverseConnection` and dial the local service (10 s bound), with
  the tunnel framing (≤ 32 KiB binary, empty binary = half-close either way, close frame ends it).
  Both EOFs end a connection; an unreachable local service closes it. A failed session attach
  revokes the forward; closing the session revokes it and cuts live bridges. The session's end
  reason is the server's `revoked` reason, `closed` after a local close, or a dropped stream
  (`error` = `TransportError`); it is surfaced, never thrown asynchronously.
  `forwardHostService` is a `host` forward (operator-listed `ip:port`).
- **SSH credentials.** `issueSSHCredential` signs a public key the caller supplies (OpenSSH
  authorized_keys line); the SDKs never generate or hold SSH keys (no crypto dependency). Pin
  the returned `hostKeys` (they match `endpoint.sshHostKeyFingerprints`).
- **Tokens.** `createToken` / `listTokens` (+ `iterTokens`) / `revokeToken` are owner-only (local
  socket or tailnet owner; a token-authenticated client gets `PermissionDenied`). `createToken`
  takes `actions` + optional `vm` (one scope per VM; none = all VMs) or explicit `scopes`, plus
  `ttlSeconds` (1..31536000), and always sends an `Idempotency-Key`. The `secret` (`slw_…`) is
  shown once; use it as the bearer token of an `https://` endpoint. The SDKs never log it; Python
  keeps it out of the model `repr`, TypeScript returns plain objects (no runtime classes).
- **Credentials.** Profile operations (`listCredentialProfiles` + `iterCredentialProfiles`,
  `createCredentialProfile`, `getCredentialProfile`, `deleteCredentialProfile`, `putCredential`,
  `deleteCredential`) are owner-only; grant operations follow the VM's token scope (`view` to list,
  `admin` to create/revoke). Profiles and grants are taken by ID, name (profiles) or the returned
  object. `putCredential` sends the write-only `secret` in the PUT body only (omitted = `""`, which
  keeps the stored one); the server never returns it, the SDKs never log it, and Python keeps it out
  of the `PutCredentialRequest` `repr`. `listCredentialGrants` returns one page (the server sends
  all of a VM's grants); `revokeCredentialGrant` returns the revoked grant. The guest sees only the
  grant's `placeholder` in `envVar` and calls the host proxy at `http://172.16.0.1:15080`.
- **Events and webhooks.** `listEvents` pages the host event stream (`types` filter of types or
  `prefix.`, `vmId`, `start` oldest/latest, `waitMs` long-poll ≤ 30 s; `nextCursor` always set);
  `iterEvents` follows it and stops at the first empty page, or with `follow` long-polls (25 s per
  poll) forever. Webhook management (owner only) wraps all eight operations; `createWebhook` and
  `rotateWebhookSecret` send an `Idempotency-Key` and return the secret once (Python keeps it out
  of the `WebhookSecret` `repr`); `updateWebhook`, `deleteWebhook` and `retryWebhookDelivery` send
  none. `verifyWebhook` / `verify_webhook` checks `X-Davit-Signature` on the raw body against
  one or more secrets (any `v1` matching any secret passes, constant-time; timestamp within
  ±300 s by default) and raises `WebhookVerificationError` with `reason` malformed | timestamp |
  signature.
- **App endpoints.** `listAppEndpoints`, `createAppEndpoint` (+ `expose`), `updateAppEndpoint`
  (+ `hide`/`show`/`renameAppEndpoint`) and `deleteAppEndpoint` take the endpoint by ID, name or
  the returned object; they send no `Idempotency-Key` (a replayed create is `Conflict`: one
  endpoint per guest port and per name). No tailnet gateway: `UnsupportedCapability`.
- **Members and shares.** `listMembers`, `addMember`, `updateMember`, `removeMember`, `listShares`,
  `createShare` (+ `shareVM`/`shareSnapshot`/`shareEnvironment`), `deleteShare` and `unshare` are
  owner-only and send no `Idempotency-Key` (`createShare` is an upsert). Members and shares are
  taken by ID (members also by login) or the returned object; `"*"` is every member. `unshare`
  finds the share with `listShares(kind, resource)`, matching the member ID or `"*"` and else
  resolving a login via `listMembers`, and raises `NotFound` client-side when there is none.
  No tailnet gateway: `UnsupportedCapability`.
- **Desktop.** `getDesktop` (none open: `NotFound`), `openDesktop(vm, port?)` (no body without a
  port; an open desktop is returned, 200) and `closeDesktop` wrap the VM's app endpoint of kind
  `desktop`; no `Idempotency-Key`. No tailnet gateway: `UnsupportedCapability`.
- **Prompts.** `followPrompt` yields each event once until the turn is no longer
  `starting`/`running` (unknown states end the follow); `waitPrompt` returns the final prompt;
  `runPrompt` returns `{prompt, text, events}`. Failed and cancelled turns are returned, not
  raised. `createPrompt` (and `runPrompt`) always send an `Idempotency-Key` (caller's or
  generated once per call): same key + body returns the existing turn, a different body is
  `IdempotencyConflict`. `listConversations` pages (`limit`, `cursor`, `nextCursor`) in
  `(createdAt, id)` order, oldest first; `iterConversations` follows the cursor. A follow `WaitTimeout` has no operation id (a prompt is not an operation) and carries
  `details.promptId` and `details.cursor`. `createPrompt` takes `attachments`, `reasoningEffort`
  and `fast` (flattened into the wire `options`) and `turnTimeoutSeconds`. `steerPrompt` sends an
  `Idempotency-Key` (one delivery; a repeat returns the first result), `interruptPrompts` none and needs exactly one of
  `conversationId` and `all`. `listVMPromptEvents` pages the VM-wide feed by opaque cursor
  (`nextCursor` always set); `followVMPromptEvents` long-polls it until the caller stops.
- **Egress.** `createVM` and `forkVM` take an optional `Egress` (`mode` off | filtered |
  allowlist, `allow` entries for allowlist only); omitted means the host default on create and the
  source's policy on fork. `updateVM` takes `egress` too; `setVMEgress` is `updateVM` with only `egress`. It replaces a VM's policy synchronously (live when running,
  kept for later boots when stopped) and returns the VM. Entries are not validated or normalized
  client-side: the server rejects bad ones with `InvalidArgument` and returns them normalized.
- **Privacy and ephemeral VMs.** `createVM` (and the `createVMs` template) and `updateVM` take an
  optional `PrivacyPolicy` (`ephemeral`, `noSnapshot`, `noOutputRetention`, `noProcessSamples`; all
  default false); `VM.privacy` reports it and forks inherit it. Omitted on create means the usual
  retention; on `updateVM` it replaces the whole policy (flags left out are off), so read the VM and
  change one flag to keep the others, as `davit privacy` does. Ephemeral VMs are deleted on any
  stop (explicit, TTL, forced) and noSnapshot VMs commit nothing at stop (no resume, no fork, no
  explicit snapshot). The flags limit what the host keeps; they are not secure erasure (host backups,
  free space, SSD wear levelling), the guest can still write to its disk and send data within its
  egress policy, and the guest agent holds recent output in guest memory while the VM runs. A server
  without feature `privacyPolicy` raises `ServerNotImplemented`. Details:
  [docs/vms-and-agents.md](../docs/vms-and-agents.md#ephemeral-and-private-vms).
- **Environments.** `createEnvironment(name, manifest)` / `createEnvironmentRevision(env,
  manifest, ifMatch?)` take an `EnvManifest`; omitted `repos`, `variables`, `secretFiles` and
  `setup` are sent empty and an unknown top-level key is refused client-side. `ifMatch` is the
  expected `latestRevision` (stale: `RevisionMismatch`, `details.latestRevision`). Lists and
  iterators as elsewhere; revisions page oldest first. `createVM`, `forkVM` and `resumeVM` take
  `environment`: an `EnvironmentRef`, or `"NAME"` / `"NAME@REVISION"` (REVISION an integer ≥ 1,
  as the CLI's `--environment`); anything else is rejected before sending.
- **Job history.** `listProcessHistory` (+ `iterProcessHistory`) pages newest first.
  `readProcessOutput` returns one raw page; `iterProcessOutput` yields decoded chunks and
  `collectProcessOutput` returns `{stdout, stderr, complete, cursor}`. Both follow `nextCursor`
  until a page is `complete` or has no `nextCursor` (nothing new yet); unknown message types are
  skipped and malformed base64 is an `internal` error.
- **Runs and push.** `createRun` returns at once (`outputGraceSeconds` 0..600, validated
  client-side); `listRuns` (+ `iterRuns`) pages oldest first; `waitRun` polls `getRun` (every
  200 ms) until `endedAt` and returns failed/cancelled runs (a budget overrun is `WaitTimeout`
  with `details.runId`). `outcome`/`decidedAt` are the result as soon as known; `state` becomes
  `outcome` after cleanup (output grace, VM delete unless `keep`); `cancelRun` once `outcome` is
  set only ends the grace. `runToCompletion` = create (`hold` with files) → push → `startRun` →
  attach and collect (falling back to `readProcessOutput` if the VM is already gone) →
  `cancelRun` once `decidedAt`/`outcome` is set (skipped for grace 0) → `waitRun`; exit codes as
  `davit run` (a `signal` kill is 128+n, reported as `signal`); a run that failed before its
  command started raises its typed error (`details.runId`).
  `pushTree(vm, path, tar, mode="merge")` sends `application/x-tar`; `mirror` is passed through.
  Python adds `push_dir` (git selection like `davit push`); TypeScript takes a ready tar.
- **Models.** Generated offline from `api/openapi.yaml` by each SDK's own script; enums are open
  (newer servers may add values), unknown fields are ignored, `{}` schemas are any-JSON.

## Contract tests

Each SDK fails its tests when `api/openapi.yaml` changes in a way it does not cover: generated
models drift; an operation is neither in `OPERATIONS` (with matching method + path) nor in
`NOT_YET_SUPPORTED`, or is implemented by the daemon but not by the SDK; an error code has no
class. Integration suites spawn the real `bin/davit daemon` (fake runtime with fake guests and
a fake harness) and leave nothing behind. Run everything with `make sdk-check` (part of
`make check`).
