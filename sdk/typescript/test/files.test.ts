// Files API unit tests: tar end-marker walker, truncation detection over
// both built-in transports, upload streaming and retry rules.

import { createHash } from "node:crypto";
import { join } from "node:path";
import { tmpdir } from "node:os";
import assert from "node:assert/strict";
import { createServer as createHttp, type IncomingMessage, type ServerResponse } from "node:http";
import { mkdtempSync, rmSync } from "node:fs";
import { describe, test } from "node:test";

import {
  InvalidState,
  DavitClient,
  TarChecker,
  TransportError,
  formatMode,
  type Transport,
  type TransportRequest,
} from "../src/index.ts";
import { buildTar, tarHeader } from "./tar.ts";

const sample = () =>
  buildTar([
    { name: "dir/", type: "5" },
    { name: "dir/a.txt", data: "hello", mode: 0o640 },
    { name: "link", type: "2", linkname: "dir/a.txt" },
    { name: "big.bin", data: new Uint8Array(1500).fill(7) },
  ]);

function check(chunks: Uint8Array[]): boolean {
  const t = new TarChecker();
  for (const c of chunks) t.push(c);
  return t.complete;
}

describe("TarChecker", () => {
  test("complete archives, whole or byte by byte", () => {
    const tar = sample();
    assert.ok(check([tar]));
    assert.ok(check(Array.from(tar, (b) => Uint8Array.of(b))));
    assert.ok(check([tar, new Uint8Array(9216)]), "zero padding after the marker is fine");
  });

  test("truncation is detected", () => {
    const tar = sample();
    assert.ok(!check([tar.subarray(0, tar.length - 512)]), "one end block missing");
    assert.ok(!check([tar.subarray(0, tar.length - 1024)]), "no end marker");
    assert.ok(!check([tar.subarray(0, 512 * 3 + 100)]), "cut inside file data");
    assert.ok(!check([]), "empty body");
  });

  test("corruption throws", () => {
    const bad = sample();
    bad[0] = bad[0]! ^ 1; // checksum no longer matches
    assert.throws(() => check([bad]), TransportError);
    const lone = new Uint8Array([...new Uint8Array(512), ...tarHeader({ name: "x" }, 0), ...new Uint8Array(1024)]);
    assert.throws(() => check([lone]), /lone zero block/);
    const garbage = new Uint8Array([...sample(), 1]);
    assert.throws(() => check([garbage]), /after its end marker/);
  });

  test("base-256 sizes and header-only typeflags", () => {
    const h = tarHeader({ name: "f" }, 0);
    h.fill(0, 124, 136);
    h[124] = 0x80;
    h[135] = 3; // size 3, base-256
    h.fill(32, 148, 156);
    let sum = 0;
    for (const x of h) sum += x;
    h.set(new TextEncoder().encode(sum.toString(8).padStart(6, "0") + "\0 "), 148);
    const data = new Uint8Array(512);
    assert.ok(check([h, data, new Uint8Array(1024)]));
    // A directory header with a non-zero size field carries no data.
    const dir = tarHeader({ name: "d/", type: "5" }, 4096);
    assert.ok(check([dir, new Uint8Array(1024)]));
  });
});

test("formatMode", () => {
  assert.equal(formatMode(0o644), "0644");
  assert.equal(formatMode(0o7), "0007");
  assert.equal(formatMode(0o4755), "04755");
  assert.equal(formatMode("0640"), "0640");
  assert.equal(formatMode("755"), "755");
  for (const bad of [-1, 0o10000, 1.5]) assert.throws(() => formatMode(bad), TypeError);
  for (const bad of ["rw-r--r--", "0999", "64"]) assert.throws(() => formatMode(bad), TypeError);
});

type Handler = (req: IncomingMessage, res: ServerResponse, body: Buffer) => void;

/** An HTTP server on a unix socket or loopback TCP; yields clients for both transports. */
async function withServer(kind: "unix" | "tcp", handler: Handler, fn: (c: DavitClient) => Promise<void>): Promise<void> {
  const dir = mkdtempSync(join(tmpdir(), "swts-f-"));
  const srv = createHttp((req, res) => {
    const parts: Buffer[] = [];
    req.on("data", (c: Buffer) => parts.push(c));
    req.on("end", () => handler(req, res, Buffer.concat(parts)));
  });
  try {
    let client: DavitClient;
    if (kind === "unix") {
      await new Promise<void>((r) => srv.listen(`${dir}/s.sock`, r));
      client = new DavitClient({ socket: `${dir}/s.sock` });
    } else {
      await new Promise<void>((r) => srv.listen(0, "127.0.0.1", r));
      client = new DavitClient({ baseUrl: `http://127.0.0.1:${(srv.address() as { port: number }).port}` });
    }
    await fn(client);
  } finally {
    srv.closeAllConnections();
    await new Promise((r) => srv.close(r));
    rmSync(dir, { recursive: true, force: true });
  }
}

for (const kind of ["unix", "tcp"] as const) {
  describe(`readFile over ${kind}`, () => {
    test("returns bytes and mode; sends path, generation, user, archive", async () => {
      let url = "";
      await withServer(
        kind,
        (req, res) => {
          url = req.url ?? "";
          res.writeHead(200, { "content-type": "application/octet-stream", "x-davit-file-mode": "0640", "content-length": "4" });
          res.end(Buffer.from([0, 1, 2, 255]));
        },
        async (c) => {
          const f = await c.readFile("vm_1", "/etc/a b", { generation: 2, user: "dev" });
          assert.deepEqual([...f.data], [0, 1, 2, 255]);
          assert.equal(f.mode, "0640");
        },
      );
      const u = new URL(url, "http://x");
      assert.equal(u.pathname, "/api/v1/vms/vm_1/files");
      assert.equal(u.searchParams.get("path"), "/etc/a b");
      assert.equal(u.searchParams.get("generation"), "2");
      assert.equal(u.searchParams.get("user"), "dev");
      assert.equal(u.searchParams.get("archive"), null);
    });

    test("a short body (Content-Length) is a TransportError", async () => {
      await withServer(
        kind,
        (_req, res) => {
          res.writeHead(200, { "content-length": "10" });
          res.write("12345");
          setTimeout(() => res.socket?.destroy(), 20);
        },
        async (c) => {
          await assert.rejects(c.readFile("vm", "/f", { generation: 1 }), TransportError);
        },
      );
    });

    test("an archive without its end marker is a TransportError", async () => {
      const tar = sample();
      await withServer(
        kind,
        (req, res) => {
          assert.equal(new URL(req.url ?? "", "http://x").searchParams.get("archive"), "tar");
          res.writeHead(200, { "content-type": "application/x-tar" });
          res.end(Buffer.from(tar.subarray(0, tar.length - 1024))); // a clean, chunked end
        },
        async (c) => {
          await assert.rejects(c.readFile("vm", "/d", { generation: 1, archive: true }), /no end marker/);
        },
      );
      await withServer(
        kind,
        (_req, res) => {
          res.writeHead(200, { "content-type": "application/x-tar" });
          res.end(Buffer.from(tar));
        },
        async (c) => {
          const s = await c.readFileStream("vm", "/d", { generation: 1, archive: true });
          let n = 0;
          for await (const chunk of s) n += chunk.length;
          assert.equal(n, tar.length);
          assert.equal(s.bytesRead, tar.length);
        },
      );
    });

    test("error answers are typed", async () => {
      await withServer(
        kind,
        (_req, res) => {
          res.writeHead(409, { "content-type": "application/json" });
          res.end(JSON.stringify({ error: { code: "invalid_state", message: "generation 1 != 2", retryable: false, requestId: "req_s" } }));
        },
        async (c) => {
          const err = await c.readFile("vm", "/f", { generation: 1 }).catch((e: unknown) => e);
          assert.ok(err instanceof InvalidState);
          assert.equal(err.status, 409);
          assert.equal(err.requestId, "req_s");
        },
      );
    });

    test("writeFile streams a body chunked and gets FileInfo back", async () => {
      let got: { body: Buffer; headers: IncomingMessage["headers"]; url: string } | undefined;
      await withServer(
        kind,
        (req, res, body) => {
          got = { body, headers: req.headers, url: req.url ?? "" };
          res.writeHead(200, { "content-type": "application/json" });
          res.end(JSON.stringify({ path: "/f", sizeBytes: body.length, mode: "0600", generation: 3 }));
        },
        async (c) => {
          async function* chunks() {
            for (let i = 0; i < 64; i++) yield new Uint8Array(64 * 1024).fill(i);
          }
          const info = await c.writeFile("vm", "/f", chunks(), { generation: 3, mode: 0o600, archive: false });
          assert.equal(info.sizeBytes, 64 * 64 * 1024);
        },
      );
      assert.equal(got!.body.length, 64 * 64 * 1024);
      assert.equal(got!.body[64 * 1024 * 5], 5);
      assert.equal(got!.headers["content-type"], "application/octet-stream");
      assert.equal(got!.headers["transfer-encoding"], "chunked");
      assert.equal(got!.headers["idempotency-key"], undefined);
      assert.equal(new URL(got!.url, "http://x").searchParams.get("mode"), "0600");
    });

    test("an early error answer to a streamed upload is the typed error", async () => {
      await withServer(
        kind,
        (_req, res) => {
          res.writeHead(409, { "content-type": "application/json", connection: "close" });
          res.end(JSON.stringify({ error: { code: "invalid_state", message: "stale", retryable: false, requestId: "r" } }));
        },
        async (c) => {
          const big = new ReadableStream<Uint8Array>({
            start(ctl) {
              for (let i = 0; i < 8; i++) ctl.enqueue(new Uint8Array(256 * 1024));
              ctl.close();
            },
          });
          await assert.rejects(c.writeFile("vm", "/f", big, { generation: 1 }), InvalidState);
        },
      );
    });
  });
}

describe("writeFile request shaping", () => {
  function recorder(handler: (req: TransportRequest, n: number) => { status: number; body: unknown }): Transport & { calls: TransportRequest[] } {
    const calls: TransportRequest[] = [];
    return {
      calls,
      async request(req) {
        calls.push(req);
        const r = handler(req, calls.length);
        return { status: r.status, headers: {}, body: JSON.stringify(r.body) };
      },
    };
  }
  const info = { path: "/f", sizeBytes: 1, mode: "0644", generation: 7 };

  test("omitted generation: GET the VM, then use its id and generation", async () => {
    const t = recorder((req) =>
      req.method === "GET" ? { status: 200, body: { id: "vm_abc", name: "web", instanceGeneration: 7 } } : { status: 200, body: info },
    );
    const c = new DavitClient({ transport: t });
    await c.writeFile("web", "/f", "x", { archive: true, mode: "0750" });
    assert.equal(t.calls[0]!.path, "/api/v1/vms/web");
    const put = t.calls[1]!;
    const u = new URL(put.path, "http://x");
    assert.equal(u.pathname, "/api/v1/vms/vm_abc/files");
    assert.equal(u.searchParams.get("generation"), "7");
    assert.equal(u.searchParams.get("archive"), "tar");
    assert.equal(u.searchParams.get("mode"), "0750");
    assert.equal(put.headers["Content-Type"], "application/x-tar");
    assert.equal(put.headers["Idempotency-Key"], undefined, "writeFile sends no idempotency key");
    assert.equal(put.body, "x");
  });

  test("bytes are retried on connect failure; streams are not", async () => {
    const refused = () => {
      throw new TransportError("refused", { connectFailed: true });
    };
    const t1 = { calls: 0, async request() { this.calls++; return refused(); } };
    await assert.rejects(new DavitClient({ transport: t1 }).writeFile("vm", "/f", new Uint8Array(3), { generation: 1 }), TransportError);
    assert.equal(t1.calls, 3);
    const t2 = { calls: 0, async request() { this.calls++; return refused(); } };
    async function* one() {
      yield new Uint8Array(3);
    }
    await assert.rejects(new DavitClient({ transport: t2 }).writeFile("vm", "/f", one(), { generation: 1 }), TransportError);
    assert.equal(t2.calls, 1);
  });

  test("client-side validation", async () => {
    const c = new DavitClient({ transport: recorder(() => ({ status: 200, body: info })) });
    await assert.rejects(c.readFile("vm", "relative/path", { generation: 1 }), TypeError);
    await assert.rejects(c.writeFile("vm", "/f", "x", { generation: 0 }), TypeError);
    await assert.rejects(c.writeFile("vm", "/f", "x", { generation: 1, mode: "rwx" }), TypeError);
    await assert.rejects(c.writeFile("vm", "/f", 42 as unknown as string, { generation: 1 }), TypeError);
    await assert.rejects(c.readFile("vm", "/f", { generation: 1 }), /no stream/);
  });
});

const sha256 = (b: Uint8Array | string) => createHash("sha256").update(b).digest("hex");

describe("snapshot tree", () => {
  const entry = (path: string, type = "file", extra: Record<string, unknown> = {}) => ({
    path,
    type,
    sizeBytes: 3,
    mode: "0644",
    modifiedAt: "2026-10-22T04:22:34Z",
    ...extra,
  });

  test("iterSnapshotTree follows nextCursor with path and limit", async () => {
    const calls: string[] = [];
    const t: Transport = {
      async request(req) {
        calls.push(req.path);
        const cursor = new URL(req.path, "http://x").searchParams.get("cursor");
        const page = cursor
          ? { path: "/etc", items: [entry("/etc/l", "symlink", { linkTarget: "/etc/a" })] }
          : { path: "/etc", items: [entry("/etc/a"), entry("/etc/sub", "dir")], nextCursor: "c2" };
        return { status: 200, headers: {}, body: JSON.stringify(page) };
      },
    };
    const c = new DavitClient({ transport: t });
    const got = [];
    for await (const e of c.iterSnapshotTree("base snap", { path: "/etc", limit: 2 })) got.push(e);
    assert.deepEqual(got.map((e) => [e.path, e.type]), [["/etc/a", "file"], ["/etc/sub", "dir"], ["/etc/l", "symlink"]]);
    assert.equal(got[2]!.linkTarget, "/etc/a");
    assert.deepEqual(calls, [
      "/api/v1/snapshots/base%20snap/tree?path=%2Fetc&limit=2",
      "/api/v1/snapshots/base%20snap/tree?path=%2Fetc&limit=2&cursor=c2",
    ]);
  });

  test("listSnapshotTree defaults to / and validates the path", async () => {
    const calls: string[] = [];
    const t: Transport = {
      async request(req) {
        calls.push(req.path);
        return { status: 200, headers: {}, body: JSON.stringify({ path: "/", items: [] }) };
      },
    };
    const c = new DavitClient({ transport: t });
    const page = await c.listSnapshotTree("snap_1");
    assert.deepEqual(page, { path: "/", items: [] });
    assert.deepEqual(calls, ["/api/v1/snapshots/snap_1/tree?path=%2F"]);
    await assert.rejects(c.listSnapshotTree("snap_1", { path: "etc" }), TypeError);
    await assert.rejects(c.readSnapshotFile("snap_1", "etc"), TypeError);
    await assert.rejects(c.exportSnapshot("snap_1", { format: "zip" as "tar" }), TypeError);
    assert.equal(calls.length, 1);
  });
});

for (const kind of ["unix", "tcp"] as const) {
  describe(`snapshot files over ${kind}`, () => {
    test("readSnapshotFile verifies X-Davit-Content-Sha256", async () => {
      const body = Buffer.from("root:x:0:0\n");
      let url = "";
      await withServer(
        kind,
        (req, res) => {
          url = req.url ?? "";
          res.writeHead(200, {
            "content-type": "application/octet-stream",
            "content-length": String(body.length),
            "x-davit-file-mode": "0600",
            "x-davit-content-sha256": sha256(body),
          });
          res.end(body);
        },
        async (c) => {
          const f = await c.readSnapshotFile("snap_1", "/etc/passwd");
          assert.deepEqual(Buffer.from(f.data), body);
          assert.equal(f.mode, "0600");
          assert.equal(f.sha256, sha256(body));
          const s = await c.readSnapshotFileStream("snap_1", "/etc/passwd");
          assert.equal(s.sha256, sha256(body));
          assert.deepEqual(Buffer.from(await s.bytes()), body);
        },
      );
      const u = new URL(url, "http://x");
      assert.equal(u.pathname, "/api/v1/snapshots/snap_1/files");
      assert.deepEqual([...u.searchParams], [["path", "/etc/passwd"]]);
    });

    test("a digest mismatch or a missing digest is a TransportError", async () => {
      const wrong = sha256("other");
      await withServer(
        kind,
        (req, res) => {
          const headers: Record<string, string> = { "content-length": "8" };
          if (new URL(req.url ?? "", "http://x").searchParams.get("path") === "/bad") headers["x-davit-content-sha256"] = wrong;
          res.writeHead(200, headers);
          res.end("tampered");
        },
        async (c) => {
          await assert.rejects(c.readSnapshotFile("snap_1", "/bad"), (err: unknown) => {
            assert.ok(err instanceof TransportError);
            assert.match(err.message, /digest mismatch/);
            assert.ok(err.message.includes(wrong) && err.message.includes(sha256("tampered")));
            return true;
          });
          const s = await c.readSnapshotFileStream("snap_1", "/bad");
          await assert.rejects(s.bytes(), /digest mismatch/);
          await assert.rejects(c.readSnapshotFile("snap_1", "/nodigest"), /no X-Davit-Content-Sha256/);
        },
      );
    });

    test("a directory reads as a checked tar", async () => {
      const tar = sample();
      let cut = false;
      await withServer(
        kind,
        (req, res) => {
          assert.equal(new URL(req.url ?? "", "http://x").searchParams.get("archive"), "tar");
          res.writeHead(200, { "content-type": "application/x-tar", "x-davit-file-mode": "0755" });
          res.end(Buffer.from(cut ? tar.subarray(0, tar.length - 1024) : tar));
        },
        async (c) => {
          const f = await c.readSnapshotFile("snap_1", "/srv", { archive: true });
          assert.deepEqual(f.data, tar);
          assert.equal(f.mode, "0755");
          assert.equal(f.sha256, undefined);
          cut = true;
          await assert.rejects(c.readSnapshotFile("snap_1", "/srv", { archive: true }), /no end marker/);
        },
      );
    });

    test("exportSnapshot streams tar (default) and raw", async () => {
      const tar = sample();
      const raw = new Uint8Array(200_000).map((_, i) => i % 251);
      const urls: string[] = [];
      await withServer(
        kind,
        (req, res) => {
          urls.push(req.url ?? "");
          const format = new URL(req.url ?? "", "http://x").searchParams.get("format");
          if (format === "tar") {
            res.writeHead(200, { "content-type": "application/x-tar" });
            res.end(Buffer.from(tar));
          } else {
            res.writeHead(200, { "content-type": "application/octet-stream", "content-length": String(raw.length) });
            res.end(Buffer.from(raw));
          }
        },
        async (c) => {
          const t = await c.exportSnapshot("base");
          assert.equal(t.archive, true);
          assert.deepEqual(await t.bytes(), tar);
          const r = await c.exportSnapshot("base", { format: "raw" });
          assert.equal(r.archive, false);
          assert.equal(r.size, raw.length);
          let n = 0;
          for await (const chunk of r) n += chunk.length;
          assert.equal(n, raw.length);
        },
      );
      assert.deepEqual(urls, ["/api/v1/snapshots/base/export?format=tar", "/api/v1/snapshots/base/export?format=raw"]);
    });
  });
}
