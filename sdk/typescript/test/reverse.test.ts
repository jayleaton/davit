// Reverse forwards against a local raw HTTP/WebSocket server.

import assert from "node:assert/strict";
import { createServer, type AddressInfo, type Server, type Socket } from "node:net";
import { describe, test } from "node:test";

import { NotFound, REVERSE_CLOSED_REASON, DavitClient, TransportError, TUNNEL_CHUNK_BYTES, type ReverseForward } from "../src/index.ts";
import { startServer, type Peer } from "./ws-server.ts";

const NOW = "2026-10-22T04:22:34Z";
const BASE = "/api/v1/vms/web/reverse-forwards";

function forward(guestPort: number, kind: string, address: string): ReverseForward {
  return { id: "rev_1", vmId: "vm_1", generation: 1, guestPort, target: { kind, address }, state: "awaiting_session", active: 0, createdAt: NOW, sessionExpiresAt: NOW };
}

const closeFrame = (code: number) => Buffer.from([code >> 8, code & 0xff]);

/** Answers a plain HTTP request and closes the connection. */
function reply(p: Peer, status: number, body?: unknown): void {
  const b = body === undefined ? Buffer.alloc(0) : Buffer.from(JSON.stringify(body));
  p.socket.end(Buffer.concat([
    Buffer.from(`HTTP/1.1 ${status} X\r\nContent-Type: application/json\r\nContent-Length: ${b.length}\r\nConnection: close\r\n\r\n`),
    b,
  ]));
}

/** Plain API calls: create answers the forward it was asked for, list one item, delete 204. */
function api(p: Peer): boolean {
  if (p.headers.upgrade) return false;
  if (p.method === "POST") {
    const req = JSON.parse(p.body.toString()) as { guestPort: number; target: { kind: string; address: string } };
    reply(p, 201, forward(req.guestPort, req.target.kind, req.target.address));
  } else if (p.method === "GET") reply(p, 200, { items: [forward(5432, "session", "x:1")] });
  else reply(p, 204);
  return true;
}

function deferred(): { promise: Promise<void>; resolve: () => void } {
  let resolve!: () => void;
  const promise = new Promise<void>((r) => (resolve = r));
  return { promise, resolve };
}

async function listen(server: Server): Promise<number> {
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  return (server.address() as AddressInfo).port;
}

function readAll(s: Socket): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const parts: Buffer[] = [];
    s.on("data", (c: Buffer) => parts.push(c));
    s.once("end", () => resolve(Buffer.concat(parts)));
    s.once("error", reject);
  });
}

async function until(pred: () => boolean, what: string): Promise<void> {
  const deadline = Date.now() + 5000;
  while (!pred()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 20));
  }
}

describe("reverse forwards", () => {
  test("wrappers: list, create, host service, revoke; ports are checked", async () => {
    const srv = await startServer(async (p) => {
      await p.readRequest();
      api(p);
    });
    try {
      const c = new DavitClient({ socket: srv.socketPath });
      assert.deepEqual((await c.listReverseForwards("web")).items.map((f) => f.id), ["rev_1"]);
      const f = await c.createReverseForward("web", 5432, { kind: "session", address: "db:5432" });
      assert.equal(f.target.kind, "session");
      const h = await c.forwardHostService("web", 8080, "10.0.0.5:8080");
      assert.equal(h.target.kind, "host");
      await c.revokeReverseForward("web", f);
      await c.revokeReverseForward("web", "rev_2");
      assert.deepEqual(srv.peers.map((p) => `${p.method} ${p.path}`), [
        `GET ${BASE}`,
        `POST ${BASE}`,
        `POST ${BASE}`,
        `DELETE ${BASE}/rev_1`,
        `DELETE ${BASE}/rev_2`,
      ]);
      assert.deepEqual(JSON.parse(srv.peers[1]!.body.toString()), { guestPort: 5432, target: { kind: "session", address: "db:5432" } });
      assert.deepEqual(JSON.parse(srv.peers[2]!.body.toString()), { guestPort: 8080, target: { kind: "host", address: "10.0.0.5:8080" } });
      for (const bad of [0, 65536, 1.5]) await assert.rejects(c.createReverseForward("web", bad, { kind: "session" }), /guestPort/);
      await assert.rejects(c.reverseForward("web", 5432, "127.0.0.1", 0), /localPort/);
    } finally {
      await srv.stop();
    }
  });

  test("bridges with half-close both ways and surfaces the revoked reason", async () => {
    const toLocal = Buffer.alloc(76_800);
    for (let i = 0; i < toLocal.length; i++) toLocal[i] = i & 0xff;
    let atLocal: Buffer = Buffer.alloc(0);
    let back: Buffer = Buffer.alloc(0);
    const sizes: number[] = [];
    const bridged = deferred();
    const local = createServer({ allowHalfOpen: true }, async (s) => {
      atLocal = await readAll(s); // until the guest's half-close
      s.end(Buffer.from(atLocal).reverse()); // still writable after EOF
    });
    const port = await listen(local);
    const srv = await startServer(async (p) => {
      await p.readRequest();
      if (api(p)) return;
      p.upgrade();
      if (p.path.endsWith("/session")) {
        p.sendJSON({ type: "ping" });
        p.send(2, "ignored"); // not an event
        p.send(1, "not json");
        p.sendJSON({ type: "connection", connectionId: "conn_1" });
        await bridged.promise;
        p.sendJSON({ type: "revoked", reason: "vm stopped" });
        p.send(8, closeFrame(1000));
        return;
      }
      p.send(1, "text is not connection bytes");
      for (let off = 0; off < toLocal.length; off += 30_000) p.send(2, toLocal.subarray(off, off + 30_000));
      p.send(2, Buffer.alloc(0)); // the guest half-closes
      for (;;) {
        const f = await p.readFrame();
        assert.equal(f.op, 2);
        if (f.payload.length === 0) break; // the local half-close
        sizes.push(f.payload.length);
        back = Buffer.concat([back, f.payload]);
      }
      assert.equal((await p.readFrame()).op, 8); // both directions ended: the client closes
      bridged.resolve();
    });
    try {
      const c = new DavitClient({ socket: srv.socketPath });
      const s = await c.reverseForward("web", 5432, "127.0.0.1", port);
      assert.equal(s.forward.target.address, `127.0.0.1:${port}`);
      assert.equal(await s.closed, "vm stopped");
      assert.equal(s.reason, "vm stopped");
      assert.equal(s.error, undefined);
      assert.equal(s.active, 0);
      s.close(); // after the end: keeps the reason
      assert.equal(s.reason, "vm stopped");
      assert.ok(atLocal.equals(toLocal));
      assert.ok(back.equals(Buffer.from(toLocal).reverse()));
      assert.ok(Math.max(...sizes) <= TUNNEL_CHUNK_BYTES);
      const create = srv.peers[0]!;
      assert.deepEqual(JSON.parse(create.body.toString()), { guestPort: 5432, target: { kind: "session", address: `127.0.0.1:${port}` } });
      assert.deepEqual(srv.peers.slice(1).map((p) => p.path).sort(), [`${BASE}/rev_1/connections/conn_1`, `${BASE}/rev_1/session`]);
      for (const p of srv.peers) assert.match(p.headers["x-request-id"] ?? "", /^req_/);
    } finally {
      await srv.stop();
      await new Promise((r) => local.close(r));
    }
  });

  test("an unreachable local service closes that connection; close() revokes", async () => {
    const unused = createServer();
    const dead = await listen(unused);
    await new Promise((r) => unused.close(r));
    const connClosed = deferred();
    const sessionClosed = deferred();
    const srv = await startServer(async (p) => {
      await p.readRequest();
      if (api(p)) return;
      p.upgrade();
      if (p.path.endsWith("/session")) {
        p.sendJSON({ type: "connection", connectionId: "conn_1" });
        if ((await p.readFrame()).op === 8) sessionClosed.resolve();
        return;
      }
      if ((await p.readFrame()).op === 8) connClosed.resolve(); // closed without bytes
    });
    try {
      const c = new DavitClient({ socket: srv.socketPath });
      const s = await c.reverseForward("web", 5432, "127.0.0.1", dead);
      await connClosed.promise;
      assert.equal(s.reason, undefined);
      s.close();
      s.close(); // idempotent
      assert.equal(await s.closed, REVERSE_CLOSED_REASON);
      assert.equal(s.error, undefined);
      await sessionClosed.promise;
    } finally {
      await srv.stop();
    }
  });

  test("close() cuts live connections; a dropped stream sets error", async () => {
    const received = deferred();
    const localEnded = deferred();
    const release = deferred();
    const local = createServer({ allowHalfOpen: true }, (s) => {
      s.once("data", () => received.resolve());
      s.on("end", () => {
        localEnded.resolve();
        s.destroy();
      });
      s.on("error", () => {});
    });
    const port = await listen(local);
    let sessions = 0;
    const srv = await startServer(async (p) => {
      await p.readRequest();
      if (api(p)) return;
      p.upgrade();
      if (p.path.endsWith("/session")) {
        if (sessions++ === 0) {
          p.sendJSON({ type: "connection", connectionId: "conn_1" });
          await p.readFrame(); // the client's close
        } else {
          await release.promise;
          p.socket.destroy(); // dropped without a close frame
        }
        return;
      }
      p.send(2, "hi");
      while ((await p.readFrame()).op !== 8);
    });
    try {
      const c = new DavitClient({ socket: srv.socketPath });
      const s = await c.reverseForward("web", 5432, "127.0.0.1", port);
      await received.promise;
      await until(() => s.active === 1, "the bridge");
      s.close();
      await localEnded.promise;
      assert.equal(await s.closed, REVERSE_CLOSED_REASON);
      assert.equal(s.active, 0);

      const dropped = await c.reverseForward("web", 5433, "127.0.0.1", port);
      release.resolve();
      assert.match(await dropped.closed, /^reverse session stream ended \(1006\)/);
      assert.ok(dropped.error instanceof TransportError);
      assert.match(dropped.error.message, /rev_1/);
    } finally {
      await srv.stop();
      await new Promise((r) => local.close(r));
    }
  });

  test("an attach error is the typed API error and the forward is revoked", async () => {
    const srv = await startServer(async (p) => {
      await p.readRequest();
      if (api(p)) return;
      reply(p, 404, { error: { code: "not_found", message: "forward gone" } });
    });
    try {
      const c = new DavitClient({ socket: srv.socketPath });
      await assert.rejects(c.reverseForward("web", 5432, "127.0.0.1", 9), (e: unknown) => e instanceof NotFound && /forward gone/.test(e.message));
      assert.deepEqual(srv.peers.map((p) => `${p.method} ${p.path}`), [`POST ${BASE}`, `GET ${BASE}/rev_1/session`, `DELETE ${BASE}/rev_1`]);
    } finally {
      await srv.stop();
    }
  });
});
