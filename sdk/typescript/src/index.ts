/**
 * TypeScript client for the davit v1 API.
 *
 * @packageDocumentation
 */

export {
  API_URL_ENV,
  BASE_PATH,
  CA_FILE_ENV,
  DEFAULT_PUSH_PATH,
  DEFAULT_WAIT_TIMEOUT_MS,
  MAX_CAPACITY_WAIT_SECONDS,
  NOT_YET_SUPPORTED,
  OPERATIONS,
  PRODUCT,
  RUN_POLL_INTERVAL_MS,
  DavitClient,
  TOKEN_ENV,
  isLoopbackHost,
  newIdempotencyKey,
  newRequestId,
  parseEnvironmentRef,
  resolveDefaultEndpoint,
} from "./client.ts";
export type {
  AttachOptions,
  BatchOptions,
  CIStatusOptions,
  CreateWebhookOptions,
  EventFilterOptions,
  ClientOptions,
  CreateCredentialGrantOptions,
  CreateCredentialProfileOptions,
  CreateEnvironmentOptions,
  CreateEnvironmentRevisionOptions,
  CreateRunOptions,
  CreateTokenOptions,
  RotateTokenOptions,
  CreateVMInput,
  CreateVMOptions,
  EnvironmentSpec,
  ExecOptions,
  ExportSnapshotOptions,
  ExtendOptions,
  FileContent,
  FileData,
  FileOptions,
  FollowPromptOptions,
  FollowVMPromptEventsOptions,
  InterruptOptions,
  SteerPromptOptions,
  ListPromptEventsOptions,
  ListVMPromptEventsOptions,
  RunOptions,
  Endpoint,
  ForkVMOptions,
  IterEventsOptions,
  ListEventsOptions,
  ListHostEventsOptions,
  ListVMsOptions,
  ListWebhookDeliveriesOptions,
  ManifestInput,
  PromptOptions,
  PutCredentialInput,
  PushTreeOptions,
  ReadProcessOutputOptions,
  RetainedOutput,
  RunInput,
  RunOutcome,
  RunToCompletionOptions,
  WaitRunOptions,
  MutationOptions,
  NoWait,
  PageOptions,
  RequestOptions,
  ReclaimVMsOptions,
  ResumeVMOptions,
  RotateWebhookSecretOptions,
  ShareOptions,
  SnapshotFileOptions,
  SnapshotTreeOptions,
  StopVMOptions,
  UpdateVMOptions,
  WaitOptions,
  WaitPromptOptions,
  WebhookRef,
  PromptResult,
  Waiting,
  WriteFileOptions,
} from "./client.ts";
export {
  Conflict,
  ERROR_CLASSES,
  IdempotencyConflict,
  Internal,
  InvalidArgument,
  InvalidState,
  NotFound,
  OperationFailed,
  OperationInProgress,
  PayloadTooLarge,
  PermissionDenied,
  ResourceExhausted,
  RevisionMismatch,
  ServerNotImplemented,
  DavitError,
  StopRequired,
  Timeout,
  TransportError,
  Unauthenticated,
  Unavailable,
  UnsupportedCapability,
  WaitTimeout,
  errorFor,
  errorFromInfo,
} from "./errors.ts";
export type { DavitErrorInit } from "./errors.ts";
export { FetchTransport, HttpsTransport, MAX_BODY_BYTES, UnixSocketTransport } from "./transport.ts";
export type { HttpsTransportOptions, RawConnection, Transport, TransportBody, TransportRequest, TransportResponse } from "./transport.ts";
export { FileReadStream, TarChecker, formatMode } from "./files.ts";
export { EXIT_DEADLINE, EXIT_LOST, ProcessStream, RunResult, STDIN_CHUNK_BYTES, processExitCode, signalExitCode } from "./process.ts";
export type { ProcessExitEvent, ProcessOutputEvent, ProcessStreamEvent } from "./process.ts";
export { TUNNEL_CHUNK_BYTES, TunnelStream } from "./tunnel.ts";
export { REVERSE_CLOSED_REASON, ReverseSession } from "./reverse.ts";
export type { LocalDialer, ReverseConnectionDialer } from "./reverse.ts";
export { DEFAULT_MAX_MESSAGE_BYTES, WebSocketConnection } from "./websocket.ts";
export { DEFAULT_WEBHOOK_TOLERANCE_SECONDS, WEBHOOK_SIGNATURE_HEADER, WebhookVerificationError, verifyWebhook } from "./webhooks.ts";
export type { VerifyWebhookOptions, WebhookVerificationReason } from "./webhooks.ts";
export type { WebSocketConnectOptions, WebSocketMessage } from "./websocket.ts";
export type * from "./schema.generated.ts";
