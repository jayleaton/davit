// Process streams (attachProcess) and run() results.

import { DavitError, TransportError, errorFor } from "./errors.ts";
import type { Process, ProcessStreamMessage } from "./schema.generated.ts";
import type { WebSocketConnection } from "./websocket.ts";

/** Exit status `run` reports when the process deadline killed it. */
export const EXIT_DEADLINE = 124;
/** Exit status `run` reports when the outcome is unknown. */
export const EXIT_LOST = 255;

const SIGNAL_NUMBERS: Readonly<Record<string, number>> = Object.freeze({
  HUP: 1, INT: 2, QUIT: 3, ILL: 4, TRAP: 5, ABRT: 6, BUS: 7, FPE: 8, KILL: 9, USR1: 10,
  SEGV: 11, USR2: 12, PIPE: 13, ALRM: 14, TERM: 15, SYS: 31,
});

/**
 * Maps a finished process to a shell-style exit status, like `davit exec`:
 * 124 for the deadline, the guest's code (& 0xff), 128+n for a signal, else 255.
 */
export function processExitCode(p: Process | undefined): number {
  if (!p) return EXIT_LOST;
  if (p.timedOut) return EXIT_DEADLINE;
  if (typeof p.exitCode === "number") return p.exitCode & 0xff;
  if (p.signal) return signalExitCode(p.signal) ?? EXIT_LOST;
  return EXIT_LOST;
}

/** `128 + n` for a signal name (`"KILL"` or `"SIGKILL"`), or undefined if unknown. */
export function signalExitCode(signal: string): number | undefined {
  const n = SIGNAL_NUMBERS[signal.toUpperCase().replace(/^SIG/, "")];
  return n === undefined ? undefined : 128 + n;
}

/** A chunk of process output. */
export interface ProcessOutputEvent {
  type: "stdout" | "stderr";
  /** Output sequence number (use as `outputCursor` to resume after it). */
  seq: number;
  data: Uint8Array;
}

/** The final stream event: the process finished. */
export interface ProcessExitEvent {
  type: "exit";
  seq: number;
  process: Process;
}

/** Events yielded by a ProcessStream. */
export type ProcessStreamEvent = ProcessOutputEvent | ProcessExitEvent;

/** Opens a WebSocket for the stream from `outputCursor`. */
export type StreamOpener = (outputCursor: number) => Promise<WebSocketConnection>;

const MAX_RECONNECTS = 3;
/** Largest raw stdin chunk per message (base64 + JSON stays far below the server's 1 MiB). */
export const STDIN_CHUNK_BYTES = 256 << 10;

/**
 * A live attachment to a process's stdio. Iterate it (once) for output and
 * the final exit event; send input with `sendStdin`/`closeStdin`; signal
 * with `signal`. If the connection drops before the exit, the stream
 * re-attaches from `cursor` (output is never duplicated; stdin is not
 * replayed and is no longer owned after a reconnect).
 */
export class ProcessStream implements AsyncIterable<ProcessStreamEvent> {
  /** VM the process runs in (as passed to attachProcess). */
  readonly vm: string;
  /** Process ID. */
  readonly processId: string;
  /** Seq of the last output chunk delivered (resume point). */
  cursor: number;
  /** The final process once the exit event arrived. */
  exit: Process | undefined;
  #ws: WebSocketConnection;
  readonly #open: StreamOpener;
  #closed = false;
  #iterating = false;

  /** @internal Use `DavitClient.attachProcess`. */
  constructor(vm: string, processId: string, cursor: number, ws: WebSocketConnection, open: StreamOpener) {
    this.vm = vm;
    this.processId = processId;
    this.cursor = cursor;
    this.#ws = ws;
    this.#open = open;
  }

  #send(msg: ProcessStreamMessage): void {
    if (this.#closed) throw new TransportError("process stream is closed");
    this.#ws.sendText(JSON.stringify(msg));
  }

  /** Writes to the process's stdin (strings are UTF-8). Needs `stdin: true` at exec and the first stream. */
  sendStdin(data: string | Uint8Array): void {
    const bytes = typeof data === "string" ? Buffer.from(data, "utf8") : Buffer.from(data.buffer, data.byteOffset, data.byteLength);
    for (let off = 0; off < bytes.length; off += STDIN_CHUNK_BYTES) {
      this.#send({ type: "stdin", data: bytes.subarray(off, off + STDIN_CHUNK_BYTES).toString("base64") });
    }
  }

  /** Closes the process's stdin (end of input). */
  closeStdin(): void {
    this.#send({ type: "stdin.close" });
  }

  /** Signals the process group (e.g. `"INT"`, `"TERM"`, `"KILL"`). */
  signal(name: string): void {
    this.#send({ type: "signal", signal: name });
  }

  /**
   * Closes the stream. A non-detached process is killed by the server if
   * this was its first stream and it has not exited yet.
   */
  close(): void {
    if (this.#closed) return;
    this.#closed = true;
    this.#ws.close(1000, "");
  }

  async *[Symbol.asyncIterator](): AsyncGenerator<ProcessStreamEvent, void, undefined> {
    if (this.#iterating) throw new TypeError("a ProcessStream can be iterated only once");
    this.#iterating = true;
    try {
      for (;;) {
        let msg: { type: string; data: Uint8Array } | null;
        try {
          msg = await this.#ws.next();
        } catch (err) {
          if (this.#closed) return;
          await this.#reconnect(err);
          continue;
        }
        if (msg === null) {
          if (this.#closed) return;
          await this.#reconnect(undefined);
          continue;
        }
        if (msg.type !== "text") continue;
        let m: ProcessStreamMessage;
        try {
          m = JSON.parse(Buffer.from(msg.data).toString("utf8")) as ProcessStreamMessage;
        } catch {
          continue;
        }
        switch (m.type) {
          case "stdout":
          case "stderr": {
            const seq = m.seq ?? 0;
            if (seq <= this.cursor) continue;
            this.cursor = seq;
            yield { type: m.type, seq, data: new Uint8Array(Buffer.from(m.data ?? "", "base64")) };
            break;
          }
          case "exit": {
            if (!m.process) throw new DavitError({ code: "internal", message: `process ${this.processId}: exit message without a process` });
            this.exit = m.process;
            this.close();
            yield { type: "exit", seq: m.seq ?? this.cursor, process: m.process };
            return;
          }
          case "error": {
            this.close();
            const e = m.error;
            throw errorFor({
              code: e?.code ?? "internal",
              message: e?.message ?? "process stream failed",
              retryable: e?.retryable ?? false,
              details: e?.details,
              requestId: e?.requestId || undefined,
            });
          }
          default:
            continue; // unknown types are ignored
        }
      }
    } finally {
      this.close();
    }
  }

  async #reconnect(cause: unknown): Promise<void> {
    let last: unknown = cause;
    for (let attempt = 1; attempt <= MAX_RECONNECTS; attempt++) {
      if (attempt > 1) await new Promise((r) => setTimeout(r, 100 * 2 ** (attempt - 2)));
      try {
        this.#ws = await this.#open(this.cursor);
        return;
      } catch (err) {
        if (!(err instanceof TransportError)) throw err;
        last = err;
      }
    }
    this.#closed = true;
    throw new TransportError(`lost the stream of process ${this.processId} after output seq ${this.cursor}; re-attach with outputCursor ${this.cursor}`, {
      cause: last,
    });
  }
}

/** Result of `DavitClient.run`. */
export class RunResult {
  /** Shell-style exit status (see `processExitCode`). */
  readonly exitCode: number;
  readonly stdout: Uint8Array;
  readonly stderr: Uint8Array;
  /** The final process from the exit message. */
  readonly process: Process;

  constructor(process: Process, stdout: Uint8Array, stderr: Uint8Array) {
    this.process = process;
    this.exitCode = processExitCode(process);
    this.stdout = stdout;
    this.stderr = stderr;
  }

  /** True when the process deadline killed it (exit status 124). */
  get timedOut(): boolean {
    return this.process.timedOut === true;
  }

  /** stdout decoded as UTF-8 (invalid bytes become U+FFFD). */
  get stdoutText(): string {
    return new TextDecoder().decode(this.stdout);
  }

  /** stderr decoded as UTF-8 (invalid bytes become U+FFFD). */
  get stderrText(): string {
    return new TextDecoder().decode(this.stderr);
  }
}

/** Concatenates byte chunks. */
export function concatBytes(chunks: Uint8Array[]): Uint8Array {
  let n = 0;
  for (const c of chunks) n += c.length;
  const out = new Uint8Array(n);
  let off = 0;
  for (const c of chunks) {
    out.set(c, off);
    off += c.length;
  }
  return out;
}
