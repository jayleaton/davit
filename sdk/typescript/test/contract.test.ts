// Contract tests: fail when api/openapi.yaml changes in a way the SDK does
// not cover.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { parse } from "yaml";

import { REGENERATE_COMMAND, SPEC_URL, OUT_URL, generate } from "../scripts/gen-schema.ts";
import { NOT_YET_SUPPORTED, OPERATIONS } from "../src/client.ts";
import { ERROR_CLASSES, DavitError, errorFor } from "../src/errors.ts";

const specText = readFileSync(SPEC_URL, "utf8");
const spec = parse(specText) as {
  paths: Record<string, Record<string, { operationId?: string; "x-davit-implemented"?: boolean }>>;
  components: { schemas: Record<string, { enum?: string[] }> };
};

const HTTP_METHODS = ["get", "put", "post", "delete", "patch", "head", "options"];

function specOperations(): Map<string, { method: string; path: string; implemented: boolean }> {
  const ops = new Map<string, { method: string; path: string; implemented: boolean }>();
  for (const [path, item] of Object.entries(spec.paths)) {
    for (const [method, op] of Object.entries(item)) {
      if (!HTTP_METHODS.includes(method) || !op.operationId) continue;
      ops.set(op.operationId, { method: method.toUpperCase(), path, implemented: op["x-davit-implemented"] === true });
    }
  }
  return ops;
}

test("every operationId is wrapped or explicitly not yet supported", () => {
  const ops = specOperations();
  assert.ok(ops.size > 0, "no operations parsed from openapi.yaml");
  const table = OPERATIONS as Record<string, readonly [string, string]>;
  for (const [id, { method, path, implemented }] of ops) {
    const inTable = Object.hasOwn(table, id);
    const deferred = NOT_YET_SUPPORTED.includes(id);
    assert.ok(inTable || deferred, `operation ${id} (${method} ${path}) is new: add it to OPERATIONS in src/client.ts or to NOT_YET_SUPPORTED`);
    assert.ok(!(inTable && deferred), `operation ${id} is in both OPERATIONS and NOT_YET_SUPPORTED`);
    if (implemented) assert.ok(inTable, `operation ${id} is implemented by the server but not wrapped by the SDK`);
    if (inTable) assert.deepEqual([...table[id]!], [method, path], `operation ${id}: SDK method/path differ from openapi.yaml`);
  }
  for (const id of [...Object.keys(table), ...NOT_YET_SUPPORTED]) {
    assert.ok(ops.has(id), `operation ${id} is listed by the SDK but no longer exists in openapi.yaml`);
  }
});

test("generated schema types are up to date", () => {
  const committed = readFileSync(OUT_URL, "utf8");
  assert.ok(committed === generate(specText), `src/schema.generated.ts is stale; run \`${REGENERATE_COMMAND}\` to regenerate`);
});

test("every Error.code value maps to its own error class", () => {
  const codes = spec.components.schemas.ErrorCode?.enum;
  assert.ok(codes && codes.length > 0, "ErrorCode enum missing from openapi.yaml");
  for (const code of codes) {
    assert.ok(Object.hasOwn(ERROR_CLASSES, code), `error code ${code} has no class in ERROR_CLASSES (src/errors.ts)`);
    const err = errorFor({ code, message: "x", status: 400 });
    assert.ok(err instanceof DavitError);
    assert.notEqual(err.constructor, DavitError, `error code ${code} maps to the base class`);
    assert.equal(err.code, code);
  }
  assert.equal(new Set(Object.values(ERROR_CLASSES)).size, Object.keys(ERROR_CLASSES).length, "two codes share a class");
});

test("generator: `{}` is unknown; unsupported constructs fail loudly", () => {
  const spec = (prop: string) => `components:\n  schemas:\n    X:\n      type: object\n      properties:\n        a: ${prop}\n`;
  assert.match(generate(spec("{}")), /a\?: unknown;/);
  assert.match(generate(spec("{description: any JSON}")), /a\?: unknown;/);
  assert.throws(() => generate(spec("{format: date-time}")), /unsupported keywords format/);
  assert.throws(() => generate(spec("{oneOf: [{type: string}]}")), /unsupported schema keyword "oneOf"/);
});
