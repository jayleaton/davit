// Events, webhooks and delivery verification against a scripted in-memory transport.

import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import { describe, test } from "node:test";

import {
  Internal,
  DavitClient,
  DavitError,
  WEBHOOK_SIGNATURE_HEADER,
  WebhookVerificationError,
  verifyWebhook,
  type Event,
  type Transport,
  type TransportRequest,
  type TransportResponse,
  type Webhook,
  type WebhookDelivery,
} from "../src/index.ts";

type Handler = (req: TransportRequest, n: number) => TransportResponse | Promise<TransportResponse>;

function fake(handler: Handler): Transport & { calls: TransportRequest[] } {
  const calls: TransportRequest[] = [];
  return {
    calls,
    async request(req) {
      calls.push(req);
      return handler(req, calls.length);
    },
  };
}

function json(status: number, body: unknown): TransportResponse {
  return { status, headers: {}, body: JSON.stringify(body) };
}

function query(req: TransportRequest): Record<string, string> {
  const i = req.path.indexOf("?");
  return i < 0 ? {} : Object.fromEntries(new URLSearchParams(req.path.slice(i + 1)));
}

function pathOf(req: TransportRequest): string {
  return req.path.split("?")[0]!;
}

const NOW = "2026-10-22T04:22:34Z";
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

function ev(seq: number, type = "vm.state"): Event {
  return { seq, type, time: NOW, vmId: "vm_1", subject: "vm_1", data: { state: "ready" } };
}

function hook(extra: Partial<Webhook> = {}): Webhook {
  return { id: "wh_1", url: "https://example.test/hook", events: ["vm."], enabled: true, createdAt: NOW, updatedAt: NOW, pending: 0, dead: 1, ...extra };
}

function delivery(extra: Partial<WebhookDelivery> = {}): WebhookDelivery {
  return { id: "dl_1", webhookId: "wh_1", eventSeq: 7, eventType: "vm.state", state: "dead", attempts: 5, lastStatus: 500, createdAt: NOW, ...extra };
}

describe("events", () => {
  test("listEvents maps filters to the query", async () => {
    const t = fake(() => json(200, { items: [ev(1)], nextCursor: "c1" }));
    const c = new DavitClient({ transport: t });
    const page = await c.listEvents({ limit: 10, types: ["vm.", "operation.state"], vmId: "web", start: "latest", waitMs: 5000 });
    assert.deepEqual(page.items.map((e) => e.seq), [1]);
    assert.equal(page.nextCursor, "c1");
    const r = t.calls[0]!;
    assert.equal(`${r.method} ${pathOf(r)}`, "GET /api/v1/events");
    assert.deepEqual(query(r), { limit: "10", types: "vm.,operation.state", vmId: "web", start: "latest", waitMs: "5000" });
    await c.listEvents({ types: "vm.", cursor: "c1" });
    assert.deepEqual(query(t.calls[1]!), { cursor: "c1", types: "vm." });
    await c.listEvents();
    assert.equal(t.calls[2]!.path, "/api/v1/events");
  });

  test("iterEvents pages until an empty page without long-polling", async () => {
    const pages = [
      { items: [ev(1), ev(2)], nextCursor: "c2" },
      { items: [ev(3)], nextCursor: "c3" },
      { items: [], nextCursor: "c3" },
    ];
    const t = fake((_, n) => json(200, pages[n - 1]));
    const c = new DavitClient({ transport: t });
    const got: number[] = [];
    for await (const e of c.iterEvents({ types: ["vm."], start: "oldest", limit: 2 })) got.push(e.seq);
    assert.deepEqual(got, [1, 2, 3]);
    const qs = t.calls.map(query);
    assert.deepEqual(qs.map((q) => q.cursor), [undefined, "c2", "c3"]);
    assert.deepEqual(qs.map((q) => q.start), ["oldest", undefined, undefined]);
    assert.ok(qs.every((q) => q.waitMs === undefined && q.types === "vm." && q.limit === "2"));
  });

  test("iterEvents follow long-polls through empty pages", async () => {
    const pages = [
      { items: [], nextCursor: "c0" },
      { items: [], nextCursor: "c0" },
      { items: [ev(5, "run.state")], nextCursor: "c5" },
      { items: [], nextCursor: "c5" },
      { items: [ev(6), ev(7)], nextCursor: "c7" },
    ];
    const t = fake((_, n) => json(200, pages[n - 1]));
    const c = new DavitClient({ transport: t });
    const got: number[] = [];
    for await (const e of c.iterEvents({ start: "latest", follow: true })) {
      got.push(e.seq);
      if (got.length === 3) break;
    }
    assert.deepEqual(got, [5, 6, 7]);
    assert.equal(t.calls.length, 5);
    const qs = t.calls.map(query);
    assert.deepEqual(qs.map((q) => q.cursor), [undefined, "c0", "c0", "c5", "c5"]);
    assert.equal(qs[0]!.start, "latest");
    assert.ok(qs.slice(1).every((q) => q.start === undefined));
    assert.ok(qs.every((q) => q.waitMs === "25000"));
  });

  test("iterEvents resumes from a cursor, checks waitMs and a missing nextCursor", async () => {
    const t = fake(() => json(200, { items: [], nextCursor: "c9" }));
    const c = new DavitClient({ transport: t });
    for await (const _ of c.iterEvents({ cursor: "c9", start: "latest" })) assert.fail("no events expected");
    assert.deepEqual(query(t.calls[0]!), { cursor: "c9" });
    await assert.rejects(c.iterEvents({ follow: true, waitMs: 30_001 }).next(), TypeError);

    const bad = new DavitClient({ transport: fake(() => json(200, { items: [ev(1)], nextCursor: "" })) });
    const it = bad.iterEvents();
    assert.equal((await it.next()).value?.seq, 1);
    await assert.rejects(it.next(), (err) => err instanceof Internal && /nextCursor/.test(err.message));
  });
});

describe("webhooks", () => {
  test("createWebhook maps the body and the idempotency key", async () => {
    const t = fake(() => json(201, { webhook: hook(), secret: "whs_s3cret" }));
    const c = new DavitClient({ transport: t });
    const got = await c.createWebhook("https://example.test/hook", { events: ["vm."], description: "ci" });
    assert.equal(got.secret, "whs_s3cret");
    assert.equal(got.webhook.dead, 1);
    const [a] = t.calls;
    assert.equal(`${a!.method} ${a!.path}`, "POST /api/v1/webhooks");
    assert.match(a!.headers["Idempotency-Key"]!, UUID);
    assert.deepEqual(JSON.parse(a!.body as string), { url: "https://example.test/hook", events: ["vm."], description: "ci" });

    await c.createWebhook("https://e.test/", { enabled: false, idempotencyKey: "k-1" });
    assert.equal(t.calls[1]!.headers["Idempotency-Key"], "k-1");
    assert.deepEqual(JSON.parse(t.calls[1]!.body as string), { url: "https://e.test/", enabled: false });
    await assert.rejects(c.createWebhook("https://e.test/", { events: "vm." as unknown as string[] }), TypeError);
    assert.equal(t.calls.length, 2);
  });

  test("list/iter, get, update, delete and rotate", async () => {
    const t = fake((req) => {
      const p = pathOf(req);
      if (req.method === "DELETE") return { status: 204, headers: {}, body: "" };
      if (p === "/api/v1/webhooks") return json(200, query(req).cursor ? { items: [hook({ id: "wh/2" })] } : { items: [hook()], nextCursor: "w1" });
      if (p.endsWith("/rotate-secret")) return json(200, { webhook: hook(), secret: "whs_new" });
      return json(200, hook({ enabled: false }));
    });
    const c = new DavitClient({ transport: t });
    const hooks: Webhook[] = [];
    for await (const h of c.iterWebhooks({ limit: 1 })) hooks.push(h);
    assert.deepEqual(hooks.map((h) => h.id), ["wh_1", "wh/2"]);
    assert.deepEqual(query(t.calls[0]!), { limit: "1" });
    assert.deepEqual(query(t.calls[1]!), { limit: "1", cursor: "w1" });
    assert.equal((await c.listWebhooks()).nextCursor, "w1");

    t.calls.length = 0;
    assert.equal((await c.getWebhook("wh_1")).enabled, false);
    await c.updateWebhook(hooks[1]!, { enabled: false, events: [] });
    await c.updateWebhook("wh_1", { url: "https://new.test/", description: "" });
    assert.equal(await c.deleteWebhook(hooks[0]!), undefined);
    const rotated = await c.rotateWebhookSecret("wh_1", { overlapSeconds: 3600 });
    await c.rotateWebhookSecret(rotated, { idempotencyKey: "rot-1" });
    assert.equal(rotated.secret, "whs_new");

    const [get, upd1, upd2, del, rot1, rot2] = t.calls;
    assert.equal(`${get!.method} ${get!.path}`, "GET /api/v1/webhooks/wh_1");
    assert.equal(`${upd1!.method} ${upd1!.path}`, "PATCH /api/v1/webhooks/wh%2F2");
    assert.deepEqual(JSON.parse(upd1!.body as string), { events: [], enabled: false });
    assert.deepEqual(JSON.parse(upd2!.body as string), { url: "https://new.test/", description: "" });
    assert.equal(upd1!.headers["Idempotency-Key"], undefined);
    assert.equal(`${del!.method} ${del!.path}`, "DELETE /api/v1/webhooks/wh_1");
    assert.equal(del!.body, undefined);
    assert.equal(del!.headers["Idempotency-Key"], undefined);
    assert.equal(`${rot1!.method} ${rot1!.path}`, "POST /api/v1/webhooks/wh_1/rotate-secret");
    assert.deepEqual(JSON.parse(rot1!.body as string), { overlapSeconds: 3600 });
    assert.match(rot1!.headers["Idempotency-Key"]!, UUID);
    assert.equal(rot2!.path, "/api/v1/webhooks/wh_1/rotate-secret");
    assert.deepEqual(JSON.parse(rot2!.body as string), {});
    assert.equal(rot2!.headers["Idempotency-Key"], "rot-1");
  });

  test("deliveries: state filter, paging and retry", async () => {
    const t = fake((req) => {
      if (req.method === "POST") return json(200, delivery({ state: "pending", attempts: 0 }));
      return json(200, query(req).cursor ? { items: [delivery({ id: "dl_2" })] } : { items: [delivery()], nextCursor: "d1" });
    });
    const c = new DavitClient({ transport: t });
    const dead: WebhookDelivery[] = [];
    for await (const d of c.iterWebhookDeliveries({ webhook: hook(), secret: "s" }, { state: "dead", limit: 5 })) dead.push(d);
    assert.deepEqual(dead.map((d) => d.id), ["dl_1", "dl_2"]);
    assert.equal(pathOf(t.calls[0]!), "/api/v1/webhooks/wh_1/deliveries");
    assert.deepEqual(query(t.calls[0]!), { limit: "5", state: "dead" });
    assert.deepEqual(query(t.calls[1]!), { limit: "5", cursor: "d1", state: "dead" });
    await c.listWebhookDeliveries("wh_1");
    assert.equal(t.calls[2]!.path, "/api/v1/webhooks/wh_1/deliveries");

    const again = await c.retryWebhookDelivery("wh_1", dead[0]!);
    assert.equal(again.state, "pending");
    const r = t.calls.at(-1)!;
    assert.equal(`${r.method} ${r.path}`, "POST /api/v1/webhooks/wh_1/deliveries/dl_1/retry");
    assert.equal(r.body, undefined);
    assert.equal(r.headers["Idempotency-Key"], undefined);
  });
});

describe("verifyWebhook", () => {
  const BODY = '{"deliveryId":"dl_1","webhookId":"wh_1","event":{"seq":7}}';
  const T = 1_792_000_000;
  const sign = (secret: string, t = T, body = BODY) => createHmac("sha256", secret).update(`${t}.${body}`).digest("hex");
  const reasonIs = (reason: string) => (err: unknown) => err instanceof WebhookVerificationError && err.reason === reason;

  test("accepts a valid signature", () => {
    const header = `t=${T},v1=${sign("whs_a")}`;
    verifyWebhook(BODY, header, "whs_a", { now: T });
    verifyWebhook(new TextEncoder().encode(BODY), header, ["whs_a"], { now: T + 300 });
    verifyWebhook(BODY, ` t=${T} , v1=${sign("whs_a").toUpperCase()} `, "whs_a", { now: new Date(T * 1000) });
    // Unknown schemes are ignored.
    verifyWebhook(BODY, `t=${T},v0=zz,v1=${sign("whs_a")}`, "whs_a", { now: T });
    assert.equal(WEBHOOK_SIGNATURE_HEADER, "X-Davit-Signature");
  });

  test("rejects a wrong secret, a tampered body and a changed timestamp", () => {
    const header = `t=${T},v1=${sign("whs_a")}`;
    assert.throws(() => verifyWebhook(BODY, header, "whs_b", { now: T }), (err) => reasonIs("signature")(err) && err instanceof DavitError);
    assert.throws(() => verifyWebhook(BODY.replace("7", "8"), header, "whs_a", { now: T }), reasonIs("signature"));
    assert.throws(() => verifyWebhook(BODY, `t=${T + 1},v1=${sign("whs_a")}`, "whs_a", { now: T }), reasonIs("signature"));
  });

  test("rotation: either secret verifies a pair", () => {
    const header = `t=${T},v1=${sign("whs_old")},v1=${sign("whs_new")}`;
    verifyWebhook(BODY, header, "whs_old", { now: T });
    verifyWebhook(BODY, header, "whs_new", { now: T });
    verifyWebhook(BODY, header, ["whs_other", "whs_new"], { now: T });
    verifyWebhook(BODY, `t=${T},v1=${sign("whs_old")}`, ["whs_new", "whs_old"], { now: T });
    assert.throws(() => verifyWebhook(BODY, header, ["whs_x", "whs_y"], { now: T }), reasonIs("signature"));
  });

  test("rejects stale and future timestamps", () => {
    const header = `t=${T},v1=${sign("whs_a")}`;
    for (const now of [T + 301, T - 301]) {
      assert.throws(() => verifyWebhook(BODY, header, "whs_a", { now }), reasonIs("timestamp"));
      verifyWebhook(BODY, header, "whs_a", { now, toleranceSeconds: 301 });
    }
  });

  test("rejects malformed headers", () => {
    const v = sign("whs_a");
    for (const header of [null, undefined, "", "garbage", `v1=${v}`, `t=${T}`, `t=abc,v1=${v}`, `t=-${T},v1=${v}`, `t=${T},t=${T},v1=${v}`, `t=${T},v1=xyz`, `t=${T},v1=${v.slice(0, -2)}`, `t=${T},v1`]) {
      assert.throws(() => verifyWebhook(BODY, header, "whs_a", { now: T }), reasonIs("malformed"), String(header));
    }
  });

  test("argument errors", () => {
    const header = `t=${T},v1=${sign("whs_a")}`;
    for (const bad of [[], "", ["whs_a", ""]]) assert.throws(() => verifyWebhook(BODY, header, bad, { now: T }), TypeError);
    assert.throws(() => verifyWebhook(BODY, header, "whs_a", { now: T, toleranceSeconds: -1 }), TypeError);
  });
});
