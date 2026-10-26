// File transfer helpers: truncation and digest checks for file reads, and mode formatting.

import { createHash, type Hash } from "node:crypto";

import { TransportError } from "./errors.ts";

const BLOCK = 512;
/** Typeflags whose size field carries no data (hard link, symlink, char, block, dir, fifo). */
const HEADER_ONLY = new Set(["1", "2", "3", "4", "5", "6"]);

/**
 * Walks a tar stream's structure as bytes arrive and reports whether it is
 * complete: every header's checksum is valid, every entry's data is present,
 * and two all-zero blocks end the archive (anything after them must be zero).
 * Throws TransportError on a corrupt header.
 */
export class TarChecker {
  #pending = new Uint8Array(0);
  /** Data bytes (block-padded) still to skip for the current entry. */
  #skip = 0;
  #zeroBlocks = 0;
  #ended = false;

  /** Feeds the next chunk of the archive. */
  push(chunk: Uint8Array): void {
    let off = 0;
    while (off < chunk.length) {
      if (this.#skip > 0) {
        const n = Math.min(this.#skip, chunk.length - off);
        this.#skip -= n;
        off += n;
        continue;
      }
      if (this.#ended) {
        for (let i = off; i < chunk.length; i++) {
          if (chunk[i] !== 0) throw new TransportError("tar archive has data after its end marker");
        }
        return;
      }
      const need = BLOCK - this.#pending.length;
      const take = chunk.subarray(off, off + need);
      off += take.length;
      if (this.#pending.length === 0 && take.length === BLOCK) {
        this.#header(take);
        continue;
      }
      const merged = new Uint8Array(this.#pending.length + take.length);
      merged.set(this.#pending);
      merged.set(take, this.#pending.length);
      this.#pending = merged;
      if (merged.length === BLOCK) {
        this.#pending = new Uint8Array(0);
        this.#header(merged);
      }
    }
  }

  /** True once the end marker was seen at a header position. */
  get complete(): boolean {
    return this.#ended && this.#skip === 0;
  }

  #header(b: Uint8Array): void {
    if (b.every((x) => x === 0)) {
      if (++this.#zeroBlocks === 2) this.#ended = true;
      return;
    }
    if (this.#zeroBlocks !== 0) throw new TransportError("tar archive has a lone zero block");
    let sum = 0;
    for (let i = 0; i < BLOCK; i++) sum += i >= 148 && i < 156 ? 32 : b[i]!;
    if (octal(b.subarray(148, 156)) !== sum) throw new TransportError("tar header checksum mismatch");
    const flag = String.fromCharCode(b[156]!);
    const size = HEADER_ONLY.has(flag) ? 0 : numeric(b.subarray(124, 136));
    this.#skip = Math.ceil(size / BLOCK) * BLOCK;
  }
}

function octal(field: Uint8Array): number {
  const s = new TextDecoder("latin1").decode(field).replace(/[\0 ]+$/g, "").replace(/^[ ]+/, "");
  if (s === "") return 0;
  if (!/^[0-7]+$/.test(s)) throw new TransportError("tar header has a malformed number");
  return parseInt(s, 8);
}

/** Size field: octal, or base-256 when the high bit of the first byte is set. */
function numeric(field: Uint8Array): number {
  if ((field[0]! & 0x80) === 0) return octal(field);
  if ((field[0]! & 0x40) !== 0) throw new TransportError("tar header has a negative size");
  let n = field[0]! & 0x3f;
  for (let i = 1; i < field.length; i++) n = n * 256 + field[i]!;
  if (!Number.isSafeInteger(n)) throw new TransportError("tar entry too large");
  return n;
}

/**
 * Formats a write mode: a number becomes `0` + octal (at least 3 digits),
 * a string must already look like `0644`.
 */
export function formatMode(mode: number | string): string {
  if (typeof mode === "number") {
    if (!Number.isInteger(mode) || mode < 0 || mode > 0o7777) throw new TypeError(`mode must be 0..0o7777, got ${mode}`);
    return "0" + mode.toString(8).padStart(3, "0");
  }
  if (!/^0?[0-7]{3,4}$/.test(mode)) throw new TypeError(`mode must be octal permission bits like "0644", got ${JSON.stringify(mode)}`);
  return mode;
}

/** @internal Extra checks of a FileReadStream. */
export interface FileReadChecks {
  /** `X-Davit-Content-Sha256` header value, if sent. */
  sha256?: string | undefined;
  /** Require the body of a regular file to hash to `sha256` (snapshot reads). */
  verifySha256?: boolean;
}

/**
 * A streaming file read. Iterate it once for the bytes; the iteration
 * throws TransportError if the transfer was cut short (Content-Length
 * mismatch, or an incomplete tar for archives) or, for snapshot file
 * reads, if the bytes do not hash to `X-Davit-Content-Sha256`.
 */
export class FileReadStream implements AsyncIterable<Uint8Array> {
  /** Octal permission bits of the file or archive root (`X-Davit-File-Mode`). */
  readonly mode: string | undefined;
  /**
   * Hex SHA-256 from the `X-Davit-Content-Sha256` header, when sent (snapshot
   * file reads; archives and exports carry it as a trailer, which is not read).
   */
  readonly sha256: string | undefined;
  /** Expected byte count (Content-Length), when the server sent one. */
  readonly size: number | undefined;
  /** Whether this is a tar archive of a directory. */
  readonly archive: boolean;
  /** Bytes received so far. */
  bytesRead = 0;
  readonly #source: AsyncIterable<Uint8Array>;
  readonly #verifySha256: boolean;
  #iterator: AsyncIterator<Uint8Array> | undefined;
  #started = false;

  /** @internal Use `DavitClient.readFileStream`. */
  constructor(source: AsyncIterable<Uint8Array>, mode: string | undefined, size: number | undefined, archive: boolean, checks: FileReadChecks = {}) {
    this.#source = source;
    this.mode = mode;
    this.size = size;
    this.archive = archive;
    this.sha256 = checks.sha256;
    this.#verifySha256 = checks.verifySha256 === true && !archive;
  }

  async *[Symbol.asyncIterator](): AsyncGenerator<Uint8Array, void, undefined> {
    if (this.#started) throw new TypeError("a FileReadStream can be iterated only once");
    this.#started = true;
    const tar = this.archive ? new TarChecker() : undefined;
    const hash: Hash | undefined = this.#verifySha256 ? createHash("sha256") : undefined;
    this.#iterator = this.#source[Symbol.asyncIterator]();
    try {
      for (;;) {
        const { value, done } = await this.#iterator.next();
        if (done) break;
        this.bytesRead += value.length;
        if (this.size !== undefined && this.bytesRead > this.size) {
          throw new TransportError(`file transfer sent more than its Content-Length (${this.size})`);
        }
        tar?.push(value);
        hash?.update(value);
        yield value;
      }
    } finally {
      await this.cancel();
    }
    if (this.size !== undefined && this.bytesRead !== this.size) {
      throw new TransportError(`file transfer truncated: got ${this.bytesRead} of ${this.size} bytes`);
    }
    if (tar && !tar.complete) throw new TransportError("tar archive truncated: no end marker");
    if (hash) {
      if (!this.sha256) throw new TransportError("file transfer has no X-Davit-Content-Sha256 header to verify");
      const got = hash.digest("hex");
      const want = this.sha256.trim().toLowerCase();
      if (got !== want) {
        throw new TransportError(`file content digest mismatch: body hashes to sha256 ${got}, X-Davit-Content-Sha256 says ${this.sha256}`);
      }
    }
  }

  /** Stops reading and releases the connection. */
  async cancel(): Promise<void> {
    if (!this.#started) {
      this.#started = true;
      this.#iterator = this.#source[Symbol.asyncIterator]();
    }
    await this.#iterator?.return?.();
  }

  /** Collects everything (verified) into one Uint8Array. */
  async bytes(): Promise<Uint8Array> {
    const parts: Uint8Array[] = [];
    let n = 0;
    for await (const c of this) {
      parts.push(c);
      n += c.length;
    }
    const out = new Uint8Array(n);
    let off = 0;
    for (const p of parts) {
      out.set(p, off);
      off += p.length;
    }
    return out;
  }

  /** The same bytes as a web ReadableStream (errors as above). */
  toReadableStream(): ReadableStream<Uint8Array> {
    const it = this[Symbol.asyncIterator]();
    return new ReadableStream<Uint8Array>({
      async pull(controller) {
        try {
          const { value, done } = await it.next();
          if (done) controller.close();
          else controller.enqueue(value);
        } catch (err) {
          controller.error(err);
        }
      },
      async cancel() {
        await it.return();
      },
    });
  }
}
