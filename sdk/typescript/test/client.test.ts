// Unit tests against a scripted in-memory transport (no daemon needed).

import { join } from "node:path";
import { tmpdir } from "node:os";
import assert from "node:assert/strict";
import { createServer } from "node:net";
import { describe, test } from "node:test";

import {
  Conflict,
  FetchTransport,
  NotFound,
  OperationFailed,
  RevisionMismatch,
  DavitClient,
  DavitError,
  StopRequired,
  TransportError,
  UnixSocketTransport,
  Unavailable,
  WaitTimeout,
  parseEnvironmentRef,
  resolveDefaultEndpoint,
  type Operation,
  type Transport,
  type TransportRequest,
  type TransportResponse,
} from "../src/index.ts";
import { startServer } from "./ws-server.ts";

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

function json(status: number, body: unknown, headers: Record<string, string> = {}): TransportResponse {
  return { status, headers, body: JSON.stringify(body) };
}

function op(state: string, extra: Partial<Operation> = {}): Operation {
  return {
    id: "op_1", kind: "create", vmId: "vm_1", owner: "me", state, phase: "", progress: 0,
    createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-01T00:00:00Z", ...extra,
  };
}

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

describe("requests", () => {
  test("headers, idempotency key, If-Match and token", async () => {
    const t = fake(() => json(202, { operation: op("queued") }));
    const c = new DavitClient({ transport: t, token: "sekrit" });
    await c.stopVM("web", { wait: false, ifMatch: 7, force: true });
    const r = t.calls[0]!;
    assert.equal(r.method, "POST");
    assert.equal(r.path, "/api/v1/vms/web/stop");
    assert.match(r.headers["X-Request-Id"]!, /^req_[0-9a-f]{16}$/);
    assert.match(r.headers["Idempotency-Key"]!, UUID);
    assert.equal(r.headers["If-Match"], "7");
    assert.equal(r.headers.Authorization, "Bearer sekrit");
    assert.equal(r.headers["Content-Type"], "application/json");
    assert.deepEqual(JSON.parse(r.body as string), { force: true });
  });

  test("recoverVM posts with a key and If-Match, then waits like resume", async () => {
    const t = fake(() => json(202, { operation: op("queued", { kind: "recover" }) }));
    const c = new DavitClient({ transport: t });
    const acc = await c.recoverVM("a/b", { wait: false, ifMatch: 4, idempotencyKey: "k-r" });
    assert.equal(acc.operation.kind, "recover");
    const r = t.calls[0]!;
    assert.equal(`${r.method} ${r.path}`, "POST /api/v1/vms/a%2Fb/recover");
    assert.equal(r.headers["Idempotency-Key"], "k-r");
    assert.equal(r.headers["If-Match"], "4");
    assert.deepEqual(JSON.parse(r.body as string), {});

    const done = op("succeeded", { kind: "recover", result: { durationMs: 5, vmId: "vm_1", bootMode: "cold_boot" } });
    const vm = { id: "vm_1", name: "web", state: "ready", instanceGeneration: 2 };
    const w = fake((req) => {
      if (req.method === "POST") return json(202, { operation: op("queued", { kind: "recover" }) });
      if (req.path.includes("/events")) return json(200, { operation: done, items: [] });
      return json(200, vm);
    });
    const got = await new DavitClient({ transport: w }).recoverVM("web");
    assert.equal(got.instanceGeneration, 2);
    assert.match(w.calls[0]!.headers["Idempotency-Key"]!, UUID);
    assert.equal(w.calls.at(-1)!.path, "/api/v1/vms/vm_1");
  });

  test("issueSSHCredential maps the request and the credential", async () => {
    const cred = {
      certificate: "ssh-ed25519-cert-v01@openssh.com AAAA", guestUser: "root", generation: 2,
      hostKeys: ["ssh-ed25519 AAAAhost"], sshTarget: "web.ts.net:22", expiresAt: "2026-01-01T00:00:00Z",
    };
    const t = fake(() => json(201, cred));
    const c = new DavitClient({ transport: t });
    assert.deepEqual(await c.issueSSHCredential("web", { publicKey: "ssh-ed25519 AAAAkey", guestUser: "root", ttlSeconds: 60 }), cred);
    await c.issueSSHCredential("web", { publicKey: "ssh-ed25519 AAAAkey" });
    const [a, b] = t.calls;
    assert.equal(`${a!.method} ${a!.path}`, "POST /api/v1/vms/web/ssh-credentials");
    assert.match(a!.headers["X-Request-Id"]!, /^req_/);
    assert.equal(a!.headers["Idempotency-Key"], undefined);
    assert.deepEqual(JSON.parse(a!.body as string), { publicKey: "ssh-ed25519 AAAAkey", guestUser: "root", ttlSeconds: 60 });
    assert.deepEqual(JSON.parse(b!.body as string), { publicKey: "ssh-ed25519 AAAAkey" });
    await assert.rejects(c.issueSSHCredential("web", { publicKey: "k", ttlSeconds: 601 }), TypeError);
  });

  test("whoami and rotateToken", async () => {
    const tok = { id: "tok_2", name: "ci", scopes: [{ actions: ["exec"] }], createdAt: "2026-01-01T00:00:00Z", expiresAt: "2026-01-02T00:00:00Z", useCount: 0, secret: "slw_new" };
    const t = fake((req) => (req.path.endsWith("/me") ? json(200, { principal: "p", auth: "token", host: "h", version: "v" }) : json(201, tok)));
    const c = new DavitClient({ transport: t });
    assert.equal((await c.whoami()).auth, "token");
    assert.equal((await c.rotateToken("tok_1", { overlapSeconds: 600, idempotencyKey: "k" })).secret, "slw_new");
    await c.rotateToken({ id: "tok_1" } as never);
    const [me, rot, plain] = t.calls;
    assert.equal(`${me!.method} ${me!.path}`, "GET /api/v1/me");
    assert.equal(`${rot!.method} ${rot!.path}`, "POST /api/v1/tokens/tok_1/rotate");
    assert.deepEqual(JSON.parse(rot!.body as string), { overlapSeconds: 600 });
    assert.equal(rot!.headers["Idempotency-Key"], "k");
    assert.deepEqual(JSON.parse(plain!.body as string), {});
    assert.ok(plain!.headers["Idempotency-Key"]);
    await assert.rejects(c.rotateToken("tok_1", { overlapSeconds: -1 }), TypeError);
  });

  test("createToken maps scopes, ttl and the idempotency key", async () => {
    const tok = { id: "tok_1", name: "ci", scopes: [{ vmId: "vm_1", actions: ["exec"] }], createdAt: "2026-01-01T00:00:00Z", expiresAt: "2026-01-02T00:00:00Z", secret: "slw_s3cret" };
    const t = fake(() => json(201, tok));
    const c = new DavitClient({ transport: t });
    assert.deepEqual(await c.createToken("ci", { actions: ["exec"], vm: "web", ttlSeconds: 3600 }), tok);
    await c.createToken("a", { actions: ["view"], vm: ["web", "db"], ttlSeconds: 1 });
    await c.createToken("b", { actions: ["admin"], ttlSeconds: 60, idempotencyKey: "k-1" });
    await c.createToken("c", { scopes: [{ actions: ["view"] }, { vmId: "web", actions: ["exec"] }], ttlSeconds: 60 });
    const [a, b, d, e] = t.calls;
    assert.equal(`${a!.method} ${a!.path}`, "POST /api/v1/tokens");
    assert.match(a!.headers["Idempotency-Key"]!, UUID);
    assert.deepEqual(JSON.parse(a!.body as string), { name: "ci", scopes: [{ vmId: "web", actions: ["exec"] }], ttlSeconds: 3600 });
    assert.deepEqual(JSON.parse(b!.body as string).scopes, [{ vmId: "web", actions: ["view"] }, { vmId: "db", actions: ["view"] }]);
    assert.deepEqual(JSON.parse(d!.body as string).scopes, [{ actions: ["admin"] }]);
    assert.equal(d!.headers["Idempotency-Key"], "k-1");
    assert.deepEqual(JSON.parse(e!.body as string).scopes, [{ actions: ["view"] }, { vmId: "web", actions: ["exec"] }]);

    await assert.rejects(c.createToken("x", { ttlSeconds: 60 }), /exactly one/);
    await assert.rejects(c.createToken("x", { actions: ["view"], scopes: [], ttlSeconds: 60 }), /exactly one/);
    await assert.rejects(c.createToken("x", { scopes: [], vm: "web", ttlSeconds: 60 }), /vm goes with actions/);
    await assert.rejects(c.createToken("x", { actions: "view" as unknown as string[], ttlSeconds: 60 }), TypeError);
    await assert.rejects(c.createToken("x", { actions: ["view"], ttlSeconds: 31_536_001 }), /ttlSeconds/);
    assert.equal(t.calls.length, 4);
  });

  test("listTokens/iterTokens page; revokeToken sends DELETE without a key", async () => {
    const tok = (id: string) => ({ id, name: "ci", scopes: [{ actions: ["view"] }], createdAt: "2026-01-01T00:00:00Z", expiresAt: "2026-01-02T00:00:00Z" });
    const t = fake((req) => {
      if (req.method === "DELETE") return { status: 204, headers: {}, body: "" };
      return req.path.includes("cursor=c1") ? json(200, { items: [tok("tok_2")] }) : json(200, { items: [tok("tok_1")], nextCursor: "c1" });
    });
    const c = new DavitClient({ transport: t });
    const ids: string[] = [];
    for await (const x of c.iterTokens({ limit: 1 })) ids.push(x.id);
    assert.deepEqual(ids, ["tok_1", "tok_2"]);
    assert.equal(`${t.calls[0]!.method} ${t.calls[0]!.path}`, "GET /api/v1/tokens?limit=1");
    assert.equal(t.calls[1]!.path, "/api/v1/tokens?limit=1&cursor=c1");
    const page = await c.listTokens({ cursor: "c1" });
    assert.equal(page.nextCursor, undefined);
    assert.equal(await c.revokeToken("tok_1"), undefined);
    await c.revokeToken(page.items[0]!);
    const [r1, r2] = t.calls.slice(-2);
    assert.equal(`${r1!.method} ${r1!.path}`, "DELETE /api/v1/tokens/tok_1");
    assert.equal(r2!.path, "/api/v1/tokens/tok_2");
    assert.equal(r1!.headers["Idempotency-Key"], undefined);
    assert.equal(r1!.body, undefined);
  });

  const TS = "2026-01-01T00:00:00Z";
  const profile = (id: string) => ({
    id, name: "work", default: false, createdAt: TS, updatedAt: TS,
    credentials: [{ name: "anthropic", kind: "api_key", provider: "anthropic", envVar: "ANTHROPIC_API_KEY", hasSecret: true, updatedAt: TS }],
  });
  const grant = (extra: Record<string, unknown> = {}) => ({
    id: "cg_1", vmId: "vm_1", profileId: "cp_1", credential: "anthropic", state: "active", placeholder: "slwp_placeholder",
    envVar: "ANTHROPIC_API_KEY", upstream: "https://api.anthropic.com", createdAt: TS, ...extra,
  });
  const noContent = (): TransportResponse => ({ status: 204, headers: {}, body: "" });

  test("credential profiles: list/iter, create with a key, get/delete escaped without one", async () => {
    const t = fake((req) => {
      if (req.method === "DELETE") return noContent();
      if (req.method === "POST") return json(201, { ...profile("cp_1"), default: true });
      if (req.path.startsWith("/api/v1/credential-profiles?")) {
        return req.path.includes("cursor=c1") ? json(200, { items: [profile("cp_2")] }) : json(200, { items: [profile("cp_1")], nextCursor: "c1" });
      }
      return json(200, profile("cp_1"));
    });
    const c = new DavitClient({ transport: t });
    const ids: string[] = [];
    for await (const p of c.iterCredentialProfiles({ limit: 1 })) ids.push(p.id);
    assert.deepEqual(ids, ["cp_1", "cp_2"]);
    assert.equal(`${t.calls[0]!.method} ${t.calls[0]!.path}`, "GET /api/v1/credential-profiles?limit=1");
    assert.equal(t.calls[1]!.path, "/api/v1/credential-profiles?limit=1&cursor=c1");

    assert.equal((await c.createCredentialProfile("work", { default: true })).default, true);
    await c.createCredentialProfile("plain", { idempotencyKey: "k-1" });
    const [mk1, mk2] = t.calls.slice(2, 4);
    assert.equal(`${mk1!.method} ${mk1!.path}`, "POST /api/v1/credential-profiles");
    assert.match(mk1!.headers["Idempotency-Key"]!, UUID);
    assert.deepEqual(JSON.parse(mk1!.body as string), { name: "work", default: true });
    assert.equal(mk2!.headers["Idempotency-Key"], "k-1");
    assert.deepEqual(JSON.parse(mk2!.body as string), { name: "plain" });

    const got = await c.getCredentialProfile("my work/x");
    assert.equal(got.credentials[0]!.hasSecret, true);
    assert.equal(await c.deleteCredentialProfile(got), undefined);
    await c.deleteCredentialProfile("a/b");
    const [get, d1, d2] = t.calls.slice(4);
    assert.equal(`${get!.method} ${get!.path}`, "GET /api/v1/credential-profiles/my%20work%2Fx");
    assert.equal(`${d1!.method} ${d1!.path}`, "DELETE /api/v1/credential-profiles/cp_1");
    assert.equal(d2!.path, "/api/v1/credential-profiles/a%2Fb");
    assert.equal(d1!.body, undefined);
    for (const r of [...t.calls.slice(0, 2), ...t.calls.slice(4)]) assert.equal(r.headers["Idempotency-Key"], undefined);
    await assert.rejects(c.getCredentialProfile(""), /missing profileId/);
    assert.equal(t.calls.length, 7);
  });

  test("putCredential sends the secret once in the PUT body; deleteCredential handles 204", async () => {
    const secret = "sk-ant-s3cret";
    const t = fake((req) => (req.method === "DELETE" ? noContent() : json(200, profile("cp_1"))));
    const c = new DavitClient({ transport: t });
    const p = await c.putCredential("work", "anthropic", { kind: "api_key", provider: "anthropic", envVar: "ANTHROPIC_API_KEY", secret });
    assert.equal(p.id, "cp_1");
    assert.ok(!JSON.stringify(p).includes(secret));
    await c.putCredential(p, "my api/1", { kind: "custom_api", baseUrl: "https://api.example.com", envVar: "X_KEY" });
    assert.equal(await c.deleteCredential("work", "a/b"), undefined);
    const [put, put2, rm] = t.calls;
    assert.equal(`${put!.method} ${put!.path}`, "PUT /api/v1/credential-profiles/work/credentials/anthropic");
    assert.deepEqual(JSON.parse(put!.body as string), { kind: "api_key", provider: "anthropic", envVar: "ANTHROPIC_API_KEY", secret });
    assert.equal(put!.headers["Idempotency-Key"], undefined);
    assert.equal(put2!.path, "/api/v1/credential-profiles/cp_1/credentials/my%20api%2F1");
    assert.deepEqual(JSON.parse(put2!.body as string), { kind: "custom_api", baseUrl: "https://api.example.com", envVar: "X_KEY", secret: "" });
    assert.equal(`${rm!.method} ${rm!.path}`, "DELETE /api/v1/credential-profiles/work/credentials/a%2Fb");
    assert.equal(rm!.body, undefined);
    assert.equal(rm!.headers["Idempotency-Key"], undefined);
  });

  test("a putCredential error does not echo the secret", async () => {
    const secret = "sk-ant-s3cret";
    const c = new DavitClient({ transport: fake(() => json(400, { error: { code: "invalid_argument", message: "bad kind", requestId: "req_1" } })) });
    const err = await c.putCredential("work", "c", { kind: "nope", envVar: "K", secret }).catch((e: unknown) => e);
    assert.ok(err instanceof DavitError);
    assert.ok(!String(err).includes(secret) && !JSON.stringify(err).includes(secret) && !(err.stack ?? "").includes(secret));
  });

  test("credential grants: list pages, create with a key, revoke returns the grant", async () => {
    const t = fake((req) => {
      if (req.method === "DELETE") return json(200, grant({ state: "revoked", revokeReason: "revoked", revokedAt: TS }));
      if (req.method === "POST") return json(201, grant());
      if (req.path.startsWith("/api/v1/credential-profiles/")) return json(200, profile("cp_1"));
      return json(200, { items: [grant()] });
    });
    const c = new DavitClient({ transport: t });
    const page = await c.listCredentialGrants("web", { limit: 10, cursor: "c1" });
    assert.equal(page.items[0]!.state, "active");
    const g = await c.createCredentialGrant("a/b", "work", "anthropic");
    assert.equal(g.placeholder, "slwp_placeholder");
    await c.createCredentialGrant("web", await c.getCredentialProfile("work"), "anthropic", { idempotencyKey: "k-2" });
    const revoked = await c.revokeCredentialGrant("web", g);
    assert.equal(revoked.revokeReason, "revoked");
    await c.revokeCredentialGrant("web", "g/1");
    const [ls, mk, , mk2, r1, r2] = t.calls;
    assert.equal(`${ls!.method} ${ls!.path}`, "GET /api/v1/vms/web/credential-grants?limit=10&cursor=c1");
    assert.equal(`${mk!.method} ${mk!.path}`, "POST /api/v1/vms/a%2Fb/credential-grants");
    assert.match(mk!.headers["Idempotency-Key"]!, UUID);
    assert.deepEqual(JSON.parse(mk!.body as string), { profile: "work", credential: "anthropic" });
    assert.equal(mk2!.headers["Idempotency-Key"], "k-2");
    assert.deepEqual(JSON.parse(mk2!.body as string), { profile: "cp_1", credential: "anthropic" });
    assert.equal(`${r1!.method} ${r1!.path}`, "DELETE /api/v1/vms/web/credential-grants/cg_1");
    assert.equal(r2!.path, "/api/v1/vms/web/credential-grants/g%2F1");
    assert.equal(r1!.body, undefined);
    for (const r of [ls, r1, r2]) assert.equal(r!.headers["Idempotency-Key"], undefined);
    await assert.rejects(c.createCredentialGrant("web", "", "anthropic"), /missing profile/);
  });

  test("setVMEgress is a synchronous PATCH without an idempotency key", async () => {
    const egress = { mode: "allowlist", allow: ["github.com"] };
    const t = fake(() => json(200, { id: "vm_1", egress }));
    const c = new DavitClient({ transport: t });
    assert.deepEqual((await c.setVMEgress("a/b", egress)).egress, egress);
    await c.setVMEgress("web", { mode: "off" });
    const [put, off] = t.calls;
    assert.equal(`${put!.method} ${put!.path}`, "PATCH /api/v1/vms/a%2Fb");
    assert.deepEqual(JSON.parse(put!.body as string), { egress });
    assert.deepEqual(JSON.parse(off!.body as string), { egress: { mode: "off" } });
    assert.equal(put!.headers["Idempotency-Key"], undefined);
    assert.equal(put!.headers["If-Match"], undefined);
    await assert.rejects(c.setVMEgress("web", { mode: "allowlist", allow: "github.com" as unknown as string[] }), TypeError);
    assert.equal(t.calls.length, 2);
  });

  test("path segments are escaped and list queries are encoded", async () => {
    const t = fake(() => json(200, { items: [] }));
    const c = new DavitClient({ transport: t });
    await c.getVM("a/b");
    await c.listVMs({ states: ["stopped", "error"], all: true, limit: 5, cursor: "c 1" });
    assert.equal(t.calls[0]!.path, "/api/v1/vms/a%2Fb");
    assert.equal(t.calls[1]!.path, "/api/v1/vms?limit=5&cursor=c+1&state=stopped&state=error&all=true");
    assert.equal(t.calls[1]!.headers["Idempotency-Key"], undefined);
    await c.listVMs({ labels: { "davit.box": "proj" } });
    assert.equal(t.calls[2]!.path, "/api/v1/vms?label=davit.box%3Dproj");
  });

  test("retries only connect failures, reusing request ID and idempotency key", async () => {
    const t = fake((_, n) => {
      if (n < 3) throw new TransportError("refused", { connectFailed: true });
      return json(202, { operation: op("queued") });
    });
    const c = new DavitClient({ transport: t });
    await c.createVM({ name: "x" }, { wait: false });
    assert.equal(t.calls.length, 3);
    assert.equal(new Set(t.calls.map((r) => r.headers["Idempotency-Key"])).size, 1);
    assert.equal(new Set(t.calls.map((r) => r.headers["X-Request-Id"])).size, 1);
  });

  test("gives up after 3 attempts", async () => {
    const t = fake(() => {
      throw new TransportError("refused", { connectFailed: true });
    });
    await assert.rejects(new DavitClient({ transport: t }).getCapabilities(), TransportError);
    assert.equal(t.calls.length, 3);
  });

  test("never retries a mid-request failure or a response", async () => {
    const t1 = fake(() => {
      throw new TransportError("reset", { connectFailed: false });
    });
    await assert.rejects(new DavitClient({ transport: t1 }).createVM({}, { wait: false }), TransportError);
    assert.equal(t1.calls.length, 1);
    const t2 = fake(() => json(503, { error: { code: "unavailable", message: "busy", retryable: true, requestId: "req_s" } }));
    await assert.rejects(new DavitClient({ transport: t2 }).getCapabilities(), Unavailable);
    assert.equal(t2.calls.length, 1);
  });

  test("foreign transport exceptions become non-retried TransportErrors", async () => {
    const t = fake(() => {
      throw new Error("boom");
    });
    const err = await new DavitClient({ transport: t }).getCapabilities().catch((e: unknown) => e);
    assert.ok(err instanceof TransportError && !err.connectFailed && err.status === null);
    assert.equal(t.calls.length, 1);
  });
});

describe("errors", () => {
  test("typed by code with server request ID and operation ID", async () => {
    const t = fake(() =>
      json(409, { error: { code: "stop_required", message: "stop first", retryable: false, requestId: "req_srv", operationId: "op_9", details: { a: 1 } } }),
    );
    const err = await new DavitClient({ transport: t }).forkVM("web").catch((e: unknown) => e);
    assert.ok(err instanceof StopRequired);
    assert.ok(err instanceof Conflict);
    assert.equal(err.status, 409);
    assert.equal(err.code, "stop_required");
    assert.equal(err.requestId, "req_srv");
    assert.equal(err.operationId, "op_9");
    assert.deepEqual(err.details, { a: 1 });
    assert.equal(err.name, "StopRequired");
  });

  test("unknown codes fall back to the base class", async () => {
    const t = fake(() => json(418, { error: { code: "teapot", message: "?", retryable: false, requestId: "r" } }));
    const err = await new DavitClient({ transport: t }).getCapabilities().catch((e: unknown) => e);
    assert.ok(err instanceof DavitError);
    assert.equal(err.constructor, DavitError);
    assert.equal(err.code, "teapot");
  });

  test("non-JSON error bodies", async () => {
    const t = fake(() => ({ status: 502, headers: { "x-request-id": "req_h" }, body: "<html>bad gateway</html>" }));
    const err = await new DavitClient({ transport: t }).getCapabilities().catch((e: unknown) => e);
    assert.ok(err instanceof DavitError);
    assert.equal(err.status, 502);
    assert.equal(err.code, "internal");
    assert.equal(err.requestId, "req_h");
  });

  test("unknown fields and enum values are tolerated", async () => {
    const t = fake(() => json(200, { id: "vm_1", state: "hibernating", brandNew: { x: 1 } }));
    const vm = await new DavitClient({ transport: t }).getVM("vm_1");
    assert.equal(vm.state, "hibernating");
  });
});

describe("endpoints", () => {
  test("baseUrl validation", () => {
    assert.throws(() => new DavitClient({ baseUrl: "http://example.com", token: "t" }), /non-loopback/);
    assert.throws(() => new DavitClient({ baseUrl: "ftp://example.com" }), TypeError);
    assert.throws(() => new DavitClient({ socket: "rel.sock" }), /absolute/);
    assert.throws(() => new DavitClient({ socket: "/a.sock", baseUrl: "https://h" }), /either/);
    assert.deepEqual(new DavitClient({ baseUrl: "https://h.example:8443/" }).endpoint, { baseUrl: "https://h.example:8443" });
    for (const u of ["http://localhost:1", "http://127.0.0.1:1", "http://[::1]:1"]) {
      assert.doesNotThrow(() => new DavitClient({ baseUrl: u, token: "t" }), u);
    }
  });

  test("default endpoint resolution", () => {
    const r = resolveDefaultEndpoint;
    assert.deepEqual(r({ DAVIT_API_URL: "unix:///x/y.sock" }, "linux"), { socket: "/x/y.sock" });
    assert.deepEqual(r({ DAVIT_API_URL: "https://h" }, "linux"), { baseUrl: "https://h" });
    assert.deepEqual(r({ XDG_RUNTIME_DIR: "/run/u", XDG_DATA_HOME: "/d", HOME: "/h" }, "linux"), { socket: "/run/u/davit/davit.sock" });
    assert.deepEqual(r({ XDG_DATA_HOME: "/d", HOME: "/h" }, "linux"), { socket: "/d/davit/davit.sock" });
    assert.deepEqual(r({ XDG_RUNTIME_DIR: "rel", HOME: "/h" }, "linux"), { socket: "/h/.local/share/davit/davit.sock" });
    assert.deepEqual(r({ XDG_RUNTIME_DIR: "/run/u", HOME: "/h" }, "darwin"), { socket: "/h/Library/Application Support/davit/davit.sock" });
    assert.throws(() => r({ DAVIT_API_URL: "unix://rel.sock" }, "linux"), TypeError);
    assert.throws(() => r({}, "linux"), TypeError);
  });
});

describe("wait", () => {
  test("follows events, reports progress, needs an empty terminal page", async () => {
    const pages = [
      { operation: op("running"), items: [{ operationId: "op_1", seq: 1, time: "", state: "running", phase: "boot", progress: 0.5 }], nextCursor: "1" },
      { operation: op("succeeded", { result: { durationMs: 3 } }), items: [{ operationId: "op_1", seq: 2, time: "", state: "succeeded", phase: "done", progress: 1 }], nextCursor: "2" },
      { operation: op("succeeded", { result: { durationMs: 3 } }), items: [] },
    ];
    const t = fake((_, n) => json(200, pages[n - 1]));
    const seen: number[] = [];
    const done = await new DavitClient({ transport: t }).wait("op_1", { onProgress: (e) => seen.push(e.seq) });
    assert.equal(done.state, "succeeded");
    assert.deepEqual(seen, [1, 2]);
    assert.deepEqual(
      t.calls.map((r) => new URL(r.path, "http://x").searchParams.get("after")),
      ["0", "1", "2"],
    );
    for (const r of t.calls) assert.ok(Number(new URL(r.path, "http://x").searchParams.get("waitMs")) <= 25000);
  });

  test("failed operations throw OperationFailed with a typed error", async () => {
    const t = fake(() => json(200, { operation: op("failed", { error: { code: "unavailable", message: "no kvm", retryable: true } }), items: [] }));
    const err = await new DavitClient({ transport: t }).wait("op_1").catch((e: unknown) => e);
    assert.ok(err instanceof OperationFailed);
    assert.ok(err.error instanceof Unavailable);
    assert.equal(err.cause, err.error);
    assert.equal(err.operationId, "op_1");
    assert.equal(err.code, "unavailable");
    assert.equal(err.operation.state, "failed");
  });

  test("budget exhaustion throws WaitTimeout without cancelling", async () => {
    const t = fake(
      (req) =>
        new Promise((resolve, reject) => {
          const timer = setTimeout(() => resolve(json(200, { operation: op("running"), items: [] })), 5000);
          req.signal?.addEventListener("abort", () => {
            clearTimeout(timer);
            reject(req.signal!.reason);
          });
        }),
    );
    const started = Date.now();
    const err = await new DavitClient({ transport: t }).wait("op_1", { timeoutMs: 50 }).catch((e: unknown) => e);
    assert.ok(err instanceof WaitTimeout, String(err));
    assert.equal(err.operationId, "op_1");
    assert.ok(Date.now() - started < 2000);
    assert.ok(t.calls.every((r) => r.method === "GET"), "a wait must never send a mutation");
  });

  test("lifecycle with wait returns the final VM; wait:false returns the 202 body", async () => {
    const t = fake((req) => {
      if (req.method === "POST") return json(202, { operation: op("queued") });
      if (req.path.includes("/events")) return json(200, { operation: op("succeeded"), items: [] });
      return json(200, { id: "vm_1", name: "web", state: "ready" });
    });
    const c = new DavitClient({ transport: t });
    const vm = await c.createVM({ name: "web" });
    assert.equal(vm.state, "ready");
    assert.equal(t.calls.at(-1)!.path, "/api/v1/vms/vm_1");
    const acc = await c.createVM({ name: "web" }, { wait: false });
    assert.equal(acc.operation.id, "op_1");
  });

  test("createVMs sends one batch and reports every item; wait follows the accepted ones", async () => {
    const batch = {
      items: [
        { index: 1, name: "web-1", accepted: { operation: op("queued"), vm: { id: "vm_1", name: "web-1", state: "provisioning" } } },
        { index: 2, name: "web-2", error: { code: "resource_exhausted", message: "full", retryable: true, requestId: "r" } },
      ],
    };
    const t = fake((req) => {
      if (req.method === "POST") return json(200, batch);
      if (req.path.includes("/events")) return json(200, { operation: op("succeeded"), items: [] });
      return json(200, { id: "vm_1", name: "web-1", state: "ready" });
    });
    const c = new DavitClient({ transport: t });
    const res = await c.createVMs(2, { name: "web", type: "big" }, { idempotencyKey: "b-1" });
    const r = t.calls[0]!;
    assert.equal(`${r.method} ${r.path}`, "POST /api/v1/vms/batch");
    assert.equal(r.headers["Idempotency-Key"], "b-1");
    assert.deepEqual(JSON.parse(r.body as string), { count: 2, template: { name: "web", type: "big" } });
    assert.equal(t.calls.length, 1);
    assert.equal(res.items[0]!.accepted!.operation.id, "op_1");
    assert.equal(res.items[1]!.error!.code, "resource_exhausted");

    const waited = await c.createVMs(2, { name: "web" }, { wait: true });
    assert.equal(waited.items[0]!.accepted!.operation.state, "succeeded");
    assert.equal(waited.items[0]!.accepted!.vm!.state, "ready");
    assert.equal(waited.items[1]!.error!.code, "resource_exhausted");
    assert.deepEqual(t.calls.slice(2).map((c) => c.path), ["/api/v1/operations/op_1/events?after=0&waitMs=25000", "/api/v1/vms/vm_1"]);
  });

  test("reclaimVMs posts the filter without an Idempotency-Key", async () => {
    const item = { vmId: "vm_1", name: "web", idleSeconds: 700, memoryBytes: 1 << 30 };
    const t = fake(() => json(200, { items: [item] }));
    const c = new DavitClient({ transport: t });
    const res = await c.reclaimVMs(600, { labels: { team: "a" }, dryRun: true });
    await c.reclaimVMs(0);
    const r = t.calls[0]!;
    assert.equal(`${r.method} ${r.path}`, "POST /api/v1/vms/reclaim");
    assert.equal(r.headers["Idempotency-Key"], undefined);
    assert.deepEqual(JSON.parse(r.body as string), { idleSeconds: 600, labels: { team: "a" }, dryRun: true });
    assert.deepEqual(JSON.parse(t.calls[1]!.body as string), { idleSeconds: 0 });
    assert.deepEqual(res.items, [item]);
  });

  test("capacityWaitSeconds sends waitSeconds on createVM and resumeVM", async () => {
    const t = fake(() => json(202, { operation: op("queued") }));
    const c = new DavitClient({ transport: t });
    await c.createVM({ name: "web" }, { wait: false, capacityWaitSeconds: 90 });
    await c.resumeVM("web", { wait: false, capacityWaitSeconds: 120 });
    await c.createVM({ name: "plain" }, { wait: false, capacityWaitSeconds: 0 });
    await c.resumeVM("web", { wait: false });
    assert.deepEqual(t.calls.map((r) => r.path), ["/api/v1/vms?waitSeconds=90", "/api/v1/vms/web/resume?waitSeconds=120", "/api/v1/vms", "/api/v1/vms/web/resume"]);
    assert.deepEqual(JSON.parse(t.calls[0]!.body as string), { name: "web" });
    for (const bad of [-1, 3601, 1.5, Number.NaN]) {
      await assert.rejects(c.createVM({ name: "web" }, { wait: false, capacityWaitSeconds: bad }), TypeError);
    }
    assert.equal(t.calls.length, 4); // rejected before sending
  });
});

test("iterators follow nextCursor", async () => {
  const t = fake((req) => {
    const cursor = new URL(req.path, "http://x").searchParams.get("cursor");
    return cursor === "p2" ? json(200, { items: [{ id: "c" }] }) : json(200, { items: [{ id: "a" }, { id: "b" }], nextCursor: "p2" });
  });
  const ids: string[] = [];
  for await (const s of new DavitClient({ transport: t }).iterSnapshots({ limit: 2 })) ids.push(s.id);
  assert.deepEqual(ids, ["a", "b", "c"]);
});

describe("built-in transports", () => {
  test("unix socket: missing socket file is a retried connect failure", async () => {
    const tr = new UnixSocketTransport(join(tmpdir(), "davit-sdk-ts-does-not-exist.sock"));
    const err = await tr.request({ method: "GET", path: "/api/v1/capabilities", headers: {} }).catch((e: unknown) => e);
    assert.ok(err instanceof TransportError && err.connectFailed, String(err));
    const c = new DavitClient({ socket: join(tmpdir(), "davit-sdk-ts-does-not-exist.sock") });
    await assert.rejects(c.getCapabilities(), TransportError);
  });

  test("fetch: connection refused on loopback is a connect failure", async () => {
    const srv = createServer();
    await new Promise<void>((resolve) => srv.listen(0, "127.0.0.1", resolve));
    const port = (srv.address() as { port: number }).port;
    await new Promise((resolve) => srv.close(resolve));
    const tr = new FetchTransport(`http://127.0.0.1:${port}`);
    const err = await tr.request({ method: "GET", path: "/api/v1/capabilities", headers: {} }).catch((e: unknown) => e);
    assert.ok(err instanceof TransportError && err.connectFailed, String(err));
  });

  test("not found over a real unix socket server", async () => {
    const { createServer: createHttp } = await import("node:http");
    const { mkdtempSync, rmSync } = await import("node:fs");
    const dir = mkdtempSync(join(tmpdir(), "swts-"));
    const sock = `${dir}/s.sock`;
    const srv = createHttp((req, res) => {
      res.writeHead(404, { "content-type": "application/json", "x-request-id": String(req.headers["x-request-id"]) });
      res.end(JSON.stringify({ error: { code: "not_found", message: "no vm", retryable: false, requestId: req.headers["x-request-id"] } }));
    });
    try {
      await new Promise<void>((resolve) => srv.listen(sock, resolve));
      const err = await new DavitClient({ socket: sock }).getVM("nope", { requestId: "req_mine" }).catch((e: unknown) => e);
      assert.ok(err instanceof NotFound);
      assert.equal(err.requestId, "req_mine");
    } finally {
      await new Promise((resolve) => srv.close(resolve));
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

describe("updateVM, extendVM, renameVM, type", () => {
  const vmBody = { id: "vm_1", name: "web", state: "ready" };

  test("updateVM: PATCH with only the given fields, If-Match, no idempotency key", async () => {
    const t = fake(() => json(200, vmBody));
    const c = new DavitClient({ transport: t });
    const vm = await c.updateVM("web", { ttlSeconds: 600, name: undefined }, { ifMatch: 7 });
    assert.equal(vm.id, "vm_1");
    const r = t.calls[0]!;
    assert.equal(r.method, "PATCH");
    assert.equal(r.path, "/api/v1/vms/web");
    assert.deepEqual(JSON.parse(r.body as string), { ttlSeconds: 600 });
    assert.equal(r.headers["If-Match"], "7");
    assert.equal(r.headers["Idempotency-Key"], undefined);
    await c.updateVM("web", {});
    assert.deepEqual(JSON.parse(t.calls[1]!.body as string), {});
    assert.equal(t.calls[1]!.headers["If-Match"], undefined);
  });

  test("extendVM mirrors the CLI; renameVM sends the name", async () => {
    const t = fake(() => json(200, vmBody));
    const c = new DavitClient({ transport: t });
    await c.extendVM("web");
    await c.extendVM("web", { ttlSeconds: 7200, ifMatch: "3" });
    await c.renameVM("web", "api");
    assert.deepEqual(t.calls.map((r) => JSON.parse(r.body as string)), [{ autoStop: true }, { autoStop: true, ttlSeconds: 7200 }, { name: "api" }]);
    assert.equal(t.calls[1]!.headers["If-Match"], "3");
    for (const bad of [0, -5, 1.5]) await assert.rejects(c.extendVM("web", { ttlSeconds: bad }), TypeError);
  });

  test("type passes through on create and fork", async () => {
    const t = fake(() => json(202, { operation: { id: "op_1" } }));
    const c = new DavitClient({ transport: t });
    await c.createVM({ name: "big", type: "big" }, { wait: false });
    await c.forkVM("web", { name: "f", type: "big", wait: false });
    await c.forkVM("web", { wait: false });
    assert.deepEqual(t.calls.map((r) => JSON.parse(r.body as string)), [{ name: "big", type: "big" }, { name: "f", type: "big" }, {}]);
  });

  test("privacy reaches createVM, createVMs and updateVM", async () => {
    const t = fake((req) => {
      if (req.method === "PATCH") return json(200, vmBody);
      if (req.path.endsWith("/batch")) return json(200, { items: [] });
      return json(202, { operation: { id: "op_1" } });
    });
    const c = new DavitClient({ transport: t });
    await c.createVM({ name: "e", privacy: { ephemeral: true } }, { wait: false });
    await c.createVM({ name: "plain" }, { wait: false });
    await c.createVMs(2, { name: "b", privacy: { noProcessSamples: true } });
    await c.updateVM("web", { privacy: { noSnapshot: true, noOutputRetention: true } });
    assert.deepEqual(t.calls.map((r) => JSON.parse(r.body as string)), [
      { name: "e", privacy: { ephemeral: true } },
      { name: "plain" },
      { count: 2, template: { name: "b", privacy: { noProcessSamples: true } } },
      { privacy: { noSnapshot: true, noOutputRetention: true } },
    ]);
  });

  test("resumeVM passes type and shape (resize)", async () => {
    const t = fake(() => json(202, { operation: { id: "op_1" } }));
    const c = new DavitClient({ transport: t });
    await c.resumeVM("web", { type: "big", wait: false });
    await c.resumeVM("web", { shape: { cpus: 4, diskBytes: 20 * 2 ** 30 }, wait: false });
    await c.resumeVM("web", { wait: false });
    assert.ok(t.calls[0]!.path.endsWith("/vms/web/resume"));
    assert.deepEqual(t.calls.map((r) => JSON.parse(r.body as string)), [{ type: "big" }, { shape: { cpus: 4, diskBytes: 20 * 2 ** 30 } }, {}]);
  });

  test("listProcesses pages with a cursor; the iterator follows nextCursor", async () => {
    const t = fake((req) => {
      const cursor = new URL(req.path, "http://x").searchParams.get("cursor");
      return json(200, cursor ? { items: [{ id: "proc_2" }] } : { items: [{ id: "proc_1" }], nextCursor: "n1" });
    });
    const c = new DavitClient({ transport: t });
    const ids: string[] = [];
    for await (const p of c.iterProcesses("web", { limit: 1 })) ids.push(p.id);
    assert.deepEqual(ids, ["proc_1", "proc_2"]);
    assert.deepEqual(t.calls.map((r) => r.path), ["/api/v1/vms/web/processes?limit=1", "/api/v1/vms/web/processes?limit=1&cursor=n1"]);
  });
});

describe("environments", () => {
  const NOW = "2026-01-01T00:00:00Z";
  const MANIFEST = {
    repos: [{ url: "https://example.com/r.git", path: "/work/r" }],
    variables: { A: "1" },
    secretFiles: [],
    setup: [{ name: "deps", argv: ["make", "deps"] }],
  };
  const env = (id = "env_1", name = "dev") => ({ id, name, latestRevision: 1, createdAt: NOW, updatedAt: NOW });
  const rev = (revision: number) => ({ environmentId: "env_1", revision, manifest: MANIFEST, createdAt: NOW });
  const route: Handler = (req) => {
    const u = new URL(req.path, "http://x");
    if (req.method === "DELETE") return { status: 204, headers: {}, body: "" };
    if (u.pathname.endsWith("/revisions")) {
      if (req.method === "POST") return json(201, rev(2));
      return u.searchParams.get("cursor") ? json(200, { items: [rev(2)] }) : json(200, { items: [rev(1)], nextCursor: "c1" });
    }
    if (u.pathname.includes("/revisions/")) return json(200, rev(Number(u.pathname.split("/").pop())));
    if (u.pathname === "/api/v1/environments") {
      if (req.method === "POST") return json(201, env());
      return u.searchParams.get("cursor") ? json(200, { items: [env("env_2", "ci")] }) : json(200, { items: [env()], nextCursor: "c1" });
    }
    return json(200, env());
  };

  test("create sends the manifest and an idempotency key; list, get, delete", async () => {
    const t = fake(route);
    const c = new DavitClient({ transport: t });
    assert.deepEqual(await c.createEnvironment("dev", MANIFEST), env());
    const create = t.calls[0]!;
    assert.equal(`${create.method} ${create.path}`, "POST /api/v1/environments");
    assert.match(create.headers["Idempotency-Key"]!, UUID);
    assert.equal(create.headers["If-Match"], undefined);
    assert.deepEqual(JSON.parse(create.body as string), { name: "dev", manifest: MANIFEST });
    await c.createEnvironment("bare", { setup: [] }, { idempotencyKey: "k-1" });
    assert.equal(t.calls[1]!.headers["Idempotency-Key"], "k-1");
    assert.deepEqual(JSON.parse(t.calls[1]!.body as string).manifest, { repos: [], variables: {}, secretFiles: [], setup: [] });
    await assert.rejects(c.createEnvironment("bad", { secret_files: [] } as never), /unknown field\(s\) secret_files/);
    await assert.rejects(c.createEnvironment("bad", null as never), TypeError);
    assert.equal(t.calls.length, 2);

    const names: string[] = [];
    for await (const e of c.iterEnvironments({ limit: 1 })) names.push(e.name);
    assert.deepEqual(names, ["dev", "ci"]);
    assert.deepEqual(t.calls.slice(2).map((r) => `${r.method} ${r.path}`), ["GET /api/v1/environments?limit=1", "GET /api/v1/environments?limit=1&cursor=c1"]);

    await c.getEnvironment("a/b");
    assert.equal(t.calls.at(-1)!.path, "/api/v1/environments/a%2Fb");
    assert.equal(await c.deleteEnvironment(env()), undefined);
    await c.deleteEnvironment("ci");
    const [d1, d2] = t.calls.slice(-2);
    assert.equal(`${d1!.method} ${d1!.path}`, "DELETE /api/v1/environments/env_1");
    assert.equal(d2!.path, "/api/v1/environments/ci");
    assert.equal(d1!.headers["Idempotency-Key"], undefined);
    assert.equal(d1!.body, undefined);
  });

  test("revisions: If-Match, idempotency key, paging, get", async () => {
    const t = fake(route);
    const c = new DavitClient({ transport: t });
    const r2 = await c.createEnvironmentRevision("dev", MANIFEST, { ifMatch: 1 });
    assert.equal(r2.revision, 2);
    const post = t.calls[0]!;
    assert.equal(`${post.method} ${post.path}`, "POST /api/v1/environments/dev/revisions");
    assert.equal(post.headers["If-Match"], "1");
    assert.match(post.headers["Idempotency-Key"]!, UUID);
    assert.deepEqual(JSON.parse(post.body as string), { manifest: MANIFEST });
    await c.createEnvironmentRevision("dev", MANIFEST, { idempotencyKey: "k-2" });
    assert.equal(t.calls[1]!.headers["If-Match"], undefined);
    assert.equal(t.calls[1]!.headers["Idempotency-Key"], "k-2");

    const revs: number[] = [];
    for await (const r of c.iterEnvironmentRevisions("dev", { limit: 1 })) revs.push(r.revision);
    assert.deepEqual(revs, [1, 2]);
    assert.deepEqual(t.calls.slice(2).map((r) => r.path), ["/api/v1/environments/dev/revisions?limit=1", "/api/v1/environments/dev/revisions?limit=1&cursor=c1"]);

    assert.equal((await c.getEnvironmentRevision("dev", 2)).revision, 2);
    assert.equal(`${t.calls.at(-1)!.method} ${t.calls.at(-1)!.path}`, "GET /api/v1/environments/dev/revisions/2");
    for (const bad of [0, -1, 1.5, Number.NaN]) await assert.rejects(c.getEnvironmentRevision("dev", bad), /revision must be/);
  });

  test("a stale If-Match throws RevisionMismatch with the latest revision", async () => {
    const t = fake(() => json(412, { error: { code: "revision_mismatch", message: "latest revision is 3, not 1", details: { latestRevision: 3 } } }));
    const c = new DavitClient({ transport: t });
    const err = await c.createEnvironmentRevision("dev", MANIFEST, { ifMatch: 1 }).catch((e: unknown) => e);
    assert.ok(err instanceof RevisionMismatch);
    assert.deepEqual(err.details, { latestRevision: 3 });
  });

  test("parseEnvironmentRef accepts NAME, NAME@REV and refs; rejects the rest", () => {
    assert.deepEqual(parseEnvironmentRef("dev"), { environment: "dev" });
    assert.deepEqual(parseEnvironmentRef("dev@3"), { environment: "dev", revision: 3 });
    assert.deepEqual(parseEnvironmentRef("env_01J@12"), { environment: "env_01J", revision: 12 });
    assert.deepEqual(parseEnvironmentRef({ environment: "dev", revision: 0 }), { environment: "dev", revision: 0 });
    for (const bad of ["", "@3", "dev@", "dev@0", "dev@-1", "dev@x", "dev@1@2", "dev@1.5", "dev@99999999999999999999"]) {
      assert.throws(() => parseEnvironmentRef(bad), /NAME@REVISION/, bad);
    }
    assert.throws(() => parseEnvironmentRef(7 as never), TypeError);
  });

  test("environment on createVM, forkVM and resumeVM", async () => {
    const t = fake(() => json(202, { operation: { id: "op_1" } }));
    const c = new DavitClient({ transport: t });
    await c.createVM({ name: "web", environment: "dev@2" }, { wait: false });
    await c.forkVM("web", { name: "copy", environment: "dev", wait: false });
    await c.resumeVM("web", { environment: { environment: "env_1" }, wait: false });
    await c.createVM({ name: "plain" }, { wait: false });
    assert.deepEqual(t.calls.map((r) => JSON.parse(r.body as string)), [
      { name: "web", environment: { environment: "dev", revision: 2 } },
      { name: "copy", environment: { environment: "dev" } },
      { environment: { environment: "env_1" } },
      { name: "plain" },
    ]);
    await assert.rejects(c.createVM({ name: "bad", environment: "dev@zero" }, { wait: false }), TypeError);
    assert.equal(t.calls.length, 4);
  });
});

describe("job history and retained output", () => {
  const b64 = (s: string) => Buffer.from(s, "binary").toString("base64");
  const entry = (id: string) => ({
    process: { id, vmId: "vm_1", generation: 1, argv: ["sh"], status: "exited", exitCode: 0, detached: true, outputCursor: 1, createdAt: "2026-01-01T00:00:00Z" },
    handle: id, stdoutBytes: 3, stderrBytes: 0, outputDropped: false,
  });

  test("listProcessHistory pages newest first; the iterator follows nextCursor", async () => {
    const t = fake((req) => (req.path.includes("cursor=c1") ? json(200, { items: [entry("p1")] }) : json(200, { items: [entry("p2")], nextCursor: "c1" })));
    const c = new DavitClient({ transport: t });
    const ids: string[] = [];
    for await (const e of c.iterProcessHistory("a/b", { limit: 1 })) ids.push(e.process.id);
    assert.deepEqual(ids, ["p2", "p1"]);
    assert.deepEqual(t.calls.map((r) => `${r.method} ${r.path}`), ["GET /api/v1/vms/a%2Fb/history?limit=1", "GET /api/v1/vms/a%2Fb/history?limit=1&cursor=c1"]);
  });

  test("history says when a privacy policy withheld the output", async () => {
    const t = fake(() => json(200, { items: [{ ...entry("p1"), stdoutBytes: 0, outputNotRetained: "policy" }] }));
    const c = new DavitClient({ transport: t });
    const { items } = await c.listProcessHistory("web");
    assert.equal(items[0]!.outputNotRetained, "policy");
    assert.equal(items[0]!.outputDropped, false); // a policy is not truncation
  });

  test("listGuestProcesses samples the guest", async () => {
    const sample = {
      sampledAt: "2026-01-01T00:00:00Z", stale: true, truncated: false,
      items: [{ pid: 42, ppid: 1, user: "davit", args: ["sleep", "9"], state: "S", cpuTimeMillis: 5, rssBytes: 4096, startedAt: "2026-01-01T00:00:00Z", processId: "proc_1" }],
    };
    const t = fake(() => json(200, sample));
    const c = new DavitClient({ transport: t });
    const l = await c.listGuestProcesses("a/b");
    assert.equal(l.stale, true);
    assert.equal(l.items[0]!.processId, "proc_1");
    assert.equal(`${t.calls[0]!.method} ${t.calls[0]!.path}`, "GET /api/v1/vms/a%2Fb/ps");
  });

  test("readProcessOutput query; iterProcessOutput and collectProcessOutput page and decode", async () => {
    const pages: Record<string, unknown> = {
      "": { items: [{ type: "stdout", seq: 1, data: b64("he") }, { type: "stderr", seq: 2, data: b64("oops\n") }], nextCursor: "2", complete: false },
      "2": { items: [{ type: "future", seq: 3 }, { type: "stdout", seq: 4, data: b64("llo\x00\n") }], nextCursor: "4", complete: true },
    };
    const t = fake((req) => json(200, pages[new URL(req.path, "http://x").searchParams.get("after") ?? ""]));
    const c = new DavitClient({ transport: t });
    const page = await c.readProcessOutput("web", "p1", { stream: "both", limit: 2 });
    assert.equal(page.nextCursor, "2");
    assert.equal(t.calls[0]!.path, "/api/v1/vms/web/processes/p1/output?stream=both&limit=2");

    const seen: Array<[string, number, string]> = [];
    for await (const ev of c.iterProcessOutput("web", "p1")) seen.push([ev.type, ev.seq, Buffer.from(ev.data).toString("binary")]);
    assert.deepEqual(seen, [["stdout", 1, "he"], ["stderr", 2, "oops\n"], ["stdout", 4, "llo\x00\n"]]);
    assert.deepEqual(t.calls.slice(1).map((r) => r.path), ["/api/v1/vms/web/processes/p1/output", "/api/v1/vms/web/processes/p1/output?after=2"]);

    const out = await c.collectProcessOutput("web", "p1");
    assert.equal(Buffer.from(out.stdout).toString("binary"), "hello\x00\n");
    assert.equal(Buffer.from(out.stderr).toString("binary"), "oops\n");
    assert.equal(out.complete, true);
    assert.equal(out.cursor, "4");
    await c.readProcessOutput("web", "p1", { after: 2 });
    assert.equal(t.calls.at(-1)!.path, "/api/v1/vms/web/processes/p1/output?after=2");
  });

  test("collectProcessOutput stops when a running job has nothing new", async () => {
    const t = fake((req) =>
      req.path.includes("after=")
        ? json(200, { items: [], complete: false })
        : json(200, { items: [{ type: "stdout", seq: 5, data: b64("x") }], nextCursor: "5", complete: false }),
    );
    const c = new DavitClient({ transport: t });
    const out = await c.collectProcessOutput("web", "p1", { stream: "stdout" });
    assert.equal(Buffer.from(out.stdout).toString(), "x");
    assert.equal(out.stderr.length, 0);
    assert.equal(out.complete, false);
    assert.equal(out.cursor, "5");
    assert.deepEqual(t.calls.map((r) => r.path), ["/api/v1/vms/web/processes/p1/output?stream=stdout", "/api/v1/vms/web/processes/p1/output?after=5&stream=stdout"]);
    assert.equal((await c.collectProcessOutput("web", "p1", { after: "5" })).cursor, "5");
  });

  test("malformed base64 output is an internal error", async () => {
    const t = fake(() => json(200, { items: [{ type: "stdout", seq: 1, data: "not base64!" }], complete: true }));
    const c = new DavitClient({ transport: t });
    await assert.rejects(c.collectProcessOutput("web", "p1"), (e: unknown) => e instanceof DavitError && e.code === "internal" && /base64/.test(e.message));
  });
});

describe("runs and push", () => {
  const run = (state: string, extra: Record<string, unknown> = {}) => ({ id: "run_1", state, keep: false, createdAt: "2026-01-01T00:00:00Z", ...extra });

  test("createRun maps the request and always sends an idempotency key", async () => {
    const t = fake(() => json(201, run("creating")));
    const c = new DavitClient({ transport: t });
    const r = await c.createRun({ argv: ["sh", "-c", "exit 3"], image: "base", environment: "dev@2", cwd: "/w", env: { A: "1" }, timeoutSeconds: 30, keep: true, hold: true });
    assert.equal(r.state, "creating");
    const req = t.calls[0]!;
    assert.equal(`${req.method} ${req.path}`, "POST /api/v1/runs");
    assert.match(req.headers["Idempotency-Key"]!, UUID);
    assert.deepEqual(JSON.parse(req.body as string), {
      argv: ["sh", "-c", "exit 3"], image: "base", environment: { environment: "dev", revision: 2 }, cwd: "/w", env: { A: "1" }, timeoutSeconds: 30, keep: true, hold: true,
    });
    await c.createRun({ argv: ["true"], snapshotId: "snap_1" }, { idempotencyKey: "k-1" });
    assert.equal(t.calls[1]!.headers["Idempotency-Key"], "k-1");
    assert.deepEqual(JSON.parse(t.calls[1]!.body as string), { argv: ["true"], snapshotId: "snap_1" });
    await assert.rejects(c.createRun({ argv: [] }), TypeError);
    await assert.rejects(c.createRun({ argv: ["true"], timeoutSeconds: -1 }), /timeoutSeconds/);
    await assert.rejects(c.createRun({ argv: ["true"], environment: "dev@0" }), TypeError);
    assert.equal(t.calls.length, 2);
  });

  test("getRun, startRun, cancelRun routes without keys", async () => {
    const t = fake(() => json(200, run("running")));
    const c = new DavitClient({ transport: t });
    await c.getRun("run_1");
    await c.startRun(run("waiting") as never);
    await c.cancelRun("a/b");
    assert.deepEqual(t.calls.map((r) => `${r.method} ${r.path}`), ["GET /api/v1/runs/run_1", "POST /api/v1/runs/run_1/start", "DELETE /api/v1/runs/a%2Fb"]);
    for (const r of t.calls) {
      assert.equal(r.headers["Idempotency-Key"], undefined);
      assert.equal(r.body, undefined);
    }
  });

  test("waitRun polls until endedAt; the budget throws WaitTimeout with the run", async () => {
    const states = [run("running"), run("running", { exitCode: 0 }), run("succeeded", { endedAt: "2026-01-01T00:00:01Z" })];
    const t = fake((_, n) => json(200, states[n - 1]));
    const c = new DavitClient({ transport: t });
    const done = await c.waitRun("run_1");
    assert.equal(done.state, "succeeded");
    assert.equal(t.calls.length, 3);
    const c2 = new DavitClient({ transport: fake(() => json(200, run("running"))) });
    const err = await c2.waitRun("run_1", { timeoutMs: 0 }).catch((e: unknown) => e);
    assert.ok(err instanceof WaitTimeout);
    assert.deepEqual(err.details, { runId: "run_1" });
    assert.match(err.message, /run run_1/);
  });

  test("pushTree sends the tar with path and mode; mirror passes through", async () => {
    const res = { path: "/w", files: 1, bytes: 1024, removed: 0, sha256: "ab", generation: 1 };
    const t = fake(() => json(200, res));
    const c = new DavitClient({ transport: t });
    const tar = new Uint8Array([1, 2, 3]);
    assert.deepEqual(await c.pushTree("a/b", "/w", tar), res);
    const r = t.calls[0]!;
    assert.equal(`${r.method} ${r.path}`, "PUT /api/v1/vms/a%2Fb/tree?path=%2Fw&mode=merge");
    assert.equal(r.headers["Content-Type"], "application/x-tar");
    assert.equal(r.headers["Idempotency-Key"], undefined);
    assert.equal(r.body, tar);
    await c.pushTree("web", "/w", tar, { mode: "mirror" });
    assert.equal(t.calls[1]!.path, "/api/v1/vms/web/tree?path=%2Fw&mode=mirror");
    await assert.rejects(c.pushTree("web", "rel", tar), /absolute/);
    assert.equal(t.calls.length, 2);
  });

  test("runToCompletion: a run that fails before its command throws its typed error", async () => {
    const failed = run("failed", { endedAt: "2026-01-01T00:00:01Z", error: { code: "not_found", message: "no image", retryable: false, requestId: "r" } });
    const t = fake((req) => (req.method === "POST" ? json(201, run("creating")) : json(200, failed)));
    const c = new DavitClient({ transport: t });
    const err = await c.runToCompletion({ argv: ["true"], image: "nope" }).catch((e: unknown) => e);
    assert.ok(err instanceof NotFound);
    assert.deepEqual(err.details, { runId: "run_1" });
    assert.deepEqual(t.calls.map((r) => r.method), ["POST", "GET"]);
  });

  test("runToCompletion with files holds the run and defaults cwd to the push path", async () => {
    const t = fake((req) => (req.method === "POST" ? json(201, run("creating")) : json(200, run("cancelled", { endedAt: "2026-01-01T00:00:01Z" }))));
    const c = new DavitClient({ transport: t });
    const out = await c.runToCompletion({ argv: ["make"] }, { files: new Uint8Array([1]) });
    assert.equal(out.exitCode, 130);
    assert.equal(out.push, undefined);
    assert.deepEqual(JSON.parse(t.calls[0]!.body as string), { argv: ["make"], hold: true, cwd: "/home/davit/work" });
  });
});

describe("run contract: listRuns, outputGraceSeconds, outcome and signal", () => {
  const NOW = "2026-01-01T00:00:00Z";
  const run = (state: string, extra: Record<string, unknown> = {}) => ({ id: "run_1", state, keep: false, createdAt: NOW, ...extra });

  test("listRuns pages oldest first; iterRuns follows nextCursor", async () => {
    const t = fake((req) => (req.path.includes("cursor=c1") ? json(200, { items: [run("succeeded", { id: "run_2" })] }) : json(200, { items: [run("failed")], nextCursor: "c1" })));
    const c = new DavitClient({ transport: t });
    const ids: string[] = [];
    for await (const r of c.iterRuns({ limit: 1 })) ids.push(r.id);
    assert.deepEqual(ids, ["run_1", "run_2"]);
    assert.deepEqual(t.calls.map((r) => `${r.method} ${r.path}`), ["GET /api/v1/runs?limit=1", "GET /api/v1/runs?limit=1&cursor=c1"]);
  });

  test("createRun sends outputGraceSeconds (0 included) and validates it", async () => {
    const t = fake(() => json(201, run("creating")));
    const c = new DavitClient({ transport: t });
    await c.createRun({ argv: ["true"], outputGraceSeconds: 0 });
    await c.createRun({ argv: ["true"], outputGraceSeconds: 600 });
    await c.createRun({ argv: ["true"] });
    assert.deepEqual(t.calls.map((r) => JSON.parse(r.body as string).outputGraceSeconds), [0, 600, undefined]);
    for (const bad of [-1, 601, 1.5]) await assert.rejects(c.createRun({ argv: ["true"], outputGraceSeconds: bad }), /outputGraceSeconds/);
    await assert.rejects(c.runToCompletion({ argv: ["true"], outputGraceSeconds: 601 }), /outputGraceSeconds/);
    assert.equal(t.calls.length, 3);
  });

  // A transport whose HTTP calls are scripted and whose attach goes to a scripted WebSocket server.
  async function scripted(polls: unknown[], onStream: Parameters<typeof startServer>[0], output?: unknown) {
    const srv = await startServer(onStream);
    const ux = new UnixSocketTransport(srv.socketPath);
    const calls: TransportRequest[] = [];
    let i = 0;
    const transport: Transport = {
      async request(req) {
        calls.push(req);
        if (req.method === "POST") return json(201, run("creating"));
        if (req.method === "DELETE") return json(200, run("running", { outcome: "failed", decidedAt: NOW }));
        if (req.path.includes("/output")) return json(200, output);
        return json(200, polls[Math.min(i++, polls.length - 1)]);
      },
      connect: (signal) => ux.connect(signal),
    };
    return { c: new DavitClient({ transport }), calls, srv };
  }
  const killed = { id: "proc_1", vmId: "vm_1", generation: 1, status: "killed", argv: ["sh"], detached: true, signal: "KILL", outputCursor: 1, createdAt: NOW };
  const started = run("running", { vmId: "vm_1", processId: "proc_1" });

  test("runToCompletion cancels once decided (not on exitCode alone) and reports the signal", async () => {
    const decided = { ...started, outcome: "failed", decidedAt: NOW, signal: "KILL" };
    const { c, calls, srv } = await scripted([started, { ...started, exitCode: 0 }, decided, { ...decided, state: "failed", endedAt: NOW }], async (p) => {
      await p.readRequest();
      p.upgrade();
      p.sendJSON({ type: "stdout", seq: 1, data: Buffer.from("out\n").toString("base64") });
      p.sendJSON({ type: "exit", seq: 1, process: killed });
    });
    try {
      const out = await c.runToCompletion({ argv: ["sh"] });
      assert.equal(out.exitCode, 137);
      assert.equal(out.signal, "KILL");
      assert.equal(Buffer.from(out.stdout).toString(), "out\n");
      assert.equal(out.run.state, "failed");
      assert.deepEqual(calls.map((r) => r.method), ["POST", "GET", "GET", "GET", "DELETE", "GET"]);
    } finally {
      await srv.stop();
    }
  });

  test("runToCompletion with outputGraceSeconds 0 never cancels and reads retained output when the VM is gone", async () => {
    const ended = { ...started, state: "succeeded", outcome: "succeeded", decidedAt: NOW, endedAt: NOW, exitCode: 0 };
    const output = { items: [{ type: "stdout", seq: 1, data: Buffer.from("late\n").toString("base64") }], nextCursor: "1", complete: true };
    const { c, calls, srv } = await scripted([started, ended], async (p) => {
      await p.readRequest();
      const body = JSON.stringify({ error: { code: "invalid_state", message: "vm is deleting", retryable: false } });
      p.socket.end(`HTTP/1.1 409 Conflict\r\nContent-Type: application/json\r\nContent-Length: ${Buffer.byteLength(body)}\r\n\r\n${body}`);
    }, output);
    try {
      const out = await c.runToCompletion({ argv: ["true"], outputGraceSeconds: 0 });
      assert.equal(out.exitCode, 0);
      assert.equal(out.signal, undefined);
      assert.equal(Buffer.from(out.stdout).toString(), "late\n");
      assert.ok(!calls.some((r) => r.method === "DELETE"));
      assert.ok(calls.some((r) => r.path === "/api/v1/vms/vm_1/processes/proc_1/output"));
      assert.equal(JSON.parse(calls[0]!.body as string).outputGraceSeconds, 0);
    } finally {
      await srv.stop();
    }
  });
});
