// A small, bounded RFC 6455 WebSocket client over an already-connected
// Node/Bun stream socket (unix, TCP or TLS). It negotiates no extensions or
// subprotocols, masks every client frame, answers pings, reassembles
// fragments, validates UTF-8 text, and bounds handshakes and messages.

import type { Socket } from "node:net";

import { DavitError, TransportError, errorFor } from "./errors.ts";
import { guardSocket, socketFailure } from "./sockets.ts";

/** Default maximum assembled incoming message size (1 MiB, the API's bound). */
export const DEFAULT_MAX_MESSAGE_BYTES = 1 << 20;
const MAX_HANDSHAKE_BYTES = 64 << 10;
const MAX_ERROR_BODY_BYTES = 1 << 20;
const MAX_CONTROL_PAYLOAD = 125;
const ACCEPT_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11";

const OP_CONT = 0;
const OP_TEXT = 1;
const OP_BINARY = 2;
const OP_CLOSE = 8;
const OP_PING = 9;
const OP_PONG = 10;

/** One received data message. */
export interface WebSocketMessage {
  type: "text" | "binary";
  /** Raw payload; for text, valid UTF-8. */
  data: Uint8Array;
}

/** Options of `WebSocketConnection.connect`. */
export interface WebSocketConnectOptions {
  /** Value of the Host header. */
  host: string;
  /** Request target, starting with `/` (may include a query). */
  path: string;
  /** Extra request headers. */
  headers?: Record<string, string>;
  /** Largest accepted incoming message (default 1 MiB). */
  maxMessageBytes?: number;
  signal?: AbortSignal;
}

type Waiter = { resolve: (m: WebSocketMessage | null) => void; reject: (e: unknown) => void };

/** Thrown for peer protocol violations (the connection is failed with 1002/1007/1009). */
function protocolError(msg: string, cause?: unknown): TransportError {
  return new TransportError(`websocket: ${msg}`, { cause });
}

/** Reports whether a comma-separated header contains token (case-insensitive). */
function hasToken(value: string | undefined, token: string): boolean {
  return (value ?? "").split(",").some((p) => p.trim().toLowerCase() === token);
}

function validCloseCode(code: number): boolean {
  return (code >= 1000 && code <= 1003) || (code >= 1007 && code <= 1014) || (code >= 3000 && code <= 4999);
}

/** A connected WebSocket. Read with `next()` or `for await`; write with `sendText`/`sendBinary`. */
export class WebSocketConnection implements AsyncIterable<WebSocketMessage> {
  readonly #socket: Socket;
  readonly #maxMessage: number;
  #buf: Buffer = Buffer.alloc(0);
  #fragments: Buffer[] = [];
  #fragmentType = 0;
  #fragmentBytes = 0;
  readonly #queue: WebSocketMessage[] = [];
  readonly #waiters: Waiter[] = [];
  #error: unknown;
  #ended = false;
  #closeSent = false;
  readonly #decoder = new TextDecoder("utf-8", { fatal: true });
  /** The peer's close code (1005 if none was given, 1006 if the transport just ended). */
  closeCode: number | undefined;
  /** The peer's close reason. */
  closeReason = "";

  private constructor(socket: Socket, rest: Buffer, maxMessageBytes: number) {
    this.#socket = socket;
    this.#maxMessage = maxMessageBytes;
    socket.on("data", (chunk: Buffer) => this.#onData(chunk));
    socket.on("error", (err) => this.#fail(new TransportError(`websocket: ${err.message}`, { cause: err })));
    // Bun reports a peer hang-up as "end" without a following "close".
    const ended = () => {
      if (this.closeCode === undefined) this.closeCode = 1006;
      this.#finish();
      socket.destroy();
    };
    socket.on("end", ended);
    socket.on("close", ended);
    if (rest.length > 0) this.#onData(rest);
    // A failure between the handshake and these listeners is already past.
    const past = socketFailure(socket);
    if (past?.error) this.#fail(new TransportError(`websocket: ${past.error.message}`, { cause: past.error }));
    else if (past) ended();
    socket.resume();
  }

  /**
   * Performs the client handshake on `socket`. On a non-101 answer, throws
   * the API error decoded from the body (typed by code, with the HTTP status)
   * or a TransportError. The socket is destroyed on any failure.
   */
  static async connect(socket: Socket, opts: WebSocketConnectOptions): Promise<WebSocketConnection> {
    guardSocket(socket); // before any await: the socket may be a caller's, unguarded
    const { createHash, randomBytes } = await import("node:crypto");
    if (!opts.path.startsWith("/")) throw new TypeError(`websocket path must start with /: ${opts.path}`);
    const key = randomBytes(16).toString("base64");
    const lines = [
      `GET ${opts.path} HTTP/1.1`,
      `Host: ${opts.host}`,
      "Upgrade: websocket",
      "Connection: Upgrade",
      `Sec-WebSocket-Key: ${key}`,
      "Sec-WebSocket-Version: 13",
    ];
    for (const [k, v] of Object.entries(opts.headers ?? {})) {
      if (/[\r\n]/.test(k + v)) throw new TypeError(`invalid header ${k}`);
      lines.push(`${k}: ${v}`);
    }
    try {
      const { status, headers, rest } = await readHandshake(socket, lines.join("\r\n") + "\r\n\r\n", opts.signal);
      if (status !== 101) throw await handshakeError(socket, status, headers, rest);
      const want = createHash("sha1").update(key + ACCEPT_GUID).digest("base64");
      if (!hasToken(headers.upgrade, "websocket")) throw protocolError("bad handshake: missing Upgrade: websocket");
      if (!hasToken(headers.connection, "upgrade")) throw protocolError("bad handshake: missing Connection: upgrade");
      if (headers["sec-websocket-accept"] !== want) throw protocolError("bad handshake: bad Sec-WebSocket-Accept");
      if (headers["sec-websocket-extensions"]) throw protocolError("bad handshake: unrequested extension");
      return new WebSocketConnection(socket, rest, opts.maxMessageBytes ?? DEFAULT_MAX_MESSAGE_BYTES);
    } catch (err) {
      socket.destroy();
      throw err;
    }
  }

  /** Resolves the next data message, or null once the connection closed cleanly. */
  next(): Promise<WebSocketMessage | null> {
    const m = this.#queue.shift();
    if (m) return Promise.resolve(m);
    if (this.#error !== undefined) return Promise.reject(this.#error);
    if (this.#ended) return Promise.resolve(null);
    return new Promise((resolve, reject) => this.#waiters.push({ resolve, reject }));
  }

  async *[Symbol.asyncIterator](): AsyncGenerator<WebSocketMessage, void, undefined> {
    for (;;) {
      const m = await this.next();
      if (m === null) return;
      yield m;
    }
  }

  /** Sends one text message as a single masked frame. */
  sendText(text: string): void {
    if (this.#closeSent || this.#ended) throw new TransportError("websocket: connection closed");
    this.#write(OP_TEXT, Buffer.from(text, "utf8"));
  }

  /**
   * Sends one binary message as a single masked frame (an empty one is
   * allowed). Returns false when the socket buffer is full; await `drain()`.
   */
  sendBinary(data: Uint8Array): boolean {
    if (this.#closeSent || this.#ended) throw new TransportError("websocket: connection closed");
    return this.#write(OP_BINARY, Buffer.from(data.buffer, data.byteOffset, data.byteLength));
  }

  /** Resolves once buffered outgoing bytes were flushed (or the socket closed). */
  drain(): Promise<void> {
    const s = this.#socket;
    if (!s.writableNeedDrain || s.destroyed) return Promise.resolve();
    return new Promise((resolve) => {
      const done = () => {
        s.off("drain", done);
        s.off("close", done);
        resolve();
      };
      s.on("drain", done);
      s.on("close", done);
    });
  }

  /** Sends a close frame (once) and ends the connection. Idempotent. */
  close(code = 1000, reason = ""): void {
    if (!this.#ended && !this.#closeSent) {
      let r = Buffer.from(reason, "utf8");
      if (r.length > MAX_CONTROL_PAYLOAD - 2) r = r.subarray(0, MAX_CONTROL_PAYLOAD - 2);
      const p = Buffer.alloc(2 + r.length);
      p.writeUInt16BE(code, 0);
      r.copy(p, 2);
      this.#sendClose(p);
    }
    this.#socket.end();
    // Do not wait forever for the peer's FIN.
    setTimeout(() => this.#socket.destroy(), 1000).unref();
    this.#finish();
  }

  #sendClose(payload: Buffer): void {
    if (this.#closeSent) return;
    this.#closeSent = true;
    try {
      this.#write(OP_CLOSE, payload);
    } catch {
      // best effort
    }
  }

  #write(op: number, payload: Buffer): boolean {
    const n = payload.length;
    const head = n <= 125 ? 2 : n <= 0xffff ? 4 : 10;
    const frame = Buffer.alloc(head + 4 + n);
    frame[0] = 0x80 | op;
    if (n <= 125) frame[1] = 0x80 | n;
    else if (n <= 0xffff) {
      frame[1] = 0x80 | 126;
      frame.writeUInt16BE(n, 2);
    } else {
      frame[1] = 0x80 | 127;
      frame.writeBigUInt64BE(BigInt(n), 2);
    }
    const mask = new Uint8Array(4);
    globalThis.crypto.getRandomValues(mask);
    frame.set(mask, head);
    for (let i = 0; i < n; i++) frame[head + 4 + i] = payload[i]! ^ mask[i & 3]!;
    return this.#socket.write(frame);
  }

  #onData(chunk: Buffer): void {
    if (this.#ended || this.#error !== undefined) return;
    this.#buf = this.#buf.length === 0 ? chunk : Buffer.concat([this.#buf, chunk]);
    try {
      while (this.#parseFrame());
    } catch (err) {
      const code = (err as { wsCode?: number }).wsCode ?? 1002;
      this.#sendClose(closePayload(code));
      this.#socket.end();
      setTimeout(() => this.#socket.destroy(), 1000).unref();
      this.#fail(err);
    }
  }

  /** Parses one frame from the buffer; returns false when more bytes are needed. */
  #parseFrame(): boolean {
    const b = this.#buf;
    if (b.length < 2) return false;
    const fin = (b[0]! & 0x80) !== 0;
    if ((b[0]! & 0x70) !== 0) throw protocolError("reserved bits set");
    const op = b[0]! & 0x0f;
    if (![OP_CONT, OP_TEXT, OP_BINARY, OP_CLOSE, OP_PING, OP_PONG].includes(op)) throw protocolError(`unknown opcode ${op}`);
    if ((b[1]! & 0x80) !== 0) throw protocolError("masked server frame");
    let len = b[1]! & 0x7f;
    let off = 2;
    if (len === 126) {
      if (b.length < 4) return false;
      len = b.readUInt16BE(2);
      off = 4;
    } else if (len === 127) {
      if (b.length < 10) return false;
      const big = b.readBigUInt64BE(2);
      if (big >> 63n) throw protocolError("payload length has MSB set");
      if (big > BigInt(this.#maxMessage)) throw tooBig();
      len = Number(big);
      off = 10;
    }
    if (op >= OP_CLOSE && (!fin || len > MAX_CONTROL_PAYLOAD)) throw protocolError("fragmented or oversize control frame");
    if (op < OP_CLOSE && this.#fragmentBytes + len > this.#maxMessage) throw tooBig();
    if (b.length < off + len) return false;
    const payload = b.subarray(off, off + len);
    this.#buf = b.subarray(off + len);

    switch (op) {
      case OP_PING:
        if (!this.#closeSent) this.#write(OP_PONG, Buffer.from(payload));
        return true;
      case OP_PONG:
        return true;
      case OP_CLOSE:
        this.#onClose(payload);
        return false;
    }
    if (op === OP_CONT) {
      if (this.#fragmentType === 0) throw protocolError("continuation without a message");
    } else {
      if (this.#fragmentType !== 0) throw protocolError("new message inside a fragmented message");
      this.#fragmentType = op;
    }
    this.#fragments.push(Buffer.from(payload));
    this.#fragmentBytes += len;
    if (fin) {
      const data = this.#fragments.length === 1 ? this.#fragments[0]! : Buffer.concat(this.#fragments);
      const type = this.#fragmentType === OP_TEXT ? "text" : "binary";
      this.#fragments = [];
      this.#fragmentType = 0;
      this.#fragmentBytes = 0;
      if (type === "text") {
        try {
          this.#decoder.decode(data);
        } catch (err) {
          throw Object.assign(protocolError("invalid UTF-8 in text message", err), { wsCode: 1007 });
        }
      }
      this.#deliver({ type, data: new Uint8Array(data.buffer, data.byteOffset, data.length) });
    }
    return true;
  }

  #onClose(p: Buffer): void {
    if (p.length === 1) throw protocolError("close payload of 1 byte");
    let code = 1005;
    if (p.length >= 2) {
      code = p.readUInt16BE(0);
      if (!validCloseCode(code)) throw protocolError(`invalid close code ${code}`);
      this.closeReason = p.subarray(2).toString("utf8");
    }
    this.closeCode = code;
    this.#sendClose(code === 1005 ? Buffer.alloc(0) : closePayload(code));
    this.#socket.end();
    setTimeout(() => this.#socket.destroy(), 1000).unref();
    this.#finish();
  }

  #deliver(m: WebSocketMessage): void {
    const w = this.#waiters.shift();
    if (w) w.resolve(m);
    else this.#queue.push(m);
  }

  #fail(err: unknown): void {
    if (this.#error !== undefined || this.#ended) return;
    this.#error = err;
    for (const w of this.#waiters.splice(0)) w.reject(err);
  }

  #finish(): void {
    if (this.#ended) return;
    this.#ended = true;
    if (this.#error === undefined) for (const w of this.#waiters.splice(0)) w.resolve(null);
  }
}

function tooBig(): TransportError {
  return Object.assign(protocolError("message exceeds the size limit"), { wsCode: 1009 });
}

function closePayload(code: number): Buffer {
  const p = Buffer.alloc(2);
  p.writeUInt16BE(code, 0);
  return p;
}

/** Writes the request and reads the response head; returns any bytes past it. */
function readHandshake(
  socket: Socket,
  request: string,
  signal?: AbortSignal,
): Promise<{ status: number; headers: Record<string, string>; rest: Buffer }> {
  return new Promise((resolve, reject) => {
    let buf = Buffer.alloc(0);
    const cleanup = () => {
      socket.off("data", onData);
      socket.off("error", onError);
      socket.off("close", onClose);
      socket.off("end", onClose);
      signal?.removeEventListener("abort", onAbort);
    };
    const fail = (err: unknown) => {
      cleanup();
      reject(err);
    };
    const onError = (err: Error) => fail(new TransportError(`websocket handshake: ${err.message}`, { cause: err }));
    const onClose = () => fail(new TransportError("websocket handshake: connection closed"));
    const onAbort = () => fail(signal!.reason);
    const onData = (chunk: Buffer) => {
      buf = Buffer.concat([buf, chunk]);
      const end = buf.indexOf("\r\n\r\n");
      if (end < 0) {
        if (buf.length > MAX_HANDSHAKE_BYTES) fail(protocolError("handshake response too large"));
        return;
      }
      cleanup();
      socket.pause();
      const [statusLine, ...lines] = buf.subarray(0, end).toString("latin1").split("\r\n");
      const m = /^HTTP\/1\.[01] (\d{3})/.exec(statusLine ?? "");
      if (!m) return reject(protocolError(`bad handshake status line ${JSON.stringify(statusLine)}`));
      const headers: Record<string, string> = {};
      for (const l of lines) {
        const i = l.indexOf(":");
        if (i <= 0) continue;
        const k = l.slice(0, i).trim().toLowerCase();
        const v = l.slice(i + 1).trim();
        headers[k] = headers[k] ? `${headers[k]}, ${v}` : v;
      }
      // Left paused: the next owner resumes once its listener is attached.
      resolve({ status: Number(m[1]), headers, rest: buf.subarray(end + 4) });
    };
    if (signal?.aborted) return reject(signal.reason);
    signal?.addEventListener("abort", onAbort, { once: true });
    socket.on("data", onData);
    socket.on("error", onError);
    socket.on("close", onClose);
    socket.on("end", onClose);
    const past = socketFailure(socket);
    if (past?.error) return onError(past.error);
    if (past) return onClose();
    socket.write(request);
  });
}

/** Reads a bounded non-101 body (Content-Length, chunked or until EOF) and builds the error. */
async function handshakeError(socket: Socket, status: number, headers: Record<string, string>, rest: Buffer): Promise<DavitError> {
  const chunked = hasToken(headers["transfer-encoding"], "chunked");
  const length = headers["content-length"] !== undefined ? Number(headers["content-length"]) : undefined;
  let body = rest;
  const complete = (): Buffer | undefined => {
    if (length !== undefined) return body.length >= length ? body.subarray(0, length) : undefined;
    if (chunked) return dechunk(body);
    return undefined;
  };
  let done = complete();
  if (done === undefined && socketFailure(socket)) done = body; // nothing more will come
  if (done === undefined) {
    done = await new Promise<Buffer>((resolve) => {
      const timer = setTimeout(() => finish(body), 5000);
      const finish = (b: Buffer) => {
        clearTimeout(timer);
        socket.off("data", onData);
        socket.off("close", onEnd);
        socket.off("end", onEnd);
        socket.off("error", onEnd);
        resolve(b);
      };
      const onData = (c: Buffer) => {
        body = Buffer.concat([body, c]);
        const d = complete();
        if (d !== undefined) finish(d);
        else if (body.length > MAX_ERROR_BODY_BYTES) finish(body.subarray(0, MAX_ERROR_BODY_BYTES));
      };
      const onEnd = () => finish(body);
      socket.on("data", onData);
      socket.on("close", onEnd);
      socket.on("end", onEnd);
      socket.on("error", onEnd);
      socket.resume();
    });
  }
  const requestId = headers["x-request-id"];
  try {
    const e = (JSON.parse(done.toString("utf8")) as { error?: { code?: string; message?: string; retryable?: boolean; details?: Record<string, unknown>; requestId?: string; operationId?: string } }).error;
    if (e && typeof e.code === "string" && e.code) {
      return errorFor({
        status,
        code: e.code,
        message: e.message ?? "",
        retryable: e.retryable ?? false,
        details: e.details,
        requestId: e.requestId || requestId,
        operationId: e.operationId,
      });
    }
  } catch {
    // fall through
  }
  return new DavitError({ status, code: "internal", message: `websocket handshake: unexpected HTTP ${status}`, requestId });
}

/** Decodes a complete chunked body, or returns undefined if incomplete. */
function dechunk(b: Buffer): Buffer | undefined {
  const parts: Buffer[] = [];
  let off = 0;
  for (;;) {
    const eol = b.indexOf("\r\n", off);
    if (eol < 0) return undefined;
    const size = parseInt(b.subarray(off, eol).toString("latin1"), 16);
    if (Number.isNaN(size)) return Buffer.concat(parts);
    if (size === 0) return Buffer.concat(parts);
    if (b.length < eol + 2 + size + 2) return undefined;
    parts.push(b.subarray(eol + 2, eol + 2 + size));
    off = eol + 2 + size + 2;
  }
}
