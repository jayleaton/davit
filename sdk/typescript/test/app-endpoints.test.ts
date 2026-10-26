// App endpoint (G11) and desktop (G15) wrappers against a scripted in-memory transport.

import assert from "node:assert/strict";
import { describe, test } from "node:test";

import {
  Conflict,
  InvalidState,
  NotFound,
  DavitClient,
  UnsupportedCapability,
  type AppEndpoint,
  type Transport,
  type TransportRequest,
  type TransportResponse,
} from "../src/index.ts";

const BASE = "/api/v1/vms/web/app-endpoints";

function endpoint(name = "port-3000", port = 3000, hidden = false, kind = "http"): AppEndpoint {
  return {
    id: "app_1", vmId: "vm_1", name, port, kind, url: "https://gw.example.ts.net:20001/", gatewayPort: 20001,
    hidden, forwardIdentity: false, state: hidden ? "hidden" : "active", createdAt: "2026-10-22T04:22:34Z",
  };
}

function json(status: number, body: unknown): TransportResponse {
  return { status, headers: {}, body: JSON.stringify(body) };
}

/** create answers the endpoint it was asked for, update applies the patch, list one item, delete 204. */
function api(): Transport & { calls: TransportRequest[] } {
  const calls: TransportRequest[] = [];
  return {
    calls,
    async request(req) {
      calls.push(req);
      const body = req.body === undefined ? {} : (JSON.parse(req.body as string) as { port?: number; name?: string; hidden?: boolean });
      if (req.method === "POST") return json(201, endpoint(body.name ?? `port-${body.port}`, body.port));
      if (req.method === "PATCH") return json(200, endpoint(body.name ?? "port-3000", 3000, body.hidden ?? false));
      if (req.method === "GET") return json(200, { items: [endpoint()] });
      return { status: 204, headers: {}, body: "" };
    },
  };
}

const shape = (r: TransportRequest) => [r.method, r.path, r.body === undefined ? undefined : JSON.parse(r.body as string)];

describe("app endpoints", () => {
  test("forwardIdentity is opt-in", async () => {
    const t = api();
    const c = new DavitClient({ transport: t });
    await c.expose("web", 3000);
    await c.expose("web", 3001, { forwardIdentity: true });
    await c.updateAppEndpoint("web", "port-3000", { forwardIdentity: false });
    assert.deepEqual(t.calls.map((r) => shape(r)[2]), [{ port: 3000 }, { port: 3001, forwardIdentity: true }, { forwardIdentity: false }]);
  });

  test("wrappers send the contract shapes", async () => {
    const t = api();
    const c = new DavitClient({ transport: t });
    const listed = (await c.listAppEndpoints("web")).items;
    assert.deepEqual(listed.map((e) => [e.id, e.gatewayPort, e.state]), [["app_1", 20001, "active"]]);
    const ep = await c.createAppEndpoint("web", 3000);
    assert.equal(ep.url, "https://gw.example.ts.net:20001/");
    assert.equal(ep.kind, "http");
    assert.equal((await c.createAppEndpoint("web", 8080, "api")).name, "api");
    assert.equal((await c.expose("web", 5173, "vite")).port, 5173);
    assert.equal((await c.updateAppEndpoint("web", ep, { name: "app", hidden: true })).hidden, true);
    assert.equal((await c.hideAppEndpoint("web", "app")).state, "hidden");
    assert.equal((await c.showAppEndpoint("web", "app")).hidden, false);
    assert.equal((await c.renameAppEndpoint("web", "app_1", "site")).name, "site");
    await c.deleteAppEndpoint("web", ep);
    await c.deleteAppEndpoint("web", "site");
    assert.deepEqual(t.calls.map(shape), [
      ["GET", BASE, undefined],
      ["POST", BASE, { port: 3000 }],
      ["POST", BASE, { port: 8080, name: "api" }],
      ["POST", BASE, { port: 5173, name: "vite" }],
      ["PATCH", `${BASE}/app_1`, { name: "app", hidden: true }],
      ["PATCH", `${BASE}/app`, { hidden: true }],
      ["PATCH", `${BASE}/app`, { hidden: false }],
      ["PATCH", `${BASE}/app_1`, { name: "site" }],
      ["DELETE", `${BASE}/app_1`, undefined],
      ["DELETE", `${BASE}/site`, undefined],
    ]);
    for (const r of t.calls) assert.equal(r.headers["Idempotency-Key"], undefined);
  });

  test("names are escaped; bad ports and empty refs never reach the server", async () => {
    const t = api();
    const c = new DavitClient({ transport: t });
    await c.hideAppEndpoint("my vm", "a/b c?d");
    await c.deleteAppEndpoint("web", "%2e%2e");
    assert.deepEqual(t.calls.map((r) => r.path), [
      "/api/v1/vms/my%20vm/app-endpoints/a%2Fb%20c%3Fd",
      `${BASE}/%252e%252e`,
    ]);
    await assert.rejects(c.deleteAppEndpoint("web", ""), /endpoint must be a non-empty ID or name/);
    for (const bad of [0, 65536, 1.5]) await assert.rejects(c.createAppEndpoint("web", bad), /port must be an integer/);
    assert.equal(t.calls.length, 2);
  });

  test("422 and 409 map to the typed errors", async () => {
    const t: Transport = {
      async request(req) {
        if (req.method === "POST") return json(422, { error: { code: "unsupported_capability", message: "no tailnet gateway", requestId: "req_srv" } });
        return json(409, { error: { code: "invalid_state", message: "name taken", requestId: "req_srv" } });
      },
    };
    const c = new DavitClient({ transport: t });
    const unsupported = await c.expose("web", 3000).catch((e: unknown) => e);
    assert.ok(unsupported instanceof UnsupportedCapability);
    assert.equal(unsupported.status, 422);
    assert.equal(unsupported.code, "unsupported_capability");
    const conflict = await c.renameAppEndpoint("web", "app_1", "taken").catch((e: unknown) => e);
    assert.ok(conflict instanceof Conflict && conflict instanceof InvalidState);
    assert.equal(conflict.status, 409);
    assert.equal(conflict.requestId, "req_srv");
  });

  test("desktop wrappers send the contract shapes", async () => {
    const calls: TransportRequest[] = [];
    const t: Transport = {
      async request(req) {
        calls.push(req);
        if (req.method === "DELETE") return { status: 204, headers: {}, body: "" };
        const port = req.body === undefined ? 5900 : (JSON.parse(req.body as string) as { port: number }).port;
        return json(req.method === "POST" && port === 5900 ? 201 : 200, endpoint("desktop", port, false, "desktop"));
      },
    };
    const c = new DavitClient({ transport: t });
    const opened = await c.openDesktop("web");
    assert.deepEqual([opened.kind, opened.name, opened.port], ["desktop", "desktop", 5900]);
    assert.equal((await c.openDesktop("web", 5901)).port, 5901);
    assert.equal((await c.getDesktop("web")).kind, "desktop");
    assert.equal(await c.closeDesktop("web"), undefined);
    await c.getDesktop("my vm");
    const DESKTOP = "/api/v1/vms/web/desktop";
    assert.deepEqual(calls.map(shape), [
      ["POST", DESKTOP, undefined],
      ["POST", DESKTOP, { port: 5901 }],
      ["GET", DESKTOP, undefined],
      ["DELETE", DESKTOP, undefined],
      ["GET", "/api/v1/vms/my%20vm/desktop", undefined],
    ]);
    // Without a port, open sends no body at all (the server default applies).
    assert.equal(calls[0]?.headers["Content-Type"], undefined);
    assert.equal(calls[1]?.headers["Content-Type"], "application/json");
    for (const r of calls) assert.equal(r.headers["Idempotency-Key"], undefined);
    for (const bad of [0, 65536, 1.5]) await assert.rejects(c.openDesktop("web", bad), /port must be an integer/);
    assert.equal(calls.length, 5);
  });

  test("desktop 404 and 422 map to the typed errors", async () => {
    const t: Transport = {
      async request(req) {
        if (req.method === "POST") return json(422, { error: { code: "unsupported_capability", message: "no tailnet gateway", requestId: "req_srv" } });
        return json(404, { error: { code: "not_found", message: "no desktop is open", requestId: "req_srv" } });
      },
    };
    const c = new DavitClient({ transport: t });
    const missing = await c.getDesktop("web").catch((e: unknown) => e);
    assert.ok(missing instanceof NotFound, `expected NotFound, got ${String(missing)}`);
    assert.equal(missing.status, 404);
    assert.equal(missing.code, "not_found");
    assert.equal(missing.requestId, "req_srv");
    await assert.rejects(c.closeDesktop("web"), NotFound);
    const unsupported = await c.openDesktop("web").catch((e: unknown) => e);
    assert.ok(unsupported instanceof UnsupportedCapability);
    assert.equal(unsupported.status, 422);
    assert.equal(unsupported.code, "unsupported_capability");
  });
});
