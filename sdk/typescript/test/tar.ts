// A tiny test-only ustar writer (the SDK itself ships no tar support).

export interface TarEntry {
  name: string;
  data?: Uint8Array | string;
  mode?: number;
  /** "0" file (default), "5" directory, "2" symlink. */
  type?: "0" | "2" | "5";
  linkname?: string;
}

function field(b: Uint8Array, off: number, len: number, value: string): void {
  b.set(new TextEncoder().encode(value).subarray(0, len), off);
}

function octal(n: number, len: number): string {
  return n.toString(8).padStart(len - 1, "0") + "\0";
}

/** Builds one 512-byte ustar header (exported for corrupt-header tests). */
export function tarHeader(e: TarEntry, size: number): Uint8Array {
  const h = new Uint8Array(512);
  field(h, 0, 100, e.name);
  field(h, 100, 8, octal(e.mode ?? (e.type === "5" ? 0o755 : 0o644), 8));
  field(h, 108, 8, octal(0, 8));
  field(h, 116, 8, octal(0, 8));
  field(h, 124, 12, octal(size, 12));
  field(h, 136, 12, octal(1_700_000_000, 12));
  field(h, 148, 8, "        ");
  field(h, 156, 1, e.type ?? "0");
  field(h, 157, 100, e.linkname ?? "");
  field(h, 257, 6, "ustar\0");
  field(h, 263, 2, "00");
  let sum = 0;
  for (const x of h) sum += x;
  field(h, 148, 8, sum.toString(8).padStart(6, "0") + "\0 ");
  return h;
}

/** Builds a complete archive with the two-zero-block end marker. */
export function buildTar(entries: TarEntry[]): Uint8Array {
  const parts: Uint8Array[] = [];
  for (const e of entries) {
    const data = typeof e.data === "string" ? new TextEncoder().encode(e.data) : (e.data ?? new Uint8Array(0));
    parts.push(tarHeader(e, e.type === "5" || e.type === "2" ? 0 : data.length));
    if (e.type !== "5" && e.type !== "2" && data.length > 0) {
      const padded = new Uint8Array(Math.ceil(data.length / 512) * 512);
      padded.set(data);
      parts.push(padded);
    }
  }
  parts.push(new Uint8Array(1024));
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let off = 0;
  for (const p of parts) {
    out.set(p, off);
    off += p.length;
  }
  return out;
}
