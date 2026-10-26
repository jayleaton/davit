# Security review (2026-10-23)

Threat model: the daemon serves its API, app endpoints, port forwards and the SSH front door on the tailnet. It also
serves a unix socket to local users and a credential proxy to each VM. Attackers considered:

- a tailnet peer that is not the owner, or that holds a token with narrow scopes;
- a member at any role;
- code inside a guest, which is untrusted by definition;
- a URL that an API user supplies, such as a webhook or a credential upstream.

Each finding below has a test that failed before the fix and passes after it.

Scope:

- `internal/gateway`
- `internal/network` (nft, userspace stack, DNS, reverse forwards, the new `netguard`)
- authorization middleware and the token, member and share code in `internal/httpapi`
- `internal/tokens`
- `internal/credproxy`
- `internal/webhooks`

## Findings

| ID | Severity | Area | Finding | Status | Test |
| --- | --- | --- | --- | --- | --- |
| SEC-1 | High | httpapi / harness | A token with exec scope, or an exec member, could name daemon secrets in `createPrompt` (or replay them through `steerPrompt`). Those secrets went into the guest, so a narrow token could read every owner secret. They now need the owner, or a token with admin scope on the VM. Members are always refused. | fixed | `TestDaemonSecretsNeedOwnerOrAdmin` |
| SEC-2 | High | httpapi | `resumeVM` and `forkVM` accepted any `environment`, which skipped environment shares and injected that environment's secret files into the VM. Members now need view on the environment (otherwise 404) and exec (otherwise 403). Tokens need admin scope on all VMs. | fixed | `TestEnvironmentUseNeedsEnvironmentRights` |
| SEC-3 | Medium | httpapi | View role (tokens and members) could read process output, prompt events and the prompt event stream. Those carry command output and model transcripts. The three routes now need exec. | fixed | `TestViewCannotReadContent` |
| SEC-4 | Low | tokens / httpapi | Streams opened with a token (events, exec, logs, tunnels) survived revoking or rotating the token, and survived its expiry. Live sessions are now tracked per token. Revoking, rotating with overlap 0, or reaching the expiry cuts them, and rotating with an overlap cuts them at the new expiry. | fixed | `TestRevokeCutsLiveSessions` |
| SEC-5 | Medium | credproxy | The secret placeholder was substituted in any header, so a guest could reflect the secret through `Referer`, a custom header or a debugging upstream. Secrets that an upstream echoed in headers, bodies or trailers reached the guest unredacted. The proxy now substitutes only in `Authorization`, `X-Api-Key`, `Api-Key` and `X-Goog-Api-Key`, and returns 400 if a placeholder appears anywhere else. Response headers, bodies and trailers are scrubbed while streaming, including a secret split across two reads. | fixed | `TestProxyPlaceholderOnlyInAuthHeaders`, `TestRedactingBodySplitSecret` |
| SEC-6 | Medium | credproxy / netguard | A credential upstream could be any address: plain http, loopback, the LAN, the tailnet or cloud metadata. The `HTTP(S)_PROXY` environment variables were also honoured. A user-defined secret could therefore send the secret, or a guest's requests, to internal services. The upstream now goes through `netguard`, which checks the address at connect time (after DNS, so rebinding doesn't help), sets no environment proxy, and requires https. | fixed | `TestProxyRefusesInternalUpstreams`, `TestPolicyRefusesInternalAddresses`, `TestTransportChecksAtConnect` |
| SEC-7 | Low | credproxy | Dot segments (`..`, `%2e%2e`, `%2f`, `%5c`, backslash) passed through to the upstream, so a guest could escape the configured base path. They now get 400. | fixed | `TestProxyRejectsDotSegments` |
| SEC-8 | Low | webhooks | Webhook deliveries could reach loopback, the LAN, the tailnet and metadata. The error message and timing of each delivery also made blind port scanning possible. Deliveries now use `netguard`, and redirects were already not followed. A URL whose host is a literal refused address is rejected on create and update. A refusal at connect time dead-letters the delivery at once rather than holding the bounded outbox for hours of retries. | fixed | `TestDeliveryRefusesInternalAddresses` |
| SEC-9 | Low | network | The egress deny list missed NAT64 (`64:ff9b::/96`, `64:ff9b:1::/48`), Teredo (`2001::/32`), 6to4 (`2002::/16`), `192.0.0.0/24` and `198.18.0.0/15`. The userspace policy did not check IPv4 addresses embedded in IPv6 ones. This is latent today because guest IPv6 is dropped. | fixed | `TestDefaultDenyCoversEmbeddedV4`, `TestPolicyOrder` |
| SEC-10 | Low | network / DNS | The filtered DNS relay forwarded any opcode (UPDATE, NOTIFY) and messages carrying records, which a guest could use against the host's resolvers. It now forwards only plain queries: opcode QUERY, one question, and no records except OPT. | fixed | `TestOnlyPlainQueries`, `TestFilteredRelaysOnlyQueries` |
| SEC-11 | Low | gateway / daemon / credproxy | The HTTP servers (tailnet API, app endpoints, unix socket, credential proxy) set no idle timeout and accepted headers up to 1 MiB, so idle keep-alive connections lived forever. `httpx.Harden` now sets a 10 s header deadline, a 2 min idle timeout and a 64 KiB header limit. | fixed | `TestHardenBoundsConnections` |
| SEC-12 | Low | network / config | `network.reverseHostServices` accepted metadata, link-local, multicast and reserved addresses. A typo or copy-paste could therefore expose cloud metadata to guests through a reverse forward. Config parsing and `Policy.HostService` now refuse them, while loopback and LAN services stay allowed. | fixed | `TestPolicyHostService`, `TestReversePolicyRefusesInternal` |
| SEC-13 | Low | events / httpapi | The event long-poll cap (64 per caller) was keyed by principal, and an owner's tokens and members share the owner's principal. A narrow token could therefore hold all 64 slots and get the owner's own long-polls refused with `resource_exhausted`. The cap now counts each grant (token, member) and the owner's own session separately. A per-owner ceiling (4× per caller, 256 by default) bounds all of an owner's grants together, without counting the owner's own session. The 1,024 per-host cap is unchanged. | fixed | `TestLongPollCapPerGrant`, `TestLongPollSlotsPerGrant` |

Count: 2 high, 3 medium, 8 low, all fixed.

## New setting

`network.allowPrivateUpstreams` (default `false`) lets credential-proxy upstreams and webhook URLs reach loopback, RFC 1918,
CGNAT/tailnet and ULA addresses, and allows plain-http credential upstreams. An example is a model server or a webhook
receiver on the LAN. Link-local, cloud metadata, multicast and reserved addresses are refused either way. This is a
behaviour change: if an existing setup points a credential upstream or webhook at a LAN address, that setup must turn
the setting on.

## Interplay with the robustness bounds

- **Webhook outbox bound vs SEC-8:** a webhook that the delivery policy refuses used to keep every event pending
  through about 12 attempts of backoff, until the outbox bound (10,000 pending per webhook) dead-lettered the
  oldest. A refusal cannot heal by retrying, so it now dead-letters on the first attempt. A literal internal URL is
  refused when the webhook is created or updated (`TestDeliveryRefusesInternalAddresses`).
- **Long-poll caps vs SEC-4:** a token's long-poll is cut by cancelling its request context. `events.List` returns
  on `ctx.Done()`, and its deferred `leave` frees the slot. `TestRevokeCutsLiveSessions` covers the cut and
  `TestLongPollLimit` covers the freed slot, so a revoked token's waiters stop counting at once. No change was needed.
- **API decoding and fuzzers vs SEC-1/2/3:** the httpapi fuzz seeds pass, and so does a 60 s run of
  `FuzzAPIBodies` (single worker, 3,770 execs). Prompt, steer, resume and fork bodies reach the new authorization
  helpers with no 500 and no malformed error body.

## Deferred

| Severity | Item | Why deferred |
| --- | --- | --- |
| Low | Member list cursors (VMs, snapshots) carry the IDs of objects the member cannot see. | The fix belongs in the state queries, and `internal/state` (apart from tokens) is outside this review's scope. The IDs are opaque and every read checks access. |
| Low | The deny list is built from host addresses captured once at daemon start, so an address added later (DHCP, a new interface) is not denied until restart. | This is daemon and lifecycle wiring, which is outside this review's scope. Default-deny on RFC 1918, CGNAT and ULA covers the usual cases. |
| Low | No per-identity connection caps, and gateway stream `Limits` (forward and SSH idle timeouts) are unset by default. | Choosing caps needs a product decision. Only the owner, tokens and members reach these streams, and every stream is revocable (SEC-4). |
| Info | The access check and the handler each resolve `{vmId}`, so a rename in between makes them see different objects. | Names are scoped to the owner, renaming needs admin on the VM, and the handler looks up under the owner principal. The worst case is a request landing on another VM that the same caller can also access. Fixing it means passing the resolved VM through the context in every handler. |
| Info | An exec member can read the owner's prompt secrets from inside the guest while a turn runs. | Exec means shell in the guest, which is documented with members. SEC-1 stops members from requesting secrets themselves. |
| Info | The SSH front door relays `direct-tcpip` to the guest sshd. | The guest's own egress policy bounds it, so it grants nothing beyond exec. |
| Info | Apps on one gateway see each other's cookies, because browsers do not scope cookies by port. | This is documented in `docs/network.md` (app endpoints). The daemon itself sets no cookies and checks identity on every request. Mutually distrusting apps need separate gateways. |
| Info | The fake runtime dials host loopback. | Development only, never wired to the Firecracker runtime. |
| Info | The credential proxy is reachable from VMs whose egress is off. | This is by design: it is the VM's only path to model APIs, and SEC-5/6 bound what it can be used for. |
| Info | `OwnerAppAuthorizer` uses the token's issuer as the principal without comparing it to the daemon owner. | No action: `GetVM` is scoped to that principal, so a token reaches only its issuer's VMs. The daemon has one owner principal. |

## Checked and found correct

- **Tokens**:
  - each secret has 32 random bytes and is stored as SHA-256;
  - lookup is by hash in SQL, with no comparison of the plaintext and no cache, so revocation takes effect at once and expiry is enforced on every request;
  - rotating with overlap 0 revokes the old token immediately;
  - tokens cannot manage tokens, members, shares or webhooks (`ownerOnly`);
  - Bearer parsing is strict.
- **Routes**: every route has an access spec (view/exec/admin/ownerOnly). This was checked for desktop, app endpoints, events, webhooks, harness turns, files, tunnels and forwards. Owner-only routes refuse tokens and members.
- **Identity**:
  - the gateway takes identity only from WhoIs on the peer address, and the unix socket from `SO_PEERCRED`;
  - the app proxy strips client `Tailscale-*` headers, and `ReverseProxy.Rewrite` drops `X-Forwarded-*`;
  - `forwardIdentity` is opt-in and only for untagged users.
- **Port forwards, expose and app endpoints**: these dial only through the guest dialer to guest loopback. No host, LAN or metadata target can be named.
- **SSH front door**: auth is the tailnet identity plus an access check on the VM, and live sessions are revocable.
- **Files and scp (`tarx`)**:
  - `..` and absolute paths are refused;
  - no write goes through a symlinked parent;
  - unsupported entry types are refused.
- **nft ruleset text**: rules are rendered only from typed `netip` values, with no strings from users. The goldens pin the full text.
- **Egress**: the default deny covers metadata, link-local, RFC 1918, CGNAT, ULA and the host's addresses. DNS rebinding is handled because enforcement happens on IP addresses in nft and in the userspace stack, not on names.
- **Webhooks**:
  - redirects are not followed;
  - the timeout is 10 s;
  - response drains are capped at 64 KiB;
  - the webhook secret only keys the HMAC signature header and is never sent.
- **Secrets on disk**: secret files are mode 0600.
