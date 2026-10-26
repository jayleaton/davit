// Tunnel streams against a local raw HTTP/WebSocket server.

import assert from "node:assert/strict";
import { describe, test } from "node:test";

import { NotFound, DavitClient, TUNNEL_CHUNK_BYTES, TransportError, type Tunnel } from "../src/index.ts";
import { startServer, type Peer } from "./ws-server.ts";

const NOW = "2026-10-22T04:22:34Z";
const TUNNEL: Tunnel = { id: "tun_1", vmId: "vm_1", generation: 1, port: 8080, createdAt: NOW, expiresAt: NOW };

const closeFrame = (code: number) => Buffer.from([code >> 8, code & 0xff]);

/** Answers a plain HTTP request and closes the connection. */
function reply(p: Peer, status: number, body?: unknown): void {
  const b = body === undefined ? Buffer.alloc(0) : Buffer.from(JSON.stringify(body));
  p.socket.end(Buffer.concat([
    Buffer.from(`HTTP/1.1 ${status} X\r\nContent-Type: application/json\r\nContent-Length: ${b.length}\r\nConnection: close\r\n\r\n`),
    b,
  ]));
}

describe("tunnel streams", () => {
  test("binary relay in ≤32 KiB messages, half-close, server close is EOF", async () => {
    const sizes: number[] = [];
    let received = Buffer.alloc(0);
    const srv = await startServer(async (p) => {
      await p.readRequest();
      p.upgrade();
      for (;;) {
        const f = await p.readFrame();
        assert.ok(f.masked);
        assert.equal(f.op, 2);
        if (f.payload.length === 0) break; // the half-close
        sizes.push(f.payload.length);
        received = Buffer.concat([received, f.payload]);
      }
      p.send(1, "ignored"); // text messages are not tunnel bytes
      p.send(2, Buffer.from(received.subarray(0, 1000)).reverse());
      p.send(2, "!");
      p.send(8, closeFrame(1000));
      await p.readFrame(); // the echoed close
    });
    try {
      const c = new DavitClient({ socket: srv.socketPath });
      const t = await c.attachTunnel("tun_1");
      const payload = Buffer.alloc(76_800);
      for (let i = 0; i < payload.length; i++) payload[i] = i & 0xff;
      await t.write(payload);
      t.closeWrite();
      t.closeWrite(); // idempotent
      await assert.rejects(t.write("late"), /closed for writing/);
      const out: Uint8Array[] = [];
      for await (const chunk of t) out.push(chunk);
      assert.deepEqual(Buffer.concat(out), Buffer.concat([Buffer.from(payload.subarray(0, 1000)).reverse(), Buffer.from("!")]));
      assert.equal(await t.read(), null); // EOF stays EOF
      assert.equal(t.tunnel, undefined);
      assert.deepEqual(received, payload);
      assert.equal(sizes.length, 3);
      assert.ok(Math.max(...sizes) <= TUNNEL_CHUNK_BYTES);
      const peer = srv.peers[0]!;
      assert.equal(peer.path, "/api/v1/tunnels/tun_1/stream");
      assert.match(peer.headers["x-request-id"] ?? "", /^req_/);
      t.close();
    } finally {
      await srv.stop();
    }
  });

  test("an abnormal close or a dropped connection is a TransportError", async () => {
    const srv = await startServer(async (p, i) => {
      await p.readRequest();
      p.upgrade();
      if (i === 0) p.send(8, closeFrame(1001)); // the guest side failed
      else p.socket.destroy();
    });
    try {
      const c = new DavitClient({ socket: srv.socketPath });
      const a = await c.attachTunnel("tun_1");
      await assert.rejects(a.read(), (e: unknown) => e instanceof TransportError && /\(1001\)/.test(e.message));
      const b = await c.attachTunnel("tun_2");
      await assert.rejects(b.read(), TransportError);
    } finally {
      await srv.stop();
    }
  });

  test("connectTunnel opens, then attaches", async () => {
    const srv = await startServer(async (p, i) => {
      await p.readRequest();
      if (i === 0) return reply(p, 201, TUNNEL);
      p.upgrade();
      assert.deepEqual((await p.readFrame()).payload, Buffer.from("ping"));
      p.send(2, "pong");
      p.send(8, closeFrame(1000));
    });
    try {
      const c = new DavitClient({ socket: srv.socketPath });
      const t = await c.connectTunnel("web", 8080);
      await t.write("ping");
      assert.equal(Buffer.from((await t.read())!).toString(), "pong");
      assert.equal(await t.read(), null);
      assert.equal(t.tunnel?.port, 8080);
      const [open, attach] = srv.peers;
      assert.equal(`${open!.method} ${open!.path}`, "POST /api/v1/vms/web/tunnels");
      assert.deepEqual(JSON.parse(open!.body.toString()), { port: 8080 });
      assert.equal(attach!.path, "/api/v1/tunnels/tun_1/stream");
      await assert.rejects(c.openTunnel("web", 0), TypeError);
    } finally {
      await srv.stop();
    }
  });

  test("an attach error is the typed API error and the tunnel is released", async () => {
    const srv = await startServer(async (p, i) => {
      await p.readRequest();
      if (i === 0) return reply(p, 201, TUNNEL);
      if (i === 1) return reply(p, 404, { error: { code: "not_found", message: "tunnel gone" } });
      reply(p, 204);
    });
    try {
      const c = new DavitClient({ socket: srv.socketPath });
      await assert.rejects(c.connectTunnel("web", 8080), (e: unknown) => e instanceof NotFound && e.status === 404 && /tunnel gone/.test(e.message));
      assert.deepEqual(srv.peers.map((p) => `${p.method} ${p.path}`), [
        "POST /api/v1/vms/web/tunnels",
        "GET /api/v1/tunnels/tun_1/stream",
        "DELETE /api/v1/tunnels/tun_1",
      ]);
    } finally {
      await srv.stop();
    }
  });
});
