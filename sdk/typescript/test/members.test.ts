// Members and shares (G13) wrappers against a scripted in-memory transport.

import assert from "node:assert/strict";
import { describe, test } from "node:test";

import {
  NotFound,
  PermissionDenied,
  DavitClient,
  UnsupportedCapability,
  type Member,
  type Share,
  type Transport,
  type TransportRequest,
  type TransportResponse,
} from "../src/index.ts";

const NOW = "2026-10-22T04:22:34Z";

function member(login = "alice@example.com", role = "viewer"): Member {
  return { id: "mem_1", login, userId: 42, role, addedAt: NOW };
}

/** A share; `role: null` leaves the role out (each member's own role). */
function share(memberId = "mem_1", kind = "vm", role: string | null = "exec", id = "shr_1"): Share {
  const s: Share = { id, kind, resourceId: "vm_1", member: memberId, credentials: false, createdAt: NOW };
  if (role !== null) s.role = role;
  return s;
}

function json(status: number, body: unknown): TransportResponse {
  return { status, headers: {}, body: JSON.stringify(body) };
}

/** Members echo the request; shares list `shares`, create echoes kind/role/credentials; delete 204. */
function api(shares: Share[] = [share()]): Transport & { calls: TransportRequest[] } {
  const calls: TransportRequest[] = [];
  return {
    calls,
    async request(req) {
      calls.push(req);
      const body = req.body === undefined ? {} : (JSON.parse(req.body as string) as Record<string, string | boolean>);
      if (req.method === "DELETE") return { status: 204, headers: {}, body: "" };
      if (req.path.startsWith("/api/v1/members")) {
        if (req.method === "GET") return json(200, { items: [member()] });
        if (req.method === "POST") return json(201, member(body.login as string, body.role as string));
        return json(200, member(undefined, body.role as string));
      }
      if (req.method === "GET") return json(200, { items: shares });
      const reply: Share = { ...share("mem_1", body.kind as string, null), credentials: body.credentials === true };
      if (body.role !== undefined) reply.role = body.role as string;
      return json(201, reply);
    },
  };
}

const shape = (r: TransportRequest) => [r.method, r.path, r.body === undefined ? undefined : JSON.parse(r.body as string)];

describe("members and shares", () => {
  test("member wrappers send the contract shapes; logins are escaped", async () => {
    const t = api();
    const c = new DavitClient({ transport: t });
    const listed = (await c.listMembers()).items;
    assert.deepEqual(listed.map((m) => [m.id, m.login, m.userId, m.role]), [["mem_1", "alice@example.com", 42, "viewer"]]);
    const added = await c.addMember("alice@example.com", "exec");
    assert.equal(added.role, "exec");
    assert.equal((await c.updateMember(added, "admin")).role, "admin");
    await c.updateMember("alice@example.com", "viewer");
    await c.removeMember(added);
    await c.removeMember("alice@example.com");
    assert.deepEqual(t.calls.map(shape), [
      ["GET", "/api/v1/members", undefined],
      ["POST", "/api/v1/members", { login: "alice@example.com", role: "exec" }],
      ["PATCH", "/api/v1/members/mem_1", { role: "admin" }],
      ["PATCH", "/api/v1/members/alice%40example.com", { role: "viewer" }],
      ["DELETE", "/api/v1/members/mem_1", undefined],
      ["DELETE", "/api/v1/members/alice%40example.com", undefined],
    ]);
    for (const r of t.calls) assert.equal(r.headers["Idempotency-Key"], undefined);
  });

  test("share wrappers send the contract shapes", async () => {
    const t = api();
    const c = new DavitClient({ transport: t });
    assert.deepEqual((await c.listShares()).items.map((s) => s.id), ["shr_1"]);
    await c.listShares("vm", "web");
    await c.listShares("snapshot");
    const created = await c.createShare("vm", "web", member(), "exec", {});
    assert.equal(created.role, "exec");
    assert.equal((await c.createShare("vm", "web", member(), { role: "admin", credentials: true })).credentials, true);
    assert.equal((await c.shareVM("web", "*")).role, undefined);
    await c.shareSnapshot("snap", "alice@example.com", "viewer");
    await c.shareEnvironment("env_1", "mem_1", { role: "admin", credentials: false });
    await c.deleteShare(created);
    await c.deleteShare("shr/2");
    assert.deepEqual(t.calls.map(shape), [
      ["GET", "/api/v1/shares", undefined],
      ["GET", "/api/v1/shares?kind=vm&resource=web", undefined],
      ["GET", "/api/v1/shares?kind=snapshot", undefined],
      ["POST", "/api/v1/shares", { kind: "vm", resource: "web", member: "mem_1", role: "exec" }],
      ["POST", "/api/v1/shares", { kind: "vm", resource: "web", member: "mem_1", role: "admin", credentials: true }],
      ["POST", "/api/v1/shares", { kind: "vm", resource: "web", member: "*" }],
      ["POST", "/api/v1/shares", { kind: "snapshot", resource: "snap", member: "alice@example.com", role: "viewer" }],
      ["POST", "/api/v1/shares", { kind: "environment", resource: "env_1", member: "mem_1", role: "admin" }],
      ["DELETE", "/api/v1/shares/shr_1", undefined],
      ["DELETE", "/api/v1/shares/shr%2F2", undefined],
    ]);
  });

  test("query parameters are escaped; empty refs never reach the server", async () => {
    const t = api();
    const c = new DavitClient({ transport: t });
    await c.listShares("vm", "my vm&x=1");
    assert.deepEqual(t.calls.map((r) => r.path), ["/api/v1/shares?kind=vm&resource=my+vm%26x%3D1"]);
    await assert.rejects(c.removeMember(""), /member must be a non-empty ID or name/);
    await assert.rejects(c.addMember("", "viewer"), /login must be a non-empty string/);
    await assert.rejects(c.deleteShare(""), /share must be a non-empty ID or name/);
    await assert.rejects(c.shareVM("", "*"), /resource must be a non-empty ID or name/);
    assert.equal(t.calls.length, 1);
  });

  test("unshare finds the share by member ID, object or *", async () => {
    const t = api([share("*", "vm", null, "shr_all"), share("mem_1", "vm", "exec", "shr_1")]);
    const c = new DavitClient({ transport: t });
    await c.unshare("vm", "web", member());
    await c.unshare("vm", "web", "*");
    assert.deepEqual(t.calls.map(shape), [
      ["GET", "/api/v1/shares?kind=vm&resource=web", undefined],
      ["DELETE", "/api/v1/shares/shr_1", undefined],
      ["GET", "/api/v1/shares?kind=vm&resource=web", undefined],
      ["DELETE", "/api/v1/shares/shr_all", undefined],
    ]);
  });

  test("unshare resolves a login through listMembers", async () => {
    const t = api([share("mem_1", "snapshot", "viewer", "shr_9")]);
    const c = new DavitClient({ transport: t });
    await c.unshare("snapshot", "snap", "alice@example.com");
    assert.deepEqual(t.calls.map(shape), [
      ["GET", "/api/v1/shares?kind=snapshot&resource=snap", undefined],
      ["GET", "/api/v1/members", undefined],
      ["DELETE", "/api/v1/shares/shr_9", undefined],
    ]);
  });

  test("unshare without a matching share is NotFound", async () => {
    const t = api([share("mem_2")]);
    const c = new DavitClient({ transport: t });
    await assert.rejects(c.unshare("vm", "web", "bob"), (e: unknown) => e instanceof NotFound && /not shared with bob/.test(e.message));
    await assert.rejects(c.unshare("vm", "web", "*"), NotFound);
    assert.ok(!t.calls.some((r) => r.method === "DELETE"));
  });

  test("422 and 403 map to the typed errors", async () => {
    const t: Transport = {
      async request(req) {
        if (req.method === "POST") return json(422, { error: { code: "unsupported_capability", message: "needs the tailnet gateway", requestId: "req_srv" } });
        return json(403, { error: { code: "permission_denied", message: "owner only", requestId: "req_srv" } });
      },
    };
    const c = new DavitClient({ transport: t });
    const unsupported = await c.addMember("alice@example.com", "viewer").catch((e: unknown) => e);
    assert.ok(unsupported instanceof UnsupportedCapability);
    assert.equal(unsupported.status, 422);
    const denied = await c.listShares().catch((e: unknown) => e);
    assert.ok(denied instanceof PermissionDenied);
    assert.equal(denied.status, 403);
    assert.equal(denied.requestId, "req_srv");
  });
});
