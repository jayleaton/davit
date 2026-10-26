// A peer that resets the connection at any point of a WebSocket's life must
// surface as a rejected promise, never as an 'error' event with no listener
// (which crashes the process). Each scenario runs in its own child process
// (fixtures/rst-client.ts), so a crash is an assertion here, not a lost file.

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { describe, test } from "node:test";
import { fileURLToPath } from "node:url";

const client = fileURLToPath(new URL("./fixtures/rst-client.ts", import.meta.url));

describe("connection resets never crash the process", () => {
  for (const scenario of ["accept", "late", "mid-head", "after-101", "after-401", "short-401"]) {
    test(scenario, () => {
      const r = spawnSync(process.execPath, [client, scenario], { encoding: "utf8", timeout: 20_000 });
      assert.equal(r.status, 0, `child crashed (status ${r.status}, signal ${r.signal}):\n${r.stderr}`);
      const settled = /^SETTLED (.*)$/m.exec(r.stdout)?.[1] ?? "";
      assert.match(settled, /TransportError|DavitError|Unauthenticated/, `outcome: ${settled}\n${r.stderr}`);
    });
  }
});
