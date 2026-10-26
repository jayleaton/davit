// A scriptable raw HTTP/WebSocket server on a unix socket for unit tests.
// Each connection is handed to `onConnection` with helpers to answer the
// handshake and to read/write frames byte-exactly (including invalid ones).

import { join } from "node:path";
import { tmpdir } from "node:os";
import { createHash } from "node:crypto";
import { mkdtempSync, rmSync } from "node:fs";
import { createServer, type Server, type Socket } from "node:net";

export interface Frame {
  fin: boolean;
  op: number;
  masked: boolean;
  payload: Buffer;
}

export class Peer {
  readonly socket: Socket;
  buf: Buffer = Buffer.alloc(0);
  readonly #waiters: Array<() => void> = [];
  ended = false;
  method = "";
  path = "";
  headers: Record<string, string> = {};
  body: Buffer = Buffer.alloc(0);

  constructor(socket: Socket) {
    this.socket = socket;
    socket.on("data", (c: Buffer) => {
      this.buf = Buffer.concat([this.buf, c]);
      this.#wake();
    });
    socket.on("close", () => {
      this.ended = true;
      this.#wake();
    });
    socket.on("error", () => {});
  }

  #wake(): void {
    for (const w of this.#waiters.splice(0)) w();
  }

  async #need(pred: () => boolean): Promise<void> {
    while (!pred()) {
      if (this.ended) throw new Error("peer closed");
      await new Promise<void>((r) => this.#waiters.push(r));
    }
  }

  /** Reads the request head (and a Content-Length body). */
  async readRequest(): Promise<void> {
    await this.#need(() => this.buf.includes("\r\n\r\n"));
    const end = this.buf.indexOf("\r\n\r\n");
    const [line, ...lines] = this.buf.subarray(0, end).toString("latin1").split("\r\n");
    this.buf = this.buf.subarray(end + 4);
    [this.method = "", this.path = ""] = (line ?? "").split(" ");
    for (const l of lines) {
      const i = l.indexOf(":");
      this.headers[l.slice(0, i).trim().toLowerCase()] = l.slice(i + 1).trim();
    }
    const n = Number(this.headers["content-length"] ?? 0);
    await this.#need(() => this.buf.length >= n);
    this.body = this.buf.subarray(0, n);
    this.buf = this.buf.subarray(n);
  }

  accept(key = this.headers["sec-websocket-key"] ?? ""): string {
    return createHash("sha1").update(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").digest("base64");
  }

  /** Answers 101 (optionally with a wrong accept key or extra bytes appended). */
  upgrade(opts: { badAccept?: boolean; extra?: Buffer } = {}): void {
    const acc = opts.badAccept ? this.accept("wrong") : this.accept();
    const head = `HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${acc}\r\n\r\n`;
    this.socket.write(Buffer.concat([Buffer.from(head), opts.extra ?? Buffer.alloc(0)]));
  }

  respond(status: number, body: unknown, opts: { chunked?: boolean } = {}): void {
    const b = Buffer.from(JSON.stringify(body));
    if (opts.chunked) {
      const half = Math.floor(b.length / 2);
      const chunk = (x: Buffer) => Buffer.concat([Buffer.from(x.length.toString(16) + "\r\n"), x, Buffer.from("\r\n")]);
      this.socket.write(`HTTP/1.1 ${status} X\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n`);
      this.socket.write(Buffer.concat([chunk(b.subarray(0, half)), chunk(b.subarray(half)), Buffer.from("0\r\n\r\n")]));
    } else {
      this.socket.write(`HTTP/1.1 ${status} X\r\nContent-Type: application/json\r\nContent-Length: ${b.length}\r\n\r\n`);
      this.socket.write(b);
    }
  }

  async readFrame(): Promise<Frame> {
    await this.#need(() => this.buf.length >= 2);
    let len = this.buf[1]! & 0x7f;
    let off = 2;
    if (len === 126) {
      await this.#need(() => this.buf.length >= 4);
      len = this.buf.readUInt16BE(2);
      off = 4;
    } else if (len === 127) {
      await this.#need(() => this.buf.length >= 10);
      len = Number(this.buf.readBigUInt64BE(2));
      off = 10;
    }
    const masked = (this.buf[1]! & 0x80) !== 0;
    const total = off + (masked ? 4 : 0) + len;
    await this.#need(() => this.buf.length >= total);
    const fin = (this.buf[0]! & 0x80) !== 0;
    const op = this.buf[0]! & 0x0f;
    const payload = Buffer.from(this.buf.subarray(off + (masked ? 4 : 0), total));
    if (masked) {
      const key = this.buf.subarray(off, off + 4);
      for (let i = 0; i < payload.length; i++) payload[i]! ^= key[i & 3]!;
    }
    this.buf = this.buf.subarray(total);
    return { fin, op, masked, payload };
  }

  /** Reads frames until a text frame and parses it as JSON. */
  async readJSON(): Promise<Record<string, unknown>> {
    for (;;) {
      const f = await this.readFrame();
      if (f.op === 1) return JSON.parse(f.payload.toString("utf8")) as Record<string, unknown>;
      if (f.op === 8) throw new Error("client closed");
    }
  }

  frame(op: number, payload: Buffer | string, opts: { fin?: boolean; mask?: boolean; rsv?: boolean } = {}): Buffer {
    const p = typeof payload === "string" ? Buffer.from(payload) : payload;
    const n = p.length;
    const head = n <= 125 ? 2 : n <= 0xffff ? 4 : 10;
    const mask = opts.mask ? 4 : 0;
    const f = Buffer.alloc(head + mask + n);
    f[0] = (opts.fin === false ? 0 : 0x80) | (opts.rsv ? 0x40 : 0) | op;
    const m = opts.mask ? 0x80 : 0;
    if (n <= 125) f[1] = m | n;
    else if (n <= 0xffff) {
      f[1] = m | 126;
      f.writeUInt16BE(n, 2);
    } else {
      f[1] = m | 127;
      f.writeBigUInt64BE(BigInt(n), 2);
    }
    p.copy(f, head + mask);
    return f;
  }

  send(op: number, payload: Buffer | string, opts: { fin?: boolean; mask?: boolean; rsv?: boolean } = {}): void {
    this.socket.write(this.frame(op, payload, opts));
  }

  sendJSON(v: unknown): void {
    this.send(1, JSON.stringify(v));
  }
}

export interface TestServer {
  socketPath: string;
  peers: Peer[];
  stop(): Promise<void>;
}

/** Starts a server; `onConnection` runs per connection (errors are recorded, not thrown). */
export async function startServer(onConnection: (peer: Peer, index: number) => Promise<void>): Promise<TestServer> {
  const dir = mkdtempSync(join(tmpdir(), "swts-ws-"));
  const socketPath = `${dir}/s.sock`;
  const peers: Peer[] = [];
  const server: Server = createServer((socket) => {
    const peer = new Peer(socket);
    const i = peers.push(peer) - 1;
    onConnection(peer, i).catch(() => socket.destroy());
  });
  await new Promise<void>((resolve) => server.listen(socketPath, resolve));
  return {
    socketPath,
    peers,
    async stop() {
      for (const p of peers) p.socket.destroy();
      await new Promise((resolve) => server.close(resolve));
      rmSync(dir, { recursive: true, force: true });
    },
  };
}
