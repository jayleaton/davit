// Integration test against a real daemon running the fake runtime (see daemon.ts).

import { tmpdir } from "node:os";
import assert from "node:assert/strict";
import { createHash, generateKeyPairSync, randomBytes } from "node:crypto";
import { lstatSync, mkdirSync, readFileSync, readlinkSync, realpathSync, statSync } from "node:fs";
import { connect, createServer, type AddressInfo, type Socket } from "node:net";
import { fileURLToPath } from "node:url";
import { after, before, describe, test } from "node:test";

import {
  IdempotencyConflict,
  InvalidArgument,
  InvalidState,
  OperationInProgress,
  Unavailable,
  UnsupportedCapability,
  NotFound,
  RevisionMismatch,
  ServerNotImplemented,
  DavitClient,
  StopRequired,
  WaitTimeout,
  newIdempotencyKey,
  processExitCode,
  type PromptEvent,
  type PromptOptions,
  type Run,
  type VM,
} from "../src/index.ts";
import { buildTar } from "./tar.ts";
import { SKIP, startDaemon, type Daemon } from "./daemon.ts";

describe("real daemon (fake runtime)", { skip: SKIP, timeout: 120_000 }, () => {
  let d: Daemon;
  let c: DavitClient;
  let web: VM;

  before(async () => {
    d = await startDaemon();
    c = d.client;
  });
  after(async () => {
    await d?.stop();
  });

  test("capabilities and images", async () => {
    const caps = await c.getCapabilities();
    assert.equal(caps.runtime.engine, "fake");
    const images = await c.listImages();
    for (const img of images.items) assert.equal((await c.getImage(img.id)).id, img.id);
  });

  test("create waits until ready", async () => {
    const phases: string[] = [];
    web = await c.createVM({ name: "web" }, { onProgress: (e) => phases.push(e.phase) });
    assert.equal(web.name, "web");
    assert.equal(web.state, "ready");
    assert.equal(web.instanceGeneration, 1);
    assert.equal(web.bootMode, "cold_boot");
    assert.ok(phases.length > 0);
  });

  test("prompts need a guest transport (UnsupportedCapability without one)", async () => {
    await assert.rejects(c.listConversations("web"), UnsupportedCapability);
    await assert.rejects(c.createPrompt("web", "hi"), UnsupportedCapability);
  });

  test("app endpoints need a tailnet gateway (UnsupportedCapability without one)", async () => {
    const feature = (await c.getCapabilities()).features.find((f) => f.name === "appEndpoints");
    assert.equal(feature?.status, "unsupported");
    const err = await c.createAppEndpoint("web", 3000).catch((e: unknown) => e);
    assert.ok(err instanceof UnsupportedCapability, `expected UnsupportedCapability, got ${String(err)}`);
    assert.equal(err.status, 422);
    assert.equal(err.code, "unsupported_capability");
  });

  test("members need a tailnet gateway", async () => {
    const feature = (await c.getCapabilities()).features.find((f) => f.name === "members");
    assert.equal(feature?.status, "unsupported");
    const err = await c.addMember("alice@example.com", "viewer").catch((e: unknown) => e);
    assert.ok(err instanceof UnsupportedCapability, `expected UnsupportedCapability, got ${String(err)}`);
    assert.equal(err.status, 422);
    assert.equal(err.code, "unsupported_capability");
  });

  test("desktop needs a tailnet gateway (UnsupportedCapability without one)", async () => {
    const feature = (await c.getCapabilities()).features.find((f) => f.name === "desktop");
    assert.equal(feature?.status, "unsupported");
    const err = await c.openDesktop("web").catch((e: unknown) => e);
    assert.ok(err instanceof UnsupportedCapability, `expected UnsupportedCapability, got ${String(err)}`);
    assert.equal(err.status, 422);
    assert.equal(err.code, "unsupported_capability");
  });

  test("fork before any stop is StopRequired", async () => {
    await assert.rejects(c.forkVM("web", { name: "too-early" }), StopRequired);
  });

  test("stale If-Match is RevisionMismatch", async () => {
    const cur = await c.getVM("web");
    await assert.rejects(c.stopVM("web", { ifMatch: cur.stateRevision - 1 }), RevisionMismatch);
  });

  test("stop commits a snapshot", async () => {
    const cur = await c.getVM("web");
    const vm = await c.stopVM("web", { ifMatch: cur.stateRevision });
    assert.equal(vm.state, "stopped");
    assert.ok(vm.lastSnapshotId);
  });

  test("createSnapshot names the committed disk", async () => {
    const snap = await c.createSnapshot("web", "base");
    assert.equal(snap.name, "base");
    assert.equal(snap.sourceVmId, web.id);
    assert.equal((await c.getSnapshot("base")).id, snap.id);
    const ids: string[] = [];
    for await (const s of c.iterVMSnapshots("web", { limit: 1 })) ids.push(s.id);
    assert.ok(ids.includes(snap.id));
  });

  test("fork from last commit and from a named snapshot", async () => {
    const f1 = await c.forkVM("web", { name: "web-fork" });
    assert.equal(f1.state, "ready");
    assert.ok(f1.sourceSnapshotId);
    const f2 = await c.forkVM("web", { name: "web-fork2", snapshot: "base" });
    assert.equal(f2.state, "ready");
    assert.equal(f2.sourceSnapshotId, (await c.getSnapshot("base")).id);
  });

  test("resume boots generation 2", async () => {
    const vm = await c.resumeVM("web");
    assert.equal(vm.state, "ready");
    assert.equal(vm.instanceGeneration, 2);
    assert.equal((await c.waitReady("web")).id, web.id);
  });

  test("idempotency key replay and conflict", async () => {
    const key = newIdempotencyKey();
    const a = await c.createVM({ name: "idem" }, { idempotencyKey: key, wait: false });
    const b = await c.createVM({ name: "idem" }, { idempotencyKey: key, wait: false });
    assert.equal(a.operation.id, b.operation.id);
    await assert.rejects(c.createVM({ name: "idem-other" }, { idempotencyKey: key, wait: false }), IdempotencyConflict);
    await c.wait(a.operation.id);
    const op = await c.getOperation(a.operation.id);
    assert.equal(op.state, "succeeded");
  });

  test("tokens: create (secret once), replay, list, revoke, typed errors", async () => {
    const key = newIdempotencyKey();
    const created = await c.createToken("ci", { actions: ["exec"], vm: "web", ttlSeconds: 3600, idempotencyKey: key });
    assert.match(created.secret, /^slw_/);
    assert.deepEqual(created.scopes, [{ vmId: web.id, actions: ["exec"] }]);
    const replay = await c.createToken("ci", { actions: ["exec"], vm: "web", ttlSeconds: 3600, idempotencyKey: key });
    assert.deepEqual([replay.id, replay.secret], [created.id, created.secret]);
    const other = await c.createToken("all", { actions: ["view"], ttlSeconds: 60 });

    const listed = new Map<string, Record<string, unknown>>();
    for await (const t of c.iterTokens({ limit: 1 })) listed.set(t.id, t as unknown as Record<string, unknown>);
    assert.ok(listed.has(created.id) && listed.has(other.id));
    assert.equal(listed.get(created.id)!.name, "ci");
    assert.equal("secret" in listed.get(created.id)!, false);

    assert.equal(await c.revokeToken(created), undefined);
    await assert.rejects(c.revokeToken(created.id), NotFound);
    assert.ok(!(await c.listTokens()).items.some((t) => t.id === created.id));
    await c.revokeToken(other.id);

    await assert.rejects(c.createToken("bad", { actions: ["root"], ttlSeconds: 60 }), InvalidArgument);
    await assert.rejects(c.createToken("gone", { actions: ["view"], vm: "no-such-vm", ttlSeconds: 60 }), NotFound);
  });

  test("iterator lists the live VMs", async () => {
    const names: string[] = [];
    for await (const vm of c.iterVMs({ limit: 1 })) names.push(vm.name);
    assert.deepEqual(names.sort(), ["idem", "web", "web-fork", "web-fork2"]);
  });

  test("delete everything, then snapshots", async () => {
    for (const name of ["web-fork", "web-fork2", "idem", "web"]) {
      const vm = await c.deleteVM(name);
      assert.equal(vm.state, "deleted", name);
      await assert.rejects(c.getVM(name), NotFound);
    }
    assert.equal((await c.listVMs()).items.length, 0);
    await c.deleteSnapshot("base");
    await assert.rejects(c.deleteSnapshot("base"), NotFound);
  });
});

describe("wait budget (fake bootDelay 2s)", { skip: SKIP, timeout: 60_000 }, () => {
  let d: Daemon;
  before(async () => {
    d = await startDaemon({ bootDelay: "2s" });
  });
  after(async () => {
    await d?.stop();
  });

  test("WaitTimeout carries the operation, which still succeeds", async () => {
    const c = d.client;
    const acc = await c.createVM({ name: "slow" }, { wait: false });
    const started = Date.now();
    const err = await c.wait(acc.operation.id, { timeoutMs: 200 }).catch((e: unknown) => e);
    assert.ok(err instanceof WaitTimeout, String(err));
    assert.equal(err.operationId, acc.operation.id);
    assert.ok(Date.now() - started < 1500, "wait overran its budget");
    const done = await c.wait(acc.operation.id);
    assert.equal(done.state, "succeeded");
    assert.equal((await c.getVM("slow")).state, "ready");
    await c.deleteVM("slow");
  });
});

describe("capacity reclaim (fake runtime)", { skip: SKIP, timeout: 60_000 }, () => {
  let d: Daemon;
  before(async () => {
    d = await startDaemon();
  });
  after(async () => {
    await d?.stop();
  });

  test("reclaimVMs lists (dry run) and stops idle VMs by label; capacityWaitSeconds is accepted", async () => {
    const c = d.client;
    const idle = await c.createVM({ name: "idle", labels: { team: "a" } }, { capacityWaitSeconds: 5 }); // room: no wait
    await c.createVM({ name: "other", labels: { team: "b" } });

    const listed = await c.reclaimVMs(0, { labels: { team: "a" }, dryRun: true });
    assert.deepEqual(listed.items.map((i) => [i.vmId, i.name, i.operationId]), [[idle.id, "idle", undefined]]);
    assert.equal(listed.items[0]!.memoryBytes, idle.shape.memoryBytes);
    assert.equal((await c.getVM("idle")).state, "ready"); // a dry run stops nothing
    assert.deepEqual((await c.reclaimVMs(3600, { dryRun: true })).items, []);
    await assert.rejects(c.reclaimVMs(-1), InvalidArgument);

    const stopped = await c.reclaimVMs(0, { labels: { team: "a" } });
    assert.equal(stopped.items.length, 1);
    const item = stopped.items[0]!;
    assert.equal(item.vmId, idle.id);
    assert.ok(item.operationId);
    const op = await c.wait(item.operationId);
    assert.equal(op.state, "succeeded");
    assert.equal(op.reason, "gc");
    assert.equal((await c.getVM("idle")).state, "stopped");
    assert.equal((await c.getVM("other")).state, "ready"); // other labels: kept
    assert.equal((await c.resumeVM("idle", { capacityWaitSeconds: 5 })).state, "ready");
  });
});

describe("egress policy (fake runtime)", { skip: SKIP, timeout: 60_000 }, () => {
  let d: Daemon;
  before(async () => {
    d = await startDaemon();
  });
  after(async () => {
    await d?.stop();
  });

  test("create normalizes, fork inherits or overrides, set is live and kept, bad input is InvalidArgument", async () => {
    const c = d.client;
    assert.deepEqual((await c.createVM({ name: "plain" })).egress, { mode: "filtered" }); // host default

    const web = await c.createVM({
      name: "web",
      egress: { mode: "allowlist", allow: ["GitHub.com", "10.9.1.2/16", "github.com", "*.Example.com"] },
    });
    const normalized = { mode: "allowlist", allow: ["*.example.com", "10.9.0.0/16", "github.com"] };
    assert.deepEqual(web.egress, normalized);

    await c.stopVM("web");
    assert.deepEqual((await c.forkVM("web", { name: "web-fork" })).egress, normalized); // inherited
    assert.deepEqual((await c.forkVM("web", { name: "web-off", egress: { mode: "off" } })).egress, { mode: "off" });

    const off = await c.setVMEgress("web", { mode: "off" }); // stopped: kept for the next boot
    assert.equal(off.id, web.id);
    assert.deepEqual(off.egress, { mode: "off" });
    assert.deepEqual((await c.resumeVM("web")).egress, { mode: "off" });
    const back = await c.setVMEgress("web", { mode: "allowlist", allow: ["github.com", "*.example.com"] }); // running: live
    assert.deepEqual(back.egress, { mode: "allowlist", allow: ["*.example.com", "github.com"] });
    assert.deepEqual((await c.getVM("web")).egress, back.egress);

    const err = await c.setVMEgress("web", { mode: "allowlist", allow: ["not a domain!"] }).catch((e: unknown) => e);
    assert.ok(err instanceof InvalidArgument, String(err));
    assert.equal(err.status, 400);
    await assert.rejects(c.setVMEgress("web", { mode: "off", allow: ["github.com"] }), InvalidArgument);
    await assert.rejects(c.createVM({ name: "bad", egress: { mode: "allowlist", allow: ["300.1.1.1/8"] } }), InvalidArgument);
    await assert.rejects(c.setVMEgress("nope", { mode: "off" }), NotFound);
    assert.deepEqual((await c.getVM("web")).egress, back.egress); // rejected calls changed nothing
  });
});

describe("processes (fake runtime with in-process guest agents)", { skip: SKIP, timeout: 120_000 }, () => {
  let d: Daemon;
  let c: DavitClient;
  before(async () => {
    d = await startDaemon({ guestAgent: true });
    c = d.client;
    await c.createVM({ name: "box" });
  });
  after(async () => {
    await d?.stop();
  });

  const until = async (pred: () => Promise<boolean>, what: string) => {
    const deadline = Date.now() + 10_000;
    while (!(await pred())) {
      if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
      await new Promise((r) => setTimeout(r, 50));
    }
  };

  test("run collects stdout, stderr and the exit status", async () => {
    const r = await c.run("box", ["sh", "-c", "echo hi; echo err >&2; exit 3"]);
    assert.equal(r.exitCode, 3);
    assert.equal(r.stdoutText, "hi\n");
    assert.equal(r.stderrText, "err\n");
    assert.equal(r.process.status, "exited");
    assert.equal(r.process.exitCode, 3);
  });

  test("run with input, env and cwd", async () => {
    const r = await c.run("box", ["sh", "-c", 'cat; printf "%s %s" "$GREETING" "$(pwd)" >&2'], {
      input: "from stdin\n",
      env: { GREETING: "hello" },
      cwd: realpathSync(tmpdir()),
    });
    assert.equal(r.exitCode, 0);
    assert.equal(r.stdoutText, "from stdin\n");
    assert.equal(r.stderrText, `hello ${realpathSync(tmpdir())}`);
  });

  test("run honours the deadline (exit 124)", async () => {
    const started = Date.now();
    const r = await c.run("box", ["sleep", "30"], { deadlineSeconds: 1 });
    assert.equal(r.exitCode, 124);
    assert.ok(r.timedOut);
    assert.ok(Date.now() - started < 10_000);
  });

  test("a stream signal interrupts the process (exit 130)", async () => {
    const p = await c.execProcess("box", ["sleep", "30"]);
    const s = await c.attachProcess("box", p.id);
    s.signal("INT");
    let exit: number | undefined;
    for await (const ev of s) if (ev.type === "exit") exit = processExitCode(ev.process);
    assert.equal(exit, 130);
  });

  test("exec idempotency: same key returns the same process", async () => {
    const key = newIdempotencyKey();
    const a = await c.execProcess("box", ["true"], { idempotencyKey: key, detached: true });
    const b = await c.execProcess("box", ["true"], { idempotencyKey: key, detached: true });
    assert.equal(a.id, b.id);
    await assert.rejects(c.execProcess("box", ["false"], { idempotencyKey: key, detached: true }), IdempotencyConflict);
  });

  test("detached exec, list, get, kill", async () => {
    const p = await c.execProcess("box", ["sleep", "30"], { detached: true });
    assert.equal(p.detached, true);
    const ids: string[] = [];
    for await (const x of c.iterProcesses("box")) ids.push(x.id);
    assert.ok(ids.includes(p.id));
    assert.equal((await c.getProcess("box", p.id)).status, "running");
    await c.killProcess("box", p.id, "TERM");
    await until(async () => (await c.getProcess("box", p.id)).status !== "running", "the kill");
    const done = await c.getProcess("box", p.id);
    assert.equal(done.status, "killed");
    assert.equal(done.signal, "TERM");
    await assert.rejects(c.killProcess("box", p.id), InvalidState);
  });

  test("attach resumes from a cursor without duplicates", async () => {
    const p = await c.execProcess("box", ["sh", "-c", "echo a; sleep 0.2; echo b; sleep 0.2; echo c"], { detached: true });
    await until(async () => (await c.getProcess("box", p.id)).status === "exited", "the process to exit");
    const collect = async (outputCursor: number) => {
      const out: Array<[number, string]> = [];
      for await (const ev of await c.attachProcess("box", p.id, { outputCursor })) {
        if (ev.type !== "exit") out.push([ev.seq, Buffer.from(ev.data).toString()]);
      }
      return out;
    };
    const all = await collect(0);
    assert.equal(all.map(([, t]) => t).join(""), "a\nb\nc\n");
    assert.ok(all.length >= 2, "expected several chunks");
    const firstSeq = all[0]![0];
    const rest = await collect(firstSeq);
    assert.deepEqual(rest, all.slice(1));
  });

  test("issueSSHCredential signs a caller key; host keys match the pinned fingerprints", async () => {
    // OpenSSH ed25519 public key: string "ssh-ed25519" + string <32-byte key>.
    const raw = generateKeyPairSync("ed25519").publicKey.export({ format: "der", type: "spki" }).subarray(-32);
    const str = (b: Buffer) => {
      const len = Buffer.alloc(4);
      len.writeUInt32BE(b.length, 0);
      return Buffer.concat([len, b]);
    };
    const blob = Buffer.concat([str(Buffer.from("ssh-ed25519")), str(raw)]);
    const cred = await c.issueSSHCredential("box", { publicKey: `ssh-ed25519 ${blob.toString("base64")} sdk-test`, ttlSeconds: 60 });
    assert.ok(cred.certificate.startsWith("ssh-ed25519-cert-v01@openssh.com "));
    assert.ok(cred.hostKeys.length > 0);
    const pinned = (await c.getVM("box")).endpoint.sshHostKeyFingerprints ?? [];
    for (const k of cred.hostKeys) {
      const fp = "SHA256:" + createHash("sha256").update(Buffer.from(k.split(" ")[1]!, "base64")).digest("base64").replace(/=+$/, "");
      assert.ok(pinned.includes(fp), `${fp} not in ${JSON.stringify(pinned)}`);
    }
    await assert.rejects(c.issueSSHCredential("box", { publicKey: "not a key" }), InvalidArgument);
  });

  test("attach to an unknown process is NotFound", async () => {
    await assert.rejects(c.attachProcess("box", "proc_nope"), NotFound);
  });

  test("tunnel round trip with half-close; a closed port is Unavailable", async () => {
    // The fake guest's loopback is the host's: tunnel to a local echo server.
    const echo = createServer({ allowHalfOpen: true }, (s) => s.pipe(s));
    await new Promise<void>((r) => echo.listen(0, "127.0.0.1", r));
    const port = (echo.address() as AddressInfo).port;
    try {
      const t = await c.connectTunnel("box", port);
      assert.equal(t.tunnel?.port, port);
      const payload = randomBytes(200_000);
      await t.write(payload);
      t.closeWrite();
      const got: Uint8Array[] = [];
      for await (const chunk of t) got.push(chunk); // ends when the echo server ends its side
      assert.ok(Buffer.concat(got).equals(payload));

      const idle = await c.openTunnel("box", port);
      await c.closeTunnel(idle);
      await assert.rejects(c.attachTunnel(idle), NotFound); // closed (and single use)
    } finally {
      await new Promise((r) => echo.close(r));
    }
    const unused = createServer();
    await new Promise<void>((r) => unused.listen(0, "127.0.0.1", r));
    const closed = (unused.address() as AddressInfo).port;
    await new Promise((r) => unused.close(r));
    await assert.rejects(c.openTunnel("box", closed), Unavailable);
  });

  test("reverse forward round trip with half-close; revoke cuts the live connection", async (t) => {
    const feature = (await c.getCapabilities()).features.find((f) => f.name === "reverseForward");
    if (feature?.status !== "available") return t.skip(`reverseForward is ${feature?.status ?? "missing"} on this daemon`);
    const echo = createServer({ allowHalfOpen: true }, (s) => s.pipe(s));
    await new Promise<void>((r) => echo.listen(0, "127.0.0.1", r));
    const echoPort = (echo.address() as AddressInfo).port;
    const unused = createServer();
    await new Promise<void>((r) => unused.listen(0, "127.0.0.1", r));
    const guestPort = (unused.address() as AddressInfo).port;
    await new Promise((r) => unused.close(r));
    // A "guest process": the fake guest's loopback is the host's.
    const dialGuest = () =>
      new Promise<Socket>((resolve, reject) => {
        const s = connect({ host: "127.0.0.1", port: guestPort, allowHalfOpen: true }, () => resolve(s));
        s.once("error", reject);
      });
    const session = await c.reverseForward("box", guestPort, "127.0.0.1", echoPort);
    try {
      assert.equal(session.forward.guestPort, guestPort);
      assert.deepEqual(session.forward.target, { kind: "session", address: `127.0.0.1:${echoPort}` });
      assert.deepEqual((await c.listReverseForwards("box")).items.map((f) => f.id), [session.forward.id]);

      const payload = randomBytes(200_000);
      const g = await dialGuest();
      const got: Buffer[] = [];
      const ended = new Promise<void>((r) => g.once("end", r));
      g.on("data", (b: Buffer) => got.push(b));
      g.end(payload); // the half-close reaches the echo server; its EOF comes back
      await ended;
      g.destroy();
      assert.ok(Buffer.concat(got).equals(payload));

      const live = await dialGuest();
      const cut = new Promise<void>((r) => {
        live.once("end", r); // the daemon ends (or resets) the guest side
        live.once("close", () => r());
      });
      live.on("error", () => {});
      const echoed = new Promise<Buffer>((r) => live.once("data", r));
      live.write("ping");
      assert.equal((await echoed).toString(), "ping");
      await c.revokeReverseForward("box", session.forward);
      await cut; // the revoke cut the live connection
      live.destroy();
      const reason = await session.closed;
      assert.ok(reason.length > 0);
      assert.equal(session.error, undefined);
      assert.deepEqual((await c.listReverseForwards("box")).items, []);
    } finally {
      session.close();
      await new Promise((r) => echo.close(r));
    }
  });
});

describe("files (fake runtime with in-process guest agents)", { skip: SKIP, timeout: 120_000 }, () => {
  let d: Daemon;
  let c: DavitClient;
  let guest: string;
  let vm: VM;
  before(async () => {
    d = await startDaemon({ guestAgent: true });
    c = d.client;
    vm = await c.createVM({ name: "files" });
    guest = `${d.dir}/guest`;
    mkdirSync(guest);
  });
  after(async () => {
    await d?.stop();
  });

  test("write bytes, read them back with the mode", async () => {
    const data = new Uint8Array(randomBytes(100_003));
    const info = await c.writeFile("files", `${guest}/blob.bin`, data, { mode: 0o640 });
    assert.equal(info.sizeBytes, data.length);
    assert.equal(info.mode, "0640");
    assert.equal(info.sha256, createHash("sha256").update(data).digest("hex"));
    assert.equal(info.generation, vm.instanceGeneration);
    assert.equal(statSync(`${guest}/blob.bin`).mode & 0o777, 0o640);
    const back = await c.readFile("files", `${guest}/blob.bin`, { generation: vm.instanceGeneration });
    assert.deepEqual(back.data, data);
    assert.equal(back.mode, "0640");
    await c.writeFile("files", `${guest}/note.txt`, "héllo\n");
    assert.equal(readFileSync(`${guest}/note.txt`, "utf8"), "héllo\n");
  });

  test("a missing path is NotFound; a stale generation is InvalidState", async () => {
    await assert.rejects(c.readFile("files", `${guest}/missing`), NotFound);
    await assert.rejects(c.readFile("files", `${guest}/blob.bin`, { generation: vm.instanceGeneration + 1 }), InvalidState);
    await assert.rejects(c.writeFile("files", `${guest}/x`, "x", { generation: vm.instanceGeneration + 1 }), InvalidState);
  });

  test("archive round trip of a small directory", async () => {
    const tar = buildTar([
      { name: "bin/", type: "5", mode: 0o755 },
      { name: "bin/tool", data: "#!/bin/sh\necho hi\n", mode: 0o755 },
      { name: "notes.txt", data: "hi", mode: 0o600 },
      { name: "tool", type: "2", linkname: "bin/tool" },
    ]);
    const info = await c.writeFile("files", `${guest}/tree`, tar, { archive: true, mode: 0o750 });
    assert.equal(info.archive, true);
    assert.equal(readFileSync(`${guest}/tree/notes.txt`, "utf8"), "hi");
    assert.equal(statSync(`${guest}/tree/bin/tool`).mode & 0o777, 0o755);
    assert.equal(statSync(`${guest}/tree`).mode & 0o777, 0o750);
    assert.ok(lstatSync(`${guest}/tree/tool`).isSymbolicLink());
    assert.equal(readlinkSync(`${guest}/tree/tool`), "bin/tool");

    const back = await c.readFile("files", `${guest}/tree`, { archive: true }); // end marker verified by the SDK
    assert.equal(back.mode, "0750");
    const text = Buffer.from(back.data).toString("latin1");
    for (const name of ["notes.txt", "bin/tool", "tool"]) assert.ok(text.includes(name), name);
    assert.ok(back.data.length % 512 === 0 && back.data.subarray(-1024).every((b) => b === 0));
  });

  test("streamed upload and streamed read of a few MiB", async () => {
    const size = 6 * 1024 * 1024 + 123;
    const sent = createHash("sha256");
    async function* chunks() {
      for (let off = 0; off < size; off += 1 << 20) {
        const b = new Uint8Array(randomBytes(Math.min(1 << 20, size - off)));
        sent.update(b);
        yield b;
      }
    }
    const info = await c.writeFile("files", `${guest}/big.bin`, chunks());
    const sentHex = sent.digest("hex");
    assert.equal(info.sizeBytes, size);
    assert.equal(info.sha256, sentHex);

    const s = await c.readFileStream("files", `${guest}/big.bin`);
    assert.equal(s.size, size);
    const got = createHash("sha256");
    let chunksSeen = 0;
    for await (const chunk of s) {
      got.update(chunk);
      chunksSeen++;
    }
    assert.equal(s.bytesRead, size);
    assert.equal(got.digest("hex"), sentHex);
    assert.ok(chunksSeen > 1, "expected the body to arrive in several chunks");
  });
});

describe("prompts (fake runtime, guest agents, fake claude harness)", { skip: SKIP, timeout: 120_000 }, () => {
  const fakeClaude = fileURLToPath(new URL("./fixtures/fake-claude", import.meta.url));
  let d: Daemon;
  let c: DavitClient;
  before(async () => {
    d = await startDaemon({ guestAgent: true }, { harness: { commands: { claude: fakeClaude } } });
    c = d.client;
    await c.createVM({ name: "agent" });
  });
  after(async () => {
    await d?.stop();
  });

  let conversationId = "";
  /** runPrompt, checking that onEvent saw exactly the events it returns. */
  const turn = async (text: string, opts: PromptOptions = {}) => {
    const seen: PromptEvent[] = [];
    const r = await c.runPrompt("agent", text, { ...opts, onEvent: (e) => seen.push(e) });
    assert.deepEqual(seen, r.events);
    return r;
  };

  test("prompt runs a turn and returns events, text and the final prompt", async () => {
    const r = await turn("hello");
    assert.equal(r.prompt.state, "succeeded");
    assert.equal(r.prompt.harness, "claude");
    assert.match(r.text, /echo: hello resumed=none/);
    assert.equal(r.prompt.usage?.inputTokens, 3);
    assert.equal(r.prompt.usage?.outputTokens, 4);
    assert.equal(r.events[0]?.type, "conversation.started");
    assert.equal(r.events.at(-1)?.type, "turn.completed");
    const call = r.events.find((e) => e.type === "tool.call");
    assert.equal(call?.toolName, "Bash");
    assert.deepEqual(call?.input, { command: "ls" }); // untyped `{}` schema: any JSON
    for (let i = 1; i < r.events.length; i++) assert.ok(r.events[i]!.seq > r.events[i - 1]!.seq);
    conversationId = r.prompt.conversationId;

    const again = await c.listPromptEvents("agent", r.prompt.id);
    assert.deepEqual(again.items.map((e) => e.seq), r.events.map((e) => e.seq));
    assert.equal((await c.getPrompt("agent", r.prompt.id)).state, "succeeded");
  });

  test("a later turn resumes the conversation; env reaches the harness", async () => {
    const r = await turn("again", { conversationId, env: { FAKE_TOKEN: "tok-1" } });
    assert.equal(r.prompt.conversationId, conversationId);
    assert.match(r.text, /resumed=sess-\d+ token=tok-1/);
    const conv = await c.getConversation("agent", conversationId);
    assert.equal(conv.turns, 2);
    assert.ok(conv.sessionId?.startsWith("sess-"));
    const list = await c.listConversations("agent", { limit: 10 });
    assert.equal(list.items[0]?.id, conversationId, "oldest first");
  });

  test("a failed turn is returned, not thrown", async () => {
    const r = await turn("please fail", { new: true });
    assert.equal(r.prompt.state, "failed");
    assert.equal(r.prompt.error?.code, "harness_failed");
    assert.notEqual(r.prompt.conversationId, conversationId);
  });

  test("one turn at a time; follow budget; cancel", async () => {
    const slow = await c.createPrompt("agent", "slow", { conversationId });
    assert.ok(["starting", "running"].includes(slow.state));
    await assert.rejects(c.createPrompt("agent", "too soon", { conversationId }), OperationInProgress);

    const started = Date.now();
    const err = await c.waitPrompt("agent", slow.id, { timeoutMs: 300 }).catch((e: unknown) => e);
    assert.ok(err instanceof WaitTimeout, String(err));
    assert.equal(err.operationId, undefined);
    assert.equal(err.details.promptId, slow.id);
    assert.equal(typeof err.details.cursor, "number");
    assert.ok(Date.now() - started < 2000);
    assert.ok(["starting", "running"].includes((await c.getPrompt("agent", slow.id)).state), "a client timeout must not cancel the turn");

    // Cancel (retry while the harness process is still starting).
    for (let i = 0; ; i++) {
      try {
        await c.cancelPrompt("agent", slow.id);
        break;
      } catch (e) {
        if (!(e instanceof OperationInProgress) || i > 100) throw e;
        await new Promise((r) => setTimeout(r, 50));
      }
    }
    const types: string[] = [];
    for await (const e of c.followPrompt("agent", slow.id)) types.push(e.type);
    assert.equal(types.at(-1), "turn.completed");
    const done = await c.getPrompt("agent", slow.id);
    assert.equal(done.state, "cancelled");
    await assert.rejects(c.cancelPrompt("agent", slow.id), InvalidState);
  });

  test("createPrompt is idempotent per key: one turn; a different body conflicts", async () => {
    const key = newIdempotencyKey();
    const a = await c.createPrompt("agent", "idem turn", { new: true, idempotencyKey: key });
    const b = await c.createPrompt("agent", "idem turn", { new: true, idempotencyKey: key });
    assert.equal(b.id, a.id);
    assert.equal(b.conversationId, a.conversationId);
    await assert.rejects(c.createPrompt("agent", "other text", { new: true, idempotencyKey: key }), IdempotencyConflict);
    const done = await c.waitPrompt("agent", a.id);
    assert.equal(done.state, "succeeded");
    const events = await c.listPromptEvents("agent", a.id);
    assert.equal(events.items.filter((e) => e.type === "turn.completed").length, 1);
    assert.equal(events.items.filter((e) => e.type === "conversation.started").length, 1);
    assert.equal((await c.getConversation("agent", a.conversationId)).turns, 1, "only one turn ran");
    // A replay after the turn finished still returns that turn.
    assert.equal((await c.createPrompt("agent", "idem turn", { new: true, idempotencyKey: key })).id, a.id);
  });

  test("conversations page oldest first with limit and cursor", async () => {
    await c.createVM({ name: "pager" });
    const first = await c.runPrompt("pager", "one", { new: true });
    const second = await c.runPrompt("pager", "two", { new: true });
    const p1 = await c.listConversations("pager", { limit: 1 });
    assert.deepEqual(p1.items.map((x) => x.id), [first.prompt.conversationId]);
    assert.ok(p1.nextCursor, "expected a nextCursor after the first page");
    const p2 = await c.listConversations("pager", { limit: 1, cursor: p1.nextCursor });
    assert.deepEqual(p2.items.map((x) => x.id), [second.prompt.conversationId]);
    if (p2.nextCursor) {
      const p3 = await c.listConversations("pager", { limit: 1, cursor: p2.nextCursor });
      assert.deepEqual(p3.items, []);
      assert.ok(!p3.nextCursor);
    }
    const all: string[] = [];
    for await (const conv of c.iterConversations("pager", { limit: 1 })) all.push(conv.id);
    assert.deepEqual(all, [first.prompt.conversationId, second.prompt.conversationId]);
  });

  /** Retries `fn` while the turn is still starting (OperationInProgress). */
  const whileStarting = async <T>(fn: () => Promise<T>): Promise<T> => {
    for (let i = 0; ; i++) {
      try {
        return await fn();
      } catch (e) {
        if (!(e instanceof OperationInProgress) || i > 100) throw e;
        await new Promise((r) => setTimeout(r, 50));
      }
    }
  };

  test("the VM prompt feed replays, pages by nextCursor, filters and follows", async () => {
    await c.createVM({ name: "feed" });
    const one = await c.runPrompt("feed", "first", { new: true });
    const two = await c.runPrompt("feed", "second", { new: true });
    const all = await c.listVMPromptEvents("feed");
    assert.deepEqual(all.items.map((i) => i.event.seq), [...one.events, ...two.events].map((e) => e.seq));
    assert.ok(all.nextCursor, "nextCursor is set");

    // Page with a small limit, resuming from nextCursor each time.
    const paged: string[] = [];
    let after: string | undefined;
    for (let n = 0; n < 50; n++) {
      const page = await c.listVMPromptEvents("feed", { after, limit: 3 });
      assert.ok(page.items.length <= 3);
      assert.ok(page.nextCursor, "nextCursor is set on every page, also an empty one");
      paged.push(...page.items.map((i) => i.cursor));
      after = page.nextCursor;
      if (page.items.length === 0) break;
    }
    assert.deepEqual(paged, all.items.map((i) => i.cursor));
    const empty = await c.listVMPromptEvents("feed", { after: all.nextCursor });
    assert.deepEqual(empty.items, []);
    assert.ok(empty.nextCursor);

    const only = await c.listVMPromptEvents("feed", { conversationId: two.prompt.conversationId });
    assert.deepEqual(only.items.map((i) => i.event.seq), two.events.map((e) => e.seq));

    // Follow from the end: a new turn's events arrive, ending with turn.completed.
    const third = await c.createPrompt("feed", "third", { conversationId: one.prompt.conversationId });
    const followed: string[] = [];
    for await (const item of c.followVMPromptEvents("feed", { after: all.nextCursor })) {
      assert.equal(item.event.promptId, third.id);
      followed.push(item.event.type);
      if (item.event.type === "turn.completed") break;
    }
    assert.ok(followed.includes("message.delta"));
  });

  test("interruptPrompts all cancels a slow turn; a repeat cancels nothing", async () => {
    await assert.rejects(async () => c.interruptPrompts("agent", {}), TypeError);
    const slow = await c.createPrompt("agent", "slow interrupt", { new: true });
    const res = await whileStarting(() => c.interruptPrompts("agent", { all: true }));
    assert.deepEqual(res.cancelled.map((p) => p.id), [slow.id]);
    assert.equal((await c.waitPrompt("agent", slow.id)).state, "cancelled");
    assert.deepEqual((await c.interruptPrompts("agent", { all: true })).cancelled, []);
    assert.deepEqual((await c.interruptPrompts("agent", { conversationId: slow.conversationId })).cancelled, []);
  });

  test("steerPrompt reaches a running claude turn natively", async () => {
    const slow = await c.createPrompt("agent", "slow steer", { new: true });
    const res = await whileStarting(() => c.steerPrompt("agent", slow.id, "also this"));
    assert.equal(res.delivery, "native");
    assert.equal(res.prompt.id, slow.id);
    const events: PromptEvent[] = [];
    const done = await c.waitPrompt("agent", slow.id, { onEvent: (e) => events.push(e) });
    assert.equal(done.state, "succeeded");
    const text = events.filter((e) => e.type === "message.delta").map((e) => e.text ?? "").join("");
    assert.match(text, /echo: also this/);
    await assert.rejects(c.steerPrompt("agent", slow.id, "too late"), InvalidState);
  });

  test("attachments, reasoning effort and fast mode reach the harness", async () => {
    const caps = await c.getCapabilities();
    const claude = caps.harnesses.find((h) => h.name === "claude");
    assert.ok(claude?.attachmentTypes.includes("text/plain"));
    const r = await turn("look at the note", {
      new: true,
      attachments: [{ name: "note.txt", mediaType: "text/plain", data: Buffer.from("hello note\n").toString("base64") }],
      reasoningEffort: "high",
      fast: true,
      turnTimeoutSeconds: 600,
    });
    assert.equal(r.prompt.state, "succeeded", JSON.stringify(r.prompt.error));
    assert.match(r.text, /effort=high fast=yes/);
    assert.ok(r.prompt.turnDeadlineAt, "turnDeadlineAt is set");
  });

  test("unknown prompt, conversation and VM are NotFound", async () => {
    await assert.rejects(c.getPrompt("agent", "prm_nope"), NotFound);
    await assert.rejects(c.getConversation("agent", "conv_nope"), NotFound);
    await assert.rejects(c.createPrompt("ghost", "hi"), NotFound);
  });
});

describe("v0 polish: profiles, updateVM, extendVM, renameVM, process paging", { skip: SKIP, timeout: 120_000 }, () => {
  let d: Daemon;
  let c: DavitClient;
  before(async () => {
    d = await startDaemon({ guestAgent: true }, { profiles: [{ name: "big", shape: { cpus: 2, memoryBytes: 1073741824, diskBytes: 0 } }] });
    c = d.client;
  });
  after(async () => {
    await d?.stop();
  });

  test("capabilities list profiles and the TTL warning", async () => {
    const caps = await c.getCapabilities();
    const big = caps.profiles.find((p) => p.name === "big");
    assert.equal(big?.shape.cpus, 2);
    assert.equal(big?.shape.memoryBytes, 1073741824);
    assert.equal(typeof caps.ttlWarningSeconds, "number");
  });

  test("create and fork with a type", async () => {
    const vm = await c.createVM({ name: "big", type: "big" });
    assert.equal(vm.shape.cpus, 2);
    assert.equal(vm.shape.memoryBytes, 1073741824);
    await c.stopVM("big");
    const f = await c.forkVM("big", { name: "big-fork", type: "big", shape: { cpus: 1 } });
    assert.equal(f.shape.cpus, 1, "shape fields override the profile");
    assert.equal(f.shape.memoryBytes, 1073741824);
    await assert.rejects(c.createVM({ name: "nope", type: "no-such-profile" }), InvalidArgument);
  });

  test("extendVM restarts the countdown; a TTL sets it; warning precedes expiry", async () => {
    const vm = await c.resumeVM("big");
    assert.ok(vm.expiresAt, "a ready VM with auto-stop has expiresAt");
    await new Promise((r) => setTimeout(r, 1100));
    const ext = await c.extendVM("big");
    assert.ok(Date.parse(ext.expiresAt!) > Date.parse(vm.expiresAt!), "extend restarts the countdown from now");
    assert.equal(ext.autoStop, true);
    const before = Date.now();
    const longer = await c.extendVM("big", { ttlSeconds: 7200 });
    assert.equal(longer.ttlSeconds, 7200);
    const exp = Date.parse(longer.expiresAt!);
    assert.ok(exp >= before + 7_190_000 && exp <= Date.now() + 7_210_000, `expiresAt ${longer.expiresAt}`);
    const caps = await c.getCapabilities();
    assert.ok(longer.autoStopWarningAt, "autoStopWarningAt is set while an auto-stop is pending");
    assert.equal(exp - Date.parse(longer.autoStopWarningAt), caps.ttlWarningSeconds * 1000);
  });

  test("rename; a stale If-Match is RevisionMismatch", async () => {
    const cur = await c.getVM("big");
    await assert.rejects(c.renameVM("big", "huge", { ifMatch: cur.stateRevision + 1000 }), RevisionMismatch);
    const renamed = await c.renameVM("big", "huge", { ifMatch: cur.stateRevision });
    assert.equal(renamed.name, "huge");
    assert.equal(renamed.id, cur.id);
    assert.equal((await c.getVM("huge")).id, cur.id);
    await assert.rejects(c.getVM("big"), NotFound);
    const again = await c.updateVM("huge", { name: "huge" }); // naturally idempotent
    assert.equal(again.name, "huge");
  });

  test("listProcesses pages oldest first", async () => {
    const started: string[] = [];
    for (let i = 0; i < 3; i++) started.push((await c.execProcess("huge", ["true"], { detached: true })).id);
    const p1 = await c.listProcesses("huge", { limit: 2 });
    assert.deepEqual(p1.items.map((p) => p.id), started.slice(0, 2));
    assert.ok(p1.nextCursor);
    const p2 = await c.listProcesses("huge", { limit: 2, cursor: p1.nextCursor });
    assert.deepEqual(p2.items.map((p) => p.id), started.slice(2));
    const all: string[] = [];
    for await (const p of c.iterProcesses("huge", { limit: 1 })) all.push(p.id);
    assert.deepEqual(all, started);
  });
});

describe("environments (fake runtime)", { skip: SKIP, timeout: 60_000 }, () => {
  let d: Daemon;
  let c: DavitClient;
  before(async () => {
    d = await startDaemon();
    c = d.client;
  });
  after(async () => {
    await d?.stop();
  });

  test("create, revise with ifMatch (stale: RevisionMismatch), list revisions, delete", async () => {
    const manifest = { repos: [], variables: { A: "1" }, secretFiles: [], setup: [{ name: "hello", argv: ["true"] }] };
    const env = await c.createEnvironment("dev", manifest);
    assert.equal(env.name, "dev");
    assert.equal(env.latestRevision, 1);
    assert.match(env.id, /^env_/);
    assert.equal((await c.createEnvironment("dev", manifest)).id, env.id, "same name + manifest replays");
    await assert.rejects(c.createEnvironment("dev", { variables: { A: "other" } }), InvalidArgument);
    assert.equal((await c.getEnvironment(env.id)).name, "dev");
    const ids: string[] = [];
    for await (const e of c.iterEnvironments({ limit: 1 })) ids.push(e.id);
    assert.deepEqual(ids, [env.id]);

    const r2 = await c.createEnvironmentRevision("dev", { variables: { A: "2" } }, { ifMatch: 1 });
    assert.equal(r2.revision, 2);
    assert.deepEqual(r2.manifest.variables, { A: "2" });
    const stale = await c.createEnvironmentRevision("dev", { variables: { A: "3" } }, { ifMatch: 1 }).catch((e: unknown) => e);
    assert.ok(stale instanceof RevisionMismatch, String(stale));
    assert.equal(stale.status, 412);
    assert.equal(stale.details.latestRevision, 2);

    const key = newIdempotencyKey();
    const r3 = await c.createEnvironmentRevision(env.id, { setup: [] }, { ifMatch: 2, idempotencyKey: key });
    const again = await c.createEnvironmentRevision("dev", { setup: [] }, { idempotencyKey: key });
    assert.deepEqual([r3.revision, again.revision], [3, 3], "the key replays the revision it minted");

    const revs: number[] = [];
    for await (const r of c.iterEnvironmentRevisions("dev", { limit: 2 })) revs.push(r.revision);
    assert.deepEqual(revs, [1, 2, 3]);
    assert.equal((await c.getEnvironmentRevision("dev", 1)).manifest.setup[0]?.name, "hello");
    assert.equal((await c.getEnvironment("dev")).latestRevision, 3);

    // The test daemon has no workspace preparer: pinning an environment is a typed 501.
    const nie = await c.createVM({ name: "ws", environment: "dev@2" }).catch((e: unknown) => e);
    assert.ok(nie instanceof ServerNotImplemented, String(nie));
    assert.equal(nie.status, 501);

    await c.deleteEnvironment("dev");
    await assert.rejects(c.getEnvironment("dev"), NotFound);
    await assert.rejects(c.deleteEnvironment(env), NotFound);
    assert.equal((await c.listEnvironments()).items.length, 0);
  });
});

describe("job history (fake runtime with in-process guest agents)", { skip: SKIP, timeout: 60_000 }, () => {
  let d: Daemon;
  let c: DavitClient;
  before(async () => {
    d = await startDaemon({ guestAgent: true });
    c = d.client;
  });
  after(async () => {
    await d?.stop();
  });

  test("a detached job's output is readable while the VM is stopped", async () => {
    await c.createVM({ name: "jobs" });
    const p = await c.execProcess("jobs", ["sh", "-c", "echo hi"], { detached: true });
    const deadline = Date.now() + 30_000;
    while (["starting", "running"].includes((await c.getProcess("jobs", p.id)).status)) {
      if (Date.now() > deadline) throw new Error("job never exited");
      await new Promise((r) => setTimeout(r, 50));
    }
    assert.equal((await c.stopVM("jobs")).state, "stopped");

    const entries = [];
    for await (const e of c.iterProcessHistory("jobs", { limit: 1 })) if (e.process.id === p.id) entries.push(e);
    assert.equal(entries.length, 1);
    assert.equal(entries[0]!.process.status, "exited");
    assert.deepEqual([entries[0]!.stdoutBytes, entries[0]!.stderrBytes, entries[0]!.outputDropped], [3, 0, false]);

    const out = await c.collectProcessOutput("jobs", p.id);
    assert.equal(Buffer.from(out.stdout).toString(), "hi\n");
    assert.equal(out.stderr.length, 0);
    assert.equal(out.complete, true);
    assert.ok(out.cursor);
    const chunks: string[] = [];
    for await (const ev of c.iterProcessOutput("jobs", p.id, { stream: "stdout" })) chunks.push(`${ev.type}:${Buffer.from(ev.data).toString()}`);
    assert.equal(chunks.join(""), "stdout:hi\n");
    for await (const ev of c.iterProcessOutput("jobs", p.id, { stream: "stderr" })) assert.fail(`unexpected ${ev.type} chunk`);
    const rest = await c.readProcessOutput("jobs", p.id, { after: out.cursor });
    assert.deepEqual(rest.items, []);
    assert.equal(rest.complete, true);
    await assert.rejects(c.readProcessOutput("jobs", "proc_missing"), NotFound);
  });
});

describe("runs and push (fake runtime with in-process guest agents)", { skip: SKIP, timeout: 120_000 }, () => {
  let d: Daemon;
  let c: DavitClient;
  let tree: string; // fake guests run on the host, so a guest path is a host path
  before(async () => {
    d = await startDaemon({ guestAgent: true });
    c = d.client;
    tree = `${d.dir}/tree`;
  });
  after(async () => {
    await d?.stop();
  });

  const untilDecided = async (id: string) => {
    const deadline = Date.now() + 30_000;
    for (;;) {
      const r = await c.getRun(id);
      if (r.endedAt !== undefined || r.exitCode !== undefined || r.error !== undefined) return r;
      if (Date.now() > deadline) throw new Error(`run ${id} never decided`);
      await new Promise((res) => setTimeout(res, 50));
    }
  };

  test("a failing command ends failed with its exit code and its VM is gone", async () => {
    const r = await c.createRun({ argv: ["sh", "-c", "exit 3"] });
    assert.equal(r.state, "creating");
    await untilDecided(r.id);
    await c.cancelRun(r.id); // outcome decided: only ends the output grace
    const done = await c.waitRun(r.id, { timeoutMs: 60_000 });
    assert.equal(done.state, "failed");
    assert.equal(done.exitCode, 3);
    assert.ok(done.endedAt && done.vmId);
    assert.equal((await c.getVM(done.vmId)).state, "deleted");
    assert.equal((await c.cancelRun(r.id)).state, "failed", "repeating is harmless");
  });

  test("keep leaves the VM; pushTree mirror is NotImplemented", async () => {
    const kept = await c.waitRun(await c.createRun({ argv: ["true"], keep: true }), { timeoutMs: 60_000 });
    assert.equal(kept.state, "succeeded");
    assert.equal(kept.exitCode, 0);
    assert.ok(kept.vmId);
    assert.ok(["ready", "running"].includes((await c.getVM(kept.vmId)).state));
    await assert.rejects(c.pushTree(kept.vmId, tree, buildTar([{ name: "x", data: "x" }]), { mode: "mirror" }), ServerNotImplemented);
  });

  test("hold, pushTree, startRun: the command reads the pushed file", async () => {
    const held = await c.createRun({ argv: ["cat", "hello.txt"], hold: true, cwd: tree });
    let r = held;
    const deadline = Date.now() + 30_000;
    while ((r = await c.getRun(held.id)).state !== "waiting") {
      if (r.endedAt || Date.now() > deadline) throw new Error(`run never waited: ${JSON.stringify(r)}`);
      await new Promise((res) => setTimeout(res, 50));
    }
    const pushed = await c.pushTree(r.vmId!, tree, buildTar([{ name: "hello.txt", data: "hi\n", mode: 0o644 }]));
    assert.equal(pushed.files, 1);
    assert.equal(pushed.path, tree);
    assert.equal((await c.startRun(held)).state, "running");
    await assert.rejects(c.startRun(held), InvalidState);
    r = await untilDecided(held.id);
    assert.ok(r.processId);
    const out = await c.collectProcessOutput(r.vmId!, r.processId);
    assert.equal(Buffer.from(out.stdout).toString(), "hi\n");
    await c.cancelRun(held);
    assert.equal((await c.waitRun(held, { timeoutMs: 60_000 })).state, "succeeded");
  });

  test("runToCompletion pushes, runs, collects the output and cleans up", async () => {
    const out = await c.runToCompletion({ argv: ["sh", "-c", "cat again.txt; echo err >&2; exit 2"] }, {
      files: buildTar([{ name: "again.txt", data: "again\n" }]), filesPath: tree, timeoutMs: 60_000,
    });
    assert.equal(out.exitCode, 2);
    assert.equal(Buffer.from(out.stdout).toString(), "again\n");
    assert.equal(Buffer.from(out.stderr).toString(), "err\n");
    assert.equal(out.push?.files, 1);
    assert.equal(out.run.state, "failed");
    assert.equal((await c.getVM(out.run.vmId!)).state, "deleted");
  });
});

describe("run contract (fake runtime with in-process guest agents)", { skip: SKIP, timeout: 60_000 }, () => {
  let d: Daemon;
  let c: DavitClient;
  before(async () => {
    d = await startDaemon({ guestAgent: true });
    c = d.client;
  });
  after(async () => {
    await d?.stop();
  });

  test("outputGraceSeconds 0 ends without cancelRun; listRuns returns it with outcome and decidedAt", async () => {
    const r = await c.createRun({ argv: ["sh", "-c", "echo done"], outputGraceSeconds: 0 });
    const done = await c.waitRun(r, { timeoutMs: 60_000 }); // no cancelRun: nothing holds the VM
    assert.equal(done.state, "succeeded");
    assert.equal(done.outcome, "succeeded");
    assert.equal(done.exitCode, 0);
    assert.ok(done.decidedAt && done.endedAt);
    assert.ok(Date.parse(done.endedAt) - Date.parse(done.decidedAt) < 4000, "below the 5 s default grace");
    assert.equal((await c.getVM(done.vmId!)).state, "deleted");
    const listed = new Map<string, Run>();
    for await (const x of c.iterRuns({ limit: 1 })) listed.set(x.id, x);
    assert.equal(listed.get(r.id)?.outcome, "succeeded");
    assert.ok(listed.get(r.id)?.decidedAt);

    // A signal kill fails with no error; the helper reports 128+n and the output.
    const killed = await c.runToCompletion({ argv: ["sh", "-c", "echo out; echo err >&2; kill -KILL $$"], outputGraceSeconds: 0 }, { timeoutMs: 60_000 });
    assert.equal(Buffer.from(killed.stdout).toString(), "out\n");
    assert.equal(Buffer.from(killed.stderr).toString(), "err\n");
    assert.equal(killed.run.state, "failed");
    assert.equal(killed.run.error, undefined);
    assert.ok(killed.signal && killed.run.signal);
    assert.equal(killed.exitCode, 137);
    assert.deepEqual((await c.listRuns()).items.map((x) => x.id).slice(-2), [r.id, killed.run.id], "oldest first");
  });
});
