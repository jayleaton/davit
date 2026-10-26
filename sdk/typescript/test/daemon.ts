// A real daemon (fake runtime) for integration tests.
// Binary: $DAVIT_BIN or <repo>/bin/davit (build it with `make build`).

import { spawn, type ChildProcess } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { DavitClient } from "../src/index.ts";

export const BIN = process.env.DAVIT_BIN || fileURLToPath(new URL("../../../bin/davit", import.meta.url));
export const SKIP = existsSync(BIN) ? false : `daemon binary ${BIN} not found; run \`make build\` (or set DAVIT_BIN)`;

export interface Daemon {
  client: DavitClient;
  /** The daemon's temp dir (fake guests see the host filesystem). */
  dir: string;
  /** The daemon's unix socket. */
  socket: string;
  stop(): Promise<void>;
}

const live = new Set<ChildProcess>();
process.once("exit", () => {
  for (const c of live) c.kill("SIGKILL");
});

/** Starts a daemon in a fresh short /tmp dir; stop() kills it and removes the dir. */
export async function startDaemon(fake?: Record<string, unknown>, extra: Record<string, unknown> = {}): Promise<Daemon> {
  // TMPDIR is honoured; keep it short (unix socket paths are limited to ~107 bytes).
  const tmp = mkdtempSync(join(tmpdir(), "swts-"));
  if (`${tmp}/s.sock`.length > 100) throw new Error(`TMPDIR too long for a unix socket path: ${tmp}`);
  let child: ChildProcess | undefined;
  let log = "";
  const stop = async () => {
    // DAVIT_TEST_DAEMON_LOG=1 prints each daemon's log (debugging aid).
    if (process.env.DAVIT_TEST_DAEMON_LOG) process.stderr.write(`--- daemon log ${tmp}\n${log}\n`);
    if (child && child.exitCode === null && child.signalCode === null) {
      const exited = new Promise((resolve) => child!.once("exit", resolve));
      child.kill("SIGTERM");
      const t = setTimeout(() => child!.kill("SIGKILL"), 5000);
      await exited;
      clearTimeout(t);
    }
    if (child) live.delete(child);
    rmSync(tmp, { recursive: true, force: true });
  };
  try {
    for (const d of ["data", "cfg", "run"]) mkdirSync(`${tmp}/${d}`, { mode: 0o700 });
    const socket = `${tmp}/s.sock`;
    const config: Record<string, unknown> = { dataDir: `${tmp}/data`, socket, runtime: "fake", logLevel: "warn" };
    if (fake) config.fake = fake;
    Object.assign(config, extra);
    if (process.env.DAVIT_TEST_DAEMON_LOG) config.logLevel = "debug";
    writeFileSync(`${tmp}/config.json`, JSON.stringify(config));
    child = spawn(BIN, ["daemon", "--config", `${tmp}/config.json`], {
      env: {
        PATH: process.env.PATH ?? "/usr/bin:/bin",
        HOME: tmp,
        XDG_CONFIG_HOME: `${tmp}/cfg`,
        XDG_DATA_HOME: `${tmp}/data`,
        XDG_RUNTIME_DIR: `${tmp}/run`,
        DAVIT_TS_AUTHKEY: "",
      },
      stdio: ["ignore", "pipe", "pipe"],
    });
    live.add(child);
    child.stdout!.on("data", (b: Buffer) => (log += b.toString()));
    child.stderr!.on("data", (b: Buffer) => (log += b.toString()));
    const client = new DavitClient({ socket, waitTimeoutMs: 30_000 });
    const deadline = Date.now() + 15_000;
    for (;;) {
      if (child.exitCode !== null) throw new Error(`daemon exited with ${child.exitCode}:\n${log}`);
      try {
        await client.getCapabilities();
        break;
      } catch (err) {
        if (Date.now() > deadline) throw new Error(`daemon did not accept on ${socket}: ${String(err)}\n${log}`);
        await new Promise((r) => setTimeout(r, 50));
      }
    }
    return { client, stop, dir: tmp, socket };
  } catch (err) {
    await stop();
    throw err;
  }
}
