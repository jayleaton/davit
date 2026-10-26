// Error classes. Every non-2xx response becomes a DavitError subclass
// chosen by the stable `code` (never by the English message). Unknown codes
// from newer servers fall back to the base class.

import type { ErrorInfo, Operation } from "./schema.generated.ts";

/** Fields shared by every SDK error. */
export interface DavitErrorInit {
  /** HTTP status, or null when the error was raised client-side. */
  status?: number | null;
  code: string;
  message: string;
  retryable?: boolean;
  details?: Record<string, unknown>;
  requestId?: string;
  operationId?: string;
  cause?: unknown;
}

/** Base class of every error thrown by the SDK. */
export class DavitError extends Error {
  /** HTTP status, or null for client-side errors (transport, waits). */
  readonly status: number | null;
  /** Stable machine-readable error code (e.g. `not_found`). */
  readonly code: string;
  /** Whether the server says retrying the same request may succeed. */
  readonly retryable: boolean;
  /** Structured, code-specific details from the server. */
  readonly details: Record<string, unknown>;
  /** The request ID the server reported (or the one the client sent). */
  readonly requestId: string | undefined;
  /** The operation tied to this error, when there is one. */
  readonly operationId: string | undefined;

  constructor(init: DavitErrorInit) {
    super(init.message, init.cause === undefined ? undefined : { cause: init.cause });
    this.name = new.target.name;
    this.status = init.status ?? null;
    this.code = init.code;
    this.retryable = init.retryable ?? false;
    this.details = init.details ?? {};
    this.requestId = init.requestId;
    this.operationId = init.operationId;
  }
}

/** `invalid_argument` (400): malformed request. */
export class InvalidArgument extends DavitError {}
/** `unauthenticated` (401): no valid identity. */
export class Unauthenticated extends DavitError {}
/** `permission_denied` (403): identity lacks the required grant. */
export class PermissionDenied extends DavitError {}
/** `not_found` (404): absent or invisible to the caller. */
export class NotFound extends DavitError {}
/** Parent of the 409 conflict codes. */
export class Conflict extends DavitError {}
/** `operation_in_progress` (409): another operation holds the VM; see `operationId`. */
export class OperationInProgress extends Conflict {}
/** `idempotency_conflict` (409): the key was reused with a different body. */
export class IdempotencyConflict extends Conflict {}
/** `invalid_state` (409): the resource's state does not allow this. */
export class InvalidState extends Conflict {}
/** `stop_required` (409): stop the VM first. */
export class StopRequired extends Conflict {}
/** `revision_mismatch` (412): `ifMatch` did not equal the current `stateRevision`. */
export class RevisionMismatch extends DavitError {}
/** `payload_too_large` (413). */
export class PayloadTooLarge extends DavitError {}
/** `unsupported_capability` (422): unsupported shape, architecture or feature. */
export class UnsupportedCapability extends DavitError {}
/** `resource_exhausted` (429): quota or host allocation limit. */
export class ResourceExhausted extends DavitError {}
/** `unavailable` (503): host or runtime temporarily unavailable. */
export class Unavailable extends DavitError {}
/** `not_implemented` (501): in the v1 contract but not served yet. */
export class ServerNotImplemented extends DavitError {}
/** `timeout` (504): the server gave up waiting on the host or guest. */
export class Timeout extends DavitError {}
/** `internal` (500): unexpected server error. */
export class Internal extends DavitError {}

/** The daemon could not be reached, or the connection failed mid-request. */
export class TransportError extends DavitError {
  /**
   * True when the request certainly never reached the server (connection
   * refused, socket file missing), so retrying is safe.
   */
  readonly connectFailed: boolean;

  constructor(message: string, options: { connectFailed?: boolean; cause?: unknown; requestId?: string } = {}) {
    super({ code: "unavailable", message, retryable: true, cause: options.cause, requestId: options.requestId });
    this.connectFailed = options.connectFailed ?? false;
  }
}

/**
 * The client stopped waiting. The awaited work keeps running on the server:
 * for an operation, `operationId` is set (poll `getOperation`); for a prompt
 * turn, `operationId` is undefined and `details` is `{promptId, cursor}`
 * (resume with `followPrompt(vm, promptId, {after: cursor})`).
 */
export class WaitTimeout extends DavitError {
  constructor(operationId: string | undefined, timeoutMs: number, cause?: unknown, details?: Record<string, unknown>) {
    const what =
      operationId !== undefined ? `operation ${operationId}` : details?.runId !== undefined ? `run ${String(details.runId)}` : `prompt ${String(details?.promptId)}`;
    super({
      code: "timeout",
      message: `stopped waiting after ${timeoutMs}ms; ${what} continues on the server`,
      operationId,
      details,
      cause,
    });
  }
}

/**
 * An awaited operation reached state `failed`. `error` is the typed error
 * built from `operation.error` (also available as `cause`).
 */
export class OperationFailed extends DavitError {
  /** The terminal operation. */
  readonly operation: Operation;
  /** Typed error for `operation.error`, e.g. an `Unavailable` instance. */
  readonly error: DavitError;
  declare readonly operationId: string;

  constructor(operation: Operation) {
    const info: ErrorInfo = operation.error ?? { code: "internal", message: "operation failed", retryable: false };
    const error = errorFromInfo(info, { operationId: operation.id });
    super({
      code: error.code,
      message: `operation ${operation.id} (${operation.kind}) failed: ${error.message}`,
      retryable: error.retryable,
      details: error.details,
      operationId: operation.id,
      cause: error,
    });
    this.operation = operation;
    this.error = error;
  }
}

type ErrorClass = new (init: DavitErrorInit) => DavitError;

/** Maps every server error code to its class. Unknown codes use DavitError. */
export const ERROR_CLASSES: Readonly<Record<string, ErrorClass>> = Object.freeze({
  invalid_argument: InvalidArgument,
  unauthenticated: Unauthenticated,
  permission_denied: PermissionDenied,
  not_found: NotFound,
  operation_in_progress: OperationInProgress,
  idempotency_conflict: IdempotencyConflict,
  invalid_state: InvalidState,
  stop_required: StopRequired,
  revision_mismatch: RevisionMismatch,
  payload_too_large: PayloadTooLarge,
  unsupported_capability: UnsupportedCapability,
  resource_exhausted: ResourceExhausted,
  unavailable: Unavailable,
  not_implemented: ServerNotImplemented,
  timeout: Timeout,
  internal: Internal,
});

/** Builds the typed error for an error code (unknown codes → DavitError). */
export function errorFor(init: DavitErrorInit): DavitError {
  const cls = Object.hasOwn(ERROR_CLASSES, init.code) ? ERROR_CLASSES[init.code]! : DavitError;
  return new cls(init);
}

/** Builds the typed error for an `ErrorInfo` (`Operation.error`, `VM.lastError`). */
export function errorFromInfo(info: ErrorInfo, extra: { status?: number | null; requestId?: string; operationId?: string } = {}): DavitError {
  return errorFor({
    status: extra.status ?? null,
    code: info.code,
    message: info.message,
    retryable: info.retryable,
    details: info.details,
    requestId: extra.requestId,
    operationId: info.operationId ?? extra.operationId,
  });
}
