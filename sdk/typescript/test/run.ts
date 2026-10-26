// Test runner for `npm test`: node:test over test/**/*.test.ts with the spec
// reporter, plus two things `node --test` lacks.
//
// - Concurrency from DAVIT_TEST_CONCURRENCY (default: node's, one file
//   per CPU but one). Each file is its own process and the integration
//   files start daemons, so on a loaded or memory-starved host fewer at once
//   is what keeps the suite from being killed. `make check` passes
//   DAVIT_CHECK_PROCS.
// - A file that fails as a whole (its process exited non-zero or was
//   killed after its tests ran) is reported with the reason: node's spec
//   reporter prints only "'test failed'" under subtests that all passed. The
//   reason is the exit code or signal and any error raised after a test
//   ended (an "asynchronous activity after the test ended" diagnostic, for
//   example a socket error with no listener).

import { readdirSync } from "node:fs";
import { availableParallelism } from "node:os";
import { join, relative } from "node:path";
import { run } from "node:test";
import { spec } from "node:test/reporters";
import { fileURLToPath } from "node:url";

const dir = fileURLToPath(new URL(".", import.meta.url));
const files = readdirSync(dir, { recursive: true, encoding: "utf8" })
  .filter((f) => f.endsWith(".test.ts"))
  .map((f) => join(dir, f))
  .sort();
const want = Number(process.env.DAVIT_TEST_CONCURRENCY);
const concurrency = Number.isInteger(want) && want > 0 ? want : Math.max(1, availableParallelism() - 1);

interface FileFailure {
  exitCode?: number | null;
  signal?: string | null;
  late: string[];
}
const fileFailures = new Map<string, FileFailure>();
const late = new Map<string, string[]>();

const stream = run({ files, concurrency });
stream.on("test:fail", (data) => {
  process.exitCode = 1;
  const err = data.details.error as Error & { failureType?: string; exitCode?: number | null; signal?: string | null };
  // The file itself (nesting 0, named after it) failed, not an assertion.
  if (data.nesting === 0 && data.file && err?.failureType === "testCodeFailure" && ("exitCode" in err || "signal" in err)) {
    fileFailures.set(data.file, { exitCode: err.exitCode, signal: err.signal, late: [] });
  }
});
stream.on("test:diagnostic", (data) => {
  if (data.file && /after the test ended/.test(data.message)) {
    late.set(data.file, [...(late.get(data.file) ?? []), data.message]);
  }
});
stream.on("test:summary", (data) => {
  if (data.file === undefined && !data.success) process.exitCode = 1;
});
stream.on("end", () => {
  if (fileFailures.size === 0) return;
  const out = ["", `!!! ${fileFailures.size} test file(s) failed as a whole, not on an assertion:`];
  for (const [file, f] of fileFailures) {
    const why =
      f.signal != null
        ? `the test process was killed by ${f.signal}` + (f.signal === "SIGKILL" ? " (the OS under memory pressure, or a timeout)" : "")
        : `the test process exited with code ${f.exitCode}`;
    out.push(`!!!   ${relative(process.cwd(), file)}: ${why}`);
    for (const m of late.get(file) ?? []) out.push(`!!!     ${m}`);
  }
  out.push(`!!!   concurrency was ${concurrency} (DAVIT_TEST_CONCURRENCY); rerun with fewer, or that file alone.`);
  process.stderr.write(out.join("\n") + "\n");
});
stream.compose(spec).pipe(process.stdout);
