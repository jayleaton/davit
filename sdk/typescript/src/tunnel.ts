// Tunnel streams (attachTunnel): a guest loopback TCP connection as bytes
// over a WebSocket.

import { TransportError } from "./errors.ts";
import type { Tunnel } from "./schema.generated.ts";
import type { WebSocketConnection } from "./websocket.ts";

/** Largest binary message the client sends (as the Go CLI and the server). */
export const TUNNEL_CHUNK_BYTES = 32 << 10;

/**
 * An attached tunnel: raw TCP bytes both ways. `read()` resolves the next
 * chunk from the guest port, or `null` once the guest side ended (the server
 * closed the WebSocket normally); any other end throws `TransportError`.
 * `write` sends bytes in messages of at most 32 KiB and waits for the socket
 * buffer to drain; `closeWrite` sends EOF to the guest port (reads keep
 * working); `close` ends both directions. Iterate it (`for await`) for the
 * chunks until EOF.
 */
export class TunnelStream implements AsyncIterable<Uint8Array> {
  /** Tunnel ID. */
  readonly tunnelId: string;
  /** The tunnel resource, when this client call opened it. */
  readonly tunnel: Tunnel | undefined;
  readonly #ws: WebSocketConnection;
  #eof = false;
  #writeClosed = false;
  #closed = false;

  /** @internal Use `DavitClient.attachTunnel` or `connectTunnel`. */
  constructor(tunnelId: string, tunnel: Tunnel | undefined, ws: WebSocketConnection) {
    this.tunnelId = tunnelId;
    this.tunnel = tunnel;
    this.#ws = ws;
  }

  #closedError(): TransportError {
    return new TransportError(`tunnel ${this.tunnelId}: stream is closed`);
  }

  /** Resolves the next bytes from the guest port, or null at EOF. */
  async read(): Promise<Uint8Array | null> {
    while (!this.#eof) {
      if (this.#closed) throw this.#closedError();
      let m;
      try {
        m = await this.#ws.next();
      } catch (err) {
        this.#eof = true;
        if (this.#closed) throw this.#closedError();
        this.#ws.close(1000, "");
        throw new TransportError(`tunnel ${this.tunnelId}: stream failed: ${err instanceof Error ? err.message : String(err)}`, { cause: err });
      }
      if (m === null) {
        this.#eof = true;
        if (this.#closed) throw this.#closedError();
        const code = this.#ws.closeCode;
        if (code === 1000 || code === 1005) return null;
        throw new TransportError(`tunnel ${this.tunnelId}: stream ended (${code ?? 1006})${this.#ws.closeReason ? ": " + this.#ws.closeReason : ""}`);
      }
      if (m.type === "binary" && m.data.length > 0) return m.data;
    }
    return null;
  }

  async *[Symbol.asyncIterator](): AsyncGenerator<Uint8Array, void, undefined> {
    for (;;) {
      const chunk = await this.read();
      if (chunk === null) return;
      yield chunk;
    }
  }

  #send(data: Uint8Array): boolean {
    try {
      return this.#ws.sendBinary(data);
    } catch (err) {
      throw new TransportError(`tunnel ${this.tunnelId}: send failed: ${err instanceof Error ? err.message : String(err)}`, { cause: err });
    }
  }

  /** Writes bytes (strings are UTF-8) to the guest port. */
  async write(data: Uint8Array | string): Promise<void> {
    if (this.#closed || this.#writeClosed) throw new TransportError(`tunnel ${this.tunnelId}: stream is closed for writing`);
    const bytes = typeof data === "string" ? Buffer.from(data, "utf8") : data;
    for (let off = 0; off < bytes.byteLength; off += TUNNEL_CHUNK_BYTES) {
      if (!this.#send(bytes.subarray(off, off + TUNNEL_CHUNK_BYTES))) await this.#ws.drain();
    }
  }

  /** Half-closes: the guest port reads EOF; reads keep working. Idempotent. */
  closeWrite(): void {
    if (this.#closed || this.#writeClosed) return;
    this.#writeClosed = true;
    this.#send(new Uint8Array(0)); // an empty binary message is the half-close
  }

  /** Closes both directions and the WebSocket. Idempotent. */
  close(): void {
    if (this.#closed) return;
    this.#closed = true;
    this.#ws.close(1000, "");
  }
}
