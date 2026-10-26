// Transports move one HTTP request/response. The client owns everything
// else (headers, retries, errors), so a custom transport stays tiny.

import type { Socket } from "node:net";

import { TransportError } from "./errors.ts";
import { guardSocket } from "./sockets.ts";

/** A request body: JSON text, bytes, or a byte stream (sent chunked, never buffered). */
export type TransportBody = string | Uint8Array | AsyncIterable<Uint8Array>;

/** One HTTP request as the client hands it to a transport. */
export interface TransportRequest {
  method: string;
  /** Absolute path including the `/api/v1` prefix and any query string. */
  path: string;
  /** Header names are as the client set them; values are final. */
  headers: Record<string, string>;
  /** Request body, if any (strings are UTF-8). */
  body?: TransportBody;
  /**
   * `"stream"`: return the response body unbuffered as `stream` (the
   * transport must end the stream with a TransportError if the connection
   * fails or closes before the response completed). Default `"text"`.
   */
  responseType?: "text" | "stream";
  signal?: AbortSignal;
}

/** One HTTP response. */
export interface TransportResponse {
  status: number;
  /** Header names lower-cased. */
  headers: Record<string, string>;
  /** The body as text (empty when `stream` is set). */
  body: string;
  /** The unbuffered body, for `responseType: "stream"`. */
  stream?: AsyncIterable<Uint8Array>;
}

/**
 * Sends requests to a davit server. Implementations must throw a
 * `TransportError` when no response was received, with `connectFailed: true`
 * only when the request certainly never reached the server (the client
 * retries only those). Aborts via `signal` should reject with the signal's
 * reason. Any other thrown value is wrapped as a non-retried TransportError.
 */
export interface Transport {
  request(req: TransportRequest): Promise<TransportResponse>;
  /**
   * Optional: opens a raw byte stream to the server for WebSocket upgrades
   * (process streams). Same error rules as `request`. Transports without it
   * cannot attach to processes.
   */
  connect?(signal?: AbortSignal): Promise<RawConnection>;
}

/** A connected stream socket plus the Host header value to use on it. */
export interface RawConnection {
  socket: Socket;
  host: string;
}

const isBun = typeof (globalThis as { Bun?: unknown }).Bun !== "undefined";

/** Response bodies larger than this are rejected (16 MiB). */
export const MAX_BODY_BYTES = 16 << 20;

const CONNECT_CODES = new Set(["ECONNREFUSED", "ENOENT", "ConnectionRefused", "FailedToOpenSocket"]);

function errorCode(err: unknown): string | undefined {
  for (let e: unknown = err, depth = 0; e && depth < 4; depth++) {
    const code = (e as { code?: unknown }).code;
    if (typeof code === "string") return code;
    e = (e as { cause?: unknown }).cause;
  }
  return undefined;
}

function describe(err: unknown): string {
  const code = errorCode(err);
  const msg = err instanceof Error ? err.message : String(err);
  return code && !msg.includes(code) ? `${msg} (${code})` : msg;
}

/** Waits until `socket` is connected; maps failures to TransportError. */
function dialed(socket: Socket, what: string, signal: AbortSignal | undefined, event: "connect" | "secureConnect"): Promise<Socket> {
  guardSocket(socket); // from creation: its owners come and go (sockets.ts)
  return new Promise((resolve, reject) => {
    const cleanup = () => {
      socket.off(event, onConnect);
      socket.off("error", onError);
      signal?.removeEventListener("abort", onAbort);
    };
    const onConnect = () => {
      cleanup();
      resolve(socket);
    };
    const onError = (err: Error) => {
      cleanup();
      socket.destroy();
      reject(new TransportError(`cannot reach ${what}: ${describe(err)}`, { connectFailed: CONNECT_CODES.has(errorCode(err) ?? ""), cause: err }));
    };
    const onAbort = () => {
      cleanup();
      socket.destroy();
      reject(signal!.reason);
    };
    if (signal?.aborted) return onAbort();
    signal?.addEventListener("abort", onAbort, { once: true });
    socket.once(event, onConnect);
    socket.once("error", onError);
  });
}

/** HTTP(S) transport over the global `fetch` (Node, Bun, Deno, browsers). */
export class FetchTransport implements Transport {
  readonly #origin: string;
  readonly #fetch: typeof fetch;

  /**
   * @param origin - scheme://host[:port] the paths are appended to.
   * @param fetchImpl - alternative `fetch` (defaults to `globalThis.fetch`).
   */
  constructor(origin: string, fetchImpl?: typeof fetch) {
    this.#origin = origin.replace(/\/+$/, "");
    const f = fetchImpl ?? globalThis.fetch;
    if (typeof f !== "function") throw new TypeError("no global fetch; pass a fetch implementation or a custom transport");
    this.#fetch = f;
  }

  async request(req: TransportRequest): Promise<TransportResponse> {
    let resp: Response;
    const body = req.body;
    const init: RequestInit & { duplex?: "half" } = { method: req.method, headers: req.headers, signal: req.signal, redirect: "manual" };
    if (typeof body === "string" || body instanceof Uint8Array) init.body = body as BodyInit;
    else if (body !== undefined) {
      init.body = toReadableStream(body);
      init.duplex = "half";
    }
    try {
      resp = await this.#fetch(this.#origin + req.path, init);
    } catch (err) {
      if (req.signal?.aborted) throw req.signal.reason;
      throw new TransportError(`cannot reach ${this.#origin}: ${describe(err)}`, {
        connectFailed: CONNECT_CODES.has(errorCode(err) ?? ""),
        cause: err,
      });
    }
    const headers: Record<string, string> = {};
    resp.headers.forEach((v, k) => {
      headers[k.toLowerCase()] = v;
    });
    if (req.responseType === "stream") {
      return { status: resp.status, headers, body: "", stream: readStream(resp.body, req.signal) };
    }
    let text: string;
    try {
      text = await resp.text();
    } catch (err) {
      if (req.signal?.aborted) throw req.signal.reason;
      throw new TransportError(`reading response: ${describe(err)}`, { cause: err });
    }
    if (text.length > MAX_BODY_BYTES) throw new TransportError("response body too large");
    return { status: resp.status, headers, body: text };
  }

  /** Opens TCP (http) or TLS (https) to the origin via node:net / node:tls. */
  async connect(signal?: AbortSignal): Promise<RawConnection> {
    const u = new URL(this.#origin);
    const host = u.hostname.replace(/^\[|\]$/g, "");
    if (u.protocol === "https:") {
      const tls = await import("node:tls");
      const port = Number(u.port || 443);
      const socket = tls.connect({ host, port, servername: /^[\d.]+$|:/.test(host) ? undefined : host, ALPNProtocols: ["http/1.1"] });
      return { socket: await dialed(socket, this.#origin, signal, "secureConnect"), host: u.host };
    }
    const net = await import("node:net");
    const socket = net.connect({ host, port: Number(u.port || 80) });
    return { socket: await dialed(socket, this.#origin, signal, "connect"), host: u.host };
  }
}

/**
 * HTTP over a local unix socket using `node:http` (Node and Bun). The module
 * is imported lazily so bundles that never use a socket stay portable.
 */
export class UnixSocketTransport implements Transport {
  /** Absolute path of the daemon socket. */
  readonly socketPath: string;

  constructor(socketPath: string) {
    this.socketPath = socketPath;
  }

  async request(req: TransportRequest): Promise<TransportResponse> {
    const body0 = req.body;
    if (isBun && body0 !== undefined && typeof body0 !== "string" && !(body0 instanceof Uint8Array)) {
      // Bun's node:http loses data on streamed request bodies (write() returns
      // false, "drain" never fires, bytes go missing): use Bun's own fetch,
      // which speaks HTTP over a unix socket with a `unix` option.
      const socketPath = this.socketPath;
      const viaUnix = ((url: string, init?: RequestInit) => fetch(url, { ...init, unix: socketPath } as RequestInit)) as typeof fetch;
      return new FetchTransport("http://localhost", viaUnix).request(req);
    }
    const http = await import("node:http");
    return nodeRequest(http.request, { socketPath: this.socketPath }, { Host: "localhost" }, `the daemon at ${this.socketPath}`, req);
  }

  /** Opens a raw connection to the socket via node:net. */
  async connect(signal?: AbortSignal): Promise<RawConnection> {
    const net = await import("node:net");
    const socket = net.createConnection({ path: this.socketPath });
    return { socket: await dialed(socket, `the daemon at ${this.socketPath}`, signal, "connect"), host: "localhost" };
  }
}

/** Options of {@link HttpsTransport}. */
export interface HttpsTransportOptions {
  /** Extra trusted CA certificates (PEM), added to Node's built-in roots. */
  ca?: string | readonly string[];
}

/**
 * HTTPS that also trusts extra CAs (an operator's private CA), for requests
 * and WebSocket upgrades alike. `fetch` cannot take a CA without a
 * dependency (undici), so Node requests go through node:https with
 * `ca: [...tls.rootCertificates, ...extra]`; Bun uses its fetch `tls.ca`
 * option. Modules are imported lazily.
 */
export class HttpsTransport implements Transport {
  readonly #origin: string;
  readonly #url: URL;
  readonly #extra: readonly string[];
  #ca: string[] | undefined;
  #agent: import("node:https").Agent | undefined;

  /** @param origin - `https://host[:port]` the paths are appended to. */
  constructor(origin: string, options: HttpsTransportOptions = {}) {
    this.#url = new URL(origin);
    if (this.#url.protocol !== "https:") throw new TypeError(`HttpsTransport needs an https:// origin, got ${origin}`);
    this.#origin = this.#url.origin;
    const ca = options.ca;
    this.#extra = ca === undefined ? [] : typeof ca === "string" ? [ca] : [...ca];
  }

  async #trusted(): Promise<string[]> {
    if (!this.#ca) {
      const tls = await import("node:tls");
      this.#ca = [...tls.rootCertificates, ...this.#extra];
    }
    return this.#ca;
  }

  #host(): string {
    return this.#url.hostname.replace(/^\[|\]$/g, "");
  }

  #servername(): string | undefined {
    const host = this.#host();
    return /^[\d.]+$|:/.test(host) ? undefined : host;
  }

  async request(req: TransportRequest): Promise<TransportResponse> {
    const ca = await this.#trusted();
    if (isBun) {
      const viaTls = ((url: string, init?: RequestInit) => fetch(url, { ...init, tls: { ca } } as RequestInit)) as typeof fetch;
      return new FetchTransport(this.#origin, viaTls).request(req);
    }
    const https = await import("node:https");
    // Idle pooled sockets are dropped after 4 s (like undici's default), well
    // before a server's idle timeout could reset one under a new request.
    this.#agent ??= new https.Agent({ keepAlive: true, timeout: 4_000, ca });
    const target = {
      host: this.#host(),
      port: Number(this.#url.port || 443),
      servername: this.#servername(),
      agent: this.#agent,
    };
    return nodeRequest(https.request, target, {}, this.#origin, req);
  }

  /** Opens TLS to the origin via node:tls, with the same trusted CAs. */
  async connect(signal?: AbortSignal): Promise<RawConnection> {
    const ca = await this.#trusted();
    const tls = await import("node:tls");
    const socket = tls.connect({
      host: this.#host(),
      port: Number(this.#url.port || 443),
      servername: this.#servername(),
      ALPNProtocols: ["http/1.1"],
      ca,
    });
    return { socket: await dialed(socket, this.#origin, signal, "secureConnect"), host: this.#url.host };
  }
}

type NodeRequestFn = (
  options: import("node:https").RequestOptions,
  callback: (res: import("node:http").IncomingMessage) => void,
) => import("node:http").ClientRequest;

/**
 * One request through node:http or node:https (`target` adds the socket path
 * or host/port/TLS options). Shared by the socket and the private-CA HTTPS
 * transports so both have identical streaming, abort and error semantics.
 */
function nodeRequest(
  send: NodeRequestFn,
  target: import("node:https").RequestOptions,
  extraHeaders: Record<string, string>,
  what: string,
  req: TransportRequest,
): Promise<TransportResponse> {
  const signal = req.signal;
  if (signal?.aborted) return Promise.reject(signal.reason);
  return new Promise<TransportResponse>((resolve, reject) => {
    let settled = false;
    let responded = false;
    const settle = () => {
      settled = true;
      signal?.removeEventListener("abort", onAbort);
    };
    const fail = (err: unknown) => {
      if (settled) return;
      settle();
      // These codes only come from connect(), before anything was sent.
      reject(
        new TransportError(`cannot reach ${what}: ${describe(err)}`, {
          connectFailed: CONNECT_CODES.has(errorCode(err) ?? ""),
          cause: err,
        }),
      );
    };
    // Aborts are handled here rather than via the `signal` request option:
    // Bun's node:http drops an aborted request without emitting an error.
    const onAbort = () => {
      if (settled) return;
      settle();
      r.destroy();
      reject(signal!.reason);
    };
    const headers: Record<string, string> = { ...req.headers, ...extraHeaders };
    const body = req.body;
    const fixed = typeof body === "string" ? Buffer.from(body, "utf8") : body instanceof Uint8Array ? body : undefined;
    if (fixed !== undefined) headers["Content-Length"] = String(fixed.byteLength);
    const r = send({ ...target, method: req.method, path: req.path, headers }, (res) => {
      responded = true;
      const out: Record<string, string> = {};
      for (const [k, v] of Object.entries(res.headers)) {
        if (v !== undefined) out[k.toLowerCase()] = Array.isArray(v) ? v.join(", ") : v;
      }
      if (req.responseType === "stream") {
        if (settled) return res.destroy();
        settle();
        // Errors surface through the iterator; never as an unhandled event.
        res.on("error", () => {});
        resolve({ status: res.statusCode ?? 0, headers: out, body: "", stream: readIncoming(res, signal) });
        return;
      }
      const chunks: Buffer[] = [];
      let size = 0;
      res.on("data", (c: Buffer) => {
        size += c.length;
        if (size > MAX_BODY_BYTES) {
          res.destroy(new Error("response body too large"));
          return;
        }
        chunks.push(c);
      });
      res.on("error", fail);
      res.on("close", () => fail(new Error("connection closed before the response completed")));
      res.on("end", () => {
        if (settled) return;
        settle();
        resolve({ status: res.statusCode ?? 0, headers: out, body: Buffer.concat(chunks).toString("utf8") });
      });
    });
    signal?.addEventListener("abort", onAbort, { once: true });
    // Once the server answered (e.g. an early error), request-side write
    // errors are irrelevant: the response decides.
    r.on("error", (err) => {
      if (!responded) fail(err);
    });
    if (fixed !== undefined || body === undefined) {
      r.end(fixed);
      return;
    }
    void (async () => {
      try {
        for await (const chunk of body) {
          if (responded || settled) break;
          if (!r.write(chunk)) {
            await new Promise<void>((done) => {
              const go = () => {
                r.off("drain", go);
                r.off("close", go);
                done();
              };
              r.on("drain", go);
              r.on("close", go);
            });
          }
        }
        if (!responded && !settled) r.end();
      } catch (err) {
        // The caller's body stream failed: abandon the request.
        if (settled) return;
        settle();
        r.destroy();
        reject(err);
      }
    })();
  });
}

/** Wraps an async iterable as a ReadableStream (for fetch request bodies). */
function toReadableStream(body: AsyncIterable<Uint8Array>): ReadableStream<Uint8Array> {
  const it = body[Symbol.asyncIterator]();
  return new ReadableStream<Uint8Array>({
    async pull(controller) {
      const { value, done } = await it.next();
      if (done) controller.close();
      else controller.enqueue(value);
    },
    async cancel() {
      await it.return?.();
    },
  });
}

/** Iterates a fetch response body; failures become TransportErrors. */
async function* readStream(body: ReadableStream<Uint8Array> | null, signal?: AbortSignal): AsyncGenerator<Uint8Array, void, undefined> {
  if (!body) return;
  const reader = body.getReader();
  let finished = false;
  try {
    for (;;) {
      let r: ReadableStreamReadResult<Uint8Array>;
      try {
        r = await reader.read();
      } catch (err) {
        if (signal?.aborted) throw signal.reason;
        throw new TransportError(`reading response: ${describe(err)}`, { cause: err });
      }
      if (r.done) {
        finished = true;
        return;
      }
      yield r.value;
    }
  } finally {
    if (!finished) await reader.cancel().catch(() => {});
  }
}

/** Iterates a node:http response; failures and early closes become TransportErrors. */
async function* readIncoming(
  res: import("node:http").IncomingMessage,
  signal?: AbortSignal,
): AsyncGenerator<Uint8Array, void, undefined> {
  const onAbort = () => res.destroy();
  signal?.addEventListener("abort", onAbort, { once: true });
  let finished = false;
  try {
    try {
      for await (const c of res as AsyncIterable<Buffer>) yield new Uint8Array(c.buffer, c.byteOffset, c.byteLength);
    } catch (err) {
      if (signal?.aborted) throw signal.reason;
      if (err instanceof TransportError) throw err;
      throw new TransportError(`reading response: ${describe(err)}`, { cause: err });
    }
    if (signal?.aborted) throw signal.reason;
    if (!res.complete) throw new TransportError("connection closed before the response completed");
    finished = true;
  } finally {
    signal?.removeEventListener("abort", onAbort);
    if (!finished) res.destroy();
  }
}
