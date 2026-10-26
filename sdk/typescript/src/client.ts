// The davit API client. Semantics follow the Go CLI (internal/cli): one
// request ID per call, an idempotency key generated once per mutation and
// reused on retry, retries only when the request never reached the server,
// and lifecycle calls that wait for their operation by default.

import {
  Internal,
  OperationFailed,
  DavitError,
  TransportError,
  WaitTimeout,
  errorFor,
  errorFromInfo,
} from "./errors.ts";
import type {
  AddMemberRequest,
  AppEndpoint,
  AppEndpointList,
  Conversation,
  ConversationList,
  CreateAppEndpointRequest,
  CreateShareRequest,
  CreateCredentialGrantRequest,
  CreateCredentialProfileRequest,
  CreateReverseForwardRequest,
  CreateTokenRequest,
  Me,
  RotateTokenRequest,
  CreateVMRequest,
  CreateVMsResult,
  CreateWebhookRequest,
  CreatedToken,
  CredentialGrant,
  CredentialGrantList,
  CredentialProfile,
  CredentialProfileList,
  Egress,
  EnvManifest,
  Environment,
  EnvironmentList,
  EnvironmentRef,
  EnvironmentRevision,
  EnvironmentRevisionList,
  ErrorResponse,
  Event,
  EventList,
  ExecRequest,
  FileInfo,
  ForkVMRequest,
  GuestProcessList,
  HistoryEntry,
  HistoryList,
  HostCapabilities,
  Image,
  ImageList,
  InterruptRequest,
  InterruptResult,
  IssueSSHCredentialRequest,
  Member,
  MemberList,
  MemberRole,
  OpenDesktopRequest,
  Operation,
  OperationAccepted,
  OperationEvent,
  OperationEventList,
  Process,
  ProcessList,
  ProcessOutput,
  Prompt,
  PromptAttachment,
  PromptEvent,
  PromptEventList,
  PromptFeed,
  PromptFeedItem,
  PromptRequest,
  PushResult,
  PutCredentialRequest,
  ReclaimVMsRequest,
  ReclaimVMsResult,
  ResumeVMRequest,
  ReverseForward,
  ReverseForwardList,
  ReverseTarget,
  RotateWebhookSecretRequest,
  Run,
  RunList,
  RunRequest,
  SSHCredential,
  ShapeOverride,
  Share,
  ShareKind,
  ShareList,
  UpdateMemberRequest,
  Snapshot,
  SnapshotEntry,
  SnapshotList,
  SnapshotTree,
  SteerResult,
  Token,
  TokenList,
  TokenScope,
  Tunnel,
  UpdateAppEndpointRequest,
  UpdateVMRequest,
  UpdateWebhookRequest,
  VM,
  VMList,
  VMState,
  Webhook,
  WebhookDelivery,
  WebhookDeliveryList,
  WebhookList,
  WebhookSecret,
} from "./schema.generated.ts";
import { EXIT_DEADLINE, ProcessStream, RunResult, concatBytes, processExitCode, signalExitCode, type ProcessOutputEvent } from "./process.ts";
import { FileReadStream, formatMode } from "./files.ts";
import { TunnelStream } from "./tunnel.ts";
import { ReverseSession } from "./reverse.ts";
import { FetchTransport, HttpsTransport, UnixSocketTransport, type Transport, type TransportBody, type TransportResponse } from "./transport.ts";
import { WebSocketConnection } from "./websocket.ts";
import { guardSocket } from "./sockets.ts";
import type { Socket } from "node:net";

/** Placeholder product name; env vars and default paths derive from it. */
export const PRODUCT = "davit";
/** Environment variable selecting the API endpoint (`unix:///abs.sock` or an https URL). */
export const API_URL_ENV = `${PRODUCT.toUpperCase()}_API_URL`;
/** Environment variable with the default bearer token (http(s) base URLs only, never the socket). */
export const TOKEN_ENV = `${PRODUCT.toUpperCase()}_TOKEN`;
/** Environment variable naming a PEM bundle of extra CAs trusted for the https endpoint. */
export const CA_FILE_ENV = `${PRODUCT.toUpperCase()}_CA_FILE`;
/** Every route lives under this prefix. */
export const BASE_PATH = "/api/v1";
/** Default budget of `wait` and of lifecycle calls with `wait: true` (10 minutes). */
export const DEFAULT_WAIT_TIMEOUT_MS = 10 * 60 * 1000;
/** Upper bound of one long-poll request. */
export const MAX_LONG_POLL_MS = 25_000;
/** Bound of each local service dial of `reverseForward` (as the CLI). */
const LOCAL_DIAL_TIMEOUT_MS = 10_000;
const MAX_ATTEMPTS = 3;
/** Interval between getRun polls in `waitRun` (like `davit run`). */
export const RUN_POLL_INTERVAL_MS = 200;
/** Guest directory of a pushed tree when none is given (the CLI's default). */
export const DEFAULT_PUSH_PATH = "/home/davit/work";

type Method = "GET" | "POST" | "DELETE" | "PUT" | "PATCH";

/** Implemented operations: operationId → [HTTP method, path template under /api/v1]. */
export const OPERATIONS = Object.freeze({
  getCapabilities: ["GET", "/capabilities"],
  listImages: ["GET", "/images"],
  getImage: ["GET", "/images/{imageId}"],
  listVMs: ["GET", "/vms"],
  createVM: ["POST", "/vms"],
  createVMs: ["POST", "/vms/batch"],
  reclaimVMs: ["POST", "/vms/reclaim"],
  getVM: ["GET", "/vms/{vmId}"],
  deleteVM: ["DELETE", "/vms/{vmId}"],
  updateVM: ["PATCH", "/vms/{vmId}"],
  stopVM: ["POST", "/vms/{vmId}/stop"],
  resumeVM: ["POST", "/vms/{vmId}/resume"],
  recoverVM: ["POST", "/vms/{vmId}/recover"],
  forkVM: ["POST", "/vms/{vmId}/fork"],
  listVMSnapshots: ["GET", "/vms/{vmId}/snapshots"],
  snapshotVM: ["POST", "/vms/{vmId}/snapshots"],
  listSnapshots: ["GET", "/snapshots"],
  getSnapshot: ["GET", "/snapshots/{snapshotId}"],
  deleteSnapshot: ["DELETE", "/snapshots/{snapshotId}"],
  listSnapshotTree: ["GET", "/snapshots/{snapshotId}/tree"],
  readSnapshotFile: ["GET", "/snapshots/{snapshotId}/files"],
  exportSnapshot: ["GET", "/snapshots/{snapshotId}/export"],
  getOperation: ["GET", "/operations/{operationId}"],
  listOperationEvents: ["GET", "/operations/{operationId}/events"],
  listProcesses: ["GET", "/vms/{vmId}/processes"],
  execProcess: ["POST", "/vms/{vmId}/processes"],
  getProcess: ["GET", "/vms/{vmId}/processes/{processId}"],
  killProcess: ["DELETE", "/vms/{vmId}/processes/{processId}"],
  attachProcess: ["GET", "/vms/{vmId}/processes/{processId}/stream"],
  readFile: ["GET", "/vms/{vmId}/files"],
  writeFile: ["PUT", "/vms/{vmId}/files"],
  createPrompt: ["POST", "/vms/{vmId}/prompts"],
  getPrompt: ["GET", "/vms/{vmId}/prompts/{promptId}"],
  cancelPrompt: ["DELETE", "/vms/{vmId}/prompts/{promptId}"],
  listPromptEvents: ["GET", "/vms/{vmId}/prompts/{promptId}/events"],
  steerPrompt: ["POST", "/vms/{vmId}/prompts/{promptId}/steer"],
  listVMPromptEvents: ["GET", "/vms/{vmId}/prompt-events"],
  interruptPrompts: ["POST", "/vms/{vmId}/interrupt"],
  listConversations: ["GET", "/vms/{vmId}/conversations"],
  getConversation: ["GET", "/vms/{vmId}/conversations/{conversationId}"],
  openTunnel: ["POST", "/vms/{vmId}/tunnels"],
  closeTunnel: ["DELETE", "/tunnels/{tunnelId}"],
  attachTunnel: ["GET", "/tunnels/{tunnelId}/stream"],
  listReverseForwards: ["GET", "/vms/{vmId}/reverse-forwards"],
  createReverseForward: ["POST", "/vms/{vmId}/reverse-forwards"],
  revokeReverseForward: ["DELETE", "/vms/{vmId}/reverse-forwards/{forwardId}"],
  attachReverseSession: ["GET", "/vms/{vmId}/reverse-forwards/{forwardId}/session"],
  attachReverseConnection: ["GET", "/vms/{vmId}/reverse-forwards/{forwardId}/connections/{connectionId}"],
  listAppEndpoints: ["GET", "/vms/{vmId}/app-endpoints"],
  createAppEndpoint: ["POST", "/vms/{vmId}/app-endpoints"],
  updateAppEndpoint: ["PATCH", "/vms/{vmId}/app-endpoints/{endpointId}"],
  deleteAppEndpoint: ["DELETE", "/vms/{vmId}/app-endpoints/{endpointId}"],
  getDesktop: ["GET", "/vms/{vmId}/desktop"],
  openDesktop: ["POST", "/vms/{vmId}/desktop"],
  closeDesktop: ["DELETE", "/vms/{vmId}/desktop"],
  issueSSHCredential: ["POST", "/vms/{vmId}/ssh-credentials"],
  listTokens: ["GET", "/tokens"],
  createToken: ["POST", "/tokens"],
  revokeToken: ["DELETE", "/tokens/{tokenId}"],
  listMembers: ["GET", "/members"],
  addMember: ["POST", "/members"],
  updateMember: ["PATCH", "/members/{memberId}"],
  removeMember: ["DELETE", "/members/{memberId}"],
  listShares: ["GET", "/shares"],
  createShare: ["POST", "/shares"],
  deleteShare: ["DELETE", "/shares/{shareId}"],
  rotateToken: ["POST", "/tokens/{tokenId}/rotate"],
  whoami: ["GET", "/me"],
  listCredentialProfiles: ["GET", "/credential-profiles"],
  createCredentialProfile: ["POST", "/credential-profiles"],
  getCredentialProfile: ["GET", "/credential-profiles/{profileId}"],
  deleteCredentialProfile: ["DELETE", "/credential-profiles/{profileId}"],
  putCredential: ["PUT", "/credential-profiles/{profileId}/credentials/{credentialName}"],
  deleteCredential: ["DELETE", "/credential-profiles/{profileId}/credentials/{credentialName}"],
  listCredentialGrants: ["GET", "/vms/{vmId}/credential-grants"],
  createCredentialGrant: ["POST", "/vms/{vmId}/credential-grants"],
  revokeCredentialGrant: ["DELETE", "/vms/{vmId}/credential-grants/{grantId}"],
  listEnvironments: ["GET", "/environments"],
  createEnvironment: ["POST", "/environments"],
  getEnvironment: ["GET", "/environments/{environmentId}"],
  deleteEnvironment: ["DELETE", "/environments/{environmentId}"],
  listEnvironmentRevisions: ["GET", "/environments/{environmentId}/revisions"],
  createEnvironmentRevision: ["POST", "/environments/{environmentId}/revisions"],
  getEnvironmentRevision: ["GET", "/environments/{environmentId}/revisions/{revision}"],
  listProcessHistory: ["GET", "/vms/{vmId}/history"],
  readProcessOutput: ["GET", "/vms/{vmId}/processes/{processId}/output"],
  listGuestProcesses: ["GET", "/vms/{vmId}/ps"],
  listRuns: ["GET", "/runs"],
  createRun: ["POST", "/runs"],
  getRun: ["GET", "/runs/{runId}"],
  cancelRun: ["DELETE", "/runs/{runId}"],
  startRun: ["POST", "/runs/{runId}/start"],
  pushTree: ["PUT", "/vms/{vmId}/tree"],
  listEvents: ["GET", "/events"],
  listWebhooks: ["GET", "/webhooks"],
  createWebhook: ["POST", "/webhooks"],
  getWebhook: ["GET", "/webhooks/{webhookId}"],
  updateWebhook: ["PATCH", "/webhooks/{webhookId}"],
  deleteWebhook: ["DELETE", "/webhooks/{webhookId}"],
  rotateWebhookSecret: ["POST", "/webhooks/{webhookId}/rotate-secret"],
  listWebhookDeliveries: ["GET", "/webhooks/{webhookId}/deliveries"],
  retryWebhookDelivery: ["POST", "/webhooks/{webhookId}/deliveries/{deliveryId}/retry"],
} as const satisfies Record<string, readonly [Method, string]>);

/** operationIds in the v1 contract that this SDK does not wrap yet (the server returns 501). */
export const NOT_YET_SUPPORTED: readonly string[] = Object.freeze([
  // Parity v1 contract: specified, answering not_implemented on the server.
]);

type OperationName = keyof typeof OPERATIONS;

/** Client construction options. With none, the endpoint comes from the environment. */
export interface ClientOptions {
  /** Absolute path of the local daemon socket. */
  socket?: string;
  /** `https://host[:port]`, or `http://` for loopback hosts only. */
  baseUrl?: string;
  /**
   * Bearer API token (sent as `Authorization: Bearer <token>`). Default:
   * `$DAVIT_TOKEN`, used only when the endpoint is an http(s) base URL
   * (never the local socket); `""` sends no token.
   */
  token?: string;
  /**
   * PEM bundle of extra CAs trusted for the `https://` endpoint (requests and
   * WebSocket streams), in addition to the built-in roots: for operator-managed
   * certificates. Default: `$DAVIT_CA_FILE`. Read at construction; a
   * missing or invalid file throws.
   */
  caFile?: string;
  /** Custom transport; `baseUrl`/`socket` are then ignored for routing. */
  transport?: Transport;
  /** Default budget for waits, in milliseconds (10 minutes). */
  waitTimeoutMs?: number;
}

/** Per-call options accepted by every method. */
export interface RequestOptions {
  /** Correlation ID to send as `X-Request-Id` (default: generated `req_<hex>`). */
  requestId?: string;
  /** Aborts the call (and, for waits, stops waiting; the server operation continues). */
  signal?: AbortSignal;
}

/** Options of the `wait` helpers. */
export interface WaitOptions extends RequestOptions {
  /** Wait budget in milliseconds (default: the client's `waitTimeoutMs`). */
  timeoutMs?: number;
  /** Called once per progress event, in order. */
  onProgress?: (event: OperationEvent) => void;
}

/** Options of lifecycle mutations. */
export interface MutationOptions extends WaitOptions {
  /** Idempotency key (default: a UUIDv4 generated once for this call). */
  idempotencyKey?: string;
  /** Expected VM `stateRevision`; a stale value fails with `RevisionMismatch`. */
  ifMatch?: number | string;
  /**
   * Follow the operation and return the final resource (default true). With
   * `false` the 202 `OperationAccepted` body is returned immediately.
   */
  wait?: boolean;
}

/** Longest `capacityWaitSeconds` the server accepts. */
export const MAX_CAPACITY_WAIT_SECONDS = 3600;

/** Options of `createVM`. */
export interface CreateVMOptions extends MutationOptions {
  /**
   * When the host has no room even after stopping idle VMs, queue the request
   * this many seconds (integer 0..3600) for capacity, first come first served
   * (feature `capacityReclaim`; wire `waitSeconds`). Default: no wait, so a
   * full host fails with `ResourceExhausted` at once, as does the end of the
   * wait. Not the operation wait (`wait`/`timeoutMs`).
   */
  capacityWaitSeconds?: number;
}

/** Options of `reclaimVMs`. */
export interface ReclaimVMsOptions extends RequestOptions {
  /** Only VMs carrying all of these labels. */
  labels?: Record<string, string>;
  /** Only list the VMs (no `operationId`), stopping none. */
  dryRun?: boolean;
}

/** Mutation options with `wait: false`. */
export type NoWait = MutationOptions & { wait: false };
/** Mutation options that wait (the default). */
export type Waiting = MutationOptions & { wait?: true };

/** Options of `createVMs`. */
export interface BatchOptions extends WaitOptions {
  /** Batch idempotency key (default: a UUIDv4 generated once for this call); keep it to retry refused items. */
  idempotencyKey?: string;
  /**
   * Also wait for every accepted operation (default false), sharing one
   * budget; each accepted item then carries its terminal operation and VM.
   * Failed operations are reported in the items, not thrown.
   */
  wait?: boolean;
}

/** Pagination options. */
export interface PageOptions extends RequestOptions {
  /** Page size (server default 50, max 200). */
  limit?: number;
  /** `nextCursor` of the previous page. */
  cursor?: string;
}

/** Options of `createShare` and the `share*` helpers. */
export interface ShareOptions extends RequestOptions {
  /** Role on the resource (default: the member's own role; `"*"`: each member's own role). */
  role?: MemberRole;
  /**
   * Let the member(s) use the VM's credential grants. Off (default), an exec
   * or admin share pauses the VM's credential grants for everyone while it exists.
   */
  credentials?: boolean;
}

/** Options of `listVMs`. */
export interface ListVMsOptions extends PageOptions {
  /** States to include (default provisioning, ready, running). */
  states?: VMState[];
  /** Include every state, including deleted tombstones. */
  all?: boolean;
  /** Only VMs carrying every one of these labels (e.g. `{ "davit.box": "myproject" }`). */
  labels?: Record<string, string>;
}

/** Options of `stopVM`. */
export interface StopVMOptions extends MutationOptions {
  /** Skip graceful guest shutdown; the commit is then crash-consistent. */
  force?: boolean;
}

/**
 * An environment to pin on create, fork or resume: an `EnvironmentRef`, or
 * `"NAME"` (the latest revision) / `"NAME@REVISION"` (see `parseEnvironmentRef`).
 */
export type EnvironmentSpec = EnvironmentRef | string;

/** Body of `createVM`: the wire request, with `environment` also accepted as `"NAME[@REV]"`. */
export type CreateVMInput = Omit<CreateVMRequest, "environment"> & {
  /**
   * Pin an environment revision and prepare its workspace. A server without
   * a workspace preparer throws `ServerNotImplemented`.
   */
  environment?: EnvironmentSpec;
};

/** Options of `resumeVM`. */
export interface ResumeVMOptions extends CreateVMOptions {
  /**
   * Resize to this administrator-defined profile for this and later boots
   * (feature `resize`); `shape` fields override it.
   */
  type?: string;
  /**
   * Resize: fields override the profile and the VM's current shape. The disk
   * can only grow (a smaller `diskBytes` is `InvalidArgument`, reason
   * `disk_shrink`) and the guest filesystem is not grown automatically. A
   * ready or running VM must be stopped first (`StopRequired`).
   */
  shape?: ShapeOverride;
  ttlSeconds?: number;
  autoStop?: boolean;
  /** Re-pin the VM to this environment revision and prepare it again. */
  environment?: EnvironmentSpec;
}

/** Options of `forkVM`. */
export interface ForkVMOptions extends MutationOptions {
  /** Name of the new VM. */
  name?: string;
  /** Administrator-defined profile (`capabilities.profiles`); `shape` fields override it. */
  type?: string;
  /** Snapshot (ID or name) of the source to fork instead of its last committed disk. */
  snapshot?: string;
  /**
   * Fork the running VM itself (RAM, processes and disk at one instant; the
   * source pauses briefly) instead of a committed disk. Needs the runtime's
   * `liveFork` capability; the source must be ready or running.
   */
  live?: boolean;
  shape?: ShapeOverride;
  ttlSeconds?: number;
  autoStop?: boolean;
  /** Egress policy of the new VM (default: the source's). */
  egress?: Egress;
  /** Re-pin another environment revision (default: the source's pin and prepared workspace). */
  environment?: EnvironmentSpec;
}

/** Options of `updateVM`, `extendVM` and `renameVM`. */
export interface UpdateVMOptions extends RequestOptions {
  /** Expected VM `stateRevision`; a stale value fails with `RevisionMismatch`. */
  ifMatch?: number | string;
}

/** Options of `rotateToken`. */
export interface RotateTokenOptions extends RequestOptions {
  /** How long the old secret keeps working (seconds; default 0: revoked at once). */
  overlapSeconds?: number;
  /** Lifetime of the new token (seconds); omitted keeps the old token's. */
  ttlSeconds?: number;
  idempotencyKey?: string;
}

/** Options of `extendVM`. */
export interface ExtendOptions extends UpdateVMOptions {
  /** New TTL in seconds (integer > 0); omitted keeps the current TTL. */
  ttlSeconds?: number;
}

/** Options of `listOperationEvents`. */
export interface ListEventsOptions extends RequestOptions {
  /** Return events with `seq` greater than this (default 0). */
  after?: number;
  /** Long-poll budget in milliseconds (0..30000). */
  waitMs?: number;
}

/** Options of `execProcess` (wire fields of ExecRequest except argv). */
export interface ExecOptions extends RequestOptions {
  /** Working directory in the guest. */
  cwd?: string;
  /** Extra environment (never echoed back). */
  env?: Record<string, string>;
  /** Guest user (default: the image's user). */
  user?: string;
  /** Kill the process group after this many seconds (exit status 124 in `run`). */
  deadlineSeconds?: number;
  /** Survive stream disconnects; re-attach later. */
  detached?: boolean;
  /** Keep stdin open for the first attached stream. */
  stdin?: boolean;
  /** Not supported by the server (422 UnsupportedCapability); use ssh. */
  tty?: boolean;
  /** Idempotency key (default: a UUIDv4 generated once for this call). */
  idempotencyKey?: string;
}

/** Options of `run`. */
export interface RunOptions extends Omit<ExecOptions, "detached" | "stdin" | "tty"> {
  /** Bytes (or UTF-8 text) to write to stdin, which is then closed. */
  input?: string | Uint8Array;
}

/** Options of `attachProcess`. */
export interface AttachOptions extends RequestOptions {
  /** Replay output with `seq` greater than this (default 0: everything retained). */
  outputCursor?: number;
}

/** Options of `readFile` and `readFileStream`. */
export interface FileOptions extends RequestOptions {
  /** Guest user whose permissions scope the access (default: the image's user). */
  user?: string;
  /**
   * Expected VM `instanceGeneration`. A mismatch fails with `InvalidState`,
   * so a transfer never lands in a different boot. Default: the VM's
   * current generation (one extra GET).
   */
  generation?: number;
  /** Transfer a directory as a tar stream of its contents. */
  archive?: boolean;
}

/** Options of `writeFile`. */
export interface WriteFileOptions extends FileOptions {
  /**
   * Permission bits: a number (`0o640`) or an octal string (`"0640"`).
   * Files: default keeps an existing file's mode, else 0644. Archives: the
   * mode of a newly created directory (default 0755).
   */
  mode?: number | string;
}

/** Data accepted by `writeFile`. Streams are sent chunked, without buffering, and are never retried. */
export type FileData = Uint8Array | string | ReadableStream<Uint8Array> | AsyncIterable<Uint8Array>;

/** Result of `readFile` and `readSnapshotFile`. */
export interface FileContent {
  data: Uint8Array;
  /** Octal permission bits of the file or archive root, e.g. `"0640"`. */
  mode: string | undefined;
  /** Verified hex SHA-256 of `data` (`readSnapshotFile` of a regular file only). */
  sha256?: string;
}

/** Options of `listSnapshotTree` and `iterSnapshotTree`. */
export interface SnapshotTreeOptions extends PageOptions {
  /** Absolute directory inside the snapshot (default `/`). */
  path?: string;
}

/** Options of `readSnapshotFile` and `readSnapshotFileStream`. */
export interface SnapshotFileOptions extends RequestOptions {
  /** Read a directory as a tar stream of its contents (required for a directory). */
  archive?: boolean;
}

/** Options of `exportSnapshot`. */
export interface ExportSnapshotOptions extends RequestOptions {
  /** `tar` (default): the whole filesystem; `raw`: the disk image byte for byte. */
  format?: "tar" | "raw";
}

/** Options of `createPrompt` (PromptRequest fields except `text`). */
export interface PromptOptions extends RequestOptions {
  /** `claude` (default; alias `claude-code`), `codex`, `opencode` or `pi` (`capabilities.harnesses`). */
  harness?: string;
  /** Continue this conversation. */
  conversationId?: string;
  /** Start a new conversation. */
  new?: boolean;
  model?: string;
  /** Guest working directory of a new conversation (default: the user's home). */
  cwd?: string;
  /** Environment for the harness process (values enter the guest; not stored). */
  env?: Record<string, string>;
  /** Names of daemon-configured secrets passed as same-named environment variables. */
  secrets?: string[];
  /**
   * Files for the turn: `{name, mediaType, path}` for a guest file or
   * `{name, mediaType, data}` with inline base64 bytes (exactly one of
   * `path`/`data`). Accepted types are in `capabilities.harnesses[].attachmentTypes`.
   */
  attachments?: PromptAttachment[];
  /** Harness reasoning effort (`capabilities.harnesses[].reasoningEfforts`), sent as `options.reasoningEffort`. */
  reasoningEffort?: string;
  /** Harness fast mode (`capabilities.harnesses[].fast`), sent as `options.fast`. */
  fast?: boolean;
  /** Bound the turn (server default 3600, at most 86400); it then fails with `turn_timeout`. */
  turnTimeoutSeconds?: number;
  /**
   * Idempotency key (default: a UUIDv4 generated once for this call). The
   * same key and body return the existing turn; a different body throws
   * `IdempotencyConflict`.
   */
  idempotencyKey?: string;
}

/**
 * Options of `createToken`: either `actions` (with optional `vm`) or
 * explicit `scopes`, plus the required `ttlSeconds`.
 */
export interface CreateTokenOptions extends RequestOptions {
  /** Actions granted on every VM in `vm`: `view`, `exec` (implies view), `admin` (implies both). */
  actions?: TokenScope["actions"];
  /** Limit the token to this VM or these VMs (name or ID; one scope each). Default: every VM. */
  vm?: string | string[];
  /** Explicit scopes instead of `actions`/`vm`. */
  scopes?: TokenScope[];
  /** Lifetime in seconds, 1..31536000 (one year). */
  ttlSeconds: number;
  /** Idempotency key (default: a UUIDv4 generated once for this call); a replay within 10 minutes returns the same token. */
  idempotencyKey?: string;
}

/** Options of `createCredentialProfile`. */
export interface CreateCredentialProfileOptions extends RequestOptions {
  /** Make it the owner's default profile (the previous default is cleared). */
  default?: boolean;
  /** Idempotency key (default: a UUIDv4 generated once for this call); a replay of the same body returns the same profile. */
  idempotencyKey?: string;
}

/**
 * Body of `putCredential`. `secret` is write-only (never returned, never sent
 * to a guest); omitted or empty keeps the stored one (a new credential needs one).
 */
export type PutCredentialInput = Omit<PutCredentialRequest, "secret"> & { secret?: string };

/** Options of `createCredentialGrant`. */
export interface CreateCredentialGrantOptions extends RequestOptions {
  /** Idempotency key (default: a UUIDv4 generated once for this call); a replay of the same body returns the same grant. */
  idempotencyKey?: string;
}

/**
 * An environment manifest. Omitted `repos`, `variables`, `secretFiles` and
 * `setup` are sent empty; unknown keys throw (a misspelled key would
 * otherwise vanish from the manifest).
 */
export type ManifestInput = Partial<EnvManifest>;

/** Options of `createEnvironment`. */
export interface CreateEnvironmentOptions extends RequestOptions {
  /** Idempotency key (default: a UUIDv4 generated once for this call). */
  idempotencyKey?: string;
}

/** Options of `createEnvironmentRevision`. */
export interface CreateEnvironmentRevisionOptions extends CreateEnvironmentOptions {
  /** Expected `latestRevision`; a stale value throws `RevisionMismatch` (`details.latestRevision`). */
  ifMatch?: number | string;
}

/** Options of `readProcessOutput`, `iterProcessOutput` and `collectProcessOutput`. */
export interface ReadProcessOutputOptions extends RequestOptions {
  /** Cursor (`nextCursor`) of a previous page; default: the beginning. */
  after?: string | number;
  /** `stdout`, `stderr` or `both` (the server default). */
  stream?: "stdout" | "stderr" | "both" | (string & {});
  /** Chunks per page (the server caps it). */
  limit?: number;
}

/** Result of `collectProcessOutput`. */
export interface RetainedOutput {
  /** The retained stdout bytes, in order. */
  stdout: Uint8Array;
  /** The retained stderr bytes, in order. */
  stderr: Uint8Array;
  /** The process had ended and every retained chunk was read (false while it still runs). */
  complete: boolean;
  /** Resume point (`after`) for a later read; undefined when no chunk was read. */
  cursor: string | undefined;
}

/** Body of `createRun`: the wire request, with `environment` also accepted as `"NAME[@REV]"`. */
export type RunInput = Omit<RunRequest, "environment"> & { environment?: EnvironmentSpec };

/** Options of `createRun`. */
export interface CreateRunOptions extends RequestOptions {
  /**
   * Idempotency key (default: a UUIDv4 generated once for this call). The
   * same key and body return the same run; another body throws `IdempotencyConflict`.
   */
  idempotencyKey?: string;
}

/** Options of `waitRun`. */
export interface WaitRunOptions extends RequestOptions {
  /** Wait budget in milliseconds (default: the client's `waitTimeoutMs`). */
  timeoutMs?: number;
}

/** Options of `pushTree`. */
export interface PushTreeOptions extends RequestOptions {
  /** `merge` (default: add and replace, keep the rest) or `mirror` (this server answers `ServerNotImplemented`). */
  mode?: "merge" | "mirror" | (string & {});
}

/** Options of `runToCompletion`. */
export interface RunToCompletionOptions extends CreateRunOptions, WaitRunOptions {
  /** A ready tar of files to push before the command (the run is then created with `hold`). */
  files?: FileData;
  /** Guest directory for `files` (default `DEFAULT_PUSH_PATH`); also the default `cwd` with files. */
  filesPath?: string;
}

/** Result of `runToCompletion`. */
export interface RunOutcome {
  /** The ended run (`succeeded`, `failed` or `cancelled`). */
  run: Run;
  /**
   * Shell-style status like `davit run`: 130 when cancelled, 124 for its
   * timeout, the command's code (`& 0xff`), 128+n for the signal that killed
   * it (`signal`), else 255.
   */
  exitCode: number;
  stdout: Uint8Array;
  stderr: Uint8Array;
  /** What was pushed (undefined without `files`). */
  push: PushResult | undefined;
  /** The signal that killed the command (`run.signal`), if any. */
  signal: string | undefined;
}

/** Filters of `listEvents` and `iterEvents`. */
export interface EventFilterOptions extends RequestOptions {
  /** Page size. */
  limit?: number;
  /** Event types or prefixes ending in "." (`vm.`, `operation.state`), as an array or a comma-separated string. */
  types?: string | readonly string[];
  /** Only events of this VM (sent as `vmId`). */
  vmId?: string;
  /** Without a cursor: `oldest` (server default) or `latest` (only events after this request). */
  start?: "oldest" | "latest";
}

/** Options of `listEvents` (one page of the host event stream). */
export interface ListHostEventsOptions extends EventFilterOptions {
  /** `nextCursor` of a previous page. */
  cursor?: string;
  /** Long-poll up to this long (0..30000 ms) for the first matching event. */
  waitMs?: number;
}

/** Options of `iterEvents`. */
export interface IterEventsOptions extends EventFilterOptions {
  /** Start after this `nextCursor` (`start` is then ignored). */
  cursor?: string;
  /** Keep long-polling for new events forever instead of stopping at the first empty page. */
  follow?: boolean;
  /** Long-poll budget of each request while following (0..30000 ms, default 25000). */
  waitMs?: number;
}

/** Fields of `createWebhook` besides the URL. */
export interface CreateWebhookOptions extends RequestOptions {
  /** Event types or prefixes ending in "."; omitted or empty means every event. */
  events?: string[];
  /** Free text. */
  description?: string;
  /** Default true. */
  enabled?: boolean;
  /** Idempotency key (default: a UUIDv4 generated once for this call). */
  idempotencyKey?: string;
}

/** Options of `rotateWebhookSecret`. */
export interface RotateWebhookSecretOptions extends RequestOptions {
  /** How long the old secret keeps signing, 0..604800 s (server default 86400; 0 drops it at once). */
  overlapSeconds?: number;
  /** Idempotency key (default: a UUIDv4 generated once for this call): a retried rotation does not rotate twice. */
  idempotencyKey?: string;
}

/** Options of `listWebhookDeliveries` and `iterWebhookDeliveries`. */
export interface ListWebhookDeliveriesOptions extends PageOptions {
  /** Only deliveries in this state. */
  state?: "pending" | "delivered" | "dead";
}

/** Options of `listPromptEvents`. */
export interface ListPromptEventsOptions extends RequestOptions {
  /** Return events with `seq` greater than this (default 0). */
  after?: number;
  /** Long-poll budget in milliseconds (0..30000). */
  waitMs?: number;
}

/** Options of `listVMPromptEvents`. */
export interface ListVMPromptEventsOptions extends RequestOptions {
  /** Opaque feed cursor (`nextCursor` or an item's `cursor`); omitted replays from the oldest event. */
  after?: string;
  /** Only this conversation. */
  conversationId?: string;
  /** Long-poll budget in milliseconds (0..30000). */
  waitMs?: number;
  /** Page size. */
  limit?: number;
}

/** Options of `followVMPromptEvents`. */
export type FollowVMPromptEventsOptions = Omit<ListVMPromptEventsOptions, "waitMs">;

/** Options of `steerPrompt`. */
export interface SteerPromptOptions extends RequestOptions {
  /** Idempotency key (default: a UUIDv4 generated once for this call). */
  idempotencyKey?: string;
}

/** Options of `interruptPrompts`: exactly one of `conversationId` and `all`. */
export interface InterruptOptions extends RequestOptions {
  /** Cancel this conversation's active turn. */
  conversationId?: string;
  /** Cancel every active turn of the caller's on the VM. */
  all?: boolean;
}

/** Options of `followPrompt` and `waitPrompt`. */
export interface FollowPromptOptions extends RequestOptions {
  /** Start after this event seq (default 0: from the first event). */
  after?: number;
  /** Wait budget in milliseconds (default: the client's `waitTimeoutMs`). */
  timeoutMs?: number;
}

/** Options of `waitPrompt` and `runPrompt`. */
export interface WaitPromptOptions extends FollowPromptOptions {
  /** Called once per event, in order. */
  onEvent?: (event: PromptEvent) => void;
}

/** Result of `runPrompt`. */
export interface PromptResult {
  /** The final turn (check `state` and `error`). */
  prompt: Prompt;
  /** Concatenated assistant `message.delta` texts. */
  text: string;
  /** Every event of the turn, in order. */
  events: PromptEvent[];
}

/** Like the CLI: a turn is over once it is neither starting nor running (unknown future states included). */
const PROMPT_ACTIVE = new Set(["starting", "running"]);

interface CallSpec {
  params?: Record<string, string>;
  query?: Array<[string, string]>;
  /** JSON body. */
  body?: unknown;
  /** Raw body with its content type (instead of `body`). */
  raw?: { body: TransportBody; contentType: string };
  /** Return the response body unbuffered. */
  stream?: boolean;
  headers?: Record<string, string>;
  opts?: RequestOptions;
}

type Env = Record<string, string | undefined>;

function hex(bytes: number): string {
  const b = new Uint8Array(bytes);
  globalThis.crypto.getRandomValues(b);
  return Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
}

/** Generates a request ID (`req_<16 hex>`). */
export function newRequestId(): string {
  return `req_${hex(8)}`;
}

/** Generates a random UUIDv4 for use as an idempotency key. */
export function newIdempotencyKey(): string {
  if (typeof globalThis.crypto.randomUUID === "function") return globalThis.crypto.randomUUID();
  const h = hex(16).split("");
  h[12] = "4";
  h[16] = "89ab"[parseInt(h[16]!, 16) & 3]!;
  const s = h.join("");
  return `${s.slice(0, 8)}-${s.slice(8, 12)}-${s.slice(12, 16)}-${s.slice(16, 20)}-${s.slice(20)}`;
}

/** Reports whether a URL hostname is a loopback address. */
export function isLoopbackHost(hostname: string): boolean {
  const h = hostname.replace(/^\[|\]$/g, "").toLowerCase();
  return h === "localhost" || h.endsWith(".localhost") || h === "::1" || /^127\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(h);
}

function isAbs(p: string | undefined): p is string {
  return typeof p === "string" && p.startsWith("/");
}

/** Where the client connects: a unix socket path or an http(s) origin. */
export type Endpoint = { socket: string } | { baseUrl: string };

/**
 * Resolves the default endpoint like the CLI: `$DAVIT_API_URL`
 * (`unix:///abs/path.sock` or an http(s) URL), else the platform socket
 * (`$XDG_RUNTIME_DIR/davit/davit.sock`, `$XDG_DATA_HOME/davit/davit.sock`
 * or `~/.local/share/davit/davit.sock` on Linux;
 * `~/Library/Application Support/davit/davit.sock` on macOS).
 */
export function resolveDefaultEndpoint(env?: Env, platform?: string): Endpoint {
  const proc = (globalThis as { process?: { env?: Env; platform?: string } }).process;
  env ??= proc?.env ?? {};
  platform ??= proc?.platform ?? "";
  const url = env[API_URL_ENV];
  if (url) {
    if (url.startsWith("unix:")) {
      const path = url.startsWith("unix://") ? url.slice("unix://".length) : url.slice("unix:".length);
      if (!isAbs(path)) throw new TypeError(`${API_URL_ENV} must be unix:///absolute/path.sock or an https URL`);
      return { socket: path };
    }
    return { baseUrl: url };
  }
  const sock = `${PRODUCT}.sock`;
  const home = env.HOME;
  if (platform === "darwin") {
    if (!isAbs(home)) throw new TypeError("cannot find the default socket: HOME is not set; pass socket or baseUrl");
    return { socket: `${home}/Library/Application Support/${PRODUCT}/${sock}` };
  }
  if (isAbs(env.XDG_RUNTIME_DIR)) return { socket: `${env.XDG_RUNTIME_DIR}/${PRODUCT}/${sock}` };
  if (isAbs(env.XDG_DATA_HOME)) return { socket: `${env.XDG_DATA_HOME}/${PRODUCT}/${sock}` };
  if (isAbs(home)) return { socket: `${home}/.local/share/${PRODUCT}/${sock}` };
  throw new TypeError("cannot find the default socket; pass socket or baseUrl (or set " + API_URL_ENV + ")");
}

function processEnv(): Env {
  return (globalThis as { process?: { env?: Env } }).process?.env ?? {};
}

type GetBuiltin = <T>(id: string) => T;

/**
 * Reads and checks the CA bundle (`caFile`, else `$DAVIT_CA_FILE`) synchronously
 * through `process.getBuiltinModule` (Node ≥ 20.16, Bun), so construction fails
 * early and bundles without node:fs stay importable. Undefined when none is set.
 */
function loadCAFile(caFile: string | undefined, env: Env): string | undefined {
  const what = caFile !== undefined ? "caFile" : CA_FILE_ENV;
  const path = caFile ?? (env[CA_FILE_ENV] || undefined);
  if (path === undefined) return undefined;
  const getBuiltin = (globalThis as { process?: { getBuiltinModule?: GetBuiltin } }).process?.getBuiltinModule;
  if (typeof getBuiltin !== "function") throw new TypeError(`${what} needs Node >= 20.16 or Bun (process.getBuiltinModule)`);
  const fs = getBuiltin<typeof import("node:fs")>("node:fs");
  const crypto = getBuiltin<typeof import("node:crypto")>("node:crypto");
  let pem: string;
  try {
    pem = fs.readFileSync(path, "utf8");
  } catch (err) {
    const code = (err as { code?: string }).code;
    throw new TypeError(`${what} ${JSON.stringify(path)}: cannot read${code ? ` (${code})` : ""}: ${err instanceof Error ? err.message : String(err)}`);
  }
  const blocks = pem.match(/-----BEGIN CERTIFICATE-----[\s\S]*?-----END CERTIFICATE-----/g);
  if (!blocks) throw new TypeError(`${what} ${JSON.stringify(path)}: no PEM certificate found`);
  for (const block of blocks) {
    try {
      new crypto.X509Certificate(block);
    } catch (err) {
      throw new TypeError(`${what} ${JSON.stringify(path)}: invalid PEM bundle: ${err instanceof Error ? err.message : String(err)}`);
    }
  }
  return blocks.join("\n") + "\n";
}

/** Parses and validates a base URL; returns its origin. */
function checkBaseUrl(baseUrl: string): { origin: string; secure: boolean } {
  let u: URL;
  try {
    u = new URL(baseUrl);
  } catch {
    throw new TypeError(`invalid baseUrl ${JSON.stringify(baseUrl)}`);
  }
  if (u.protocol === "https:") return { origin: u.origin, secure: true };
  if (u.protocol === "http:") {
    if (!isLoopbackHost(u.hostname)) {
      throw new TypeError(`refusing plain http to non-loopback host ${u.hostname}; use https`);
    }
    return { origin: u.origin, secure: false };
  }
  throw new TypeError(`baseUrl must be https:// (or http:// for loopback), got ${u.protocol}`);
}

function checkPort(port: number, name: string): void {
  if (!Number.isInteger(port) || port < 1 || port > 65535) throw new TypeError(`${name} must be an integer in 1..65535, got ${String(port)}`);
}

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason);
    const t = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(t);
      reject(signal!.reason);
    };
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

/** Wraps a streamed 2xx file response with its size, mode and digest checks. */
function fileReadStream(resp: TransportResponse, archive: boolean, verifySha256: boolean): FileReadStream {
  const len = resp.headers["content-length"];
  const size = len !== undefined && /^\d+$/.test(len) ? Number(len) : undefined;
  return new FileReadStream(resp.stream!, resp.headers["x-davit-file-mode"], size, archive, {
    sha256: resp.headers["x-davit-content-sha256"],
    verifySha256,
  });
}

function checkAbsolute(path: string): string {
  if (typeof path !== "string" || !path.startsWith("/")) throw new TypeError(`path must be absolute, got ${JSON.stringify(path)}`);
  return path;
}

/** The `waitSeconds` query of createVM/resumeVM; none for an omitted or 0 wait. */
function capacityWaitQuery(seconds: number | undefined): Array<[string, string]> {
  if (seconds === undefined) return [];
  if (!Number.isInteger(seconds) || seconds < 0 || seconds > MAX_CAPACITY_WAIT_SECONDS) {
    throw new TypeError(`capacityWaitSeconds must be an integer in 0..${MAX_CAPACITY_WAIT_SECONDS}, got ${String(seconds)}`);
  }
  return seconds > 0 ? [["waitSeconds", String(seconds)]] : [];
}

function isTerminal(state: string): boolean {
  return state === "succeeded" || state === "failed";
}

/**
 * Client for the davit v1 API.
 *
 * ```ts
 * const client = new DavitClient();                       // local socket
 * const vm = await client.createVM({ name: "web" });        // waits until ready
 * ```
 */
export class DavitClient {
  readonly #transport: Transport;
  readonly #token: string | undefined;
  readonly #waitTimeoutMs: number;
  /** Where requests go (`{socket}` or `{baseUrl}`), or undefined for a custom transport. */
  readonly endpoint: Endpoint | undefined;

  constructor(options: ClientOptions = {}) {
    this.#waitTimeoutMs = options.waitTimeoutMs ?? DEFAULT_WAIT_TIMEOUT_MS;
    if (options.socket !== undefined && options.baseUrl !== undefined) {
      throw new TypeError("pass either socket or baseUrl, not both");
    }
    if (options.transport) {
      if (options.caFile !== undefined) throw new TypeError("caFile does not apply to a custom transport");
      this.#token = options.token || undefined;
      this.#transport = options.transport;
      this.endpoint = undefined;
      return;
    }
    const ep: Endpoint =
      options.socket !== undefined
        ? { socket: options.socket }
        : options.baseUrl !== undefined
          ? { baseUrl: options.baseUrl }
          : resolveDefaultEndpoint();
    const env = processEnv();
    if ("socket" in ep) {
      if (!isAbs(ep.socket)) throw new TypeError(`socket must be an absolute path, got ${JSON.stringify(ep.socket)}`);
      // The socket is owner-only: $DAVIT_TOKEN and $DAVIT_CA_FILE never apply to it.
      if (options.caFile !== undefined) throw new TypeError("caFile applies to https base URLs, not to the local socket");
      this.#token = options.token || undefined;
      this.#transport = new UnixSocketTransport(ep.socket);
      this.endpoint = ep;
    } else {
      const { origin, secure } = checkBaseUrl(ep.baseUrl);
      this.#token = (options.token ?? env[TOKEN_ENV]) || undefined;
      const ca = loadCAFile(options.caFile, env);
      this.#transport = secure && ca !== undefined ? new HttpsTransport(origin, { ca }) : new FetchTransport(origin);
      this.endpoint = { baseUrl: origin };
    }
  }

  // ---- plumbing --------------------------------------------------------

  async #call<T>(op: OperationName, spec: CallSpec = {}): Promise<T> {
    const { resp, requestId } = await this.#send(op, spec);
    return this.#decode<T>(resp, requestId);
  }

  /** Like #call but returns a 2xx response with its body unbuffered in `stream`. */
  async #callStream(op: OperationName, spec: CallSpec): Promise<TransportResponse> {
    const { resp, requestId } = await this.#send(op, { ...spec, stream: true });
    if (resp.status >= 200 && resp.status <= 299) {
      if (resp.stream) return resp;
      throw new TypeError(`${op}: the transport returned no stream for responseType "stream"`);
    }
    // An error answer: read a bounded body and decode it (this throws).
    const parts: Uint8Array[] = [];
    let n = 0;
    if (resp.stream) {
      try {
        for await (const c of resp.stream) {
          parts.push(c);
          n += c.length;
          if (n >= 1 << 20) break;
        }
      } catch {
        // decode what arrived
      }
    }
    const text = resp.stream ? new TextDecoder().decode(Buffer.concat(parts).subarray(0, 1 << 20)) : resp.body;
    return this.#decode<never>({ ...resp, body: text }, requestId);
  }

  async #send(op: OperationName, spec: CallSpec): Promise<{ resp: TransportResponse; requestId: string }> {
    const [method, template] = OPERATIONS[op];
    const path = template.replace(/\{(\w+)\}/g, (_, name: string) => {
      const v = spec.params?.[name];
      if (v === undefined || v === "") throw new TypeError(`${op}: missing ${name}`);
      return encodeURIComponent(v);
    });
    const qs = spec.query?.length ? "?" + new URLSearchParams(spec.query).toString() : "";
    const requestId = spec.opts?.requestId ?? newRequestId();
    const headers: Record<string, string> = { Accept: "application/json", "X-Request-Id": requestId, ...spec.headers };
    let body: TransportBody | undefined;
    if (spec.raw !== undefined) {
      body = spec.raw.body;
      headers["Content-Type"] = spec.raw.contentType;
    } else if (spec.body !== undefined) {
      body = JSON.stringify(spec.body);
      headers["Content-Type"] = "application/json";
    }
    if (this.#token) headers.Authorization = `Bearer ${this.#token}`;
    const signal = spec.opts?.signal;
    // A streamed body cannot be sent twice.
    const replayable = body === undefined || typeof body === "string" || body instanceof Uint8Array;

    for (let attempt = 1; ; attempt++) {
      try {
        const resp = await this.#transport.request({
          method,
          path: BASE_PATH + path + qs,
          headers,
          body,
          signal,
          responseType: spec.stream ? "stream" : "text",
        });
        return { resp, requestId };
      } catch (err) {
        if (signal?.aborted) throw signal.reason;
        const te =
          err instanceof TransportError
            ? err
            : new TransportError(`transport failed: ${err instanceof Error ? err.message : String(err)}`, { cause: err });
        if (!te.connectFailed || !replayable || attempt >= MAX_ATTEMPTS) throw te;
        await sleep(100 * 2 ** (attempt - 1), signal);
      }
    }
  }

  #decode<T>(resp: TransportResponse, sentRequestId: string): T {
    const headerRequestId = resp.headers["x-request-id"] ?? sentRequestId;
    if (resp.status < 200 || resp.status > 299) {
      let parsed: Partial<ErrorResponse> | undefined;
      try {
        parsed = JSON.parse(resp.body) as Partial<ErrorResponse>;
      } catch {
        parsed = undefined;
      }
      const e = parsed?.error;
      if (!e || typeof e.code !== "string" || e.code === "") {
        throw new DavitError({
          status: resp.status,
          code: "internal",
          message: `unexpected HTTP ${resp.status} from server`,
          requestId: headerRequestId,
        });
      }
      throw errorFor({
        status: resp.status,
        code: e.code,
        message: e.message ?? "",
        retryable: e.retryable ?? false,
        details: e.details,
        requestId: e.requestId || headerRequestId,
        operationId: e.operationId,
      });
    }
    if (resp.status === 204 || resp.body === "") return undefined as T;
    try {
      return JSON.parse(resp.body) as T;
    } catch (err) {
      throw new DavitError({
        status: resp.status,
        code: "internal",
        message: `decoding response: ${err instanceof Error ? err.message : String(err)}`,
        requestId: headerRequestId,
        cause: err,
      });
    }
  }

  #mutationHeaders(opts: MutationOptions): Record<string, string> {
    const h: Record<string, string> = { "Idempotency-Key": opts.idempotencyKey ?? newIdempotencyKey() };
    if (opts.ifMatch !== undefined) h["If-Match"] = String(opts.ifMatch);
    return h;
  }

  async #mutate(op: OperationName, params: Record<string, string>, body: unknown, opts: MutationOptions, query?: Array<[string, string]>): Promise<OperationAccepted> {
    return this.#call<OperationAccepted>(op, { params, query, body, headers: this.#mutationHeaders(opts), opts });
  }

  async #lifecycle(op: OperationName, params: Record<string, string>, body: unknown, opts: MutationOptions, query?: Array<[string, string]>): Promise<VM | OperationAccepted> {
    const acc = await this.#mutate(op, params, body, opts, query);
    if (opts.wait === false) return acc;
    const done = await this.wait(acc.operation.id, opts);
    return this.getVM(done.vmId || acc.operation.vmId, { signal: opts.signal });
  }

  async *#paginate<T>(fetchPage: (cursor: string | undefined) => Promise<{ items: T[]; nextCursor?: string }>, cursor?: string): AsyncGenerator<T, void, undefined> {
    for (;;) {
      const page = await fetchPage(cursor);
      yield* page.items;
      if (!page.nextCursor) return;
      cursor = page.nextCursor;
    }
  }

  static #page(opts: PageOptions): Array<[string, string]> {
    const q: Array<[string, string]> = [];
    if (opts.limit !== undefined) q.push(["limit", String(opts.limit)]);
    if (opts.cursor) q.push(["cursor", opts.cursor]);
    return q;
  }

  // ---- capabilities and images ----------------------------------------

  /** Reports what this host and runtime support. */
  getCapabilities(opts: RequestOptions = {}): Promise<HostCapabilities> {
    return this.#call("getCapabilities", { opts });
  }

  /** Lists one page of images. */
  listImages(opts: PageOptions = {}): Promise<ImageList> {
    return this.#call("listImages", { query: DavitClient.#page(opts), opts });
  }

  /** Iterates over all images, following `nextCursor`. */
  iterImages(opts: Omit<PageOptions, "cursor"> = {}): AsyncGenerator<Image, void, undefined> {
    return this.#paginate((cursor) => this.listImages({ ...opts, cursor }));
  }

  /** Gets one image by ID. */
  getImage(imageId: string, opts: RequestOptions = {}): Promise<Image> {
    return this.#call("getImage", { params: { imageId }, opts });
  }

  // ---- VMs ---------------------------------------------------------------

  /** Lists one page of VMs (by default provisioning, ready and running ones). */
  listVMs(opts: ListVMsOptions = {}): Promise<VMList> {
    const q = DavitClient.#page(opts);
    for (const s of opts.states ?? []) q.push(["state", s]);
    if (opts.all) q.push(["all", "true"]);
    for (const [k, v] of Object.entries(opts.labels ?? {})) q.push(["label", `${k}=${v}`]);
    return this.#call("listVMs", { query: q, opts });
  }

  /** Iterates over all matching VMs, following `nextCursor`. */
  iterVMs(opts: Omit<ListVMsOptions, "cursor"> = {}): AsyncGenerator<VM, void, undefined> {
    return this.#paginate((cursor) => this.listVMs({ ...opts, cursor }));
  }

  /**
   * Renames a VM, changes its TTL and/or replaces its egress or privacy
   * policy (synchronous; returns the VM). Only the fields given change.
   * `ttlSeconds` sets the TTL and, while the VM is ready or running, restarts
   * the auto-stop countdown from now; `autoStop: true` alone restarts it with
   * the current TTL; `egress` is live on a running VM (see `setVMEgress`).
   * `privacy` replaces the whole policy (flags left out are off): read the VM
   * and change one flag to keep the others (feature `privacyPolicy`).
   * Naturally idempotent: sends no Idempotency-Key. Supports `ifMatch`.
   */
  updateVM(vm: string, req: UpdateVMRequest, opts: UpdateVMOptions = {}): Promise<VM> {
    const body: UpdateVMRequest = {};
    if (req.name !== undefined) body.name = req.name;
    if (req.ttlSeconds !== undefined) body.ttlSeconds = req.ttlSeconds;
    if (req.autoStop !== undefined) body.autoStop = req.autoStop;
    if (req.egress !== undefined) body.egress = egressBody(req.egress);
    if (req.privacy !== undefined) body.privacy = { ...req.privacy };
    const headers: Record<string, string> = {};
    if (opts.ifMatch !== undefined) headers["If-Match"] = String(opts.ifMatch);
    return this.#call("updateVM", { params: { vmId: vm }, body, headers, opts });
  }

  /**
   * Extends a VM's life like `davit extend`: re-enables auto-stop and
   * restarts its countdown from now, optionally with a new TTL.
   */
  async extendVM(vm: string, opts: ExtendOptions = {}): Promise<VM> {
    const req: UpdateVMRequest = { autoStop: true };
    if (opts.ttlSeconds !== undefined) {
      if (!Number.isInteger(opts.ttlSeconds) || opts.ttlSeconds <= 0) {
        throw new TypeError(`ttlSeconds must be an integer > 0, got ${String(opts.ttlSeconds)}`);
      }
      req.ttlSeconds = opts.ttlSeconds;
    }
    return this.updateVM(vm, req, opts);
  }

  /** Renames a VM (like `davit rename`). */
  renameVM(vm: string, newName: string, opts: UpdateVMOptions = {}): Promise<VM> {
    return this.updateVM(vm, { name: newName }, opts);
  }

  /** Gets a VM by ID or name (deleted tombstones resolve by ID only). */
  getVM(vm: string, opts: RequestOptions = {}): Promise<VM> {
    return this.#call("getVM", { params: { vmId: vm }, opts });
  }

  /**
   * Creates a VM from an image or a snapshot. Waits until the operation
   * finishes and returns the VM, unless `wait: false`. `req.egress` sets its
   * egress policy (default: the host's `network.egress`, else `filtered`).
   * `req.environment` (`EnvironmentRef` or `"NAME[@REV]"`) pins an environment.
   * `req.privacy` sets its privacy policy (feature `privacyPolicy`), e.g.
   * `{ ephemeral: true }` deletes the VM when it stops; not secure erasure.
   * `opts.capacityWaitSeconds` queues the request for room on a full host.
   */
  createVM(req: CreateVMInput, opts: CreateVMOptions & { wait: false }): Promise<OperationAccepted>;
  createVM(req?: CreateVMInput, opts?: CreateVMOptions & { wait?: true }): Promise<VM>;
  createVM(req?: CreateVMInput, opts?: CreateVMOptions): Promise<VM | OperationAccepted>;
  async createVM(req: CreateVMInput = {}, opts: CreateVMOptions = {}): Promise<VM | OperationAccepted> {
    const query = capacityWaitQuery(opts.capacityWaitSeconds);
    const { environment, ...rest } = req;
    const body: CreateVMRequest = rest;
    if (environment !== undefined) body.environment = parseEnvironmentRef(environment);
    return this.#lifecycle("createVM", {}, body, opts, query);
  }

  /**
   * Creates `count` VMs (1-100) from one template (feature `batchCreate`).
   * A template `name` N names them N-1 ... N-count (unnamed without it). The
   * template is validated once (an invalid one throws and creates nothing);
   * each item is admitted on its own, so the result has one item per VM, in
   * order, with its `accepted` operation or its `error` (e.g.
   * `resource_exhausted`). Each item is idempotent on the batch key plus its
   * index: the same `idempotencyKey` returns the same operations and creates
   * only the items refused before.
   */
  async createVMs(count: number, template: CreateVMRequest = {}, opts: BatchOptions = {}): Promise<CreateVMsResult> {
    const headers = { "Idempotency-Key": opts.idempotencyKey ?? newIdempotencyKey() };
    const res = await this.#call<CreateVMsResult>("createVMs", { body: { count, template }, headers, opts });
    if (!opts.wait) return res;
    const deadline = Date.now() + (opts.timeoutMs ?? this.#waitTimeoutMs);
    const items = [];
    for (const item of res.items) {
      if (!item.accepted) {
        items.push(item);
        continue;
      }
      let op: Operation;
      try {
        op = await this.wait(item.accepted.operation.id, { ...opts, timeoutMs: Math.max(0, deadline - Date.now()) });
      } catch (err) {
        if (!(err instanceof OperationFailed)) throw err;
        op = err.operation;
      }
      const vm = op.state === "succeeded" ? await this.getVM(op.vmId, { signal: opts.signal }) : item.accepted.vm;
      items.push({ ...item, accepted: { operation: op, vm } });
    }
    return { items };
  }

  /**
   * Stops your VMs that have been idle at least `idleSeconds` (`davit box
   * gc`; feature `capacityReclaim`). A VM qualifies when it is ready with no
   * operation, runs no process, has no stream or tunnel open, holds no
   * prompt-turn lease and has had no exec or tunnel activity for
   * `idleSeconds`; ephemeral and no-snapshot VMs never do, and no idle timeout
   * is needed. Each is stopped (`Operation.reason` `gc`), oldest activity
   * first, and listed with its stop's `operationId`; the stops are not waited
   * for (use `wait`). `dryRun` only lists them. Sends no Idempotency-Key:
   * each stop is keyed by its VM's generation and revision, so a repeat stops
   * nothing twice.
   */
  reclaimVMs(idleSeconds: number, opts: ReclaimVMsOptions = {}): Promise<ReclaimVMsResult> {
    const body: ReclaimVMsRequest = { idleSeconds };
    if (opts.labels !== undefined) body.labels = { ...opts.labels };
    if (opts.dryRun) body.dryRun = true;
    return this.#call("reclaimVMs", { body, opts });
  }

  /** Deletes a VM. Waits and returns the deleted tombstone, unless `wait: false`. */
  deleteVM(vm: string, opts: NoWait): Promise<OperationAccepted>;
  deleteVM(vm: string, opts?: Waiting): Promise<VM>;
  deleteVM(vm: string, opts?: MutationOptions): Promise<VM | OperationAccepted>;
  deleteVM(vm: string, opts: MutationOptions = {}): Promise<VM | OperationAccepted> {
    return this.#lifecycle("deleteVM", { vmId: vm }, undefined, opts);
  }

  /** Stops a VM, committing its disk. Stopping a stopped VM is a no-op. */
  stopVM(vm: string, opts: StopVMOptions & { wait: false }): Promise<OperationAccepted>;
  stopVM(vm: string, opts?: StopVMOptions & { wait?: true }): Promise<VM>;
  stopVM(vm: string, opts?: StopVMOptions): Promise<VM | OperationAccepted>;
  stopVM(vm: string, opts: StopVMOptions = {}): Promise<VM | OperationAccepted> {
    return this.#lifecycle("stopVM", { vmId: vm }, opts.force ? { force: true } : {}, opts);
  }

  /**
   * Resumes a stopped or errored VM, optionally resized (`type`, `shape`).
   * Resuming a ready VM is a no-op. Making room and `capacityWaitSeconds`
   * work as on `createVM`.
   */
  resumeVM(vm: string, opts: ResumeVMOptions & { wait: false }): Promise<OperationAccepted>;
  resumeVM(vm: string, opts?: ResumeVMOptions & { wait?: true }): Promise<VM>;
  resumeVM(vm: string, opts?: ResumeVMOptions): Promise<VM | OperationAccepted>;
  async resumeVM(vm: string, opts: ResumeVMOptions = {}): Promise<VM | OperationAccepted> {
    const query = capacityWaitQuery(opts.capacityWaitSeconds);
    const body: ResumeVMRequest = {};
    if (opts.type !== undefined) body.type = opts.type;
    if (opts.shape !== undefined) body.shape = opts.shape;
    if (opts.ttlSeconds !== undefined) body.ttlSeconds = opts.ttlSeconds;
    if (opts.autoStop !== undefined) body.autoStop = opts.autoStop;
    if (opts.environment !== undefined) body.environment = parseEnvironmentRef(opts.environment);
    return this.#lifecycle("resumeVM", { vmId: vm }, body, opts, query);
  }

  /**
   * Power-cycles a wedged VM on its current disk (feature `recover`): the VMM
   * is killed without a RAM or disk commit and the disk cold boots as is, so
   * files survive crash-consistent (like a power loss), processes do not, and
   * no snapshot is taken. For ready, running and error VMs that still have a
   * working disk; a stopped VM is refused (`invalid_state`: use `resumeVM`).
   */
  recoverVM(vm: string, opts: NoWait): Promise<OperationAccepted>;
  recoverVM(vm: string, opts?: Waiting): Promise<VM>;
  recoverVM(vm: string, opts?: MutationOptions): Promise<VM | OperationAccepted>;
  recoverVM(vm: string, opts: MutationOptions = {}): Promise<VM | OperationAccepted> {
    return this.#lifecycle("recoverVM", { vmId: vm }, {}, opts);
  }

  /** Creates a new VM from `source`'s last committed disk or one of its snapshots. */
  forkVM(source: string, opts: ForkVMOptions & { wait: false }): Promise<OperationAccepted>;
  forkVM(source: string, opts?: ForkVMOptions & { wait?: true }): Promise<VM>;
  forkVM(source: string, opts?: ForkVMOptions): Promise<VM | OperationAccepted>;
  async forkVM(source: string, opts: ForkVMOptions = {}): Promise<VM | OperationAccepted> {
    const body: ForkVMRequest = {};
    if (opts.name !== undefined) body.name = opts.name;
    if (opts.type !== undefined) body.type = opts.type;
    if (opts.snapshot !== undefined) body.snapshotId = opts.snapshot;
    if (opts.shape !== undefined) body.shape = opts.shape;
    if (opts.ttlSeconds !== undefined) body.ttlSeconds = opts.ttlSeconds;
    if (opts.autoStop !== undefined) body.autoStop = opts.autoStop;
    if (opts.live) body.live = true;
    if (opts.egress !== undefined) body.egress = opts.egress;
    if (opts.environment !== undefined) body.environment = parseEnvironmentRef(opts.environment);
    return this.#lifecycle("forkVM", { vmId: source }, body, opts);
  }

  /**
   * Replaces a VM's egress policy and returns the VM (`updateVM` with only
   * `egress`). Live on a running VM
   * (new connections and DNS queries; flows the new policy no longer allows
   * are closed), kept for later boots of a stopped one. `allow` (mode
   * `allowlist` only) takes domains (`github.com` also matches subdomains,
   * `*.example.com` only subdomains) and IPv4 addresses/CIDRs; the VM reports
   * them normalized (lowercase, masked, sorted, deduplicated). Synchronous and
   * naturally idempotent (no Idempotency-Key). A bad mode or entry throws
   * `InvalidArgument`; a running lifecycle operation `OperationInProgress`
   * (retryable); a running VM that cannot be updated `Unavailable` (the old
   * policy stays).
   */
  async setVMEgress(vm: string, egress: Egress, opts: UpdateVMOptions = {}): Promise<VM> {
    return this.updateVM(vm, { egress }, opts);
  }

  // ---- snapshots ---------------------------------------------------------

  /** Lists one page of snapshots whose source is `vm`. */
  listVMSnapshots(vm: string, opts: PageOptions = {}): Promise<SnapshotList> {
    return this.#call("listVMSnapshots", { params: { vmId: vm }, query: DavitClient.#page(opts), opts });
  }

  /** Iterates over all snapshots of `vm`. */
  iterVMSnapshots(vm: string, opts: Omit<PageOptions, "cursor"> = {}): AsyncGenerator<Snapshot, void, undefined> {
    return this.#paginate((cursor) => this.listVMSnapshots(vm, { ...opts, cursor }));
  }

  /**
   * Names the committed disk of a stopped VM (operationId `snapshotVM`).
   * Waits and returns the Snapshot, unless `wait: false`.
   */
  createSnapshot(vm: string, name: string, opts: NoWait): Promise<OperationAccepted>;
  createSnapshot(vm: string, name: string, opts?: Waiting): Promise<Snapshot>;
  createSnapshot(vm: string, name: string, opts?: MutationOptions): Promise<Snapshot | OperationAccepted>;
  async createSnapshot(vm: string, name: string, opts: MutationOptions = {}): Promise<Snapshot | OperationAccepted> {
    const acc = await this.#mutate("snapshotVM", { vmId: vm }, { name }, opts);
    if (opts.wait === false) return acc;
    const done = await this.wait(acc.operation.id, opts);
    const id = done.result?.snapshotId;
    if (!id) {
      throw new DavitError({ code: "internal", message: `snapshot operation ${done.id} succeeded without a snapshotId`, operationId: done.id });
    }
    return this.getSnapshot(id, { signal: opts.signal });
  }

  /** Lists one page of snapshots visible to the caller. */
  listSnapshots(opts: PageOptions = {}): Promise<SnapshotList> {
    return this.#call("listSnapshots", { query: DavitClient.#page(opts), opts });
  }

  /** Iterates over all visible snapshots. */
  iterSnapshots(opts: Omit<PageOptions, "cursor"> = {}): AsyncGenerator<Snapshot, void, undefined> {
    return this.#paginate((cursor) => this.listSnapshots({ ...opts, cursor }));
  }

  /** Gets a snapshot by ID or name. */
  getSnapshot(snapshot: string, opts: RequestOptions = {}): Promise<Snapshot> {
    return this.#call("getSnapshot", { params: { snapshotId: snapshot }, opts });
  }

  /** Deletes a snapshot reference (synchronous; a repeat throws NotFound). */
  async deleteSnapshot(snapshot: string, opts: RequestOptions = {}): Promise<void> {
    await this.#call<undefined>("deleteSnapshot", { params: { snapshotId: snapshot }, opts });
  }

  // ---- snapshot files (read without booting) -----------------------------

  /**
   * Lists one page of the entries of directory `path` (default `/`) in a
   * snapshot (ID or name), read on the host without booting it. Paths resolve
   * inside the snapshot (`..` stops at `/`, symlinks stay within it); a symlink
   * named by `path` is followed and symlink entries carry `linkTarget`.
   */
  async listSnapshotTree(snapshot: string, opts: SnapshotTreeOptions = {}): Promise<SnapshotTree> {
    const path = checkAbsolute(opts.path ?? "/");
    const query: Array<[string, string]> = [["path", path], ...DavitClient.#page(opts)];
    return this.#call("listSnapshotTree", { params: { snapshotId: snapshot }, query, opts });
  }

  /** Iterates every entry of directory `path` in a snapshot, following `nextCursor`. */
  iterSnapshotTree(snapshot: string, opts: Omit<SnapshotTreeOptions, "cursor"> = {}): AsyncGenerator<SnapshotEntry, void, undefined> {
    return this.#paginate((cursor) => this.listSnapshotTree(snapshot, { ...opts, cursor }));
  }

  /**
   * Reads a snapshot file (or, with `archive: true`, a directory as tar) into
   * memory. A regular file is checked against `Content-Length` and its
   * `X-Davit-Content-Sha256` digest (returned as `sha256`); a mismatch or a
   * cut-short transfer throws TransportError.
   */
  async readSnapshotFile(snapshot: string, path: string, opts: SnapshotFileOptions = {}): Promise<FileContent> {
    const stream = await this.readSnapshotFileStream(snapshot, path, opts);
    const data = await stream.bytes();
    const out: FileContent = { data, mode: stream.mode };
    if (!stream.archive && stream.sha256 !== undefined) out.sha256 = stream.sha256;
    return out;
  }

  /**
   * Reads a snapshot file (or a directory as tar) without buffering it.
   * Iterate the result once; the iteration throws TransportError if the
   * transfer was cut short or a file's bytes do not match
   * `X-Davit-Content-Sha256`. An archive's digest is an HTTP trailer and is
   * not read.
   */
  async readSnapshotFileStream(snapshot: string, path: string, opts: SnapshotFileOptions = {}): Promise<FileReadStream> {
    const query: Array<[string, string]> = [["path", checkAbsolute(path)]];
    if (opts.archive) query.push(["archive", "tar"]);
    const resp = await this.#callStream("readSnapshotFile", { params: { snapshotId: snapshot }, query, opts, headers: { Accept: "*/*" } });
    return fileReadStream(resp, opts.archive === true, true);
  }

  /**
   * Streams a snapshot export without booting it: `format: "tar"` (default)
   * is the whole filesystem (checked for its tar end marker), `"raw"` the disk
   * image byte for byte (a chunked stream; writing it sparse is up to you). Exports are disk-sized, so there is no buffered variant. The
   * stream digest is an HTTP trailer and is not read.
   */
  async exportSnapshot(snapshot: string, opts: ExportSnapshotOptions = {}): Promise<FileReadStream> {
    const format = opts.format ?? "tar";
    if (format !== "tar" && format !== "raw") throw new TypeError(`format must be "tar" or "raw", got ${JSON.stringify(format)}`);
    const resp = await this.#callStream("exportSnapshot", { params: { snapshotId: snapshot }, query: [["format", format]], opts, headers: { Accept: "*/*" } });
    return fileReadStream(resp, format === "tar", false);
  }

  // ---- processes -------------------------------------------------------

  /**
   * Starts a process in the VM. `argv` runs directly; no shell is inferred
   * (use `["sh", "-c", "..."]`). Returns the process; follow its output with
   * `attachProcess`, or use `run` for the common case.
   */
  async execProcess(vm: string, argv: string[], opts: ExecOptions = {}): Promise<Process> {
    if (!Array.isArray(argv) || argv.length === 0 || argv.some((a) => typeof a !== "string")) {
      throw new TypeError("argv must be a non-empty array of strings");
    }
    if (opts.deadlineSeconds !== undefined && !(Number.isInteger(opts.deadlineSeconds) && opts.deadlineSeconds >= 1)) {
      throw new TypeError(`deadlineSeconds must be an integer >= 1, got ${String(opts.deadlineSeconds)}`);
    }
    const body: ExecRequest = { argv: [...argv] };
    if (opts.cwd !== undefined) body.cwd = opts.cwd;
    if (opts.env !== undefined) body.env = opts.env;
    if (opts.user !== undefined) body.user = opts.user;
    if (opts.deadlineSeconds !== undefined) body.deadlineSeconds = opts.deadlineSeconds;
    if (opts.detached !== undefined) body.detached = opts.detached;
    if (opts.stdin !== undefined) body.stdin = opts.stdin;
    if (opts.tty !== undefined) body.tty = opts.tty;
    const headers = { "Idempotency-Key": opts.idempotencyKey ?? newIdempotencyKey() };
    return this.#call("execProcess", { params: { vmId: vm }, body, headers, opts });
  }

  /**
   * Lists one page of the VM's recorded processes, oldest first in
   * `(createdAt, id)` order. A start interrupted by a daemon crash is `lost`.
   */
  listProcesses(vm: string, opts: PageOptions = {}): Promise<ProcessList> {
    return this.#call("listProcesses", { params: { vmId: vm }, query: DavitClient.#page(opts), opts });
  }

  /** Iterates over all the VM's processes (oldest first), following `nextCursor`. */
  iterProcesses(vm: string, opts: Omit<PageOptions, "cursor"> = {}): AsyncGenerator<Process, void, undefined> {
    return this.#paginate((cursor) => this.listProcesses(vm, { ...opts, cursor }));
  }

  /** Gets one process. */
  getProcess(vm: string, processId: string, opts: RequestOptions = {}): Promise<Process> {
    return this.#call("getProcess", { params: { vmId: vm, processId }, opts });
  }

  /**
   * Signals a process group and returns the process. `signal` is a name with
   * or without `SIG` (HUP, INT, QUIT, KILL, TERM, USR1, USR2, CONT, STOP,
   * WINCH); omitted, the server sends SIGTERM. Naturally idempotent (no
   * Idempotency-Key); signalling a finished process throws `InvalidState`.
   */
  killProcess(vm: string, processId: string, signal?: string, opts: RequestOptions = {}): Promise<Process> {
    const query: Array<[string, string]> = signal !== undefined ? [["signal", signal]] : [];
    return this.#call("killProcess", { params: { vmId: vm, processId }, query, opts });
  }

  /**
   * Attaches to a process's stdio over a WebSocket. Iterate the returned
   * stream for output and the final exit; it re-attaches from its cursor if
   * the connection drops. Needs a socket or baseUrl endpoint (or a custom
   * transport with `connect`); Node and Bun only.
   */
  async attachProcess(vm: string, processId: string, opts: AttachOptions = {}): Promise<ProcessStream> {
    const start = opts.outputCursor ?? 0;
    const open = (cursor: number) => this.#openStream(vm, processId, cursor, opts);
    return new ProcessStream(vm, processId, start, await open(start), open);
  }

  async #openStream(vm: string, processId: string, cursor: number, opts: RequestOptions): Promise<WebSocketConnection> {
    const path = BASE_PATH + OPERATIONS.attachProcess[1]
      .replace("{vmId}", encodeURIComponent(vm))
      .replace("{processId}", encodeURIComponent(processId)) + `?outputCursor=${Math.max(0, Math.floor(cursor))}`;
    return this.#openWebSocket("attachProcess", path, opts);
  }

  /** Dials the endpoint (retrying only unreached connects) and upgrades `path`. */
  async #openWebSocket(op: OperationName, path: string, opts: RequestOptions): Promise<WebSocketConnection> {
    const transport = this.#transport;
    if (typeof transport.connect !== "function") {
      throw new TypeError(`${op} needs a transport with connect() (the built-in socket and baseUrl transports have it)`);
    }
    const headers: Record<string, string> = { "X-Request-Id": opts.requestId ?? newRequestId() };
    if (this.#token) headers.Authorization = `Bearer ${this.#token}`;
    for (let attempt = 1; ; attempt++) {
      let conn;
      try {
        conn = await transport.connect(opts.signal);
      } catch (err) {
        if (opts.signal?.aborted) throw opts.signal.reason;
        const te = err instanceof TransportError ? err : new TransportError(`connect failed: ${String(err)}`, { cause: err });
        if (!te.connectFailed || attempt >= MAX_ATTEMPTS) throw te;
        await sleep(100 * 2 ** (attempt - 1), opts.signal);
        continue;
      }
      return WebSocketConnection.connect(conn.socket, { host: conn.host, path, headers, signal: opts.signal });
    }
  }

  /**
   * Runs `argv` to completion: exec (not detached), attach from the start,
   * write `input` (then close stdin), and collect output until the exit.
   * A non-zero exit is not an error: check `exitCode` (124 = deadline).
   */
  async run(vm: string, argv: string[], opts: RunOptions = {}): Promise<RunResult> {
    const { input, ...exec } = opts;
    const p = await this.execProcess(vm, argv, { ...exec, detached: false, stdin: input !== undefined });
    const stream = await this.attachProcess(vm, p.id, { outputCursor: 0, signal: opts.signal, requestId: opts.requestId });
    try {
      if (input !== undefined) {
        stream.sendStdin(input);
        stream.closeStdin();
      }
      const out: Uint8Array[] = [];
      const err: Uint8Array[] = [];
      for await (const ev of stream) {
        if (ev.type === "exit") return new RunResult(ev.process, concatBytes(out), concatBytes(err));
        (ev.type === "stdout" ? out : err).push(ev.data);
      }
      throw new TransportError(`process ${p.id}: stream ended without an exit`);
    } finally {
      stream.close();
    }
  }

  // ---- job history and retained output -----------------------------------

  /**
   * Lists one page of the VM's managed jobs across generations, newest
   * first, each with its retained `stdoutBytes`/`stderrBytes`. Reads host
   * records, so it works while the VM is stopped. A host without guest
   * transport throws `ServerNotImplemented`.
   */
  listProcessHistory(vm: string, opts: PageOptions = {}): Promise<HistoryList> {
    return this.#call("listProcessHistory", { params: { vmId: vm }, query: DavitClient.#page(opts), opts });
  }

  /**
   * Samples every process in the VM's guest (`ps`). A running VM is sampled
   * live (managed jobs carry their `processId`); a VM that is not running
   * answers with its last sample (`stale`), or NotFound if it never had one.
   */
  listGuestProcesses(vm: string, opts: RequestOptions = {}): Promise<GuestProcessList> {
    return this.#call("listGuestProcesses", { params: { vmId: vm }, opts });
  }

  /** Iterates over all the VM's managed jobs (newest first), following `nextCursor`. */
  iterProcessHistory(vm: string, opts: Omit<PageOptions, "cursor"> = {}): AsyncGenerator<HistoryEntry, void, undefined> {
    return this.#paginate((cursor) => this.listProcessHistory(vm, { ...opts, cursor }));
  }

  /**
   * Reads one page of a job's host-retained output (works while the VM is
   * stopped). Items are `stdout`/`stderr` messages whose `data` is base64;
   * `iterProcessOutput` decodes them. Pass `nextCursor` as `after` to
   * continue; `complete` is true once the process ended and every retained
   * chunk was returned.
   */
  readProcessOutput(vm: string, processId: string, opts: ReadProcessOutputOptions = {}): Promise<ProcessOutput> {
    const q: Array<[string, string]> = [];
    if (opts.after !== undefined && opts.after !== "") q.push(["after", String(opts.after)]);
    if (opts.stream) q.push(["stream", opts.stream]);
    if (opts.limit !== undefined) q.push(["limit", String(opts.limit)]);
    return this.#call("readProcessOutput", { params: { vmId: vm, processId }, query: q, opts });
  }

  /**
   * Yields a job's retained output chunks, decoded, following `nextCursor`
   * until a page is `complete` or brings no new chunk (the process may still
   * be running: read again later). Unknown message types are skipped. The
   * generator's return value is `{ complete, cursor }` of the last page.
   */
  async *iterProcessOutput(vm: string, processId: string, opts: ReadProcessOutputOptions = {}): AsyncGenerator<ProcessOutputEvent, { complete: boolean; cursor: string | undefined }, undefined> {
    let cursor = opts.after === undefined || opts.after === "" ? undefined : String(opts.after);
    for (;;) {
      const page = await this.readProcessOutput(vm, processId, { ...opts, after: cursor });
      for (const m of page.items) {
        if (m.type === "stdout" || m.type === "stderr") yield { type: m.type, seq: m.seq ?? 0, data: decodeBase64(m.data, `process ${processId} output seq ${m.seq ?? 0}`) };
      }
      cursor = page.nextCursor || cursor;
      if (page.complete || !page.nextCursor) return { complete: page.complete, cursor };
    }
  }

  /**
   * Reads a job's retained output to the end (as `iterProcessOutput`) and
   * returns its stdout and stderr bytes, whether it is `complete`, and the
   * `cursor` to resume a later read. Works while the VM is stopped.
   */
  async collectProcessOutput(vm: string, processId: string, opts: ReadProcessOutputOptions = {}): Promise<RetainedOutput> {
    const out: Uint8Array[] = [];
    const err: Uint8Array[] = [];
    const it = this.iterProcessOutput(vm, processId, opts);
    for (;;) {
      const r = await it.next();
      if (r.done) return { stdout: concatBytes(out), stderr: concatBytes(err), complete: r.value.complete, cursor: r.value.cursor };
      (r.value.type === "stdout" ? out : err).push(r.value.data);
    }
  }

  // ---- runs and push -----------------------------------------------------

  /**
   * Creates a VM, runs `req.argv` in it, keeps the exit code and cleans up
   * (operationId `createRun`). Resolves the run at once (state `creating`);
   * follow it with `waitRun`, or use `runToCompletion`.
   *
   * - `outcome` (with `decidedAt`) is the result as soon as it is known;
   *   `state` becomes it only after cleanup (output grace, then the VM is
   *   deleted unless `keep`), when `endedAt` is set.
   * - `failed` covers a non-zero exit (`exitCode`), a signal kill (`signal`,
   *   no error), the timeout (`error.code` `timeout`) and setup failures
   *   (`error`).
   * - After the command exits, the VM stays for an output grace so its output
   *   can still be read through `processId`: `req.outputGraceSeconds`
   *   (0..600, 0 none; omitted: the host default, 5 s unless configured).
   *   Once `outcome` is set, `cancelRun` only ends that grace and keeps it.
   * - With `hold` the run stops in `waiting` until `startRun` (push files
   *   first with `pushTree`).
   *
   * Always sends an Idempotency-Key.
   */
  async createRun(req: RunInput, opts: CreateRunOptions = {}): Promise<Run> {
    if (!Array.isArray(req?.argv) || req.argv.length === 0 || req.argv.some((a) => typeof a !== "string")) {
      throw new TypeError("argv must be a non-empty array of strings");
    }
    const t = req.timeoutSeconds;
    if (t !== undefined && !(Number.isInteger(t) && t >= 0)) throw new TypeError(`timeoutSeconds must be an integer >= 0, got ${String(t)}`);
    const g = req.outputGraceSeconds;
    if (g !== undefined && !(Number.isInteger(g) && g >= 0 && g <= 600)) throw new TypeError(`outputGraceSeconds must be an integer in 0..600, got ${String(g)}`);
    const { environment, ...rest } = req;
    const body: RunRequest = { ...rest, argv: [...req.argv] };
    if (environment !== undefined) body.environment = parseEnvironmentRef(environment);
    const headers = { "Idempotency-Key": opts.idempotencyKey ?? newIdempotencyKey() };
    return this.#call("createRun", { body, headers, opts });
  }

  /** Lists one page of the caller's runs, oldest first. */
  listRuns(opts: PageOptions = {}): Promise<RunList> {
    return this.#call("listRuns", { query: DavitClient.#page(opts), opts });
  }

  /** Iterates over all runs (oldest first), following `nextCursor`. */
  iterRuns(opts: Omit<PageOptions, "cursor"> = {}): AsyncGenerator<Run, void, undefined> {
    return this.#paginate((cursor) => this.listRuns({ ...opts, cursor }));
  }

  /** Gets a run: its state, VM, process, exit code and error. */
  getRun(run: string | Run, opts: RequestOptions = {}): Promise<Run> {
    return this.#call("getRun", { params: { runId: runId(run) }, opts });
  }

  /** Starts the command of a run created with `hold`; a run that is not `waiting` throws `InvalidState`. */
  startRun(run: string | Run, opts: RequestOptions = {}): Promise<Run> {
    return this.#call("startRun", { params: { runId: runId(run) }, opts });
  }

  /**
   * Cancels a run, or ends the output grace of a run whose outcome is
   * decided. Before the outcome it kills the command and deletes the VM
   * (unless `keep`), and `outcome` (later `state`) becomes `cancelled`; after
   * it (`outcome`/`decidedAt` set) the outcome is kept and cleanup starts at once. On an ended
   * run it changes nothing. Resolves the run as it is now; `waitRun` for the end.
   */
  cancelRun(run: string | Run, opts: RequestOptions = {}): Promise<Run> {
    return this.#call("cancelRun", { params: { runId: runId(run) }, opts });
  }

  /**
   * Polls `getRun` until the run has ended (`endedAt` set) and returns it. A
   * failed or cancelled run is returned, not thrown. Throws `WaitTimeout`
   * (`details.runId`) when the budget runs out; that never cancels the run.
   */
  waitRun(run: string | Run, opts: WaitRunOptions = {}): Promise<Run> {
    return this.#pollRun(runId(run), (r) => r.endedAt !== undefined, opts);
  }

  async #pollRun(id: string, done: (r: Run) => boolean, opts: WaitRunOptions): Promise<Run> {
    const budget = opts.timeoutMs ?? this.#waitTimeoutMs;
    const deadline = Date.now() + budget;
    for (;;) {
      const r = await this.getRun(id, { signal: opts.signal, requestId: opts.requestId });
      if (done(r)) return r;
      const left = deadline - Date.now();
      if (left <= 0) throw new WaitTimeout(undefined, budget, undefined, { runId: id });
      await sleep(Math.min(RUN_POLL_INTERVAL_MS, left), opts.signal);
    }
  }

  /**
   * Applies a tar of files under the guest directory `path` (operationId
   * `pushTree`). `data` is the tar (bytes or a stream, sent once), sent as
   * `application/x-tar`. `mode: "merge"` (default) adds and replaces files
   * and keeps the others; `"mirror"` is passed through and this server
   * answers `ServerNotImplemented`. Naturally idempotent: no Idempotency-Key.
   * The SDK does not select files: build the tar yourself (the Python SDK
   * and `davit push` select a git work tree).
   */
  async pushTree(vm: string, path: string, data: FileData, opts: PushTreeOptions = {}): Promise<PushResult> {
    if (typeof path !== "string" || !path.startsWith("/")) throw new TypeError(`path must be absolute, got ${JSON.stringify(path)}`);
    let body: TransportBody;
    if (typeof data === "string" || data instanceof Uint8Array) body = data;
    else if (data instanceof ReadableStream) body = iterateReadable(data);
    else if (data && typeof (data as AsyncIterable<Uint8Array>)[Symbol.asyncIterator] === "function") body = data;
    else throw new TypeError("data must be a Uint8Array, string, ReadableStream or AsyncIterable<Uint8Array>");
    const query: Array<[string, string]> = [["path", path], ["mode", opts.mode ?? "merge"]];
    return this.#call("pushTree", { params: { vmId: vm }, query, raw: { body, contentType: "application/x-tar" }, opts });
  }

  /**
   * Does a whole run and returns its exit code and output, like `davit
   * run`: `createRun` (with `hold` when `opts.files` is given); wait for the
   * VM; `pushTree` the files to `filesPath` and `startRun`; attach to the
   * command and collect its output until it exits (if the VM is already gone,
   * the rest comes from the host-retained output); once `outcome`/`decidedAt`
   * is set, `cancelRun` to end the output grace (skipped with
   * `outputGraceSeconds: 0`); then `waitRun`. `exitCode` and `signal` report
   * how the command ended.
   *
   * A failed or cancelled command is returned (check `exitCode` and `run`).
   * A run that failed before its command started throws its typed error
   * (`details.runId`); a failed push cancels the run and rethrows.
   */
  async runToCompletion(req: Omit<RunInput, "hold">, opts: RunToCompletionOptions = {}): Promise<RunOutcome> {
    const { files, filesPath = DEFAULT_PUSH_PATH } = opts;
    const wait: WaitRunOptions = { timeoutMs: opts.timeoutMs, signal: opts.signal, requestId: opts.requestId };
    const spec: RunInput = { ...req };
    if (files !== undefined) {
      spec.hold = true;
      spec.cwd ??= filesPath;
    }
    let run = await this.createRun(spec, opts);
    run = await this.#pollRun(run.id, (r) => r.endedAt !== undefined || r.state === "waiting" || !!r.processId, wait);
    let push: PushResult | undefined;
    if (files !== undefined && run.state === "waiting" && run.endedAt === undefined) {
      try {
        push = await this.pushTree(run.vmId ?? "", filesPath, files, wait);
      } catch (err) {
        await this.cancelRun(run.id, wait).catch(() => {}); // nothing ran: give the VM back
        throw err;
      }
      await this.startRun(run.id, wait).catch((err: unknown) => {
        if (!(err instanceof DavitError && err.code === "invalid_state")) throw err; // cancelled meanwhile
      });
      run = await this.#pollRun(run.id, (r) => r.endedAt !== undefined || !!r.processId, wait);
    }
    if (!run.processId) {
      // The command never started.
      if (run.endedAt === undefined) run = await this.waitRun(run.id, wait);
      if (run.error && run.state !== "cancelled") {
        const e = run.error;
        throw errorFor({ code: e.code, message: e.message, retryable: e.retryable, details: { ...(e.details ?? {}), runId: run.id }, requestId: e.requestId });
      }
      return { run, exitCode: runExitCode(run, undefined), stdout: new Uint8Array(0), stderr: new Uint8Array(0), push, signal: run.signal };
    }
    const out: Uint8Array[] = [];
    const err: Uint8Array[] = [];
    let final: Process | undefined;
    let cursor = 0;
    try {
      const stream = await this.attachProcess(run.vmId ?? "", run.processId, wait);
      try {
        for await (const ev of stream) {
          if (ev.type === "exit") final = ev.process;
          else {
            (ev.type === "stdout" ? out : err).push(ev.data);
            cursor = ev.seq;
          }
        }
      } finally {
        stream.close();
      }
    } catch (e) {
      // The stream can end with the VM (cleanup) or find it already deleting
      // (a short grace); the run says how it went.
      if (!(e instanceof TransportError || (e instanceof DavitError && (e.code === "invalid_state" || e.code === "not_found")))) throw e;
    }
    if (final === undefined) {
      // Read the rest from the host-retained output.
      try {
        for await (const ev of this.iterProcessOutput(run.vmId ?? "", run.processId, { ...wait, after: cursor || undefined })) {
          (ev.type === "stdout" ? out : err).push(ev.data);
        }
      } catch (e) {
        if (!(e instanceof DavitError && (e.code === "not_implemented" || e.code === "not_found"))) throw e;
      }
    }
    run = await this.#pollRun(run.id, (r) => r.endedAt !== undefined || r.decidedAt !== undefined || r.outcome !== undefined, wait);
    if (run.endedAt === undefined && req.outputGraceSeconds !== 0) await this.cancelRun(run.id, wait); // decided: end the output grace
    run = await this.waitRun(run.id, wait);
    return { run, exitCode: runExitCode(run, final), stdout: concatBytes(out), stderr: concatBytes(err), push, signal: run.signal ?? final?.signal };
  }

  // ---- tunnels -----------------------------------------------------------

  /**
   * Connects to `port` on the guest's loopback and returns the tunnel. It
   * holds a live connection: attach it once (`attachTunnel`) before
   * `expiresAt` (30 s), else the server closes it. `connectTunnel` does both.
   * A port nothing listens on throws `Unavailable`.
   */
  async openTunnel(vm: string, port: number, opts: RequestOptions = {}): Promise<Tunnel> {
    if (!Number.isInteger(port) || port < 1 || port > 65535) throw new TypeError(`port must be an integer in 1..65535, got ${String(port)}`);
    return this.#call("openTunnel", { params: { vmId: vm }, body: { port }, opts });
  }

  /** Closes an unattached tunnel. An attached or expired one is NotFound (it ends with its stream). */
  async closeTunnel(tunnel: string | Tunnel, opts: RequestOptions = {}): Promise<void> {
    const tunnelId = typeof tunnel === "string" ? tunnel : tunnel.id;
    await this.#call<undefined>("closeTunnel", { params: { tunnelId }, opts });
  }

  /**
   * Attaches to an opened tunnel's bytes over a WebSocket (single use).
   * Needs a socket or baseUrl endpoint (or a transport with `connect`).
   */
  async attachTunnel(tunnel: string | Tunnel, opts: RequestOptions = {}): Promise<TunnelStream> {
    const tunnelId = typeof tunnel === "string" ? tunnel : tunnel.id;
    if (!tunnelId) throw new TypeError("attachTunnel: missing tunnelId");
    const path = BASE_PATH + OPERATIONS.attachTunnel[1].replace("{tunnelId}", encodeURIComponent(tunnelId));
    const ws = await this.#openWebSocket("attachTunnel", path, opts);
    return new TunnelStream(tunnelId, typeof tunnel === "string" ? undefined : tunnel, ws);
  }

  /**
   * Opens and attaches a tunnel to `port` on the guest's loopback. If the
   * attach fails, the opened tunnel is closed (best effort).
   */
  async connectTunnel(vm: string, port: number, opts: RequestOptions = {}): Promise<TunnelStream> {
    const tunnel = await this.openTunnel(vm, port, opts);
    try {
      return await this.attachTunnel(tunnel, opts);
    } catch (err) {
      await this.closeTunnel(tunnel, { signal: opts.signal }).catch(() => {});
      throw err;
    }
  }

  // ---- reverse forwards --------------------------------------------------

  /** Lists the VM's open reverse forwards (owner, or a token with admin). */
  async listReverseForwards(vm: string, opts: RequestOptions = {}): Promise<ReverseForwardList> {
    return this.#call("listReverseForwards", { params: { vmId: vm }, opts });
  }

  /**
   * Listens on `127.0.0.1:guestPort` in the VM and bridges every guest
   * connection to `target` (owner, or a token with admin). A `session`
   * target must attach its session before `sessionExpiresAt` (30 s):
   * `reverseForward` does that and the bridging. A `host` target is an
   * `ip:port` the operator listed in `network.reverseHostServices`
   * (`forwardHostService`). One forward per guest port (`Conflict`); a guest
   * agent that cannot listen is `UnsupportedCapability`.
   */
  async createReverseForward(vm: string, guestPort: number, target: ReverseTarget, opts: RequestOptions = {}): Promise<ReverseForward> {
    checkPort(guestPort, "guestPort");
    const body: CreateReverseForwardRequest = { guestPort, target };
    return this.#call("createReverseForward", { params: { vmId: vm }, body, opts });
  }

  /** Revokes a reverse forward and cuts its live connections. */
  async revokeReverseForward(vm: string, forward: string | ReverseForward, opts: RequestOptions = {}): Promise<void> {
    const forwardId = typeof forward === "string" ? forward : forward.id;
    await this.#call<undefined>("revokeReverseForward", { params: { vmId: vm, forwardId }, opts });
  }

  /**
   * Bridges `127.0.0.1:guestPort` in the VM to the operator-listed host
   * service `address` (`ip:port`); the daemon dials it. Revoke with
   * `revokeReverseForward`.
   */
  async forwardHostService(vm: string, guestPort: number, address: string, opts: RequestOptions = {}): Promise<ReverseForward> {
    return this.createReverseForward(vm, guestPort, { kind: "host", address }, opts);
  }

  /**
   * Makes `localHost:localPort` (reachable from this client) reachable from
   * `127.0.0.1:guestPort` in the VM, like `davit forward --reverse VM
   * GUESTPORT:HOST:PORT`: creates a `session` forward (labelled
   * `localHost:localPort`), attaches its session stream and bridges each
   * guest connection. `close()` on the result revokes the forward; its
   * `closed` promise resolves with the reason the session ended. If the
   * attach fails, the forward is revoked (best effort). Node and Bun only.
   */
  async reverseForward(vm: string, guestPort: number, localHost: string, localPort: number, opts: RequestOptions = {}): Promise<ReverseSession> {
    checkPort(localPort, "localPort");
    const address = `${localHost.includes(":") ? `[${localHost}]` : localHost}:${localPort}`;
    const forward = await this.createReverseForward(vm, guestPort, { kind: "session", address }, opts);
    const params = { vmId: vm, forwardId: forward.id };
    let ws: WebSocketConnection;
    try {
      ws = await this.#openWebSocket("attachReverseSession", DavitClient.#wsPath("attachReverseSession", params), opts);
    } catch (err) {
      await this.revokeReverseForward(vm, forward, { signal: opts.signal }).catch(() => {});
      throw err;
    }
    const dialConnection = (connectionId: string) =>
      this.#openWebSocket("attachReverseConnection", DavitClient.#wsPath("attachReverseConnection", { ...params, connectionId }), {});
    const dialLocal = async () => {
      const { connect } = await import("node:net");
      return new Promise<Socket>((resolve, reject) => {
        const sock = guardSocket(connect({ host: localHost, port: localPort, allowHalfOpen: true }));
        sock.setTimeout(LOCAL_DIAL_TIMEOUT_MS, () => sock.destroy(new Error(`connect ${address}: timed out`)));
        sock.once("error", reject);
        sock.once("connect", () => {
          sock.setTimeout(0);
          sock.off("error", reject);
          resolve(sock);
        });
      });
    };
    return new ReverseSession(forward, ws, dialConnection, dialLocal);
  }

  static #wsPath(op: OperationName, params: Record<string, string>): string {
    return BASE_PATH + OPERATIONS[op][1].replace(/\{(\w+)\}/g, (_, name: string) => encodeURIComponent(params[name] ?? ""));
  }

  // ---- app endpoints -----------------------------------------------------

  /** Lists the VM's app endpoints with their tailnet URLs, oldest first. */
  async listAppEndpoints(vm: string, opts: RequestOptions = {}): Promise<AppEndpointList> {
    return this.#call("listAppEndpoints", { params: { vmId: vm }, opts });
  }

  /**
   * Gives the app on guest loopback `port` a stable tailnet-private URL,
   * `https://<gateway>:<gatewayPort>/` on the shared tailnet gateway, served
   * to the owner or a bearer token with `exec` on the VM. The endpoint
   * belongs to the VM: a stop keeps the URL (requests get 503 until it runs
   * again), `deleteAppEndpoint` removes it, forks inherit none. `name`
   * defaults to `port-<port>`. One endpoint per port and per name
   * (`Conflict`); no tailnet gateway is `UnsupportedCapability`.
   */
  async createAppEndpoint(
    vm: string,
    port: number,
    name?: string | { name?: string; forwardIdentity?: boolean },
    opts: RequestOptions = {},
  ): Promise<AppEndpoint> {
    checkPort(port, "port");
    const o = typeof name === "string" ? { name } : (name ?? {});
    const body: CreateAppEndpointRequest = { port };
    if (o.name !== undefined) body.name = o.name;
    if (o.forwardIdentity) body.forwardIdentity = true;
    return this.#call("createAppEndpoint", { params: { vmId: vm }, body, opts });
  }

  /** Alias of `createAppEndpoint`: `(await client.expose("web", 3000)).url`. */
  expose(vm: string, port: number, name?: string | { name?: string; forwardIdentity?: boolean }, opts: RequestOptions = {}): Promise<AppEndpoint> {
    return this.createAppEndpoint(vm, port, name, opts);
  }

  /**
   * Renames, hides or shows an app endpoint (by ID, name or object); only
   * the given fields change. `hidden: true` closes its gateway port and cuts
   * live connections; `hidden: false` serves it again at the same URL. A
   * rename keeps the URL.
   */
  async updateAppEndpoint(vm: string, endpoint: string | AppEndpoint, req: UpdateAppEndpointRequest, opts: RequestOptions = {}): Promise<AppEndpoint> {
    const body: UpdateAppEndpointRequest = {};
    if (req.name !== undefined) body.name = req.name;
    if (req.hidden !== undefined) body.hidden = req.hidden;
    if (req.forwardIdentity !== undefined) body.forwardIdentity = req.forwardIdentity;
    return this.#call("updateAppEndpoint", { params: { vmId: vm, endpointId: appEndpointRef(endpoint) }, body, opts });
  }

  /** Stops serving an endpoint, keeping its URL (`updateAppEndpoint` with `hidden: true`). */
  hideAppEndpoint(vm: string, endpoint: string | AppEndpoint, opts: RequestOptions = {}): Promise<AppEndpoint> {
    return this.updateAppEndpoint(vm, endpoint, { hidden: true }, opts);
  }

  /** Serves a hidden endpoint again at the same URL (`hidden: false`). */
  showAppEndpoint(vm: string, endpoint: string | AppEndpoint, opts: RequestOptions = {}): Promise<AppEndpoint> {
    return this.updateAppEndpoint(vm, endpoint, { hidden: false }, opts);
  }

  /** Renames an endpoint; its URL (port-based) does not change. */
  renameAppEndpoint(vm: string, endpoint: string | AppEndpoint, name: string, opts: RequestOptions = {}): Promise<AppEndpoint> {
    return this.updateAppEndpoint(vm, endpoint, { name }, opts);
  }

  /** Deletes an endpoint, freeing its gateway port and cutting live connections. */
  async deleteAppEndpoint(vm: string, endpoint: string | AppEndpoint, opts: RequestOptions = {}): Promise<void> {
    await this.#call<undefined>("deleteAppEndpoint", { params: { vmId: vm, endpointId: appEndpointRef(endpoint) }, opts });
  }

  // ---- desktop -----------------------------------------------------------

  /** The VM's open desktop, its app endpoint of kind `desktop`; none open is `NotFound`. */
  async getDesktop(vm: string, opts: RequestOptions = {}): Promise<AppEndpoint> {
    return this.#call("getDesktop", { params: { vmId: vm }, opts });
  }

  /**
   * Serves the VM's VNC desktop as a tailnet-private web viewer and returns
   * it (an already open one as is). The desktop is the VM's app endpoint of
   * kind `desktop` for the guest VNC server on loopback `port` (server
   * default 5900, x11vnc in the desktop image profile). Its `url` serves the
   * built-in viewer and `<url>websockify` a websockify-compatible RFB stream
   * (noVNC works). `view` rights are view-only (the gateway drops input),
   * `exec` adds input. A stop cuts the stream (503 at the same URL until it
   * runs again); `closeDesktop` removes it. No tailnet gateway is
   * `UnsupportedCapability`.
   */
  async openDesktop(vm: string, port?: number, opts: RequestOptions = {}): Promise<AppEndpoint> {
    let body: OpenDesktopRequest | undefined;
    if (port !== undefined) {
      checkPort(port, "port");
      body = { port };
    }
    return this.#call("openDesktop", { params: { vmId: vm }, body, opts });
  }

  /** Closes the VM's desktop, cutting every viewer. */
  async closeDesktop(vm: string, opts: RequestOptions = {}): Promise<void> {
    await this.#call<undefined>("closeDesktop", { params: { vmId: vm }, opts });
  }

  // ---- ssh ---------------------------------------------------------------

  /**
   * Signs `req.publicKey` (the caller's OpenSSH authorized_keys line; the SDK
   * never generates keys) for SSH into the VM's current generation as
   * `req.guestUser` (default `user`) for `req.ttlSeconds` (1..600, default
   * 600). Pin the returned `hostKeys` (they match
   * `vm.endpoint.sshHostKeyFingerprints`); never disable host-key checking.
   * An invalid key throws InvalidArgument; a VM without host keys
   * InvalidState; a runtime without guest transport UnsupportedCapability.
   */
  async issueSSHCredential(vm: string, req: IssueSSHCredentialRequest, opts: RequestOptions = {}): Promise<SSHCredential> {
    if (typeof req?.publicKey !== "string" || req.publicKey === "") throw new TypeError("publicKey must be a non-empty string");
    const t = req.ttlSeconds;
    if (t !== undefined && !(Number.isInteger(t) && t >= 1 && t <= 600)) throw new TypeError(`ttlSeconds must be an integer in 1..600, got ${String(t)}`);
    const body: IssueSSHCredentialRequest = { publicKey: req.publicKey };
    if (req.guestUser !== undefined) body.guestUser = req.guestUser;
    if (t !== undefined) body.ttlSeconds = t;
    return this.#call("issueSSHCredential", { params: { vmId: vm }, body, opts });
  }

  // ---- tokens ------------------------------------------------------------

  /**
   * Creates a scoped, expiring API token. Only the owner identity (local
   * socket or tailnet WhoIs owner) may manage tokens; a client using a token
   * gets PermissionDenied. The returned `secret` (`slw_...`) is shown only
   * once: use it as the `token` of an `https://` client on another tailnet
   * host. Always sends an Idempotency-Key. A bad name, ttl or action throws
   * InvalidArgument; an unknown scoped VM NotFound.
   */
  async createToken(name: string, opts: CreateTokenOptions): Promise<CreatedToken> {
    const { actions, vm, scopes, ttlSeconds: ttl } = opts;
    if ((actions === undefined) === (scopes === undefined)) throw new TypeError("createToken: give exactly one of actions or scopes");
    if (scopes !== undefined && vm !== undefined) throw new TypeError("createToken: vm goes with actions; put vmId in each scope instead");
    if (actions !== undefined && !Array.isArray(actions)) throw new TypeError("createToken: actions must be an array of action names");
    if (!(Number.isInteger(ttl) && ttl >= 1 && ttl <= 31_536_000)) throw new TypeError(`ttlSeconds must be an integer in 1..31536000, got ${String(ttl)}`);
    let body: CreateTokenRequest;
    if (scopes !== undefined) {
      body = { name, scopes, ttlSeconds: ttl };
    } else {
      const vms = typeof vm === "string" ? [vm] : (vm ?? []);
      const acts = [...actions!];
      body = { name, scopes: vms.length ? vms.map((vmId) => ({ vmId, actions: acts })) : [{ actions: acts }], ttlSeconds: ttl };
    }
    const headers = { "Idempotency-Key": opts.idempotencyKey ?? newIdempotencyKey() };
    return this.#call("createToken", { body, headers, opts });
  }

  /** Lists one page of the caller's active tokens (never their secrets). */
  listTokens(opts: PageOptions = {}): Promise<TokenList> {
    return this.#call("listTokens", { query: DavitClient.#page(opts), opts });
  }

  /** Iterates over all active tokens, following `nextCursor`. */
  iterTokens(opts: Omit<PageOptions, "cursor"> = {}): AsyncGenerator<Token, void, undefined> {
    return this.#paginate((cursor) => this.listTokens({ ...opts, cursor }));
  }

  /** Revokes a token by ID; it stops working on its next request. A repeat throws NotFound. */
  async revokeToken(token: string | Token | CreatedToken, opts: RequestOptions = {}): Promise<void> {
    const tokenId = typeof token === "string" ? token : token.id;
    await this.#call<undefined>("revokeToken", { params: { tokenId }, opts });
  }

  // ---- members and shares ------------------------------------------------

  /**
   * Lists the tailnet users who may use shared resources. Members and shares
   * are owner-only (local socket or the owner's tailnet identity); tokens and
   * members get PermissionDenied. No tailnet gateway is UnsupportedCapability
   * (capabilities feature `members`).
   */
  listMembers(opts: RequestOptions = {}): Promise<MemberList> {
    return this.#call("listMembers", { opts });
  }

  /**
   * Adds a tailnet user as a member with a default `role`. The login is
   * pinned to the stable tailnet user ID on the member's first connection;
   * from then on a recycled login is refused. A member sees and uses only
   * what is shared with it (`createShare`). The same login twice is Conflict.
   */
  async addMember(login: string, role: MemberRole, opts: RequestOptions = {}): Promise<Member> {
    if (typeof login !== "string" || login === "") throw new TypeError("login must be a non-empty string");
    const body: AddMemberRequest = { login, role };
    return this.#call("addMember", { body, opts });
  }

  /** Changes a member's role (by ID, login or object); its live sessions are cut. */
  async updateMember(member: string | Member, role: MemberRole, opts: RequestOptions = {}): Promise<Member> {
    const body: UpdateMemberRequest = { role };
    return this.#call("updateMember", { params: { memberId: nonEmptyRef(member, "member") }, body, opts });
  }

  /** Removes a member (by ID, login or object) with its shares, cutting its live sessions. */
  async removeMember(member: string | Member, opts: RequestOptions = {}): Promise<void> {
    await this.#call<undefined>("removeMember", { params: { memberId: nonEmptyRef(member, "member") }, opts });
  }

  /** Lists shares, optionally of one `kind` and `resource` (ID, name or object; give `kind` with it). */
  listShares(kind?: ShareKind, resource?: string | { id: string }, opts: RequestOptions = {}): Promise<ShareList> {
    const query: Array<[string, string]> = [];
    if (kind !== undefined) query.push(["kind", kind]);
    if (resource !== undefined) query.push(["resource", nonEmptyRef(resource, "resource")]);
    return this.#call("listShares", { query, opts });
  }

  /**
   * Shares a VM, snapshot or environment template with a member (ID, login,
   * object) or `"*"` for every member. Roles nest as viewer < exec < admin;
   * `role` defaults to the member's own role. Sharing the same resource with
   * the same member again replaces the role and `credentials` (a downgrade
   * cuts that member's live sessions). Without `credentials` an exec or admin
   * share pauses the VM's credential grants for everyone while it exists.
   */
  async createShare(
    kind: ShareKind,
    resource: string | { id: string },
    member: string | Member,
    role?: MemberRole | ShareOptions,
    opts: RequestOptions = {},
  ): Promise<Share> {
    const o: ShareOptions = typeof role === "string" ? { ...opts, role } : { ...opts, ...role };
    const body: CreateShareRequest = { kind, resource: nonEmptyRef(resource, "resource"), member: nonEmptyRef(member, "member") };
    if (o.role !== undefined) body.role = o.role;
    if (o.credentials) body.credentials = true;
    return this.#call("createShare", { body, opts: o });
  }

  /** `createShare` of a VM. */
  shareVM(vm: string | VM, member: string | Member, role?: MemberRole | ShareOptions, opts: RequestOptions = {}): Promise<Share> {
    return this.createShare("vm", vm, member, role, opts);
  }

  /** `createShare` of a snapshot. */
  shareSnapshot(snapshot: string | Snapshot, member: string | Member, role?: MemberRole | ShareOptions, opts: RequestOptions = {}): Promise<Share> {
    return this.createShare("snapshot", snapshot, member, role, opts);
  }

  /** `createShare` of an environment template. */
  shareEnvironment(environment: string | Environment, member: string | Member, role?: MemberRole | ShareOptions, opts: RequestOptions = {}): Promise<Share> {
    return this.createShare("environment", environment, member, role, opts);
  }

  /**
   * Unshares by share ID or object; the members' live sessions on the
   * resource (API streams, SSH, app endpoint connections, their reverse
   * forwards) are cut.
   */
  async deleteShare(share: string | Share, opts: RequestOptions = {}): Promise<void> {
    await this.#call<undefined>("deleteShare", { params: { shareId: nonEmptyRef(share, "share") }, opts });
  }

  /**
   * Deletes the share of `resource` with `member` (ID, login, object or
   * `"*"`), found with `listShares`. Throws NotFound when there is none.
   */
  async unshare(kind: ShareKind, resource: string | { id: string }, member: string | Member, opts: RequestOptions = {}): Promise<void> {
    const ref = nonEmptyRef(member, "member");
    const shares = (await this.listShares(kind, resource, opts)).items.filter((s) => s.kind === kind);
    let match = shares.find((s) => s.member === ref);
    if (match === undefined && ref !== "*" && shares.length > 0) {
      // A share names its member by ID: resolve a login.
      const ids = new Set((await this.listMembers(opts)).items.filter((m) => m.id === ref || m.login === ref).map((m) => m.id));
      match = shares.find((s) => ids.has(s.member));
    }
    if (match === undefined) {
      throw errorFor({ code: "not_found", message: `${kind} ${refOf(resource)} is not shared with ${ref}` });
    }
    await this.deleteShare(match, opts);
  }

  /**
   * Replaces a token's secret, keeping its name and scopes. The old secret
   * stops working at once, or after `overlapSeconds` (its expiry only moves
   * earlier); the new token lives `ttlSeconds`, else as long as the old one
   * was issued for. The new `secret` is shown once. Always sends an
   * Idempotency-Key (a replay within 10 minutes returns the same token).
   */
  async rotateToken(token: string | Token | CreatedToken, opts: RotateTokenOptions = {}): Promise<CreatedToken> {
    const tokenId = typeof token === "string" ? token : token.id;
    if (!tokenId) throw new TypeError("rotateToken: token id must be non-empty");
    const body: RotateTokenRequest = {};
    if (opts.overlapSeconds !== undefined) {
      if (!(Number.isInteger(opts.overlapSeconds) && opts.overlapSeconds >= 0)) throw new TypeError("overlapSeconds must be a non-negative integer");
      if (opts.overlapSeconds > 0) body.overlapSeconds = opts.overlapSeconds;
    }
    if (opts.ttlSeconds !== undefined) body.ttlSeconds = opts.ttlSeconds;
    const headers = { "Idempotency-Key": opts.idempotencyKey ?? newIdempotencyKey() };
    return this.#call("rotateToken", { params: { tokenId }, body, headers, opts });
  }

  /** Who this client is to the daemon: principal, auth method and, for a token, its scopes. Never a secret. */
  whoami(opts: RequestOptions = {}): Promise<Me> {
    return this.#call("whoami", { opts });
  }

  // ---- credentials -------------------------------------------------------

  /** Lists one page of credential profiles (secrets are never returned). */
  listCredentialProfiles(opts: PageOptions = {}): Promise<CredentialProfileList> {
    return this.#call("listCredentialProfiles", { query: DavitClient.#page(opts), opts });
  }

  /** Iterates over all credential profiles, following `nextCursor`. */
  iterCredentialProfiles(opts: Omit<PageOptions, "cursor"> = {}): AsyncGenerator<CredentialProfile, void, undefined> {
    return this.#paginate((cursor) => this.listCredentialProfiles({ ...opts, cursor }));
  }

  /**
   * Creates an empty credential profile. Always sends an Idempotency-Key: a
   * replay of the same body returns the same profile.
   */
  createCredentialProfile(name: string, opts: CreateCredentialProfileOptions = {}): Promise<CredentialProfile> {
    const body: CreateCredentialProfileRequest = { name };
    if (opts.default !== undefined) body.default = opts.default;
    const headers = { "Idempotency-Key": opts.idempotencyKey ?? newIdempotencyKey() };
    return this.#call("createCredentialProfile", { body, headers, opts });
  }

  /** Returns a credential profile by ID or name. */
  getCredentialProfile(profile: string | CredentialProfile, opts: RequestOptions = {}): Promise<CredentialProfile> {
    return this.#call("getCredentialProfile", { params: { profileId: refOf(profile) }, opts });
  }

  /** Deletes a profile and its secrets; its grants are revoked (`profile_deleted`). */
  async deleteCredentialProfile(profile: string | CredentialProfile, opts: RequestOptions = {}): Promise<void> {
    await this.#call<undefined>("deleteCredentialProfile", { params: { profileId: refOf(profile) }, opts });
  }

  /**
   * Creates or replaces a credential in a profile and returns the profile.
   * The `secret` is write-only: stored on the host only, never returned, never
   * logged by the SDK; omitted or empty keeps the stored one. `api_key`
   * credentials of providers `anthropic` and `openai` default their upstream to
   * the provider's API; `custom_api` needs `baseUrl`. Naturally idempotent: no
   * Idempotency-Key is sent.
   */
  putCredential(profile: string | CredentialProfile, name: string, req: PutCredentialInput, opts: RequestOptions = {}): Promise<CredentialProfile> {
    const body: PutCredentialRequest = { kind: req.kind, envVar: req.envVar, secret: req.secret ?? "" };
    if (req.provider !== undefined) body.provider = req.provider;
    if (req.baseUrl !== undefined) body.baseUrl = req.baseUrl;
    return this.#call("putCredential", { params: { profileId: refOf(profile), credentialName: name }, body, opts });
  }

  /** Removes a credential and its secret; its grants are revoked (`credential_removed`). */
  async deleteCredential(profile: string | CredentialProfile, name: string, opts: RequestOptions = {}): Promise<void> {
    await this.#call<undefined>("deleteCredential", { params: { profileId: refOf(profile), credentialName: name }, opts });
  }

  /** Lists the VM's credential grants, active and revoked, oldest first. */
  listCredentialGrants(vm: string, opts: PageOptions = {}): Promise<CredentialGrantList> {
    return this.#call("listCredentialGrants", { params: { vmId: vm }, query: DavitClient.#page(opts), opts });
  }

  /**
   * Grants a VM proxied use of a credential. The guest sees only the grant's
   * `placeholder` (in `envVar`) and points its client at the host credential
   * proxy, `http://172.16.0.1:15080`; the proxy swaps the placeholder for the
   * secret on requests to the credential's `upstream`, for this VM only. Stop
   * keeps the grant dormant, resume reuses it, a fork child inherits none,
   * delete revokes it. Always sends an Idempotency-Key.
   */
  async createCredentialGrant(vm: string, profile: string | CredentialProfile, credential: string, opts: CreateCredentialGrantOptions = {}): Promise<CredentialGrant> {
    const ref = refOf(profile);
    if (!ref) throw new TypeError("createCredentialGrant: missing profile");
    const body: CreateCredentialGrantRequest = { profile: ref, credential };
    const headers = { "Idempotency-Key": opts.idempotencyKey ?? newIdempotencyKey() };
    return this.#call("createCredentialGrant", { params: { vmId: vm }, body, headers, opts });
  }

  /** Revokes a grant and returns it; its placeholder stops working at once (requests in flight are cut). */
  revokeCredentialGrant(vm: string, grant: string | CredentialGrant, opts: RequestOptions = {}): Promise<CredentialGrant> {
    return this.#call("revokeCredentialGrant", { params: { vmId: vm, grantId: refOf(grant) }, opts });
  }

  // ---- environments ------------------------------------------------------

  /** Lists one page of the caller's environments. */
  listEnvironments(opts: PageOptions = {}): Promise<EnvironmentList> {
    return this.#call("listEnvironments", { query: DavitClient.#page(opts), opts });
  }

  /** Iterates over all environments, following `nextCursor`. */
  iterEnvironments(opts: Omit<PageOptions, "cursor"> = {}): AsyncGenerator<Environment, void, undefined> {
    return this.#paginate((cursor) => this.listEnvironments({ ...opts, cursor }));
  }

  /**
   * Creates an environment with its first revision (`latestRevision` 1).
   * The manifest holds no secret values (`secretFiles` name host secrets).
   * Always sends an Idempotency-Key; the server also returns the existing
   * environment for the same name and an identical first manifest. A bad
   * name or manifest, or a taken name, throws `InvalidArgument`.
   */
  async createEnvironment(name: string, manifest: ManifestInput, opts: CreateEnvironmentOptions = {}): Promise<Environment> {
    const body = { name, manifest: manifestBody(manifest) };
    const headers = { "Idempotency-Key": opts.idempotencyKey ?? newIdempotencyKey() };
    return this.#call("createEnvironment", { body, headers, opts });
  }

  /** Gets an environment by ID (`env_...`) or name. */
  getEnvironment(environment: string, opts: RequestOptions = {}): Promise<Environment> {
    return this.#call("getEnvironment", { params: { environmentId: environment }, opts });
  }

  /**
   * Deletes an environment and its revisions (VMs keep their pinned copy).
   * Naturally idempotent (no Idempotency-Key); a repeat throws NotFound.
   */
  async deleteEnvironment(environment: string | Environment, opts: RequestOptions = {}): Promise<void> {
    const environmentId = typeof environment === "string" ? environment : environment.id;
    await this.#call<undefined>("deleteEnvironment", { params: { environmentId }, opts });
  }

  /** Lists one page of an environment's revisions, oldest first. */
  listEnvironmentRevisions(environment: string, opts: PageOptions = {}): Promise<EnvironmentRevisionList> {
    return this.#call("listEnvironmentRevisions", { params: { environmentId: environment }, query: DavitClient.#page(opts), opts });
  }

  /** Iterates over all revisions of an environment (oldest first). */
  iterEnvironmentRevisions(environment: string, opts: Omit<PageOptions, "cursor"> = {}): AsyncGenerator<EnvironmentRevision, void, undefined> {
    return this.#paginate((cursor) => this.listEnvironmentRevisions(environment, { ...opts, cursor }));
  }

  /**
   * Mints the next immutable revision from a complete manifest. `ifMatch`
   * is the expected `latestRevision` (stale: `RevisionMismatch`). Always
   * sends an Idempotency-Key: a retry with the same key returns the revision
   * it minted.
   */
  async createEnvironmentRevision(environment: string, manifest: ManifestInput, opts: CreateEnvironmentRevisionOptions = {}): Promise<EnvironmentRevision> {
    const headers: Record<string, string> = { "Idempotency-Key": opts.idempotencyKey ?? newIdempotencyKey() };
    if (opts.ifMatch !== undefined) headers["If-Match"] = String(opts.ifMatch);
    const body = { manifest: manifestBody(manifest) };
    return this.#call("createEnvironmentRevision", { params: { environmentId: environment }, body, headers, opts });
  }

  /** Gets one revision (an integer >= 1) of an environment. */
  async getEnvironmentRevision(environment: string, revision: number, opts: RequestOptions = {}): Promise<EnvironmentRevision> {
    if (!Number.isInteger(revision) || revision < 1) throw new TypeError(`revision must be an integer >= 1, got ${String(revision)}`);
    return this.#call("getEnvironmentRevision", { params: { environmentId: environment, revision: String(revision) }, opts });
  }

  // ---- events and webhooks -------------------------------------------------

  /**
   * Lists one page of the host event stream, in `seq` order. `nextCursor`
   * is always set: it continues after the last item, or where the stream was
   * when the page is empty. Skip event types you do not know.
   */
  listEvents(opts: ListHostEventsOptions = {}): Promise<EventList> {
    const q = DavitClient.#page(opts);
    const types = typeof opts.types === "string" ? opts.types : opts.types?.join(",");
    if (types) q.push(["types", types]);
    if (opts.vmId) q.push(["vmId", opts.vmId]);
    if (opts.start !== undefined) q.push(["start", opts.start]);
    if (opts.waitMs) q.push(["waitMs", String(Math.max(0, Math.floor(opts.waitMs)))]);
    return this.#call("listEvents", { query: q, opts });
  }

  /**
   * Iterates over events in `seq` order, following `nextCursor`. Without
   * `follow` it ends at the first empty page (the current end of the
   * stream); with `follow: true` it never ends: each empty page is followed
   * by another long-poll of up to `waitMs` (default 25000). Stop with
   * `break` or `signal`.
   */
  async *iterEvents(opts: IterEventsOptions = {}): AsyncGenerator<Event, void, undefined> {
    const { follow = false, waitMs = MAX_LONG_POLL_MS, ...filter } = opts;
    if (!(Number.isInteger(waitMs) && waitMs >= 0 && waitMs <= 30_000)) throw new TypeError(`iterEvents: waitMs must be an integer in 0..30000, got ${String(waitMs)}`);
    let cursor = opts.cursor;
    for (;;) {
      const page = await this.listEvents({ ...filter, cursor, start: cursor ? undefined : filter.start, waitMs: follow ? waitMs : 0 });
      yield* page.items;
      if (!page.nextCursor) throw new Internal({ code: "internal", message: "listEvents returned a page without nextCursor" });
      cursor = page.nextCursor;
      if (page.items.length === 0 && !follow) return;
    }
  }

  /** Lists one page of webhooks (never their secrets). Owner only. */
  listWebhooks(opts: PageOptions = {}): Promise<WebhookList> {
    return this.#call("listWebhooks", { query: DavitClient.#page(opts), opts });
  }

  /** Iterates over all webhooks, following `nextCursor`. */
  iterWebhooks(opts: Omit<PageOptions, "cursor"> = {}): AsyncGenerator<Webhook, void, undefined> {
    return this.#paginate((cursor) => this.listWebhooks({ ...opts, cursor }));
  }

  /**
   * Creates a webhook that receives signed event deliveries. Owner only. The
   * returned `secret` is shown only once; give it to the receiver for
   * `verifyWebhook`. Always sends an Idempotency-Key.
   */
  async createWebhook(url: string, opts: CreateWebhookOptions = {}): Promise<WebhookSecret> {
    if (opts.events !== undefined && !Array.isArray(opts.events)) throw new TypeError("createWebhook: events must be an array of event types");
    const body: CreateWebhookRequest = { url };
    if (opts.events !== undefined) body.events = [...opts.events];
    if (opts.description !== undefined) body.description = opts.description;
    if (opts.enabled !== undefined) body.enabled = opts.enabled;
    const headers = { "Idempotency-Key": opts.idempotencyKey ?? newIdempotencyKey() };
    return this.#call("createWebhook", { body, headers, opts });
  }

  /** Returns a webhook by ID (or the object returned by another call). */
  getWebhook(webhook: WebhookRef, opts: RequestOptions = {}): Promise<Webhook> {
    return this.#call("getWebhook", { params: { webhookId: webhookIdOf(webhook) }, opts });
  }

  /**
   * Changes a webhook's URL, event filter, description or enabled state; only
   * the given fields change (`events: []` subscribes to every event).
   * Naturally idempotent: no Idempotency-Key is sent.
   */
  updateWebhook(webhook: WebhookRef, req: UpdateWebhookRequest, opts: RequestOptions = {}): Promise<Webhook> {
    const body: UpdateWebhookRequest = {};
    if (req.url !== undefined) body.url = req.url;
    if (req.events !== undefined) body.events = [...req.events];
    if (req.description !== undefined) body.description = req.description;
    if (req.enabled !== undefined) body.enabled = req.enabled;
    return this.#call("updateWebhook", { params: { webhookId: webhookIdOf(webhook) }, body, opts });
  }

  /** Deletes a webhook and its pending deliveries. A repeat throws NotFound. */
  async deleteWebhook(webhook: WebhookRef, opts: RequestOptions = {}): Promise<void> {
    await this.#call<undefined>("deleteWebhook", { params: { webhookId: webhookIdOf(webhook) }, opts });
  }

  /**
   * Replaces a webhook's signing secret and returns the new one (shown once).
   * Until `webhook.previousSecretExpiresAt` deliveries are signed with both
   * secrets, so receivers can switch over. Always sends an Idempotency-Key.
   */
  rotateWebhookSecret(webhook: WebhookRef, opts: RotateWebhookSecretOptions = {}): Promise<WebhookSecret> {
    const body: RotateWebhookSecretRequest = {};
    if (opts.overlapSeconds !== undefined) body.overlapSeconds = opts.overlapSeconds;
    const headers = { "Idempotency-Key": opts.idempotencyKey ?? newIdempotencyKey() };
    return this.#call("rotateWebhookSecret", { params: { webhookId: webhookIdOf(webhook) }, body, headers, opts });
  }

  /** Lists one page of a webhook's deliveries, newest first, optionally only one `state`. */
  listWebhookDeliveries(webhook: WebhookRef, opts: ListWebhookDeliveriesOptions = {}): Promise<WebhookDeliveryList> {
    const q = DavitClient.#page(opts);
    if (opts.state !== undefined) q.push(["state", opts.state]);
    return this.#call("listWebhookDeliveries", { params: { webhookId: webhookIdOf(webhook) }, query: q, opts });
  }

  /** Iterates over a webhook's deliveries, newest first, following `nextCursor`. */
  iterWebhookDeliveries(webhook: WebhookRef, opts: Omit<ListWebhookDeliveriesOptions, "cursor"> = {}): AsyncGenerator<WebhookDelivery, void, undefined> {
    return this.#paginate((cursor) => this.listWebhookDeliveries(webhook, { ...opts, cursor }));
  }

  /** Delivers a dead (or delivered) delivery again, now; returns it pending again. */
  retryWebhookDelivery(webhook: WebhookRef, delivery: string | WebhookDelivery, opts: RequestOptions = {}): Promise<WebhookDelivery> {
    return this.#call("retryWebhookDelivery", { params: { webhookId: webhookIdOf(webhook), deliveryId: refOf(delivery) }, opts });
  }

  // ---- files -------------------------------------------------------------

  /** Validates path/generation and resolves the VM reference and generation. */
  async #fileTarget(vm: string, path: string, opts: FileOptions): Promise<{ vmId: string; query: Array<[string, string]> }> {
    if (typeof path !== "string" || !path.startsWith("/")) throw new TypeError(`path must be absolute, got ${JSON.stringify(path)}`);
    let generation = opts.generation;
    let vmId = vm;
    if (generation === undefined) {
      const v = await this.getVM(vm, { signal: opts.signal, requestId: opts.requestId });
      generation = v.instanceGeneration;
      vmId = v.id;
    } else if (!Number.isInteger(generation) || generation < 1) {
      throw new TypeError(`generation must be an integer >= 1, got ${String(generation)}`);
    }
    const query: Array<[string, string]> = [["path", path], ["generation", String(generation)]];
    if (opts.user) query.push(["user", opts.user]);
    if (opts.archive) query.push(["archive", "tar"]);
    return { vmId, query };
  }

  /**
   * Reads a guest file (or, with `archive: true`, a directory as tar) into
   * memory. Throws TransportError if the transfer was cut short.
   */
  async readFile(vm: string, path: string, opts: FileOptions = {}): Promise<FileContent> {
    const stream = await this.readFileStream(vm, path, opts);
    return { data: await stream.bytes(), mode: stream.mode };
  }

  /**
   * Reads a guest file (or a directory as tar) without buffering it. Iterate
   * the result once (or call `toReadableStream()`); the iteration throws
   * TransportError if the transfer was cut short.
   */
  async readFileStream(vm: string, path: string, opts: FileOptions = {}): Promise<FileReadStream> {
    const { vmId, query } = await this.#fileTarget(vm, path, opts);
    const resp = await this.#callStream("readFile", { params: { vmId }, query, opts, headers: { Accept: "*/*" } });
    return fileReadStream(resp, opts.archive === true, false);
  }

  /**
   * Atomically replaces a guest file with `data` (or, with `archive: true`,
   * extracts a tar stream into the directory `path`). Strings are UTF-8.
   * Returns the agent-verified FileInfo.
   */
  async writeFile(vm: string, path: string, data: FileData, opts: WriteFileOptions = {}): Promise<FileInfo> {
    const mode = opts.mode !== undefined ? formatMode(opts.mode) : undefined;
    let body: TransportBody;
    if (typeof data === "string" || data instanceof Uint8Array) body = data;
    else if (data instanceof ReadableStream) body = iterateReadable(data);
    else if (data && typeof (data as AsyncIterable<Uint8Array>)[Symbol.asyncIterator] === "function") body = data;
    else throw new TypeError("data must be a Uint8Array, string, ReadableStream or AsyncIterable<Uint8Array>");
    const { vmId, query } = await this.#fileTarget(vm, path, opts);
    if (mode !== undefined) query.push(["mode", mode]);
    const contentType = opts.archive ? "application/x-tar" : "application/octet-stream";
    // No Idempotency-Key: a write is a full atomic replace, naturally idempotent.
    return this.#call("writeFile", { params: { vmId }, query, raw: { body, contentType }, opts });
  }

  // ---- prompts and conversations ----------------------------------------

  /**
   * Starts one turn of a harness conversation (Claude Code, Codex, OpenCode, pi) in the
   * VM. Always sends an Idempotency-Key (yours or one generated once per
   * call, reused on retries). Returns the accepted turn; follow it with `followPrompt` or
   * `waitPrompt`, or use `prompt` for both. A turn while another is active
   * in the conversation throws `OperationInProgress`.
   */
  async createPrompt(vm: string, text: string, opts: PromptOptions = {}): Promise<Prompt> {
    if (typeof text !== "string" || text === "") throw new TypeError("text must be a non-empty string");
    const body: PromptRequest = { text };
    if (opts.harness !== undefined) body.harness = opts.harness;
    if (opts.conversationId !== undefined) body.conversationId = opts.conversationId;
    if (opts.new) body.new = true;
    if (opts.model !== undefined) body.model = opts.model;
    if (opts.cwd !== undefined) body.cwd = opts.cwd;
    if (opts.env !== undefined) body.env = opts.env;
    if (opts.secrets !== undefined) body.secrets = opts.secrets;
    if (opts.attachments !== undefined) body.attachments = opts.attachments;
    if (opts.reasoningEffort !== undefined || opts.fast !== undefined) {
      body.options = {};
      if (opts.reasoningEffort !== undefined) body.options.reasoningEffort = opts.reasoningEffort;
      if (opts.fast !== undefined) body.options.fast = opts.fast;
    }
    if (opts.turnTimeoutSeconds !== undefined) body.turnTimeoutSeconds = opts.turnTimeoutSeconds;
    const headers = { "Idempotency-Key": opts.idempotencyKey ?? newIdempotencyKey() };
    return this.#call("createPrompt", { params: { vmId: vm }, body, headers, opts });
  }

  /** Gets one turn. */
  getPrompt(vm: string, promptId: string, opts: RequestOptions = {}): Promise<Prompt> {
    return this.#call("getPrompt", { params: { vmId: vm, promptId }, opts });
  }

  /** Cancels a running turn (SIGTERM to the harness); it ends `cancelled`. */
  cancelPrompt(vm: string, promptId: string, opts: RequestOptions = {}): Promise<Prompt> {
    return this.#call("cancelPrompt", { params: { vmId: vm, promptId }, opts });
  }

  /**
   * Sends `text` to a running turn. `delivery: "native"`: the same turn took
   * it (`prompt` is that turn). `"restart"`: the turn was cancelled and
   * `prompt` is the new turn of the same conversation carrying the message.
   * Sends an Idempotency-Key (`idempotencyKey`, or a UUIDv4 generated once
   * per call and reused on retry): the message is delivered once, and a
   * repeat returns the first result. A finished turn throws `InvalidState`,
   * one still starting `OperationInProgress` (retryable), a harness with
   * neither `UnsupportedCapability`.
   */
  steerPrompt(vm: string, promptId: string, text: string, opts: SteerPromptOptions = {}): Promise<SteerResult> {
    if (typeof text !== "string" || text === "") throw new TypeError("text must be a non-empty string");
    const headers = { "Idempotency-Key": opts.idempotencyKey ?? newIdempotencyKey() };
    return this.#call("steerPrompt", { params: { vmId: vm, promptId }, body: { text }, headers, opts });
  }

  /**
   * Cancels one conversation's active turn (`conversationId`) or every
   * active turn on the VM (`all: true`) and returns the cancelled turns.
   * No Idempotency-Key: a repeat finds nothing active and returns an empty list.
   */
  interruptPrompts(vm: string, opts: InterruptOptions): Promise<InterruptResult> {
    const byConversation = opts.conversationId !== undefined && opts.conversationId !== "";
    if (byConversation === (opts.all === true)) throw new TypeError("interruptPrompts needs exactly one of conversationId and all: true");
    const body: InterruptRequest = byConversation ? { conversationId: opts.conversationId } : { all: true };
    return this.#call("interruptPrompts", { params: { vmId: vm }, body, opts });
  }

  /**
   * Lists the VM's prompt events across turns after the feed cursor `after`,
   * optionally long-polling up to `waitMs`. `nextCursor` is set on every page
   * (also an empty one): pass it back as `after` to continue.
   */
  listVMPromptEvents(vm: string, opts: ListVMPromptEventsOptions = {}): Promise<PromptFeed> {
    const q: Array<[string, string]> = [];
    if (opts.after !== undefined && opts.after !== "") q.push(["after", opts.after]);
    if (opts.conversationId !== undefined) q.push(["conversationId", opts.conversationId]);
    if (opts.waitMs !== undefined) q.push(["waitMs", String(Math.max(0, Math.floor(opts.waitMs)))]);
    if (opts.limit !== undefined) q.push(["limit", String(opts.limit)]);
    return this.#call("listVMPromptEvents", { params: { vmId: vm }, query: q, opts });
  }

  /**
   * Yields the VM's prompt feed (long-polling) from `after` on, without end:
   * break out of the loop or abort `signal` to stop. Resume later with the
   * last item's `cursor` as `after`.
   */
  async *followVMPromptEvents(vm: string, opts: FollowVMPromptEventsOptions = {}): AsyncGenerator<PromptFeedItem, void, undefined> {
    let after = opts.after;
    for (;;) {
      const page = await this.listVMPromptEvents(vm, { ...opts, after, waitMs: MAX_LONG_POLL_MS });
      for (const item of page.items) {
        after = item.cursor;
        yield item;
      }
      if (page.nextCursor) after = page.nextCursor;
    }
  }

  /** Lists a turn's events after `after`, optionally long-polling up to `waitMs`. */
  listPromptEvents(vm: string, promptId: string, opts: ListPromptEventsOptions = {}): Promise<PromptEventList> {
    const q: Array<[string, string]> = [["after", String(opts.after ?? 0)]];
    if (opts.waitMs !== undefined) q.push(["waitMs", String(Math.max(0, Math.floor(opts.waitMs)))]);
    return this.#call("listPromptEvents", { params: { vmId: vm, promptId }, query: q, opts });
  }

  /**
   * Yields a turn's events in order until it is terminal (the last event of
   * a finished turn is `turn.completed`). Throws `WaitTimeout` when the
   * budget runs out; that never cancels the turn.
   */
  async *followPrompt(vm: string, promptId: string, opts: FollowPromptOptions = {}): AsyncGenerator<PromptEvent, Prompt, undefined> {
    const budget = opts.timeoutMs ?? this.#waitTimeoutMs;
    const deadline = Date.now() + budget;
    let after = opts.after ?? 0;
    for (;;) {
      const remaining = deadline - Date.now();
      if (remaining <= 0) throw new WaitTimeout(undefined, budget, undefined, { promptId, cursor: after });
      const timer = AbortSignal.timeout(remaining);
      const signal = opts.signal ? AbortSignal.any([opts.signal, timer]) : timer;
      let page: PromptEventList;
      try {
        page = await this.listPromptEvents(vm, promptId, { after, waitMs: Math.min(remaining, MAX_LONG_POLL_MS), requestId: opts.requestId, signal });
      } catch (err) {
        if (timer.aborted && !opts.signal?.aborted) throw new WaitTimeout(undefined, budget, err, { promptId, cursor: after });
        throw err;
      }
      for (const ev of page.items) {
        after = ev.seq;
        yield ev;
      }
      if (page.items.length === 0 && !PROMPT_ACTIVE.has(page.prompt.state)) return page.prompt;
    }
  }

  /**
   * Follows a turn to its end and returns the final Prompt. A failed or
   * cancelled turn is returned, not thrown: check `state` and `error`.
   */
  async waitPrompt(vm: string, promptId: string, opts: WaitPromptOptions = {}): Promise<Prompt> {
    const it = this.followPrompt(vm, promptId, opts);
    for (;;) {
      const r = await it.next();
      if (r.done) return r.value;
      opts.onEvent?.(r.value);
    }
  }

  /**
   * Runs one turn to completion (`createPrompt` then `waitPrompt`) and
   * returns the final Prompt, every event, and `text`: the concatenated
   * assistant `message.delta` texts. `onEvent` still sees events as they
   * arrive. A failed or cancelled turn is returned, not thrown.
   */
  async runPrompt(vm: string, text: string, opts: PromptOptions & WaitPromptOptions = {}): Promise<PromptResult> {
    const events: PromptEvent[] = [];
    const started = await this.createPrompt(vm, text, opts);
    const prompt = await this.waitPrompt(vm, started.id, {
      ...opts,
      after: 0,
      onEvent: (e) => {
        events.push(e);
        opts.onEvent?.(e);
      },
    });
    const out = events
      .filter((e) => e.type === "message.delta" && (e.role === undefined || e.role === "" || e.role === "assistant"))
      .map((e) => e.text ?? "")
      .join("");
    return { prompt, text: out, events };
  }

  /** Lists one page of the VM's harness conversations, in `(createdAt, id)` order (oldest first). */
  listConversations(vm: string, opts: PageOptions = {}): Promise<ConversationList> {
    return this.#call("listConversations", { params: { vmId: vm }, query: DavitClient.#page(opts), opts });
  }

  /** Iterates over all the VM's conversations (oldest first), following `nextCursor`. */
  iterConversations(vm: string, opts: Omit<PageOptions, "cursor"> = {}): AsyncGenerator<Conversation, void, undefined> {
    return this.#paginate((cursor) => this.listConversations(vm, { ...opts, cursor }));
  }

  /** Gets one conversation. */
  getConversation(vm: string, conversationId: string, opts: RequestOptions = {}): Promise<Conversation> {
    return this.#call("getConversation", { params: { vmId: vm, conversationId }, opts });
  }

  // ---- operations --------------------------------------------------------

  /** Gets an operation's current state and terminal result. */
  getOperation(operationId: string, opts: RequestOptions = {}): Promise<Operation> {
    return this.#call("getOperation", { params: { operationId }, opts });
  }

  /** Lists progress events after `after`, optionally long-polling up to `waitMs`. */
  listOperationEvents(operationId: string, opts: ListEventsOptions = {}): Promise<OperationEventList> {
    const q: Array<[string, string]> = [["after", String(opts.after ?? 0)]];
    if (opts.waitMs !== undefined) q.push(["waitMs", String(Math.max(0, Math.floor(opts.waitMs)))]);
    return this.#call("listOperationEvents", { params: { operationId }, query: q, opts });
  }

  /**
   * Follows an operation until it is terminal and returns it. Throws
   * `OperationFailed` if it failed and `WaitTimeout` when the budget runs
   * out; a client timeout never cancels the server operation.
   */
  async wait(operationId: string, opts: WaitOptions = {}): Promise<Operation> {
    const budget = opts.timeoutMs ?? this.#waitTimeoutMs;
    const deadline = Date.now() + budget;
    let after = 0;
    for (;;) {
      const remaining = deadline - Date.now();
      if (remaining <= 0) throw new WaitTimeout(operationId, budget);
      const timer = AbortSignal.timeout(remaining);
      const signal = opts.signal ? AbortSignal.any([opts.signal, timer]) : timer;
      let page: OperationEventList;
      try {
        page = await this.listOperationEvents(operationId, {
          after,
          waitMs: Math.min(remaining, MAX_LONG_POLL_MS),
          requestId: opts.requestId,
          signal,
        });
      } catch (err) {
        if (timer.aborted && !opts.signal?.aborted) throw new WaitTimeout(operationId, budget, err);
        throw err;
      }
      for (const ev of page.items) {
        opts.onProgress?.(ev);
        after = ev.seq;
      }
      const op = page.operation;
      if (isTerminal(op.state) && page.items.length === 0) {
        if (op.state === "failed") throw new OperationFailed(op);
        return op;
      }
    }
  }

  /**
   * Waits for a VM's active operation (if any), then re-reads it. Returns
   * the VM when it is `ready` or `running`; otherwise throws the typed error
   * of its `lastError` (or `InvalidState`).
   */
  async waitReady(vm: VM | string, opts: WaitOptions = {}): Promise<VM> {
    let current = typeof vm === "string" ? await this.getVM(vm, opts) : vm;
    if (current.activeOperationId) {
      try {
        await this.wait(current.activeOperationId, opts);
      } catch (err) {
        if (!(err instanceof OperationFailed)) throw err;
      }
    }
    current = await this.getVM(current.id, { signal: opts.signal });
    if (current.state === "ready" || current.state === "running") return current;
    if (current.lastError) throw errorFromInfo(current.lastError, { operationId: current.lastError.operationId });
    throw errorFor({ code: "invalid_state", message: `VM ${current.name || current.id} is ${current.state}, not ready` });
  }
}

async function* iterateReadable(stream: ReadableStream<Uint8Array>): AsyncGenerator<Uint8Array, void, undefined> {
  const reader = stream.getReader();
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) return;
      yield value;
    }
  } finally {
    reader.releaseLock();
  }
}

/**
 * Parses `"NAME"` (the latest revision) or `"NAME@REVISION"` like the CLI's
 * `--environment`; NAME may also be an environment ID and REVISION is an
 * integer >= 1. An `EnvironmentRef` object is returned as a copy. Throws
 * `TypeError` on anything else.
 */
export function parseEnvironmentRef(value: EnvironmentSpec): EnvironmentRef {
  if (typeof value === "object" && value !== null && typeof value.environment === "string") {
    const ref: EnvironmentRef = { environment: value.environment };
    if (value.revision !== undefined) ref.revision = value.revision;
    return ref;
  }
  if (typeof value !== "string") throw new TypeError('environment must be an EnvironmentRef or a "NAME[@REVISION]" string');
  const at = value.indexOf("@");
  const name = at < 0 ? value : value.slice(0, at);
  const rev = at < 0 ? undefined : value.slice(at + 1);
  if (name === "" || (rev !== undefined && !(/^\d+$/.test(rev) && Number(rev) >= 1 && Number.isSafeInteger(Number(rev))))) {
    throw new TypeError(`environment ${JSON.stringify(value)}: want NAME or NAME@REVISION (REVISION >= 1)`);
  }
  return rev === undefined ? { environment: name } : { environment: name, revision: Number(rev) };
}

const MANIFEST_KEYS = new Set(["repos", "variables", "secretFiles", "setup", "credentialProfile"]);

/** Validates a manifest's top level and fills omitted collections. */
function manifestBody(m: ManifestInput): EnvManifest {
  if (typeof m !== "object" || m === null || Array.isArray(m)) throw new TypeError("manifest must be an object (EnvManifest)");
  const unknown = Object.keys(m).filter((k) => !MANIFEST_KEYS.has(k)).sort();
  if (unknown.length) throw new TypeError(`invalid manifest: unknown field(s) ${unknown.join(", ")}`);
  const body: EnvManifest = { repos: m.repos ?? [], variables: m.variables ?? {}, secretFiles: m.secretFiles ?? [], setup: m.setup ?? [] };
  if (m.credentialProfile !== undefined) body.credentialProfile = m.credentialProfile;
  return body;
}

/** Decodes standard base64, refusing malformed data (an `internal` error, like a malformed body). */
function decodeBase64(data: string | undefined, what: string): Uint8Array {
  const s = data ?? "";
  if (s.length % 4 !== 0 || !/^[A-Za-z0-9+/]*={0,2}$/.test(s)) {
    throw new DavitError({ code: "internal", message: `decoding response: ${what}: data is not base64` });
  }
  return new Uint8Array(Buffer.from(s, "base64"));
}

function runId(run: string | Run): string {
  const id = typeof run === "string" ? run : run?.id;
  if (typeof id !== "string" || id === "") throw new TypeError("run id must be a non-empty string");
  return id;
}

/** The exit status `davit run` reports for an ended run. */
function runExitCode(run: Run, final: Process | undefined): number {
  if ((run.outcome ?? run.state) === "cancelled") return 130;
  if (run.error?.code === "timeout") return EXIT_DEADLINE;
  if (typeof run.exitCode === "number") return run.exitCode & 0xff;
  const sig = run.signal ? signalExitCode(run.signal) : undefined;
  return sig ?? processExitCode(final);
}

/** Validates and copies an egress policy for a request body. */
function egressBody(egress: Egress): Egress {
  if (typeof egress?.mode !== "string") throw new TypeError("egress.mode must be a string");
  const body: Egress = { mode: egress.mode };
  if (egress.allow !== undefined) {
    if (!Array.isArray(egress.allow)) throw new TypeError('egress.allow must be an array of strings, e.g. ["github.com"]');
    body.allow = [...egress.allow];
  }
  return body;
}

/** An app endpoint's ID or name for its path segment; must be non-empty. */
function appEndpointRef(endpoint: string | AppEndpoint): string {
  const ref = refOf(endpoint);
  if (!ref) throw new TypeError("endpoint must be a non-empty ID or name");
  return ref;
}

/** A non-empty ID, name or login for a path segment or reference field. */
function nonEmptyRef(value: string | { id: string }, what: string): string {
  const ref = refOf(value);
  if (!ref) throw new TypeError(`${what} must be a non-empty ID or name`);
  return ref;
}

/** An ID or name for a path segment or reference field; a resource object gives its ID. */
function refOf(value: string | { id: string }): string {
  return typeof value === "string" ? value : value.id;
}

/** A webhook by ID, the Webhook, or the WebhookSecret returned by create/rotate. */
export type WebhookRef = string | Webhook | WebhookSecret;

function webhookIdOf(webhook: WebhookRef): string {
  return typeof webhook === "string" ? webhook : "webhook" in webhook ? webhook.webhook.id : webhook.id;
}
