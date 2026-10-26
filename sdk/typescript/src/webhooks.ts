// Receiver-side verification of signed webhook deliveries.

import { createHmac, timingSafeEqual } from "node:crypto";

import { DavitError } from "./errors.ts";

/** Header carrying `t=<unix seconds>,v1=<hex>[,v1=<hex>...]` on every delivery. */
export const WEBHOOK_SIGNATURE_HEADER = "X-Davit-Signature";
/** Largest accepted distance, in seconds, between the signed timestamp and now. */
export const DEFAULT_WEBHOOK_TOLERANCE_SECONDS = 300;

/** Why a delivery failed verification. */
export type WebhookVerificationReason = "malformed" | "timestamp" | "signature";

/**
 * A webhook delivery failed signature verification; reject it (for example
 * with 400). `reason` is `malformed` (missing or unparsable header),
 * `timestamp` (outside the tolerance, too old or too far in the future) or
 * `signature` (no `v1` matches any secret, or the body was changed).
 */
export class WebhookVerificationError extends DavitError {
  readonly reason: WebhookVerificationReason;

  constructor(message: string, reason: WebhookVerificationReason) {
    super({ code: "invalid_signature", message });
    this.reason = reason;
  }
}

/** Options of `verifyWebhook`. */
export interface VerifyWebhookOptions {
  /** Seconds the signed timestamp may differ from now in either direction (default 300). */
  toleranceSeconds?: number;
  /** Current time: unix seconds or a Date (default: `Date.now()`). */
  now?: number | Date;
}

const HEX64 = /^[0-9a-fA-F]{64}$/;
const DIGITS = /^[0-9]{1,15}$/;

/**
 * Checks the `X-Davit-Signature` header of a delivery and returns when it
 * is authentic and fresh; throws `WebhookVerificationError` otherwise.
 *
 * Each `v1` is `hex(HMAC-SHA256(secret, "<t>." + rawBody))`. During a secret
 * rotation a delivery carries one `v1` per live secret; it verifies when any
 * `v1` matches any of `secrets` (pass the old and the new secret while you
 * switch over). Comparison is constant-time. Pass the raw body exactly as
 * received, not re-serialized JSON (a string is encoded as UTF-8). A missing
 * header (`null`/`undefined`) fails as `malformed`. No secret, an empty
 * secret or a negative tolerance throws `TypeError`.
 */
export function verifyWebhook(
  body: Uint8Array | string,
  signature: string | null | undefined,
  secrets: string | readonly string[],
  opts: VerifyWebhookOptions = {},
): void {
  const keys = typeof secrets === "string" ? [secrets] : [...secrets];
  if (keys.length === 0 || keys.some((k) => typeof k !== "string" || k === "")) {
    throw new TypeError("verifyWebhook: secrets must be one or more non-empty strings");
  }
  const tolerance = opts.toleranceSeconds ?? DEFAULT_WEBHOOK_TOLERANCE_SECONDS;
  if (!(tolerance >= 0)) throw new TypeError("verifyWebhook: toleranceSeconds must be >= 0");
  const raw = typeof body === "string" ? new TextEncoder().encode(body) : body;

  const { timestamp, candidates } = parseSignature(signature);
  const now = opts.now === undefined ? Date.now() / 1000 : opts.now instanceof Date ? opts.now.getTime() / 1000 : opts.now;
  if (Math.abs(now - timestamp) > tolerance) {
    throw new WebhookVerificationError(`signature timestamp ${timestamp} is outside the ${tolerance}s tolerance`, "timestamp");
  }
  let matched = false;
  for (const key of keys) {
    const mac = createHmac("sha256", key);
    mac.update(`${timestamp}.`);
    mac.update(raw);
    const expected = mac.digest();
    for (const candidate of candidates) {
      // No early exit: every pair is compared.
      if (timingSafeEqual(expected, candidate)) matched = true;
    }
  }
  if (!matched) throw new WebhookVerificationError("no v1 signature matches the secret", "signature");
}

function parseSignature(header: string | null | undefined): { timestamp: number; candidates: Uint8Array[] } {
  if (!header) throw new WebhookVerificationError("missing signature header", "malformed");
  let timestamp: number | undefined;
  const candidates: Uint8Array[] = [];
  for (const part of header.split(",")) {
    const s = part.trim();
    const eq = s.indexOf("=");
    if (eq < 0) throw new WebhookVerificationError("malformed signature header", "malformed");
    const key = s.slice(0, eq);
    const value = s.slice(eq + 1);
    if (key === "t") {
      if (timestamp !== undefined || !DIGITS.test(value)) throw new WebhookVerificationError("malformed signature timestamp", "malformed");
      timestamp = Number(value);
    } else if (key === "v1") {
      if (!HEX64.test(value)) throw new WebhookVerificationError("malformed v1 signature", "malformed");
      candidates.push(Buffer.from(value, "hex"));
    }
    // Other schemes (a future v2) are ignored.
  }
  if (timestamp === undefined || candidates.length === 0) {
    throw new WebhookVerificationError("signature header needs t= and at least one v1=", "malformed");
  }
  return { timestamp, candidates };
}
