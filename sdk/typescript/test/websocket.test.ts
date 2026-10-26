// WebSocket client and process stream tests against a local raw server.

import assert from "node:assert/strict";
import { createConnection } from "node:net";
import { describe, test } from "node:test";

import {
  InvalidState,
  NotFound,
  DavitClient,
  TransportError,
  WebSocketConnection,
  processExitCode,
  type Process,
} from "../src/index.ts";
import { startServer, type Peer } from "./ws-server.ts";

async function dial(socketPath: string, opts: { maxMessageBytes?: number; headers?: Record<string, string> } = {}): Promise<WebSocketConnection> {
  const socket = createConnection({ path: socketPath });
  await new Promise<void>((resolve, reject) => socket.once("connect", resolve).once("error", reject));
  return WebSocketConnection.connect(socket, { host: "localhost", path: "/ws?x=1", ...opts });
}

const text = (b: Uint8Array) => Buffer.from(b).toString("utf8");

describe("websocket client", () => {
  test("handshake headers, masked echo, close handshake", async () => {
    let closeFrame: { op: number; payload: Buffer } | undefined;
    const srv = await startServer(async (p) => {
      await p.readRequest();
      p.upgrade();
      const f = await p.readFrame();
      assert.ok(f.masked, "client frames must be masked");
      p.send(1, f.payload);
      p.send(8, Buffer.concat([Buffer.from([0x03, 0xe8]), Buffer.from("bye")]));
      closeFrame = await p.readFrame();
    });
    try {
      const ws = await dial(srv.socketPath, { headers: { "X-Request-Id": "req_1" } });
      const peer = srv.peers[0]!;
      assert.equal(peer.method, "GET");
      assert.equal(peer.path, "/ws?x=1");
      assert.equal(peer.headers["sec-websocket-version"], "13");
      assert.equal(peer.headers["x-request-id"], "req_1");
      ws.sendText("héllo");
      const m = await ws.next();
      assert.equal(m?.type, "text");
      assert.equal(text(m!.data), "héllo");
      assert.equal(await ws.next(), null);
      assert.equal(ws.closeCode, 1000);
      assert.equal(ws.closeReason, "bye");
      await new Promise((r) => setTimeout(r, 50));
      assert.equal(closeFrame?.op, 8, "client must echo the close frame");
      assert.throws(() => ws.sendText("late"), TransportError);
    } finally {
      await srv.stop();
    }
  });

  test("fragments are reassembled; pings get pongs; frames in the 101 packet are kept", async () => {
    let pong: Buffer | undefined;
    const srv = await startServer(async (p) => {
      await p.readRequest();
      p.upgrade({ extra: p.frame(1, "early") });
      p.send(1, "frag", { fin: false });
      p.send(9, "ping!");
      p.send(0, "men", { fin: false });
      p.send(0, "ted");
      p.send(2, Buffer.alloc(70000, 7));
      pong = (await p.readFrame()).payload;
    });
    try {
      const ws = await dial(srv.socketPath);
      assert.equal(text((await ws.next())!.data), "early");
      assert.equal(text((await ws.next())!.data), "fragmented");
      const big = await ws.next();
      assert.equal(big?.type, "binary");
      assert.equal(big?.data.length, 70000);
      await new Promise((r) => setTimeout(r, 20));
      assert.equal(pong?.toString(), "ping!");
      ws.close();
    } finally {
      await srv.stop();
    }
  });

  for (const [name, script, wantClose] of [
    ["oversize message → 1009", (p: Peer) => p.send(1, Buffer.alloc(2000, 97)), 1009],
    ["oversize fragmented message → 1009", (p: Peer) => { p.send(1, Buffer.alloc(600, 97), { fin: false }); p.send(0, Buffer.alloc(600, 97)); }, 1009],
    ["masked server frame → 1002", (p: Peer) => p.send(1, "x", { mask: true }), 1002],
    ["reserved bits → 1002", (p: Peer) => p.send(1, "x", { rsv: true }), 1002],
    ["unknown opcode → 1002", (p: Peer) => p.send(3, "x"), 1002],
    ["continuation without a message → 1002", (p: Peer) => p.send(0, "x"), 1002],
    ["fragmented control frame → 1002", (p: Peer) => p.send(9, "x", { fin: false }), 1002],
    ["invalid UTF-8 text → 1007", (p: Peer) => p.send(1, Buffer.from([0xff, 0xfe])), 1007],
  ] as const) {
    test(name, async () => {
      let code: number | undefined;
      const srv = await startServer(async (p) => {
        await p.readRequest();
        p.upgrade();
        script(p);
        const f = await p.readFrame();
        if (f.op === 8) code = f.payload.readUInt16BE(0);
      });
      try {
        const ws = await dial(srv.socketPath, { maxMessageBytes: 1024 });
        await assert.rejects(ws.next(), TransportError);
        await new Promise((r) => setTimeout(r, 30));
        assert.equal(code, wantClose);
      } finally {
        await srv.stop();
      }
    });
  }

  test("bad Sec-WebSocket-Accept is rejected", async () => {
    const srv = await startServer(async (p) => {
      await p.readRequest();
      p.upgrade({ badAccept: true });
    });
    try {
      await assert.rejects(dial(srv.socketPath), /Sec-WebSocket-Accept/);
    } finally {
      await srv.stop();
    }
  });

  for (const chunked of [false, true]) {
    test(`non-101 answers become typed API errors (${chunked ? "chunked" : "content-length"})`, async () => {
      const srv = await startServer(async (p) => {
        await p.readRequest();
        p.respond(404, { error: { code: "not_found", message: "no such process", retryable: false, requestId: "req_x" } }, { chunked });
      });
      try {
        const err = await dial(srv.socketPath).catch((e: unknown) => e);
        assert.ok(err instanceof NotFound, String(err));
        assert.equal(err.status, 404);
        assert.equal(err.requestId, "req_x");
      } finally {
        await srv.stop();
      }
    });
  }

  test("peer vanishing without a close frame ends with code 1006", async () => {
    const srv = await startServer(async (p) => {
      await p.readRequest();
      p.upgrade();
      p.socket.destroy();
    });
    try {
      const ws = await dial(srv.socketPath);
      assert.equal(await ws.next(), null);
      assert.equal(ws.closeCode, 1006);
    } finally {
      await srv.stop();
    }
  });
});

function proc(extra: Partial<Process> = {}): Process {
  return { id: "proc_1", vmId: "vm_1", generation: 1, status: "exited", argv: ["x"], detached: false, outputCursor: 0, createdAt: "2026-01-01T00:00:00Z", ...extra };
}
const b64 = (s: string) => Buffer.from(s).toString("base64");

describe("process streams", () => {
  test("reconnects from the cursor and drops duplicate output", async () => {
    const cursors: string[] = [];
    const srv = await startServer(async (p, i) => {
      await p.readRequest();
      cursors.push(new URL(p.path, "http://x").searchParams.get("outputCursor") ?? "");
      assert.equal(p.headers.authorization, "Bearer tok");
      p.upgrade();
      if (i === 0) {
        p.sendJSON({ type: "stdout", seq: 1, data: b64("a") });
        p.sendJSON({ type: "stderr", seq: 2, data: b64("b") });
        await new Promise((r) => setTimeout(r, 20));
        p.socket.destroy(); // drop without exit
        return;
      }
      p.sendJSON({ type: "stderr", seq: 2, data: b64("b") }); // duplicate
      p.sendJSON({ type: "future-type", seq: 9 }); // ignored
      p.sendJSON({ type: "stdout", seq: 3, data: b64("c") });
      p.sendJSON({ type: "exit", seq: 3, process: proc({ exitCode: 0, outputCursor: 3 }) });
    });
    try {
      const c = new DavitClient({ socket: srv.socketPath, token: "tok" });
      const s = await c.attachProcess("vm_1", "proc_1");
      const seen: string[] = [];
      for await (const ev of s) seen.push(ev.type === "exit" ? `exit@${ev.seq}` : `${ev.type}:${ev.seq}:${text(ev.data)}`);
      assert.deepEqual(seen, ["stdout:1:a", "stderr:2:b", "stdout:3:c", "exit@3"]);
      assert.deepEqual(cursors, ["0", "2"]);
      assert.equal(s.cursor, 3);
      assert.equal(s.exit?.exitCode, 0);
      assert.match(srv.peers[0]!.path, /^\/api\/v1\/vms\/vm_1\/processes\/proc_1\/stream\?outputCursor=0$/);
    } finally {
      await srv.stop();
    }
  });

  test("gives up re-attaching with a TransportError naming the cursor", async () => {
    const srv = await startServer(async (p, i) => {
      if (i > 0) {
        p.socket.destroy();
        return;
      }
      await p.readRequest();
      p.upgrade();
      p.sendJSON({ type: "stdout", seq: 4, data: b64("x") });
      await new Promise((r) => setTimeout(r, 20));
      p.socket.destroy();
    });
    try {
      const s = await new DavitClient({ socket: srv.socketPath }).attachProcess("vm", "p", { outputCursor: 3 });
      const err = await (async () => {
        for await (const _ of s);
      })().catch((e: unknown) => e);
      assert.ok(err instanceof TransportError, String(err));
      assert.match(err.message, /outputCursor 4/);
    } finally {
      await srv.stop();
    }
  });

  test("a server error message throws the typed error", async () => {
    const srv = await startServer(async (p) => {
      await p.readRequest();
      p.upgrade();
      p.sendJSON({ type: "error", error: { code: "invalid_state", message: "cursor too old", retryable: false } });
    });
    try {
      const s = await new DavitClient({ socket: srv.socketPath }).attachProcess("vm", "p");
      await assert.rejects(
        (async () => {
          for await (const _ of s);
        })(),
        InvalidState,
      );
    } finally {
      await srv.stop();
    }
  });

  test("run: exec body, stdin chunks, stdin.close, collected output", async () => {
    let execBody: Record<string, unknown> | undefined;
    let key: string | undefined;
    const big = "x".repeat(300 * 1024);
    const srv = await startServer(async (p) => {
      await p.readRequest();
      if (p.method === "POST") {
        execBody = JSON.parse(p.body.toString("utf8")) as Record<string, unknown>;
        key = p.headers["idempotency-key"];
        p.respond(201, proc({ status: "running" }));
        return;
      }
      p.upgrade();
      let seq = 0;
      let input = "";
      for (;;) {
        const m = await p.readJSON();
        if (m.type === "stdin") input += Buffer.from(String(m.data), "base64").toString();
        if (m.type === "stdin.close") break;
      }
      p.sendJSON({ type: "stdout", seq: ++seq, data: b64(input.slice(0, 5)) });
      p.sendJSON({ type: "stderr", seq: ++seq, data: b64(String(input.length)) });
      p.sendJSON({ type: "exit", seq, process: proc({ exitCode: 259 }) });
    });
    try {
      const c = new DavitClient({ socket: srv.socketPath });
      const r = await c.run("vm_1", ["cat"], { input: "hello" + big, env: { A: "1" }, deadlineSeconds: 5 });
      assert.deepEqual(execBody, { argv: ["cat"], env: { A: "1" }, deadlineSeconds: 5, detached: false, stdin: true });
      assert.ok(key && key.length > 0);
      assert.equal(r.stdoutText, "hello");
      assert.equal(r.stderrText, String(5 + big.length));
      assert.equal(r.exitCode, 259 & 0xff);
      assert.ok(srv.peers.length === 2, "stdin of 300 KiB must not need a second stream");
    } finally {
      await srv.stop();
    }
  });
});

test("exit status mapping matches the CLI", () => {
  assert.equal(processExitCode(proc({ exitCode: 3 })), 3);
  assert.equal(processExitCode(proc({ exitCode: 3, timedOut: true, signal: "KILL" })), 124);
  assert.equal(processExitCode(proc({ status: "killed", signal: "TERM" })), 143);
  assert.equal(processExitCode(proc({ status: "killed", signal: "SIGINT" })), 130);
  assert.equal(processExitCode(proc({ status: "lost" })), 255);
  assert.equal(processExitCode(undefined), 255);
});

test("execProcess validates argv", async () => {
  const c = new DavitClient({ transport: { request: async () => ({ status: 201, headers: {}, body: "{}" }) } });
  await assert.rejects(c.execProcess("vm", []), TypeError);
  await assert.rejects(c.execProcess("vm", "ls -l" as unknown as string[]), TypeError);
  for (const bad of [0, 1.5, -1, Number.NaN]) await assert.rejects(c.execProcess("vm", ["x"], { deadlineSeconds: bad }), TypeError);
  await assert.rejects(c.attachProcess("vm", "p"), /connect\(\)/);
});
