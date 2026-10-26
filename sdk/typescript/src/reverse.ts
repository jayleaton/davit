// Reverse forward sessions (attachReverseSession + attachReverseConnection):
// guest loopback connections bridged to a service this client reaches.

import type { Socket } from "node:net";

import { TransportError } from "./errors.ts";
import { socketFailure } from "./sockets.ts";
import type { ReverseForward } from "./schema.generated.ts";
import { TUNNEL_CHUNK_BYTES } from "./tunnel.ts";
import type { WebSocketConnection } from "./websocket.ts";

/** `ReverseSession.reason` after the client closed the session itself. */
export const REVERSE_CLOSED_REASON = "closed";

/** Opens attachReverseConnection for one announced connection ID. */
export type ReverseConnectionDialer = (connectionId: string) => Promise<WebSocketConnection>;
/** Opens the TCP connection to the local service. */
export type LocalDialer = () => Promise<Socket>;

const message = (err: unknown) => (err instanceof Error ? err.message : String(err));

/** One guest connection: an attachReverseConnection WebSocket spliced to a local socket. */
class Bridge {
  readonly #ws: WebSocketConnection;
  readonly #sock: Socket;
  #closed = false;
  #localEof = false;
  #remoteEof = false;

  constructor(ws: WebSocketConnection, sock: Socket) {
    this.#ws = ws;
    this.#sock = sock;
  }

  /** Splices both directions; resolves once the connection ended. */
  async run(): Promise<void> {
    const sock = this.#sock;
    sock.on("data", (chunk: Buffer) => {
      try {
        let full = false;
        for (let off = 0; off < chunk.length; off += TUNNEL_CHUNK_BYTES) {
          if (!this.#ws.sendBinary(chunk.subarray(off, off + TUNNEL_CHUNK_BYTES))) full = true;
        }
        if (full) {
          sock.pause();
          void this.#ws.drain().then(() => sock.resume());
        }
      } catch {
        this.close();
      }
    });
    sock.on("end", () => {
      // The local service half-closed: an empty binary message tells the guest.
      this.#localEof = true;
      try {
        if (!this.#closed) this.#ws.sendBinary(new Uint8Array(0));
      } catch {
        this.close();
        return;
      }
      if (this.#remoteEof) this.close();
    });
    sock.on("error", () => this.close());
    sock.on("close", () => this.close());
    if (socketFailure(sock)) this.close(); // it failed before these listeners
    try {
      for (;;) {
        const m = await this.#ws.next();
        if (m === null || this.#closed) break; // a close frame ends the connection
        if (m.type !== "binary") continue;
        if (m.data.length > 0) {
          if (!sock.write(m.data)) await new Promise<void>((resolve) => {
            const done = () => {
              sock.off("drain", done);
              sock.off("close", done);
              resolve();
            };
            sock.on("drain", done);
            sock.on("close", done);
          });
          continue;
        }
        this.#remoteEof = true; // the guest half-closed
        sock.end();
        if (this.#localEof) break;
      }
    } catch {
      // a failed stream ends the connection
    } finally {
      this.close();
    }
  }

  /** Ends both directions. Idempotent. */
  close(): void {
    if (this.#closed) return;
    this.#closed = true;
    this.#ws.close(1000, "");
    this.#sock.destroy();
  }
}

/**
 * A session reverse forward held open by this client (as `davit forward
 * --reverse`). Every connection a guest process makes to
 * `127.0.0.1:<guestPort>` in the VM is announced on the session stream; the
 * session claims it (attachReverseConnection), dials the local service and
 * splices the two with half-close in both directions. A local service that
 * cannot be reached closes that one connection.
 *
 * `close()` closes the session stream, which revokes the forward, and cuts
 * every live connection. `closed` resolves (it never rejects) with `reason`
 * once the session ended.
 */
export class ReverseSession {
  /** The forward as created. */
  readonly forward: ReverseForward;
  /**
   * Resolves with `reason` once the session ended: the server's `revoked`
   * reason, `REVERSE_CLOSED_REASON` after `close()`, or a description of a
   * dropped stream (then `error` is set too). Never rejects.
   */
  readonly closed: Promise<string>;
  /** Why the session ended; undefined while it runs. */
  reason: string | undefined;
  /** Set when the session stream ended without a `revoked` message. */
  error: TransportError | undefined;
  readonly #ws: WebSocketConnection;
  readonly #dialConnection: ReverseConnectionDialer;
  readonly #dialLocal: LocalDialer;
  readonly #bridges = new Set<Bridge>();
  #closing = false;
  #resolve!: (reason: string) => void;

  /** @internal Use `DavitClient.reverseForward`. */
  constructor(forward: ReverseForward, ws: WebSocketConnection, dialConnection: ReverseConnectionDialer, dialLocal: LocalDialer) {
    this.forward = forward;
    this.#ws = ws;
    this.#dialConnection = dialConnection;
    this.#dialLocal = dialLocal;
    this.closed = new Promise((resolve) => (this.#resolve = resolve));
    void this.#read();
  }

  /** Guest connections this client is bridging right now. */
  get active(): number {
    return this.#bridges.size;
  }

  /** Closes the session stream (revoking the forward) and every live connection. Idempotent. */
  close(): void {
    if (this.#closing) return;
    this.#closing = true;
    this.#ws.close(1000, "");
    this.#finish(REVERSE_CLOSED_REASON);
  }

  async #read(): Promise<void> {
    let revoked: string | undefined;
    const decoder = new TextDecoder();
    try {
      for (;;) {
        const m = await this.#ws.next();
        if (m === null) break;
        if (m.type !== "text") continue;
        let ev: unknown;
        try {
          ev = JSON.parse(decoder.decode(m.data));
        } catch {
          continue; // not a session event: ignored, as unknown types are
        }
        if (typeof ev !== "object" || ev === null) continue;
        const { type, connectionId, reason } = ev as Record<string, unknown>;
        if (type === "connection" && typeof connectionId === "string" && connectionId) void this.#serve(connectionId);
        else if (type === "revoked") revoked = typeof reason === "string" && reason ? reason : "revoked";
      }
    } catch (err) {
      if (revoked === undefined && !this.#closing) {
        this.#dropped(`reverse session stream failed: ${message(err)}`, err);
        return;
      }
    }
    if (revoked !== undefined || this.#closing) return this.#finish(revoked ?? REVERSE_CLOSED_REASON);
    const code = this.#ws.closeCode;
    if (code === 1000 || code === 1005) return this.#finish("session closed by the server");
    this.#dropped(`reverse session stream ended (${code ?? 1006})${this.#ws.closeReason ? ": " + this.#ws.closeReason : ""}`);
  }

  #dropped(reason: string, cause?: unknown): void {
    const err = new TransportError(`reverse forward ${this.forward.id}: ${reason}`, { cause });
    this.#finish(reason, err);
  }

  #finish(reason: string, error?: TransportError): void {
    if (this.reason !== undefined) return;
    this.reason = reason;
    this.error = error;
    this.#closing = true;
    this.#ws.close(1000, "");
    for (const b of this.#bridges) b.close(); // the revoke cut them on the server side too
    this.#bridges.clear();
    this.#resolve(reason);
  }

  async #serve(connectionId: string): Promise<void> {
    let ws: WebSocketConnection;
    try {
      ws = await this.#dialConnection(connectionId);
    } catch {
      return; // unclaimed: the server drops the guest connection
    }
    let sock: Socket;
    try {
      sock = await this.#dialLocal();
    } catch {
      ws.close(1000, ""); // the local service is unreachable: end this connection
      return;
    }
    const bridge = new Bridge(ws, sock);
    if (this.reason !== undefined) return bridge.close();
    this.#bridges.add(bridge);
    try {
      await bridge.run();
    } finally {
      this.#bridges.delete(bridge);
    }
  }
}
