// Prompt and conversation unit tests against a scripted transport.

import assert from "node:assert/strict";
import { test } from "node:test";

import { DavitClient, TransportError, WaitTimeout, type Prompt, type PromptEvent, type Transport, type TransportRequest } from "../src/index.ts";

function fake(handler: (req: TransportRequest, n: number) => unknown | Promise<unknown>): Transport & { calls: TransportRequest[] } {
  const calls: TransportRequest[] = [];
  return {
    calls,
    async request(req) {
      calls.push(req);
      const out = await handler(req, calls.length);
      return { status: req.method === "POST" ? 201 : 200, headers: {}, body: JSON.stringify(out) };
    },
  };
}

function prompt(state: string, extra: Partial<Prompt> = {}): Prompt {
  return { id: "prm_1", conversationId: "conv_1", vmId: "vm_1", harness: "claude", state, eventCursor: 0, createdAt: "2026-01-01T00:00:00Z", ...extra };
}

function ev(seq: number, type: string, extra: Partial<PromptEvent> = {}): PromptEvent {
  return { seq, time: "2026-01-01T00:00:00Z", promptId: "prm_1", conversationId: "conv_1", type, ...extra };
}

const query = (r: TransportRequest) => new URL(r.path, "http://x").searchParams;

test("createPrompt body: text plus only the options given; an idempotency key always", async () => {
  const t = fake(() => prompt("starting"));
  const c = new DavitClient({ transport: t });
  await c.createPrompt("web", "hi", { harness: "codex", new: true, env: { A: "1" }, secrets: ["TOKEN"] });
  await c.createPrompt("web", "hi", { new: false });
  const r = t.calls[0]!;
  assert.equal(r.method, "POST");
  assert.equal(r.path, "/api/v1/vms/web/prompts");
  assert.deepEqual(JSON.parse(r.body as string), { text: "hi", harness: "codex", new: true, env: { A: "1" }, secrets: ["TOKEN"] });
  assert.match(r.headers["Idempotency-Key"]!, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
  assert.notEqual(t.calls[1]!.headers["Idempotency-Key"], r.headers["Idempotency-Key"], "a fresh key per call");
  await c.createPrompt("web", "hi", { idempotencyKey: "mine" });
  assert.equal(t.calls[2]!.headers["Idempotency-Key"], "mine");
  assert.deepEqual(JSON.parse(t.calls[1]!.body as string), { text: "hi" }, "new is sent only when true");
  await assert.rejects(c.createPrompt("web", ""), TypeError);
});

test("paths of get, cancel, events, conversations", async () => {
  const t = fake((req) => (req.path.includes("conversations") && !req.path.includes("conv_") ? { items: [] } : req.path.includes("/events") ? { prompt: prompt("running"), items: [] } : prompt("running")));
  const c = new DavitClient({ transport: t });
  await c.getPrompt("web", "prm_1");
  await c.cancelPrompt("web", "prm_1");
  await c.listPromptEvents("web", "prm_1", { after: 4, waitMs: 1500.7 });
  await c.listConversations("web", { limit: 5 });
  await c.getConversation("web", "conv_1");
  assert.deepEqual(
    t.calls.map((r) => `${r.method} ${r.path}`),
    [
      "GET /api/v1/vms/web/prompts/prm_1",
      "DELETE /api/v1/vms/web/prompts/prm_1",
      "GET /api/v1/vms/web/prompts/prm_1/events?after=4&waitMs=1500",
      "GET /api/v1/vms/web/conversations?limit=5",
      "GET /api/v1/vms/web/conversations/conv_1",
    ],
  );
});

test("an unknown future state counts as terminal (like the CLI)", async () => {
  const t = fake(() => ({ prompt: prompt("archived"), items: [] }));
  const p = await new DavitClient({ transport: t }).waitPrompt("web", "prm_1");
  assert.equal(p.state, "archived");
  assert.equal(t.calls.length, 1);
});

test("followPrompt stops only on an empty page of a terminal turn", async () => {
  const pages = [
    { prompt: prompt("running"), items: [ev(1, "conversation.started"), ev(2, "message.delta", { role: "assistant", text: "a" })] },
    // terminal but with items: must keep reading
    { prompt: prompt("succeeded"), items: [ev(3, "future.type"), ev(4, "turn.completed", { status: "succeeded" })] },
    { prompt: prompt("succeeded", { eventCursor: 4 }), items: [] },
  ];
  const t = fake((_req, n) => pages[n - 1]);
  const c = new DavitClient({ transport: t });
  const it = c.followPrompt("web", "prm_1");
  const seen: string[] = [];
  let r = await it.next();
  while (!r.done) {
    seen.push(r.value.type);
    r = await it.next();
  }
  assert.deepEqual(seen, ["conversation.started", "message.delta", "future.type", "turn.completed"]);
  assert.equal(r.value.eventCursor, 4);
  assert.deepEqual(t.calls.map((x) => query(x).get("after")), ["0", "2", "4"]);
  for (const x of t.calls) assert.ok(Number(query(x).get("waitMs")) <= 25000);
});

test("runPrompt(): events in order via onEvent; failed turns are returned", async () => {
  const t = fake((req) => {
    if (req.method === "POST") return prompt("starting");
    if (query(req).get("after") === "0") {
      return {
        prompt: prompt("failed"),
        items: [
          ev(1, "message.delta", { role: "assistant", text: "Hel" }),
          ev(2, "message.delta", { role: "user", text: "ignored" }),
          ev(3, "message.delta", { text: "lo" }),
          ev(4, "turn.completed", { status: "failed", error: { code: "harness_failed", message: "x" } }),
        ],
      };
    }
    return { prompt: prompt("failed", { error: { code: "harness_failed", message: "x" } }), items: [] };
  });
  const seen: number[] = [];
  const r = await new DavitClient({ transport: t }).runPrompt("web", "hi", { onEvent: (e) => seen.push(e.seq) });
  const p = r.prompt;
  assert.deepEqual(seen, [1, 2, 3, 4]);
  assert.deepEqual(r.events.map((e) => e.seq), [1, 2, 3, 4]);
  assert.equal(p.state, "failed");
  assert.equal(p.error?.code, "harness_failed");
});

test("the follow budget throws WaitTimeout carrying the prompt id, and sends no cancel", async () => {
  const t = fake(
    (req) =>
      new Promise((resolve, reject) => {
        const timer = setTimeout(() => resolve({ prompt: prompt("running"), items: [] }), 5000);
        req.signal?.addEventListener("abort", () => {
          clearTimeout(timer);
          reject(req.signal!.reason);
        });
      }),
  );
  const err = await new DavitClient({ transport: t }).waitPrompt("web", "prm_1", { timeoutMs: 50 }).catch((e: unknown) => e);
  assert.ok(err instanceof WaitTimeout, String(err));
  assert.equal(err.operationId, undefined);
  assert.deepEqual(err.details, { promptId: "prm_1", cursor: 0 });
  assert.ok(t.calls.every((r) => r.method === "GET"));
});

test("createPrompt reuses its key on never-reached-the-daemon retries; runPrompt passes the key", async () => {
  const t = fake((req, n) => {
    if (req.method === "POST" && n < 3) throw new TransportError("refused", { connectFailed: true });
    if (req.method === "POST") return prompt("starting");
    return { prompt: prompt("succeeded"), items: [] };
  });
  const c = new DavitClient({ transport: t });
  await c.createPrompt("web", "hi");
  const keys = t.calls.map((r) => r.headers["Idempotency-Key"]);
  assert.equal(keys.length, 3);
  assert.equal(new Set(keys).size, 1);
  await c.runPrompt("web", "hi", { idempotencyKey: "run-key" });
  assert.equal(t.calls.find((r) => r.method === "POST" && r.headers["Idempotency-Key"] === "run-key")?.path, "/api/v1/vms/web/prompts");
  assert.ok(t.calls.filter((r) => r.method === "GET").every((r) => r.headers["Idempotency-Key"] === undefined));
});

test("conversations page with limit and cursor; the iterator follows nextCursor", async () => {
  const t = fake((req) => {
    const cursor = query(req).get("cursor");
    if (!cursor) return { items: [{ id: "conv_1" }], nextCursor: "c2" };
    if (cursor === "c2") return { items: [{ id: "conv_2" }], nextCursor: "c3" };
    return { items: [], nextCursor: "" };
  });
  const c = new DavitClient({ transport: t });
  await c.listConversations("web", { limit: 1, cursor: "abc" });
  assert.equal(t.calls[0]!.path, "/api/v1/vms/web/conversations?limit=1&cursor=abc");
  const ids: string[] = [];
  for await (const conv of c.iterConversations("web", { limit: 1 })) ids.push(conv.id);
  assert.deepEqual(ids, ["conv_1", "conv_2"]);
  assert.deepEqual(t.calls.slice(1).map((r) => r.path), [
    "/api/v1/vms/web/conversations?limit=1",
    "/api/v1/vms/web/conversations?limit=1&cursor=c2",
    "/api/v1/vms/web/conversations?limit=1&cursor=c3",
  ]);
});

test("naturally idempotent calls send no Idempotency-Key (killProcess, cancelPrompt, deleteSnapshot)", async () => {
  const t = fake(() => ({}));
  const c = new DavitClient({ transport: t });
  await c.killProcess("web", "proc_1", "KILL");
  await c.cancelPrompt("web", "prm_1");
  await c.deleteSnapshot("base");
  assert.equal(t.calls[0]!.path, "/api/v1/vms/web/processes/proc_1?signal=KILL");
  for (const r of t.calls) assert.equal(r.headers["Idempotency-Key"], undefined, `${r.method} ${r.path}`);
});

test("prompt controls: createPrompt fields, steer, interrupt and the VM feed", async () => {
  const t = fake((req) =>
    req.path.includes("/steer")
      ? { delivery: "native", prompt: prompt("running") }
      : req.path.includes("/interrupt")
        ? { cancelled: [] }
        : req.path.includes("/prompt-events")
          ? { items: [], nextCursor: "c2" }
          : prompt("starting"),
  );
  const c = new DavitClient({ transport: t });
  const attachments = [{ name: "a.png", mediaType: "image/png", data: "iVBO" }];
  await c.createPrompt("web", "hi", { attachments, reasoningEffort: "high", fast: false, turnTimeoutSeconds: 60 });
  await c.createPrompt("web", "hi", { fast: true });
  assert.deepEqual(JSON.parse(t.calls[0]!.body as string), { text: "hi", attachments, options: { reasoningEffort: "high", fast: false }, turnTimeoutSeconds: 60 });
  assert.deepEqual(JSON.parse(t.calls[1]!.body as string), { text: "hi", options: { fast: true } });

  assert.equal((await c.steerPrompt("web", "prm_1", "more")).delivery, "native");
  await c.interruptPrompts("web", { all: true });
  await c.interruptPrompts("web", { conversationId: "conv_1" });
  await c.listVMPromptEvents("web");
  await c.listVMPromptEvents("web", { after: "c1", conversationId: "conv_1", waitMs: 10.5, limit: 2 });
  const calls = t.calls.slice(2);
  assert.deepEqual(
    calls.map((r) => `${r.method} ${r.path}`),
    [
      "POST /api/v1/vms/web/prompts/prm_1/steer",
      "POST /api/v1/vms/web/interrupt",
      "POST /api/v1/vms/web/interrupt",
      "GET /api/v1/vms/web/prompt-events",
      "GET /api/v1/vms/web/prompt-events?after=c1&conversationId=conv_1&waitMs=10&limit=2",
    ],
  );
  assert.deepEqual(calls.slice(0, 3).map((r) => JSON.parse(r.body as string)), [{ text: "more" }, { all: true }, { conversationId: "conv_1" }]);
  assert.match(String(calls[0]!.headers["Idempotency-Key"]), /^[0-9a-f-]{36}$/, "steer sends a generated key");
  for (const r of calls.slice(1, 3)) assert.equal(r.headers["Idempotency-Key"], undefined, "interrupt sends no key");
  await c.steerPrompt("web", "prm_1", "more", { idempotencyKey: "steer-1" });
  assert.equal(t.calls.at(-1)!.headers["Idempotency-Key"], "steer-1");
  assert.throws(() => c.interruptPrompts("web", {}), TypeError);
  assert.throws(() => c.interruptPrompts("web", { all: true, conversationId: "conv_1" }), TypeError);
  assert.throws(() => c.steerPrompt("web", "prm_1", ""), TypeError);
});

test("followVMPromptEvents keeps the cursor across pages, including empty ones", async () => {
  const pages = [
    { items: [{ cursor: "c1", event: ev(1, "conversation.started") }], nextCursor: "c1" },
    { items: [], nextCursor: "c1b" },
    { items: [{ cursor: "c2", event: ev(2, "turn.completed") }], nextCursor: "c2" },
  ];
  const t = fake((_req, n) => pages[n - 1]);
  const seen: string[] = [];
  for await (const item of new DavitClient({ transport: t }).followVMPromptEvents("web", { after: "c0" })) {
    seen.push(item.cursor);
    if (item.event.type === "turn.completed") break;
  }
  assert.deepEqual(seen, ["c1", "c2"]);
  assert.deepEqual(t.calls.map((r) => query(r).get("after")), ["c0", "c1", "c1b"]);
  assert.ok(t.calls.every((r) => query(r).get("waitMs") === "25000"));
});
