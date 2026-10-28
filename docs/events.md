# Events, webhooks and OTLP export

davit publishes one ordered stream of host events: VM, operation, run, prompt and CI job changes. You can read it
three ways: the API and CLI (`listEvents`, `davit events`), signed webhooks, or an OpenTelemetry collector.

## The stream

| Type | When | `data` |
|---|---|---|
| `vm.created`, `vm.state` | a VM appears; its state or generation changes | `name`, `state`, `previousState`, `generation`, `stateRevision`, `error` (code) |
| `operation.created`, `operation.state` | an operation is queued; it starts, succeeds, fails | `kind`, `state`, `previousState`, `phase`, `error` (code); `operation.created` also has `reason` for a stop the host decided on: `idle`, `ttl`, `reclaim` (made room for another VM) or `gc` (`reclaimVMs`) |
| `operation.progress` | each journaled operation step | `seq`, `state`, `phase`, `progress` |
| `run.created`, `run.state` | a run is created; its state changes | `state`, `previousState`, `exitCode`, `error` (code) |
| `ci.job.created`, `ci.job.state`, `ci.job.refused` | a GitHub Actions job is queued for this host; its state changes; the host refuses it ([ci-runners.md](ci-runners.md#watching-it)). Owned by the host owner; subject: the CI job; `vmId`: its box, once it has one | `repo`, `jobId`, `job`, `state`, `previousState`, `runner`, `result`, `conclusion`, `error` (code) |
| `prompt.created`, `prompt.state` | a prompt turn starts; its state changes | `conversationId`, `harness`, `state`, `previousState`, `error` (code) |
| `golden.evicted` | the host evicted a golden (a booted memory snapshot of an image or project image) to keep the data dir's free-space floor; the next create that needs it captures it again. Owned by the snapshot's owner for a project image (subject: the snapshot), else by the host owner (subject: the golden) | `goldenId`, `snapshotId` (project image), `imageDigest`, `bytes` (freed), `reason` |
| `vm.autodeleted` | the host's low-disk cleanup deleted an unused box (a stopped instance, or a project image with no instances) to get free space back above the floor ([project-boxes.md](project-boxes.md#when-the-host-is-low-on-disk)). Owned by the box's owner; subject: the VM | `vmId`, `name`, `box`, `kind` (`image`, `instance`), `lastUsedAt`, `unusedSeconds`, `freedBytes`, `deletedAt`, `reason` |

- **Ordering:** every event has a `seq`, a `time`, the `vmId` and a `subject` (the VM, operation, run or
  prompt ID).
- **Lossless capture:** events are written by the database in the same transaction as the change they
  describe, so none is lost or reordered, even across crashes.
- **Redaction:** `data` is a fixed allowlist. Environment values, prompt text and secrets are never included.
- **Retention:** events are kept for 7 days.
- **Unknown types:** consumers must tolerate types they don't know.

```console
$ davit events                              # everything retained, oldest first
$ davit events --types vm.,operation.state  # types, or prefixes ending in "."
$ davit events --vm box --follow            # new events as they happen (Ctrl-C ends)
```

**API:** `GET /api/v1/events?types=&vmId=&cursor=&limit=&start=latest&waitMs=25000`. It returns a page and a
`nextCursor`, which is always present. `waitMs` long-polls for the first new event. A token needs `view` on
all VMs.

## Webhooks

```console
$ davit webhook create https://ci.example.com/davit --event vm. --event operation.state
signing secret (shown once; verify X-Davit-Signature with it): whsec_…
$ davit webhook deliveries wh_… --state dead
$ davit webhook retry wh_… whd_…
$ davit webhook rotate wh_… --overlap 24h
```

Each matching event is POSTed as JSON `{"deliveryId", "webhookId", "event"}`, with these headers:

- `X-Davit-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256(secret, "<t>.<raw body>")>`
- `X-Davit-Delivery: <deliveryId>`
- `X-Davit-Event: <type>`

**Verifying a delivery.** Recompute the HMAC over `t`, a dot and the raw body. Accept the delivery if it
matches any `v1` (compare in constant time) and `t` is within a few minutes of now. The SDKs do this for you:
`davit.verify_webhook(body, signature, secret)` in Python and `verifyWebhook(...)` in TypeScript.

**Rotation:** `rotate` issues a new secret. The old one keeps signing too (a second `v1`) until the overlap
ends, so receivers can switch over without dropping deliveries.

**Delivery semantics:**
- **At least once:** deduplicate by `deliveryId`.
- **Order:** each webhook gets events in `seq` order, but a retried delivery can arrive after later ones.
- **Success:** a 2xx answer. Redirects are not followed.
- **Retries:** failures back off exponentially from 2 s up to 1 h. After 12 attempts the delivery is
  dead-lettered; `webhook retry` sends it again.
- **Restarts:** pending deliveries are kept in the database and resume after a daemon restart.
- **Isolation:** each webhook has its own worker and a 10 s timeout per attempt. A slow or failing endpoint
  never delays VM operations or other webhooks.
- **History:** a new webhook gets events from its creation onward, with no replay. A disabled webhook skips
  events until it is enabled again.

**Security:**
- Webhooks are owner-only. Tokens cannot manage them.
- Signing secrets are stored in `<dataDir>/webhook-secrets` (0600 files), not in the database.
- URLs must be http(s) and must not carry credentials. Authenticate deliveries by their signature.
- Deliveries reach only public addresses by default, checked at connect time after DNS. A URL whose host is a
  loopback, private (RFC 1918), tailnet (CGNAT, ULA), link-local, metadata or multicast address is refused when the
  webhook is created or updated. A name that resolves to one is refused when delivering: the delivery is
  dead-lettered at once with `refused by policy`, without retries. A receiver on the host or LAN needs
  `network.allowPrivateUpstreams` ([network.md](network.md#credential-proxy-internalcredproxy)). Link-local and
  metadata addresses stay refused even then. **Behaviour change:** earlier versions delivered to any address.

## OTLP export (optional, off by default)

```json
{"telemetry": {"otlpEndpoint": "http://collector:4318", "otlpHeaders": {"Authorization": "Bearer …"}, "intervalSeconds": 10}}
```

- **Wire format:** OTLP/HTTP with JSON encoding, with no extra dependencies. Events go to `/v1/logs` (body =
  event type, attributes `davit.event.*`) and gauges to `/v1/metrics`.
- **Metrics:** `davit.vms` and `davit.webhook.deliveries` by `state`, plus the exporter's own
  exported/dropped/failed counters.
- **Isolation:** export runs beside the daemon with a bounded queue. If the collector is slow, down or
  failing, events are dropped and counted, never queued without bound, and the daemon never waits for it.
