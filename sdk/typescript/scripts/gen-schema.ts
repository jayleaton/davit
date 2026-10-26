// Generates src/schema.generated.ts from api/openapi.yaml components.schemas.
//
//   node scripts/gen-schema.ts          (or: npm run gen)
//
// Offline and dependency-light on purpose: it understands exactly the subset
// of JSON Schema the davit contract uses and fails loudly on anything else,
// so a new construct in the spec cannot be silently mistranslated.
//
// Output rules:
// - schemas marked `x-davit-implemented: false` are skipped;
// - string enums become OPEN unions (`"a" | "b" | (string & {})`) because
//   the API may add values and clients must tolerate them;
// - objects become interfaces with camelCase wire names; optional = not in
//   `required`; `additionalProperties` maps become `Record<string, T>`;
// - `format: date-time` stays an ISO 8601 / RFC 3339 `string`;
// - integers (including int64) are `number`;
// - an empty schema `{}` (any JSON value; description/title allowed) is
//   `unknown`; a typeless schema
//   with other keywords fails.

import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { parse } from "yaml";

/** Path of the OpenAPI document relative to this script. */
export const SPEC_URL = new URL("../../../api/openapi.yaml", import.meta.url);
/** Path of the generated module relative to this script. */
export const OUT_URL = new URL("../src/schema.generated.ts", import.meta.url);
/** The command that regenerates the output (shown in drift errors). */
export const REGENERATE_COMMAND = "cd sdk/typescript && npm run gen";

type Schema = Record<string, unknown>;

function isSchema(v: unknown): v is Schema {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

const SUPPORTED_KEYS = new Set([
  "type", "description", "enum", "properties", "required", "items",
  "additionalProperties", "$ref", "format", "minimum", "maximum", "minLength",
  "maxLength", "minItems", "maxItems", "pattern", "default", "not", "allOf",
  "x-davit-implemented", "contentMediaType", "title",
]);

function refName(ref: string): string {
  const prefix = "#/components/schemas/";
  if (!ref.startsWith(prefix)) throw new Error(`unsupported $ref ${ref}`);
  return ref.slice(prefix.length);
}

function literal(v: unknown): string {
  if (typeof v !== "string") throw new Error(`unsupported non-string enum value ${String(v)}`);
  return JSON.stringify(v);
}

function check(where: string, s: Schema): void {
  for (const k of Object.keys(s)) {
    if (!SUPPORTED_KEYS.has(k)) throw new Error(`${where}: unsupported schema keyword "${k}"`);
  }
}

/** Renders a schema used in a type position (property, item, map value). */
function typeExpr(where: string, s: unknown, indent: string): string {
  if (!isSchema(s)) throw new Error(`${where}: schema must be an object`);
  check(where, s);
  if (typeof s.$ref === "string") return refName(s.$ref);
  if (Array.isArray(s.enum)) return openUnion(s.enum);
  if (Array.isArray(s.allOf)) return s.allOf.map((p, i) => typeExpr(`${where}.allOf[${i}]`, p, indent)).join(" & ");
  switch (s.type) {
    case "string":
      return "string";
    case "integer":
    case "number":
      return "number";
    case "boolean":
      return "boolean";
    case "array": {
      const item = typeExpr(`${where}[]`, s.items, indent);
      return /^[\w.]+$/.test(item) ? `${item}[]` : `Array<${item}>`;
    }
    case "object":
      return objectExpr(where, s, indent);
    case undefined: {
      if (s.properties !== undefined) return objectExpr(where, s, indent);
      // `{}` (optionally with a description or title) is any JSON value.
      const extra = Object.keys(s).filter((k) => k !== "description" && k !== "title");
      if (extra.length === 0) return "unknown";
      throw new Error(`${where}: schema without a type has unsupported keywords ${extra.join(", ")}`);
    }
  }
  throw new Error(`${where}: unsupported type ${String(s.type)}`);
}

function openUnion(values: unknown[]): string {
  return [...values.map(literal), "(string & {})"].join(" | ");
}

function objectExpr(where: string, s: Schema, indent: string): string {
  const props = s.properties;
  const ap = s.additionalProperties;
  if (props === undefined || (isSchema(props) && Object.keys(props).length === 0)) {
    if (ap === true || ap === undefined) return "Record<string, unknown>";
    return `Record<string, ${typeExpr(`${where}{}`, ap, indent)}>`;
  }
  if (ap !== undefined && ap !== false) throw new Error(`${where}: properties together with additionalProperties are unsupported`);
  return `{\n${members(where, s, indent + "  ")}${indent}}`;
}

function members(where: string, s: Schema, indent: string): string {
  if (!isSchema(s.properties)) throw new Error(`${where}: properties must be an object`);
  const required = new Set(Array.isArray(s.required) ? (s.required as string[]) : []);
  let out = "";
  for (const [name, prop] of Object.entries(s.properties)) {
    if (!/^[A-Za-z_$][\w$]*$/.test(name)) throw new Error(`${where}.${name}: unsupported property name`);
    const p = isSchema(prop) ? prop : {};
    out += doc(docText(p), indent);
    out += `${indent}${name}${required.has(name) ? "" : "?"}: ${typeExpr(`${where}.${name}`, prop, indent)};\n`;
  }
  return out;
}

function docText(s: Schema): string {
  const parts: string[] = [];
  if (typeof s.description === "string") parts.push(s.description.trim());
  if (s.format === "date-time") parts.push("ISO 8601 / RFC 3339 UTC timestamp string.");
  return parts.join("\n\n");
}

function doc(text: string, indent: string): string {
  if (!text) return "";
  const lines = text.replaceAll("*/", "*\\/").split("\n");
  if (lines.length === 1) return `${indent}/** ${lines[0]} */\n`;
  return `${indent}/**\n${lines.map((l) => (l ? `${indent} * ${l}` : `${indent} *`)).join("\n")}\n${indent} */\n`;
}

/** Generates the TypeScript module text for the given OpenAPI YAML text. */
export function generate(specText: string): string {
  const spec: unknown = parse(specText);
  const schemas = isSchema(spec) && isSchema(spec.components) ? spec.components.schemas : undefined;
  if (!isSchema(schemas)) throw new Error("openapi.yaml has no components.schemas");

  let out =
    "// Code generated by scripts/gen-schema.ts from api/openapi.yaml. DO NOT EDIT.\n" +
    `// Regenerate with: ${REGENERATE_COMMAND}\n` +
    "//\n" +
    "// Wire types of the davit v1 API. Field names are the JSON wire names.\n" +
    "// Timestamps are ISO 8601 / RFC 3339 strings (parse with `new Date(s)`).\n" +
    "// Enum unions are open: newer servers may send values not listed here.\n";

  for (const [name, raw] of Object.entries(schemas)) {
    if (!isSchema(raw)) throw new Error(`${name}: schema must be an object`);
    if (raw["x-davit-implemented"] === false) continue;
    check(name, raw);
    out += "\n" + doc(docText(raw), "");
    const isObject = raw.type === "object" || raw.properties !== undefined;
    if (isObject && raw.allOf === undefined && isSchema(raw.properties) && Object.keys(raw.properties).length > 0) {
      if (raw.additionalProperties !== undefined && raw.additionalProperties !== false) {
        throw new Error(`${name}: properties together with additionalProperties are unsupported`);
      }
      out += `export interface ${name} {\n${members(name, raw, "  ")}}\n`;
    } else {
      out += `export type ${name} = ${typeExpr(name, raw, "")};\n`;
    }
  }
  return out;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const text = generate(readFileSync(SPEC_URL, "utf8"));
  writeFileSync(OUT_URL, text);
  console.log(`wrote ${fileURLToPath(OUT_URL)}`);
}
