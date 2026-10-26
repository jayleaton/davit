// Remote endpoint configuration: $DAVIT_TOKEN and $DAVIT_CA_FILE (private-CA TLS).

import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { createServer as createHttpServer, type IncomingHttpHeaders, type Server } from "node:http";
import { createServer as createHttpsServer } from "node:https";
import { connect as netConnect, type AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, afterEach, before, describe, test } from "node:test";
import { createServer as createTlsServer } from "node:tls";
import { fileURLToPath } from "node:url";

import { CA_FILE_ENV, DavitClient, TOKEN_ENV, TransportError } from "../src/index.ts";
import { SKIP as NO_DAEMON, startDaemon, type Daemon } from "./daemon.ts";

const ENV_NAMES = [TOKEN_ENV, CA_FILE_ENV, "DAVIT_API_URL"];
const saved = Object.fromEntries(ENV_NAMES.map((k) => [k, process.env[k]]));
for (const k of ENV_NAMES) delete process.env[k];
afterEach(() => {
  for (const k of ENV_NAMES) delete process.env[k];
});
after(() => {
  for (const [k, v] of Object.entries(saved)) if (v !== undefined) process.env[k] = v;
});

const dir = mkdtempSync(join(tmpdir(), "swts-remote-"));
after(() => rmSync(dir, { recursive: true, force: true }));

function listen(server: Server, where: string | { host: string; port: number }): Promise<void> {
  return new Promise((resolve) => server.listen(where, resolve));
}

function close(server: { close(cb: () => void): unknown; closeAllConnections?: () => void }): Promise<void> {
  server.closeAllConnections?.();
  return new Promise((resolve) => server.close(() => resolve()));
}

/** A plain HTTP server recording request headers; answers `{"items":[]}`. */
async function recorder(where: string | { host: string; port: number }): Promise<{ server: Server; seen: IncomingHttpHeaders[] }> {
  const seen: IncomingHttpHeaders[] = [];
  const server = createHttpServer((req, res) => {
    seen.push(req.headers);
    res.setHeader("Content-Type", "application/json");
    res.end('{"items":[]}');
  });
  await listen(server, where);
  return { server, seen };
}

describe("token from the environment", () => {
  test("names", () => {
    assert.deepEqual([TOKEN_ENV, CA_FILE_ENV], ["DAVIT_TOKEN", "DAVIT_CA_FILE"]);
  });

  test("used for http(s) base URLs, explicit token wins, empty sends none", async () => {
    const { server, seen } = await recorder({ host: "127.0.0.1", port: 0 });
    try {
      const url = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
      process.env[TOKEN_ENV] = "slw_env";
      await new DavitClient({ baseUrl: url }).listVMs();
      assert.equal(seen.at(-1)!.authorization, "Bearer slw_env");
      process.env.DAVIT_API_URL = url;
      await new DavitClient().listVMs();
      assert.equal(seen.at(-1)!.authorization, "Bearer slw_env");
      await new DavitClient({ baseUrl: url, token: "slw_arg" }).listVMs();
      assert.equal(seen.at(-1)!.authorization, "Bearer slw_arg");
      await new DavitClient({ baseUrl: url, token: "" }).listVMs();
      assert.equal(seen.at(-1)!.authorization, undefined);
    } finally {
      await close(server);
    }
  });

  test("never sent over the unix socket", async () => {
    const sock = join(dir, "h.sock");
    const { server, seen } = await recorder(sock);
    try {
      process.env[TOKEN_ENV] = "slw_env";
      await new DavitClient({ socket: sock }).listVMs();
      process.env.DAVIT_API_URL = `unix://${sock}`;
      await new DavitClient().listVMs();
      assert.equal(seen.length, 2);
      for (const h of seen) assert.equal(h.authorization, undefined);
    } finally {
      await close(server);
    }
  });
});

describe("CA file configuration", () => {
  test("missing, junk or broken files fail at construction", () => {
    const junk = join(dir, "junk.pem");
    writeFileSync(junk, "not a certificate\n");
    const broken = join(dir, "broken.pem");
    writeFileSync(broken, "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n");
    assert.throws(() => new DavitClient({ baseUrl: "https://h", caFile: join(dir, "missing.pem") }), /caFile .*missing\.pem.*cannot read \(ENOENT\)/);
    assert.throws(() => new DavitClient({ baseUrl: "https://h", caFile: junk }), /no PEM certificate/);
    assert.throws(() => new DavitClient({ baseUrl: "https://h", caFile: broken }), /invalid PEM bundle/);
    process.env[CA_FILE_ENV] = join(dir, "missing.pem");
    assert.throws(() => new DavitClient({ baseUrl: "https://h" }), /DAVIT_CA_FILE .*cannot read/);
    // The socket ignores the environment and rejects an explicit CA file.
    new DavitClient({ socket: "/run/x/davit.sock" });
    assert.throws(() => new DavitClient({ socket: "/run/x/davit.sock", caFile: junk }), /not to the local socket/);
  });
});

// ---- a real TLS server with a throwaway private CA (openssl) ---------------

function haveOpenssl(): boolean {
  try {
    execFileSync("openssl", ["version"], { stdio: "ignore" });
    return true;
  } catch {
    return false;
  }
}
const NO_OPENSSL = haveOpenssl() ? false : "openssl not installed";

/** Writes ca.pem, server.pem and server.key (SAN 127.0.0.1, localhost) into a new dir. */
function makeCerts(): string {
  const d = mkdtempSync(join(dir, "tls-"));
  const ssl = (...args: string[]) => execFileSync("openssl", args, { cwd: d, stdio: "pipe", timeout: 60_000 });
  const ec = ["-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes"];
  ssl("req", "-x509", ...ec, "-keyout", "ca.key", "-out", "ca.pem", "-days", "2", "-subj", "/CN=davit test CA",
    "-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign,cRLSign");
  ssl("req", "-new", ...ec, "-keyout", "server.key", "-out", "server.csr", "-subj", "/CN=127.0.0.1");
  writeFileSync(join(d, "ext.cnf"),
    "basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth\n" +
    "subjectAltName=IP:127.0.0.1,DNS:localhost\nsubjectKeyIdentifier=hash\nauthorityKeyIdentifier=keyid\n");
  ssl("x509", "-req", "-in", "server.csr", "-CA", "ca.pem", "-CAkey", "ca.key", "-CAcreateserial",
    "-out", "server.pem", "-days", "2", "-extfile", "ext.cnf");
  return d;
}

const WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11";

describe("private CA over TLS (requests and WebSockets)", { skip: NO_OPENSSL }, () => {
  let certs: string;
  let url: string;
  let server: ReturnType<typeof createHttpsServer>;
  const seen: { path: string; auth: string | undefined }[] = [];
  const conns = new Set<{ destroy(): void }>();

  before(async () => {
    certs = makeCerts();
    server = createHttpsServer({ key: readFile(certs, "server.key"), cert: readFile(certs, "server.pem") }, (req, res) => {
      seen.push({ path: req.url ?? "", auth: req.headers.authorization });
      res.setHeader("Content-Type", "application/json");
      res.end('{"items":[]}');
    });
    server.on("upgrade", (req, socket) => {
      seen.push({ path: req.url ?? "", auth: req.headers.authorization });
      const accept = createHash("sha1").update(`${req.headers["sec-websocket-key"]}${WS_GUID}`).digest("base64");
      socket.write(`HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`);
      socket.end(Buffer.from([0x88, 0x02, 0x03, 0xe8])); // normal close: EOF
      socket.on("error", () => {});
    });
    server.on("tlsClientError", () => {}); // clients without the CA hang up
    server.on("connection", (c: { destroy(): void }) => conns.add(c));
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    url = `https://127.0.0.1:${(server.address() as AddressInfo).port}`;
  });
  after(async () => {
    for (const c of conns) c.destroy(); // upgraded sockets are not closed by closeAllConnections
    await close(server);
  });

  test("trusted with caFile (and from the environment)", async () => {
    seen.length = 0;
    const client = new DavitClient({ baseUrl: url, token: "slw_t", caFile: join(certs, "ca.pem") });
    assert.deepEqual((await client.listVMs()).items, []);
    const stream = await client.attachTunnel("tun_1");
    assert.equal(await stream.read(), null);
    stream.close();
    assert.deepEqual(seen, [
      { path: "/api/v1/vms", auth: "Bearer slw_t" },
      { path: "/api/v1/tunnels/tun_1/stream", auth: "Bearer slw_t" },
    ]);
    process.env.DAVIT_API_URL = url;
    process.env[TOKEN_ENV] = "slw_env";
    process.env[CA_FILE_ENV] = join(certs, "ca.pem");
    assert.deepEqual((await new DavitClient().listVMs()).items, []);
    assert.equal(seen.at(-1)!.auth, "Bearer slw_env");
  });

  test("rejected without the CA, for requests and WebSockets", async () => {
    seen.length = 0;
    const client = new DavitClient({ baseUrl: url });
    await assert.rejects(client.listVMs(), (err: unknown) => err instanceof TransportError && /certificate|self.signed|UNABLE_TO|fetch failed/i.test(describeErr(err)));
    await assert.rejects(client.attachTunnel("tun_1"), (err: unknown) => err instanceof TransportError && /certificate|self.signed|UNABLE_TO/i.test(describeErr(err)));
    assert.deepEqual(seen, []);
  });

  describe("the remote-https example, end to end", { skip: NO_DAEMON, timeout: 120_000 }, () => {
    let d: Daemon;
    let proxy: ReturnType<typeof createTlsServer>;
    let proxyUrl: string;
    const piped = new Set<{ destroy(): void }>();

    before(async () => {
      d = await startDaemon({ guestAgent: true });
      // Terminates TLS with the throwaway CA and pipes each connection to the daemon socket.
      proxy = createTlsServer({ key: readFile(certs, "server.key"), cert: readFile(certs, "server.pem") }, (tls) => {
        const up = netConnect({ path: d.socket });
        piped.add(tls).add(up);
        tls.on("error", () => up.destroy());
        up.on("error", () => tls.destroy());
        tls.pipe(up).pipe(tls);
      });
      proxy.on("tlsClientError", () => {});
      await new Promise<void>((resolve) => proxy.listen(0, "127.0.0.1", resolve));
      proxyUrl = `https://127.0.0.1:${(proxy.address() as AddressInfo).port}`;
    });
    after(async () => {
      for (const s of piped) s.destroy();
      if (proxy) await close(proxy);
      await d?.stop();
    });

    test("creates, runs uname -a and deletes over HTTPS with DAVIT_CA_FILE; exits 1 without it", async () => {
      const env: Record<string, string> = { PATH: process.env.PATH ?? "/usr/bin:/bin", DAVIT_API_URL: proxyUrl, DAVIT_TOKEN: "slw_example", DAVIT_CA_FILE: join(certs, "ca.pem") };
      const ok = await runExample(["ts-example"], env);
      assert.equal(ok.code, 0, ok.stdout + ok.stderr);
      assert.match(ok.stdout, /^engine: fake$/m);
      assert.match(ok.stdout, /^created: ts-example vm_\S+ ready$/m);
      assert.match(ok.stdout, /^Linux /m); // uname -a ran (fake guests run on the host)
      assert.match(ok.stdout, /^deleted: ts-example$/m);
      for await (const vm of d.client.iterVMs()) assert.notEqual(vm.name, "ts-example");
      delete env.DAVIT_CA_FILE;
      const bad = await runExample([], env);
      assert.equal(bad.code, 1, bad.stdout + bad.stderr);
      assert.match(bad.stderr, /^unavailable: .*(certificate|UNABLE_TO)/im);
    });
  });
});

const EXAMPLE = fileURLToPath(new URL("../examples/remote-https.ts", import.meta.url));

function runExample(args: string[], env: Record<string, string>): Promise<{ code: number | null; stdout: string; stderr: string }> {
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [EXAMPLE, ...args], { env, stdio: ["ignore", "pipe", "pipe"] });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (b: Buffer) => (stdout += b.toString()));
    child.stderr.on("data", (b: Buffer) => (stderr += b.toString()));
    const timer = setTimeout(() => child.kill("SIGKILL"), 90_000);
    child.on("error", reject);
    child.on("close", (code) => {
      clearTimeout(timer);
      resolve({ code, stdout, stderr });
    });
  });
}

function readFile(d: string, name: string): Buffer {
  return readFileSync(join(d, name));
}

function describeErr(err: unknown): string {
  const parts: string[] = [];
  for (let e: unknown = err, i = 0; e && i < 5; i++) {
    parts.push(String((e as { message?: unknown }).message ?? e), String((e as { code?: unknown }).code ?? ""));
    e = (e as { cause?: unknown }).cause;
  }
  return parts.join(" ");
}
