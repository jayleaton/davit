"""Synchronous client for the davit HTTP API v1."""

from __future__ import annotations

import base64
import binascii
import contextlib
import dataclasses
import json
import os
import socket
import ssl
import tarfile
import tempfile
import time
from collections.abc import Callable, Iterable, Iterator, Mapping, Sequence
from datetime import timedelta
from typing import IO, Any, Literal, TypeVar, overload
from urllib.parse import quote

import httpx

from . import _transport, _websocket
from ._model import Model
from .errors import (
    DavitError,
    Internal,
    InvalidState,
    NotFound,
    OperationFailed,
    ServerNotImplemented,
    TransportError,
    WaitTimeout,
    error_for,
    error_from_info,
)
from .files import FileContent, FileStream, WriteData, body_of, check_path, format_mode
from .models import (
    VM,
    AddMemberRequest,
    AppEndpoint,
    AppEndpointList,
    CIStatus,
    Conversation,
    ConversationList,
    CreateAppEndpointRequest,
    CreateCredentialGrantRequest,
    CreateCredentialProfileRequest,
    CreatedToken,
    CreateEnvironmentRequest,
    CreateEnvironmentRevisionRequest,
    CreateReverseForwardRequest,
    CreateShareRequest,
    CreateTokenRequest,
    CreateVMRequest,
    CreateVMsRequest,
    CreateVMsResult,
    CreateWebhookRequest,
    CredentialGrant,
    CredentialGrantList,
    CredentialKind,
    CredentialProfile,
    CredentialProfileList,
    Egress,
    EgressMode,
    Environment,
    EnvironmentList,
    EnvironmentRef,
    EnvironmentRevision,
    EnvironmentRevisionList,
    EnvManifest,
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
    Me,
    Member,
    MemberList,
    MemberRole,
    OpenDesktopRequest,
    OpenTunnelRequest,
    Operation,
    OperationAccepted,
    OperationEvent,
    OperationEventList,
    OperationState,
    PrivacyPolicy,
    Process,
    ProcessList,
    ProcessOutput,
    Prompt,
    PromptAttachment,
    PromptEvent,
    PromptEventList,
    PromptFeed,
    PromptFeedItem,
    PromptOptions,
    PromptRequest,
    PromptState,
    PruneImagesRequest,
    PruneImagesResult,
    PushResult,
    PutCredentialRequest,
    ReclaimVMsRequest,
    ReclaimVMsResult,
    RecoverVMRequest,
    ResumeVMRequest,
    ReverseForward,
    ReverseForwardList,
    ReverseTarget,
    ReverseTargetKind,
    RotateTokenRequest,
    RotateWebhookSecretRequest,
    Run,
    RunList,
    RunRequest,
    RunState,
    ShapeOverride,
    Share,
    ShareKind,
    ShareList,
    Snapshot,
    SnapshotEntry,
    SnapshotList,
    SnapshotTree,
    SnapshotVMRequest,
    SSHCredential,
    SteerRequest,
    SteerResult,
    StopVMRequest,
    Token,
    TokenList,
    TokenScope,
    Tunnel,
    UpdateAppEndpointRequest,
    UpdateMemberRequest,
    UpdateVMRequest,
    UpdateWebhookRequest,
    VMList,
    VMState,
    Webhook,
    WebhookDelivery,
    WebhookDeliveryList,
    WebhookList,
    WebhookSecret,
)
from .processes import (
    EXIT_DEADLINE,
    ProcessStream,
    RetainedOutput,
    RunResult,
    StreamEvent,
    exit_status,
    signal_status,
)
from .prompts import PromptResult
from .push import DEFAULT_PUSH_PATH, git_selection, write_tree_tar
from .reverse import ReverseSession
from .runs import RunOutcome
from .tunnels import TunnelStream

__all__ = [
    "NOT_YET_SUPPORTED",
    "OPERATIONS",
    "Client",
    "DeliveryState",
    "EnvironmentSpec",
    "EventStart",
    "ExportFormat",
    "ProgressCallback",
    "parse_environment_ref",
]

OPERATIONS: dict[str, tuple[str, str]] = {
    "getCapabilities": ("GET", "/capabilities"),
    "listImages": ("GET", "/images"),
    "getImage": ("GET", "/images/{imageId}"),
    "listVMs": ("GET", "/vms"),
    "createVM": ("POST", "/vms"),
    "createVMs": ("POST", "/vms/batch"),
    "reclaimVMs": ("POST", "/vms/reclaim"),
    "pruneImages": ("POST", "/images/prune"),
    "getVM": ("GET", "/vms/{vmId}"),
    "updateVM": ("PATCH", "/vms/{vmId}"),
    "deleteVM": ("DELETE", "/vms/{vmId}"),
    "stopVM": ("POST", "/vms/{vmId}/stop"),
    "resumeVM": ("POST", "/vms/{vmId}/resume"),
    "recoverVM": ("POST", "/vms/{vmId}/recover"),
    "forkVM": ("POST", "/vms/{vmId}/fork"),
    "listVMSnapshots": ("GET", "/vms/{vmId}/snapshots"),
    "snapshotVM": ("POST", "/vms/{vmId}/snapshots"),
    "listSnapshots": ("GET", "/snapshots"),
    "getSnapshot": ("GET", "/snapshots/{snapshotId}"),
    "deleteSnapshot": ("DELETE", "/snapshots/{snapshotId}"),
    "listSnapshotTree": ("GET", "/snapshots/{snapshotId}/tree"),
    "readSnapshotFile": ("GET", "/snapshots/{snapshotId}/files"),
    "exportSnapshot": ("GET", "/snapshots/{snapshotId}/export"),
    "getOperation": ("GET", "/operations/{operationId}"),
    "listOperationEvents": ("GET", "/operations/{operationId}/events"),
    "listProcesses": ("GET", "/vms/{vmId}/processes"),
    "execProcess": ("POST", "/vms/{vmId}/processes"),
    "getProcess": ("GET", "/vms/{vmId}/processes/{processId}"),
    "killProcess": ("DELETE", "/vms/{vmId}/processes/{processId}"),
    "attachProcess": ("GET", "/vms/{vmId}/processes/{processId}/stream"),
    "readFile": ("GET", "/vms/{vmId}/files"),
    "writeFile": ("PUT", "/vms/{vmId}/files"),
    "createPrompt": ("POST", "/vms/{vmId}/prompts"),
    "getPrompt": ("GET", "/vms/{vmId}/prompts/{promptId}"),
    "cancelPrompt": ("DELETE", "/vms/{vmId}/prompts/{promptId}"),
    "listPromptEvents": ("GET", "/vms/{vmId}/prompts/{promptId}/events"),
    "steerPrompt": ("POST", "/vms/{vmId}/prompts/{promptId}/steer"),
    "listVMPromptEvents": ("GET", "/vms/{vmId}/prompt-events"),
    "interruptPrompts": ("POST", "/vms/{vmId}/interrupt"),
    "listConversations": ("GET", "/vms/{vmId}/conversations"),
    "getConversation": ("GET", "/vms/{vmId}/conversations/{conversationId}"),
    "openTunnel": ("POST", "/vms/{vmId}/tunnels"),
    "closeTunnel": ("DELETE", "/tunnels/{tunnelId}"),
    "attachTunnel": ("GET", "/tunnels/{tunnelId}/stream"),
    "listReverseForwards": ("GET", "/vms/{vmId}/reverse-forwards"),
    "createReverseForward": ("POST", "/vms/{vmId}/reverse-forwards"),
    "revokeReverseForward": ("DELETE", "/vms/{vmId}/reverse-forwards/{forwardId}"),
    "attachReverseSession": ("GET", "/vms/{vmId}/reverse-forwards/{forwardId}/session"),
    "attachReverseConnection": (
        "GET",
        "/vms/{vmId}/reverse-forwards/{forwardId}/connections/{connectionId}",
    ),
    "listAppEndpoints": ("GET", "/vms/{vmId}/app-endpoints"),
    "createAppEndpoint": ("POST", "/vms/{vmId}/app-endpoints"),
    "updateAppEndpoint": ("PATCH", "/vms/{vmId}/app-endpoints/{endpointId}"),
    "deleteAppEndpoint": ("DELETE", "/vms/{vmId}/app-endpoints/{endpointId}"),
    "getDesktop": ("GET", "/vms/{vmId}/desktop"),
    "openDesktop": ("POST", "/vms/{vmId}/desktop"),
    "closeDesktop": ("DELETE", "/vms/{vmId}/desktop"),
    "issueSSHCredential": ("POST", "/vms/{vmId}/ssh-credentials"),
    "listTokens": ("GET", "/tokens"),
    "createToken": ("POST", "/tokens"),
    "revokeToken": ("DELETE", "/tokens/{tokenId}"),
    "listMembers": ("GET", "/members"),
    "addMember": ("POST", "/members"),
    "updateMember": ("PATCH", "/members/{memberId}"),
    "removeMember": ("DELETE", "/members/{memberId}"),
    "listShares": ("GET", "/shares"),
    "createShare": ("POST", "/shares"),
    "deleteShare": ("DELETE", "/shares/{shareId}"),
    "rotateToken": ("POST", "/tokens/{tokenId}/rotate"),
    "whoami": ("GET", "/me"),
    "listCredentialProfiles": ("GET", "/credential-profiles"),
    "createCredentialProfile": ("POST", "/credential-profiles"),
    "getCredentialProfile": ("GET", "/credential-profiles/{profileId}"),
    "deleteCredentialProfile": ("DELETE", "/credential-profiles/{profileId}"),
    "putCredential": ("PUT", "/credential-profiles/{profileId}/credentials/{credentialName}"),
    "deleteCredential": ("DELETE", "/credential-profiles/{profileId}/credentials/{credentialName}"),
    "listCredentialGrants": ("GET", "/vms/{vmId}/credential-grants"),
    "createCredentialGrant": ("POST", "/vms/{vmId}/credential-grants"),
    "revokeCredentialGrant": ("DELETE", "/vms/{vmId}/credential-grants/{grantId}"),
    "listEnvironments": ("GET", "/environments"),
    "createEnvironment": ("POST", "/environments"),
    "getEnvironment": ("GET", "/environments/{environmentId}"),
    "deleteEnvironment": ("DELETE", "/environments/{environmentId}"),
    "listEnvironmentRevisions": ("GET", "/environments/{environmentId}/revisions"),
    "createEnvironmentRevision": ("POST", "/environments/{environmentId}/revisions"),
    "getEnvironmentRevision": ("GET", "/environments/{environmentId}/revisions/{revision}"),
    "listProcessHistory": ("GET", "/vms/{vmId}/history"),
    "readProcessOutput": ("GET", "/vms/{vmId}/processes/{processId}/output"),
    "listGuestProcesses": ("GET", "/vms/{vmId}/ps"),
    "listRuns": ("GET", "/runs"),
    "createRun": ("POST", "/runs"),
    "getRun": ("GET", "/runs/{runId}"),
    "cancelRun": ("DELETE", "/runs/{runId}"),
    "startRun": ("POST", "/runs/{runId}/start"),
    "pushTree": ("PUT", "/vms/{vmId}/tree"),
    "listEvents": ("GET", "/events"),
    "listWebhooks": ("GET", "/webhooks"),
    "createWebhook": ("POST", "/webhooks"),
    "getWebhook": ("GET", "/webhooks/{webhookId}"),
    "updateWebhook": ("PATCH", "/webhooks/{webhookId}"),
    "deleteWebhook": ("DELETE", "/webhooks/{webhookId}"),
    "rotateWebhookSecret": ("POST", "/webhooks/{webhookId}/rotate-secret"),
    "listWebhookDeliveries": ("GET", "/webhooks/{webhookId}/deliveries"),
    "retryWebhookDelivery": ("POST", "/webhooks/{webhookId}/deliveries/{deliveryId}/retry"),
    "getCIStatus": ("GET", "/ci/status"),
}
"""operationId → (HTTP method, path template under ``/api/v1``) this client calls."""

# Parity v1 contract operations answering not_implemented on the server:
# none since every gap of the cut shipped.
NOT_YET_SUPPORTED: frozenset[str] = frozenset()
"""operationIds in the API contract that this client does not wrap yet."""

DEFAULT_TIMEOUT = 30.0
"""Per-request HTTP timeout in seconds."""
DEFAULT_WAIT_TIMEOUT = 600.0
"""Default operation wait budget in seconds (10 minutes, like the CLI)."""
MAX_POLL_MS = 25_000
RUN_POLL_INTERVAL = 0.2
"""Seconds between getRun polls in :meth:`Client.wait_run` (like ``davit run``)."""
"""Longest single events long-poll; the server caps waitMs at 30000."""
LOCAL_DIAL_TIMEOUT = 10.0
"""Bound of each local service dial of :meth:`Client.reverse_forward` (as the CLI)."""

EventStart = Literal["oldest", "latest"]
"""Where :meth:`Client.list_events` starts without a cursor."""
DeliveryState = Literal["pending", "delivered", "dead"]
"""``list_webhook_deliveries`` state filter."""

ExportFormat = Literal["tar", "raw"]
"""``export_snapshot`` formats: a filesystem tar, or the raw disk image."""

ProgressCallback = Callable[[OperationEvent], None]
"""Called once per operation event, in order, while waiting."""
PromptCallback = Callable[[PromptEvent], None]
"""Called once per prompt event, in order, while waiting for a turn."""

_TERMINAL = (OperationState.SUCCEEDED, OperationState.FAILED)
_PROMPT_ACTIVE = (PromptState.STARTING, PromptState.RUNNING)
_READY = (VMState.READY, VMState.RUNNING)
_P = TypeVar("_P")
_M = TypeVar("_M", bound=Model)
Ttl = int | float | timedelta


def _decode(model: type[_M], data: Any) -> _M:
    """Parse a 2xx body; a malformed one is an ``Internal`` error, like the Go CLI."""
    try:
        return model.from_dict(data)
    except ValueError as exc:
        raise Internal(f"decoding response: {exc}") from exc


def _check_port(port: int, name: str) -> None:
    if isinstance(port, bool) or not isinstance(port, int) or not 1 <= port <= 65535:
        raise ValueError(f"{name} must be an integer in 1..65535")


def _safe_extract(tar: tarfile.TarFile, root: str) -> None:
    if hasattr(tarfile, "data_filter"):  # Python 3.12+, 3.11.4+, 3.10.12+
        tar.extractall(root, filter="data")
        return
    base = os.path.realpath(root)  # pragma: no cover - old interpreters only
    for member in tar.getmembers():  # pragma: no cover
        target = os.path.realpath(os.path.join(base, member.name))
        if member.name.startswith("/") or not (target == base or target.startswith(base + os.sep)):
            raise ValueError(f"tar entry {member.name!r} escapes {root}")
        if not (member.isreg() or member.isdir() or member.issym()):
            raise ValueError(f"tar entry {member.name!r} is not a file, directory or symlink")
        if member.issym():
            link = os.path.realpath(os.path.join(os.path.dirname(target), member.linkname))
            if not (link == base or link.startswith(base + os.sep)):
                raise ValueError(f"tar symlink {member.name!r} points outside {root}")
    tar.extractall(root)


def _ref(
    value: str
    | CredentialProfile
    | CredentialGrant
    | AppEndpoint
    | Webhook
    | WebhookDelivery
    | Member
    | Share
    | VM
    | Snapshot
    | Environment,
    what: str,
) -> str:
    """An ID or name for a path segment or reference field; models give their ID."""
    ref = value if isinstance(value, str) else value.id
    if not ref:
        raise ValueError(f"{what} must be a non-empty ID or name")
    return ref


def _webhook_ref(webhook: str | Webhook | WebhookSecret) -> str:
    return _ref(webhook.webhook if isinstance(webhook, WebhookSecret) else webhook, "webhook")


def _check_argv(argv: Sequence[str]) -> list[str]:
    if isinstance(argv, (str, bytes)) or not all(isinstance(a, str) for a in argv):
        raise TypeError("argv must be a list of strings, e.g. ['sh', '-c', 'echo hi']")
    if not argv:
        raise ValueError("argv must not be empty")
    return list(argv)


def _check_deadline(seconds: int | None) -> int | None:
    if seconds is None:
        return None
    if isinstance(seconds, bool) or not isinstance(seconds, int) or seconds < 1:
        raise ValueError("deadline_seconds must be an integer >= 1")
    return seconds


def _seconds(ttl: Ttl | None) -> int | None:
    if ttl is None:
        return None
    return int(ttl.total_seconds() if isinstance(ttl, timedelta) else ttl)


EnvironmentSpec = EnvironmentRef | str
"""An environment for create, fork and resume: an ``EnvironmentRef``, or ``"NAME"`` /
``"NAME@REVISION"``."""


def parse_environment_ref(value: EnvironmentSpec) -> EnvironmentRef:
    """Parse ``"NAME"`` (the latest revision) or ``"NAME@REVISION"`` like ``--environment``.

    ``NAME`` may also be an environment ID. ``REVISION`` is a decimal integer
    >= 1. An :class:`~davit.models.EnvironmentRef` is returned unchanged.

    Raises:
        ValueError: An empty name or a revision that is not an integer >= 1.
        TypeError: ``value`` is neither a string nor an ``EnvironmentRef``.
    """
    if isinstance(value, EnvironmentRef):
        return value
    if not isinstance(value, str):
        raise TypeError("environment must be an EnvironmentRef or a 'NAME[@REVISION]' string")
    name, found, rev = value.partition("@")
    if not name or (found and not (rev.isascii() and rev.isdigit() and int(rev) >= 1)):
        raise ValueError(f"environment {value!r}: want NAME or NAME@REVISION (REVISION >= 1)")
    return EnvironmentRef(environment=name, revision=int(rev) if found else None)


def _env_ref(value: EnvironmentSpec | None) -> EnvironmentRef | None:
    return None if value is None else parse_environment_ref(value)


Manifest = EnvManifest | Mapping[str, Any]
"""An environment manifest: an ``EnvManifest``, or its wire JSON object (camelCase keys)."""


def _manifest(manifest: Manifest) -> EnvManifest:
    if isinstance(manifest, EnvManifest):
        return manifest
    if isinstance(manifest, Mapping):
        unknown = sorted(set(manifest) - {wire for _, wire, _, _ in EnvManifest._wire_})
        if unknown:  # a misspelled key would otherwise vanish from the manifest
            raise ValueError(f"invalid manifest: unknown field(s) {unknown}")
        try:
            return EnvManifest.from_dict(dict(manifest))
        except ValueError as exc:
            raise ValueError(f"invalid manifest: {exc}") from exc
    raise TypeError("manifest must be an EnvManifest or a mapping (the wire JSON object)")


def _run_id(run: str | Run) -> str:
    run_id = run.id if isinstance(run, Run) else run
    if not isinstance(run_id, str) or not run_id:
        raise ValueError("run id must be a non-empty string")
    return run_id


def _run_exit(run: Run, final: Process | None) -> int:
    """The exit status ``davit run`` reports for an ended run.

    130 cancelled, 124 timeout, the exit code, 128+n for ``run.signal``, else
    the final process's status (255 when unknown).
    """
    if (run.outcome or run.state) == RunState.CANCELLED:
        return 130
    if run.error is not None and run.error.code == "timeout":
        return EXIT_DEADLINE
    if run.exit_code is not None:
        return run.exit_code & 0xFF
    if run.signal and (status := signal_status(run.signal)) is not None:
        return status
    return exit_status(final)


def _decided(run: Run) -> bool:
    return bool(run.ended_at or run.decided_at or run.outcome)


def _check_grace(seconds: int | None) -> int | None:
    if seconds is not None and (
        isinstance(seconds, bool) or not isinstance(seconds, int) or not 0 <= seconds <= 600
    ):
        raise ValueError("output_grace_seconds must be an integer in 0..600")
    return seconds


MAX_CAPACITY_WAIT = 3600
"""Longest ``capacity_wait`` the server accepts, in seconds."""


def _capacity_wait(wait: Ttl | None) -> int:
    """``capacity_wait`` as whole seconds (0: no wait); bounds as the server's ``waitSeconds``."""
    if wait is None:
        return 0
    if isinstance(wait, bool) or not isinstance(wait, (int, float, timedelta)):
        raise TypeError("capacity_wait must be seconds (int or float) or a timedelta")
    seconds = _seconds(wait) or 0
    if not 0 <= seconds <= MAX_CAPACITY_WAIT:
        raise ValueError(f"capacity_wait must be 0..{MAX_CAPACITY_WAIT} seconds")
    return seconds


def _b64(data: str | None, what: str) -> bytes:
    try:
        return base64.b64decode(data or "", validate=True)
    except (binascii.Error, ValueError) as exc:
        raise Internal(f"decoding response: {what}: data is not base64") from exc


class Client:
    """Client for one davit daemon.

    Connects to a local unix ``socket``, or to ``base_url`` (``https://host``, or
    ``http://`` for loopback hosts only) with an optional bearer ``token``. With
    neither, uses ``$DAVIT_API_URL`` (``unix:///abs/path.sock`` or a URL) and
    falls back to the default socket path. Routes live under ``/api/v1``.

    Lifecycle methods (create, fork, stop, resume, delete, snapshot) wait for
    their operation by default and return the final resource; pass
    ``wait=False`` to get the 202 :class:`~davit.models.OperationAccepted`.
    Every mutation sends an ``Idempotency-Key`` (yours, or a UUID generated once
    per call and reused on retry). A request is retried, at most three times in
    total, only when it certainly never reached the server.

    Use as a context manager, or call :meth:`close`.

    Args:
        socket: Absolute path of the daemon's unix socket.
        base_url: Remote API origin, without ``/api/v1``.
        token: Bearer token sent as ``Authorization: Bearer <token>``. Default:
            ``$DAVIT_TOKEN``, used only for http(s) base URLs (never the
            local socket); ``""`` sends no token.
        ca_file: PEM bundle of extra CAs trusted for the https endpoint (and its
            WebSocket streams), in addition to the system roots, for
            operator-managed certificates. Default: ``$DAVIT_CA_FILE``.
        timeout: Per-request HTTP timeout in seconds.
        wait_timeout: Default wait budget for operations, in seconds.
        transport: Custom httpx transport (testing, proxies); overrides ``socket``
            for the connection but not for URL selection.
    """

    def __init__(
        self,
        *,
        socket: str | None = None,
        base_url: str | None = None,
        token: str | None = None,
        ca_file: str | None = None,
        timeout: float = DEFAULT_TIMEOUT,
        wait_timeout: float = DEFAULT_WAIT_TIMEOUT,
        transport: httpx.BaseTransport | None = None,
    ) -> None:
        sock, self.base_url = _transport.resolve_target(socket, base_url, token)
        self.socket = sock
        token = _transport.resolve_token(token, sock)
        self._token = token
        self._ssl_context = _transport.tls_context(ca_file, sock)
        self.timeout = timeout
        self.wait_timeout = wait_timeout
        if transport is None and sock is not None:
            transport = httpx.HTTPTransport(uds=sock)
        headers = {"User-Agent": f"{_transport.PRODUCT}-python/{_transport.VERSION}"}
        if token:
            headers["Authorization"] = f"Bearer {token}"
        self._headers = headers
        verify: ssl.SSLContext | bool = self._ssl_context or True
        self._http = httpx.Client(
            transport=transport, headers=headers, timeout=timeout, verify=verify
        )

    def close(self) -> None:
        """Close the underlying connection pool."""
        self._http.close()

    def __enter__(self) -> Client:
        return self

    def __exit__(self, *exc_info: object) -> None:
        self.close()

    def __repr__(self) -> str:
        where = f"socket={self.socket!r}" if self.socket else f"base_url={self.base_url!r}"
        return f"Client({where})"

    # ---- plumbing -------------------------------------------------------

    def _call(
        self,
        operation_id: str,
        path_params: Mapping[str, str] | None = None,
        *,
        params: list[tuple[str, str]] | None = None,
        body: Any = None,
        headers: Mapping[str, str] | None = None,
        timeout: float | None = None,
    ) -> Any:
        method, path = self._route(operation_id, path_params)
        hdrs = {"X-Request-Id": _transport.new_request_id(), **(headers or {})}
        _, data = _transport.send(
            self._http,
            method,
            self.base_url + path,
            headers=hdrs,
            params=params,
            body=body,
            timeout=timeout,
        )
        return data

    @staticmethod
    def _route(operation_id: str, path_params: Mapping[str, str] | None) -> tuple[str, str]:
        method, template = OPERATIONS[operation_id]
        params = {k: quote(v, safe="") for k, v in (path_params or {}).items()}
        return method, template.format(**params)

    def _mutate(
        self,
        operation_id: str,
        path_params: Mapping[str, str] | None,
        body: Any,
        idempotency_key: str | None,
        if_match: int | str | None,
        capacity_wait: int = 0,
    ) -> OperationAccepted:
        headers = {"Idempotency-Key": idempotency_key or _transport.new_idempotency_key()}
        if if_match is not None:
            headers["If-Match"] = str(if_match)
        params: list[tuple[str, str]] | None = None
        timeout: float | None = None
        if capacity_wait > 0:  # the request itself may queue that long for room
            params = [("waitSeconds", str(capacity_wait))]
            timeout = self.timeout + capacity_wait
        data = self._call(
            operation_id, path_params, params=params, body=body, headers=headers, timeout=timeout
        )
        return _decode(OperationAccepted, data)

    def _lifecycle(
        self,
        accepted: OperationAccepted,
        wait: bool,
        wait_timeout: float | None,
        on_progress: ProgressCallback | None,
    ) -> VM | OperationAccepted:
        if not wait:
            return accepted
        op = self.wait(accepted.operation.id, timeout=wait_timeout, on_progress=on_progress)
        return self.get_vm(op.vm_id)

    @staticmethod
    def _page_params(limit: int | None, cursor: str | None) -> list[tuple[str, str]]:
        params: list[tuple[str, str]] = []
        if limit is not None:
            params.append(("limit", str(limit)))
        if cursor:
            params.append(("cursor", cursor))
        return params

    @staticmethod
    def _follow(fetch: Callable[[str | None], tuple[list[_P], str | None]]) -> Iterator[_P]:
        cursor: str | None = None
        while True:
            items, cursor = fetch(cursor)
            yield from items
            if not cursor:
                return

    # ---- capabilities and images ----------------------------------------

    def get_capabilities(self) -> HostCapabilities:
        """Report what this host and runtime support."""
        return _decode(HostCapabilities, self._call("getCapabilities"))

    def list_images(self, *, limit: int | None = None, cursor: str | None = None) -> ImageList:
        """Return one page of images available on the host."""
        return _decode(ImageList, self._call("listImages", params=self._page_params(limit, cursor)))

    def iter_images(self, *, limit: int | None = None) -> Iterator[Image]:
        """Iterate over every image, following ``nextCursor``."""

        def fetch(cursor: str | None) -> tuple[list[Image], str | None]:
            page = self.list_images(limit=limit, cursor=cursor)
            return page.items, page.next_cursor

        return self._follow(fetch)

    def get_image(self, image_id: str) -> Image:
        """Get one image by ID."""
        return _decode(Image, self._call("getImage", {"imageId": image_id}))

    # ---- VMs --------------------------------------------------------------

    def list_vms(
        self,
        *,
        states: Iterable[str] | None = None,
        all: bool = False,
        limit: int | None = None,
        cursor: str | None = None,
        labels: Mapping[str, str] | None = None,
    ) -> VMList:
        """Return one page of VMs ordered by ``(createdAt, id)``.

        Args:
            states: Only these states (default on the server: provisioning,
                ready and running). Ignored when ``all`` is true.
            all: Include every state, including deleted tombstones.
            limit: Page size (1..200, server default 50).
            cursor: ``next_cursor`` from the previous page.
            labels: Only VMs carrying every one of these labels
                (e.g. ``{"davit.box": "myproject"}``).
        """
        params = [("state", str(s)) for s in states or ()]
        if all:
            params.append(("all", "true"))
        params += [("label", f"{k}={v}") for k, v in (labels or {}).items()]
        params += self._page_params(limit, cursor)
        return _decode(VMList, self._call("listVMs", params=params))

    def iter_vms(
        self,
        *,
        states: Iterable[str] | None = None,
        all: bool = False,
        limit: int | None = None,
        labels: Mapping[str, str] | None = None,
    ) -> Iterator[VM]:
        """Iterate over every matching VM, following ``nextCursor``."""
        states = list(states) if states is not None else None

        def fetch(cursor: str | None) -> tuple[list[VM], str | None]:
            page = self.list_vms(states=states, all=all, limit=limit, cursor=cursor, labels=labels)
            return page.items, page.next_cursor

        return self._follow(fetch)

    def get_vm(self, vm: str) -> VM:
        """Get a VM by ID or name (deleted tombstones resolve by ID only)."""
        return _decode(VM, self._call("getVM", {"vmId": vm}))

    @overload
    def create_vm(
        self,
        name: str | None = ...,
        *,
        image: str | None = ...,
        snapshot: str | None = ...,
        shape: ShapeOverride | None = ...,
        type: str | None = ...,
        env: Mapping[str, str] | None = ...,
        ttl: Ttl | None = ...,
        auto_stop: bool | None = ...,
        require_golden: bool | None = ...,
        egress: Egress | None = ...,
        privacy: PrivacyPolicy | None = ...,
        environment: EnvironmentSpec | None = ...,
        labels: Mapping[str, str] | None = ...,
        idle_timeout: Ttl | None = ...,
        capacity_wait: Ttl | None = ...,
        wait: Literal[True] = ...,
        wait_timeout: float | None = ...,
        idempotency_key: str | None = ...,
        on_progress: ProgressCallback | None = ...,
    ) -> VM: ...
    @overload
    def create_vm(
        self,
        name: str | None = ...,
        *,
        image: str | None = ...,
        snapshot: str | None = ...,
        shape: ShapeOverride | None = ...,
        type: str | None = ...,
        env: Mapping[str, str] | None = ...,
        ttl: Ttl | None = ...,
        auto_stop: bool | None = ...,
        require_golden: bool | None = ...,
        egress: Egress | None = ...,
        privacy: PrivacyPolicy | None = ...,
        environment: EnvironmentSpec | None = ...,
        labels: Mapping[str, str] | None = ...,
        idle_timeout: Ttl | None = ...,
        capacity_wait: Ttl | None = ...,
        wait: Literal[False],
        wait_timeout: float | None = ...,
        idempotency_key: str | None = ...,
        on_progress: ProgressCallback | None = ...,
    ) -> OperationAccepted: ...
    def create_vm(
        self,
        name: str | None = None,
        *,
        image: str | None = None,
        snapshot: str | None = None,
        shape: ShapeOverride | None = None,
        type: str | None = None,
        env: Mapping[str, str] | None = None,
        ttl: Ttl | None = None,
        auto_stop: bool | None = None,
        require_golden: bool | None = None,
        egress: Egress | None = None,
        privacy: PrivacyPolicy | None = None,
        environment: EnvironmentSpec | None = None,
        labels: Mapping[str, str] | None = None,
        idle_timeout: Ttl | None = None,
        capacity_wait: Ttl | None = None,
        wait: bool = True,
        wait_timeout: float | None = None,
        idempotency_key: str | None = None,
        on_progress: ProgressCallback | None = None,
    ) -> VM | OperationAccepted:
        """Create a VM from an image (default image when omitted) or a snapshot.

        Args:
            name: VM name, unique per owner (generated by the server if omitted).
            image: Image digest or ref; exclusive with ``snapshot``.
            snapshot: Snapshot ID to start from; exclusive with ``image``.
            shape: Partial shape; omitted fields keep the defaults.
            type: Administrator-defined profile (``capabilities.profiles``);
                ``shape`` fields override it.
            env: Guest environment (never echoed back).
            ttl: Auto-stop after this many seconds (or a ``timedelta``).
            auto_stop: ``False`` disables TTL auto-stop.
            require_golden: Fail instead of falling back to a cold boot.
            egress: Egress policy, e.g. ``Egress(mode=EgressMode.ALLOWLIST,
                allow=["github.com"])`` (default: the host's ``network.egress``,
                else ``filtered``).
            privacy: Ephemeral/privacy policy (feature ``privacyPolicy``), e.g.
                ``PrivacyPolicy(ephemeral=True)`` deletes the VM when it stops;
                ``no_snapshot``, ``no_output_retention`` and ``no_process_samples``
                limit what the host keeps. Not secure erasure (see the README).
            environment: Pin an environment revision and prepare its workspace:
                an :class:`~davit.models.EnvironmentRef`, ``"NAME"`` (the latest
                revision) or ``"NAME@REVISION"`` (see :func:`parse_environment_ref`).
                A server without a workspace preparer raises
                :class:`~davit.ServerNotImplemented`.
            labels: Key/value tags (``list_vms(labels=...)`` filters on them);
                ``davit.box`` marks a project box.
            idle_timeout: Stop the VM (keeping its disk) after this many seconds
                (or a ``timedelta``) with no exec, process stream or ssh session.
            capacity_wait: When the host has no room even after stopping idle
                VMs, queue the request this long (seconds, or a ``timedelta``;
                0..3600) for capacity, first come first served (feature
                ``capacityReclaim``; ``waitSeconds``). Default: no wait, so a
                full host raises :class:`~davit.errors.ResourceExhausted` at
                once, as does the end of the wait. Not the operation wait
                (``wait``/``wait_timeout``).
            wait: Wait for the operation and return the VM (default), or
                return the ``OperationAccepted`` immediately.
            wait_timeout: Wait budget in seconds (default ``self.wait_timeout``).
            idempotency_key: Deduplicate separate calls (generated if omitted).
            on_progress: Called for each operation event while waiting.
        """
        body = CreateVMRequest(
            name=name,
            image=image,
            snapshot_id=snapshot,
            shape=shape,
            type=type,
            env=dict(env) if env is not None else None,
            ttl_seconds=_seconds(ttl),
            auto_stop=auto_stop,
            require_golden=require_golden,
            egress=egress,
            privacy=privacy,
            environment=_env_ref(environment),
            labels=dict(labels) if labels is not None else None,
            idle_timeout_seconds=_seconds(idle_timeout),
        )
        acc = self._mutate(
            "createVM", None, body.to_dict(), idempotency_key, None, _capacity_wait(capacity_wait)
        )
        return self._lifecycle(acc, wait, wait_timeout, on_progress)

    def create_vms(
        self,
        count: int,
        name: str | None = None,
        *,
        image: str | None = None,
        snapshot: str | None = None,
        shape: ShapeOverride | None = None,
        type: str | None = None,
        env: Mapping[str, str] | None = None,
        ttl: Ttl | None = None,
        auto_stop: bool | None = None,
        require_golden: bool | None = None,
        egress: Egress | None = None,
        privacy: PrivacyPolicy | None = None,
        wait: bool = False,
        wait_timeout: float | None = None,
        idempotency_key: str | None = None,
    ) -> CreateVMsResult:
        """Create ``count`` VMs (1-100) from one template (feature ``batchCreate``).

        The template takes the arguments of :meth:`create_vm`; ``name`` N names
        the VMs N-1 ... N-count (unnamed without it). The template is validated
        once: an invalid one raises and creates nothing. Each item is admitted
        on its own, so the result has one ``BatchItem`` per VM, in order, with
        its ``accepted`` operation or its ``error`` (e.g. ``resource_exhausted``
        when the host is full). Each item is idempotent on the batch key plus
        its index: calling again with the same ``idempotency_key`` returns the
        same operations and creates only the items that were refused before.

        Args:
            count: How many VMs, 1-100.
            privacy: Every VM's privacy policy, as in :meth:`create_vm`.
            wait: Also wait for every accepted operation (one shared budget);
                each accepted item then carries the terminal operation (state
                ``succeeded`` or ``failed`` with its error) and the VM.
                Failures are reported in the items, not raised.
            wait_timeout: Wait budget in seconds (default ``self.wait_timeout``).
            idempotency_key: Batch key (generated if omitted); keep it to
                retry a partially refused batch.

        Raises:
            WaitTimeout: ``wait`` ran out of budget; the operations continue.
        """
        template = CreateVMRequest(
            name=name,
            image=image,
            snapshot_id=snapshot,
            shape=shape,
            type=type,
            env=dict(env) if env is not None else None,
            ttl_seconds=_seconds(ttl),
            auto_stop=auto_stop,
            require_golden=require_golden,
            egress=egress,
            privacy=privacy,
        )
        body = CreateVMsRequest(count=count, template=template)
        headers = {"Idempotency-Key": idempotency_key or _transport.new_idempotency_key()}
        result = _decode(
            CreateVMsResult, self._call("createVMs", body=body.to_dict(), headers=headers)
        )
        if not wait:
            return result
        budget = self.wait_timeout if wait_timeout is None else wait_timeout
        deadline = time.monotonic() + budget
        items = []
        for item in result.items:
            if item.accepted is not None:
                op_id = item.accepted.operation.id
                try:
                    op = self.wait(op_id, timeout=max(0.0, deadline - time.monotonic()))
                except OperationFailed as exc:
                    op = exc.operation
                vm = (
                    self.get_vm(op.vm_id)
                    if op.state == OperationState.SUCCEEDED
                    else item.accepted.vm
                )
                item = dataclasses.replace(item, accepted=OperationAccepted(operation=op, vm=vm))
            items.append(item)
        return CreateVMsResult(items=items)

    def reclaim_vms(
        self,
        idle_seconds: Ttl,
        *,
        labels: Mapping[str, str] | None = None,
        dry_run: bool = False,
    ) -> ReclaimVMsResult:
        """Stop your VMs that have been idle at least ``idle_seconds`` (``davit box gc``).

        Feature ``capacityReclaim``. A VM qualifies when it is ready with no
        operation, runs no process, has no stream or tunnel open, holds no
        prompt-turn lease and has had no exec or tunnel activity for
        ``idle_seconds``; ephemeral and no-snapshot VMs never do, and no idle
        timeout is needed. Each is stopped (``Operation.reason`` ``gc``),
        oldest activity first, and listed with its stop's ``operation_id``;
        the stops are not waited for (use :meth:`wait`). Sends no
        ``Idempotency-Key``: each stop is keyed by its VM's generation and
        revision, so a repeat stops nothing twice.

        Args:
            idle_seconds: Minimum idle time, in seconds (or a ``timedelta``).
            labels: Only VMs carrying all of these labels.
            dry_run: Only list the VMs (no ``operation_id``), stopping none.
        """
        body = ReclaimVMsRequest(
            idle_seconds=_seconds(idle_seconds) or 0,
            labels=dict(labels) if labels is not None else None,
            dry_run=True if dry_run else None,
        )
        return _decode(ReclaimVMsResult, self._call("reclaimVMs", body=body.to_dict()))

    def prune_images(self, *, dry_run: bool = False) -> PruneImagesResult:
        """Remove the host's image blobs that no image uses (``davit box image prune``).

        Host owner only. A blob goes when no manifest names its digest, no
        image the daemon serves uses it, and it has not changed for an hour.

        Args:
            dry_run: Only list the blobs, removing none.
        """
        body = PruneImagesRequest(dry_run=True if dry_run else None)
        return _decode(PruneImagesResult, self._call("pruneImages", body=body.to_dict()))

    @overload
    def fork_vm(
        self,
        source: str,
        name: str | None = ...,
        *,
        snapshot: str | None = ...,
        live: bool = ...,
        shape: ShapeOverride | None = ...,
        type: str | None = ...,
        ttl: Ttl | None = ...,
        auto_stop: bool | None = ...,
        egress: Egress | None = ...,
        environment: EnvironmentSpec | None = ...,
        wait: Literal[True] = ...,
        wait_timeout: float | None = ...,
        idempotency_key: str | None = ...,
        if_match: int | str | None = ...,
        on_progress: ProgressCallback | None = ...,
    ) -> VM: ...
    @overload
    def fork_vm(
        self,
        source: str,
        name: str | None = ...,
        *,
        snapshot: str | None = ...,
        live: bool = ...,
        shape: ShapeOverride | None = ...,
        type: str | None = ...,
        ttl: Ttl | None = ...,
        auto_stop: bool | None = ...,
        egress: Egress | None = ...,
        environment: EnvironmentSpec | None = ...,
        wait: Literal[False],
        wait_timeout: float | None = ...,
        idempotency_key: str | None = ...,
        if_match: int | str | None = ...,
        on_progress: ProgressCallback | None = ...,
    ) -> OperationAccepted: ...
    def fork_vm(
        self,
        source: str,
        name: str | None = None,
        *,
        snapshot: str | None = None,
        live: bool = False,
        shape: ShapeOverride | None = None,
        type: str | None = None,
        ttl: Ttl | None = None,
        auto_stop: bool | None = None,
        egress: Egress | None = None,
        environment: EnvironmentSpec | None = None,
        wait: bool = True,
        wait_timeout: float | None = None,
        idempotency_key: str | None = None,
        if_match: int | str | None = None,
        on_progress: ProgressCallback | None = None,
    ) -> VM | OperationAccepted:
        """Create a new VM from ``source``'s committed disk, a snapshot, or the running VM.

        A source that has never been stopped has no committed disk and raises
        :class:`~davit.errors.StopRequired`.

        Args:
            source: Source VM ID or name.
            name: Name of the new VM.
            snapshot: Snapshot (ID or name) of ``source`` to fork instead.
            live: Fork the running VM itself (RAM, processes and disk at one
                instant; the source pauses briefly) instead of a committed
                disk. Needs the runtime's ``live_fork`` capability; the source
                must be ready or running and the shape is the source's.
            egress: Egress policy of the new VM (default: ``source``'s).
            environment: Re-pin another environment revision (default: the
                source's pin and prepared workspace).
            if_match: Expected ``state_revision`` of the source.

        The remaining arguments behave as in :meth:`create_vm`.
        """
        body = ForkVMRequest(
            snapshot_id=snapshot,
            name=name,
            shape=shape,
            type=type,
            ttl_seconds=_seconds(ttl),
            auto_stop=auto_stop,
            live=live or None,
            egress=egress,
            environment=_env_ref(environment),
        )
        acc = self._mutate("forkVM", {"vmId": source}, body.to_dict(), idempotency_key, if_match)
        return self._lifecycle(acc, wait, wait_timeout, on_progress)

    @overload
    def stop_vm(
        self,
        vm: str,
        *,
        force: bool = ...,
        wait: Literal[True] = ...,
        wait_timeout: float | None = ...,
        idempotency_key: str | None = ...,
        if_match: int | str | None = ...,
        on_progress: ProgressCallback | None = ...,
    ) -> VM: ...
    @overload
    def stop_vm(
        self,
        vm: str,
        *,
        force: bool = ...,
        wait: Literal[False],
        wait_timeout: float | None = ...,
        idempotency_key: str | None = ...,
        if_match: int | str | None = ...,
        on_progress: ProgressCallback | None = ...,
    ) -> OperationAccepted: ...
    def stop_vm(
        self,
        vm: str,
        *,
        force: bool = False,
        wait: bool = True,
        wait_timeout: float | None = None,
        idempotency_key: str | None = None,
        if_match: int | str | None = None,
        on_progress: ProgressCallback | None = None,
    ) -> VM | OperationAccepted:
        """Stop a VM, committing its disk (a stopped VM is a safe no-op).

        Args:
            force: Skip graceful guest shutdown (crash-consistent commit).
            if_match: Expected ``state_revision``; a stale value raises
                :class:`~davit.errors.RevisionMismatch`.

        The remaining arguments behave as in :meth:`create_vm`.
        """
        body = StopVMRequest(force=True if force else None)
        acc = self._mutate("stopVM", {"vmId": vm}, body.to_dict(), idempotency_key, if_match)
        return self._lifecycle(acc, wait, wait_timeout, on_progress)

    @overload
    def resume_vm(
        self,
        vm: str,
        *,
        type: str | None = ...,
        shape: ShapeOverride | None = ...,
        ttl: Ttl | None = ...,
        auto_stop: bool | None = ...,
        environment: EnvironmentSpec | None = ...,
        capacity_wait: Ttl | None = ...,
        wait: Literal[True] = ...,
        wait_timeout: float | None = ...,
        idempotency_key: str | None = ...,
        if_match: int | str | None = ...,
        on_progress: ProgressCallback | None = ...,
    ) -> VM: ...
    @overload
    def resume_vm(
        self,
        vm: str,
        *,
        type: str | None = ...,
        shape: ShapeOverride | None = ...,
        ttl: Ttl | None = ...,
        auto_stop: bool | None = ...,
        environment: EnvironmentSpec | None = ...,
        capacity_wait: Ttl | None = ...,
        wait: Literal[False],
        wait_timeout: float | None = ...,
        idempotency_key: str | None = ...,
        if_match: int | str | None = ...,
        on_progress: ProgressCallback | None = ...,
    ) -> OperationAccepted: ...
    def resume_vm(
        self,
        vm: str,
        *,
        type: str | None = None,
        shape: ShapeOverride | None = None,
        ttl: Ttl | None = None,
        auto_stop: bool | None = None,
        environment: EnvironmentSpec | None = None,
        capacity_wait: Ttl | None = None,
        wait: bool = True,
        wait_timeout: float | None = None,
        idempotency_key: str | None = None,
        if_match: int | str | None = None,
        on_progress: ProgressCallback | None = None,
    ) -> VM | OperationAccepted:
        """Resume a stopped or errored VM from its last committed disk.

        A ready VM is a safe no-op. ``environment`` re-pins the VM to that
        revision and prepares it again. ``type`` and ``shape`` resize the VM
        for this and later boots (feature ``resize``): ``shape`` fields
        override the profile, which overrides the VM's current shape. The disk
        can only grow (a smaller ``shape.disk_bytes`` raises
        :class:`~davit.errors.InvalidArgument`, reason ``disk_shrink``), and
        a ready or running VM must be stopped first
        (:class:`~davit.errors.StopRequired`). The guest filesystem is not
        grown automatically. Making room and ``capacity_wait`` work as in
        :meth:`create_vm`. Other arguments behave as in :meth:`create_vm` and
        :meth:`stop_vm`.
        """
        body = ResumeVMRequest(
            type=type,
            shape=shape,
            ttl_seconds=_seconds(ttl),
            auto_stop=auto_stop,
            environment=_env_ref(environment),
        )
        acc = self._mutate(
            "resumeVM",
            {"vmId": vm},
            body.to_dict(),
            idempotency_key,
            if_match,
            _capacity_wait(capacity_wait),
        )
        return self._lifecycle(acc, wait, wait_timeout, on_progress)

    @overload
    def recover_vm(
        self,
        vm: str,
        *,
        wait: Literal[True] = ...,
        wait_timeout: float | None = ...,
        idempotency_key: str | None = ...,
        if_match: int | str | None = ...,
        on_progress: ProgressCallback | None = ...,
    ) -> VM: ...
    @overload
    def recover_vm(
        self,
        vm: str,
        *,
        wait: Literal[False],
        wait_timeout: float | None = ...,
        idempotency_key: str | None = ...,
        if_match: int | str | None = ...,
        on_progress: ProgressCallback | None = ...,
    ) -> OperationAccepted: ...
    def recover_vm(
        self,
        vm: str,
        *,
        wait: bool = True,
        wait_timeout: float | None = None,
        idempotency_key: str | None = None,
        if_match: int | str | None = None,
        on_progress: ProgressCallback | None = None,
    ) -> VM | OperationAccepted:
        """Power-cycle a wedged VM on its current disk (feature ``recover``).

        The VMM is killed without a RAM or disk commit and the VM's current
        disk cold boots as is: files survive crash-consistent, as after a
        power loss; processes do not; no snapshot is taken. Accepted for
        ready, running and error VMs that still have a working disk; a
        stopped VM raises ``invalid_state`` (use :meth:`resume_vm`).
        Arguments behave as in :meth:`stop_vm`.
        """
        acc = self._mutate(
            "recoverVM", {"vmId": vm}, RecoverVMRequest().to_dict(), idempotency_key, if_match
        )
        return self._lifecycle(acc, wait, wait_timeout, on_progress)

    @overload
    def delete_vm(
        self,
        vm: str,
        *,
        wait: Literal[True] = ...,
        wait_timeout: float | None = ...,
        idempotency_key: str | None = ...,
        if_match: int | str | None = ...,
        on_progress: ProgressCallback | None = ...,
    ) -> VM: ...
    @overload
    def delete_vm(
        self,
        vm: str,
        *,
        wait: Literal[False],
        wait_timeout: float | None = ...,
        idempotency_key: str | None = ...,
        if_match: int | str | None = ...,
        on_progress: ProgressCallback | None = ...,
    ) -> OperationAccepted: ...
    def delete_vm(
        self,
        vm: str,
        *,
        wait: bool = True,
        wait_timeout: float | None = None,
        idempotency_key: str | None = None,
        if_match: int | str | None = None,
        on_progress: ProgressCallback | None = None,
    ) -> VM | OperationAccepted:
        """Delete a VM; when waiting, returns its ``deleted`` tombstone.

        Named snapshots of the VM survive. Arguments behave as in
        :meth:`stop_vm`.
        """
        acc = self._mutate("deleteVM", {"vmId": vm}, None, idempotency_key, if_match)
        return self._lifecycle(acc, wait, wait_timeout, on_progress)

    def update_vm(
        self,
        vm: str,
        *,
        name: str | None = None,
        ttl: Ttl | None = None,
        auto_stop: bool | None = None,
        egress: Egress | None = None,
        privacy: PrivacyPolicy | None = None,
        keep: bool | None = None,
        keep_hot: bool | None = None,
        if_match: int | str | None = None,
    ) -> VM:
        """Rename a VM, change its TTL or replace its egress or privacy policy
        (PATCH, synchronous); returns the VM.

        Only the given fields change. ``ttl`` (seconds or ``timedelta``) sets the
        TTL and, while the VM is ready or running, restarts the auto-stop
        countdown from now; ``auto_stop=True`` alone restarts it with the current
        TTL. ``egress`` is live on a running VM (see :meth:`set_vm_egress`).
        ``privacy`` replaces the whole policy (flags left unset are off); read
        the VM and change one field to keep the others.
        Naturally idempotent: no ``Idempotency-Key`` is sent.

        Args:
            keep: Pin (``True``) or unpin the VM against the low-disk auto-delete.
            keep_hot: ``True`` keeps the VM off the host's cold storage tier;
                ``False`` lets it go cold again.
            if_match: Expected ``state_revision``; a stale value raises
                :class:`~davit.errors.RevisionMismatch`.
        """
        body = UpdateVMRequest(
            name=name,
            ttl_seconds=_seconds(ttl),
            auto_stop=auto_stop,
            egress=egress,
            privacy=privacy,
            keep=keep,
            keep_hot=keep_hot,
        )
        headers = {"If-Match": str(if_match)} if if_match is not None else None
        data = self._call("updateVM", {"vmId": vm}, body=body.to_dict(), headers=headers)
        return _decode(VM, data)

    def extend_vm(
        self, vm: str, ttl: Ttl | None = None, *, if_match: int | str | None = None
    ) -> VM:
        """Restart a VM's auto-stop countdown, like ``davit extend``.

        With ``ttl`` (seconds > 0, or a ``timedelta``) it also becomes the VM's
        TTL. Auto-stop is (re-)enabled either way.
        """
        if ttl is not None:
            if isinstance(ttl, bool) or not isinstance(ttl, (int, float, timedelta)):
                raise TypeError("ttl must be seconds (int/float) or a timedelta")
            if (_seconds(ttl) or 0) <= 0:
                raise ValueError("ttl must be at least one second")
        return self.update_vm(vm, ttl=ttl, auto_stop=True, if_match=if_match)

    def rename_vm(self, vm: str, new_name: str, *, if_match: int | str | None = None) -> VM:
        """Rename a VM, like ``davit rename`` (the server validates the name)."""
        return self.update_vm(vm, name=new_name, if_match=if_match)

    def set_vm_egress(
        self,
        vm: str,
        mode: EgressMode | str,
        *,
        allow: Sequence[str] | None = None,
        if_match: int | str | None = None,
    ) -> VM:
        """Replace a VM's egress policy and return the VM (``updateVM`` with
        only ``egress``).

        Live on a running VM (new connections and DNS queries; flows the new
        policy no longer allows are closed) and kept for later boots of a
        stopped one. Synchronous and naturally idempotent: no
        ``Idempotency-Key`` is sent.

        Args:
            vm: VM ID or name.
            mode: ``"off"``, ``"filtered"`` or ``"allowlist"`` (:class:`EgressMode`).
            allow: Allowlist entries (``allowlist`` only): domains (``github.com``
                also matches subdomains, ``*.example.com`` only subdomains) and
                IPv4 addresses/CIDRs. The VM reports them normalized (lowercase,
                masked, sorted, deduplicated).

        Raises:
            InvalidArgument: Bad mode or entry, or ``allow`` without ``allowlist``.
            OperationInProgress: A lifecycle operation is running (retryable).
            Unavailable: A running VM could not be updated; the old policy stays.
        """
        if isinstance(allow, (str, bytes)):
            raise TypeError("allow must be a list of strings, e.g. ['github.com']")
        egress = Egress(mode=EgressMode(mode), allow=list(allow) if allow is not None else None)
        return self.update_vm(vm, egress=egress, if_match=if_match)

    def wait_ready(self, vm: str | VM, *, timeout: float | None = None) -> VM:
        """Wait for a VM's active operation (if any) and return it once ready.

        Raises the typed error from ``VM.lastError`` (or
        :class:`~davit.errors.InvalidState`) if the VM ends up neither
        ``ready`` nor ``running``.
        """
        current = vm if isinstance(vm, VM) else self.get_vm(vm)
        if current.active_operation_id:
            self.wait(current.active_operation_id, timeout=timeout)
        current = self.get_vm(current.id)
        if current.state in _READY:
            return current
        if current.last_error is not None:
            raise error_from_info(current.last_error)
        raise InvalidState(f"vm {current.id} is {current.state}, not ready")

    # ---- snapshots --------------------------------------------------------

    def list_vm_snapshots(
        self, vm: str, *, limit: int | None = None, cursor: str | None = None
    ) -> SnapshotList:
        """Return one page of snapshots whose source is ``vm``."""
        data = self._call("listVMSnapshots", {"vmId": vm}, params=self._page_params(limit, cursor))
        return _decode(SnapshotList, data)

    def iter_vm_snapshots(self, vm: str, *, limit: int | None = None) -> Iterator[Snapshot]:
        """Iterate over every snapshot of ``vm``, following ``nextCursor``."""

        def fetch(cursor: str | None) -> tuple[list[Snapshot], str | None]:
            page = self.list_vm_snapshots(vm, limit=limit, cursor=cursor)
            return page.items, page.next_cursor

        return self._follow(fetch)

    def list_snapshots(
        self, *, limit: int | None = None, cursor: str | None = None
    ) -> SnapshotList:
        """Return one page of snapshots visible to the caller."""
        return _decode(
            SnapshotList, self._call("listSnapshots", params=self._page_params(limit, cursor))
        )

    def iter_snapshots(self, *, limit: int | None = None) -> Iterator[Snapshot]:
        """Iterate over every visible snapshot, following ``nextCursor``."""

        def fetch(cursor: str | None) -> tuple[list[Snapshot], str | None]:
            page = self.list_snapshots(limit=limit, cursor=cursor)
            return page.items, page.next_cursor

        return self._follow(fetch)

    def get_snapshot(self, snapshot: str) -> Snapshot:
        """Get a snapshot by ID or by the name of one of the caller's named snapshots."""
        return _decode(Snapshot, self._call("getSnapshot", {"snapshotId": snapshot}))

    @overload
    def create_snapshot(
        self,
        vm: str,
        name: str,
        *,
        wait: Literal[True] = ...,
        wait_timeout: float | None = ...,
        idempotency_key: str | None = ...,
        if_match: int | str | None = ...,
        on_progress: ProgressCallback | None = ...,
    ) -> Snapshot: ...
    @overload
    def create_snapshot(
        self,
        vm: str,
        name: str,
        *,
        wait: Literal[False],
        wait_timeout: float | None = ...,
        idempotency_key: str | None = ...,
        if_match: int | str | None = ...,
        on_progress: ProgressCallback | None = ...,
    ) -> OperationAccepted: ...
    def create_snapshot(
        self,
        vm: str,
        name: str,
        *,
        wait: bool = True,
        wait_timeout: float | None = None,
        idempotency_key: str | None = None,
        if_match: int | str | None = None,
        on_progress: ProgressCallback | None = None,
    ) -> Snapshot | OperationAccepted:
        """Name the committed disk state of a stopped VM (operation ``snapshotVM``).

        A ready or running VM raises :class:`~davit.errors.StopRequired`.
        When waiting, returns the new :class:`~davit.models.Snapshot`.
        Arguments behave as in :meth:`stop_vm`.
        """
        body = SnapshotVMRequest(name=name)
        acc = self._mutate("snapshotVM", {"vmId": vm}, body.to_dict(), idempotency_key, if_match)
        if not wait:
            return acc
        op = self.wait(acc.operation.id, timeout=wait_timeout, on_progress=on_progress)
        if op.result is None or not op.result.snapshot_id:
            raise Internal("snapshot operation succeeded without a snapshotId", operation_id=op.id)
        return self.get_snapshot(op.result.snapshot_id)

    def delete_snapshot(self, snapshot: str) -> None:
        """Delete a snapshot reference by ID or name (synchronous).

        A snapshot something still depends on raises
        :class:`~davit.errors.InvalidState`; a repeat raises
        :class:`~davit.errors.NotFound`.
        """
        self._call("deleteSnapshot", {"snapshotId": snapshot})

    # ---- snapshot files (read without booting) ------------------------------

    def list_snapshot_tree(
        self,
        snapshot: str,
        *,
        path: str = "/",
        limit: int | None = None,
        cursor: str | None = None,
    ) -> SnapshotTree:
        """Return one page of the entries of directory ``path`` in a snapshot.

        The snapshot (ID or name) is read on the host without booting it.
        Paths resolve inside the snapshot (``..`` stops at ``/``, symlinks stay
        within it); a symlink named by ``path`` is followed, and entries that are
        symlinks carry ``link_target``. The cursor is bound to the snapshot and path.
        """
        check_path(path)
        params = [("path", path), *self._page_params(limit, cursor)]
        data = self._call("listSnapshotTree", {"snapshotId": snapshot}, params=params)
        return _decode(SnapshotTree, data)

    def iter_snapshot_tree(
        self, snapshot: str, *, path: str = "/", limit: int | None = None
    ) -> Iterator[SnapshotEntry]:
        """Iterate over every entry of directory ``path`` in a snapshot (follows ``nextCursor``)."""

        def fetch(cursor: str | None) -> tuple[list[SnapshotEntry], str | None]:
            page = self.list_snapshot_tree(snapshot, path=path, limit=limit, cursor=cursor)
            return page.items, page.next_cursor

        return self._follow(fetch)

    def _snapshot_stream(
        self,
        operation_id: str,
        snapshot: str,
        params: list[tuple[str, str]],
        *,
        path: str,
        archive: bool,
        verify_sha256: bool,
        what: str,
    ) -> FileStream:
        _, route = self._route(operation_id, {"snapshotId": snapshot})
        headers = {"X-Request-Id": _transport.new_request_id()}
        resp = _transport.open_stream(
            self._http, "GET", self.base_url + route, headers=headers, params=params
        )
        return FileStream(resp, path=path, archive=archive, verify_sha256=verify_sha256, what=what)

    def read_snapshot_file_stream(
        self, snapshot: str, path: str, *, archive: bool = False
    ) -> FileStream:
        """Open a streamed read of a snapshot file (or, with ``archive``, a directory as tar).

        Reads the snapshot (ID or name) without booting it; path resolution as
        in :meth:`list_snapshot_tree`. Consume the returned
        :class:`~davit.files.FileStream` inside a ``with`` block. A regular
        file is verified when fully consumed: ``Content-Length`` and the
        ``X-Davit-Content-Sha256`` digest (``stream.sha256``); a truncated or
        corrupted transfer raises :class:`~davit.TransportError` at the end.
        An archive is checked for its tar end marker; its digest is an HTTP
        trailer that is not read. A directory needs ``archive=True``.
        """
        check_path(path)
        params = [("path", path)]
        if archive:
            params.append(("archive", "tar"))
        return self._snapshot_stream(
            "readSnapshotFile",
            snapshot,
            params,
            path=path,
            archive=archive,
            verify_sha256=not archive,
            what=f"{path} in snapshot {snapshot}",
        )

    def read_snapshot_file(self, snapshot: str, path: str, *, archive: bool = False) -> FileContent:
        """Read a whole snapshot file into memory; see :meth:`read_snapshot_file_stream`.

        A file's bytes are checked against ``X-Davit-Content-Sha256`` (a
        mismatch raises :class:`~davit.TransportError`); the verified digest is
        returned as ``sha256``.
        """
        with self.read_snapshot_file_stream(snapshot, path, archive=archive) as stream:
            data = stream.read()
            return FileContent(data, stream.mode, None if archive else stream.sha256)

    def export_snapshot(self, snapshot: str, *, format: ExportFormat = "tar") -> FileStream:
        """Open a streamed export of a snapshot (ID or name), without booting it.

        ``format="tar"`` is the whole filesystem (as :meth:`read_snapshot_file_stream`
        of ``/`` with ``archive=True``, checked for its end marker); ``"raw"`` is
        the disk image byte for byte (a chunked stream; writing it sparse is up
        to you). Exports are disk-sized: use ``copy_to(fileobj)`` or
        iterate inside a ``with`` block. The stream digest is an HTTP trailer that
        is not read.
        """
        if format not in ("tar", "raw"):
            raise ValueError(f"format must be 'tar' or 'raw', got {format!r}")
        return self._snapshot_stream(
            "exportSnapshot",
            snapshot,
            [("format", format)],
            path="/",
            archive=format == "tar",
            verify_sha256=False,
            what=f"{format} export of snapshot {snapshot}",
        )

    # ---- processes --------------------------------------------------------

    def exec_process(
        self,
        vm: str,
        argv: Sequence[str],
        *,
        cwd: str | None = None,
        env: Mapping[str, str] | None = None,
        user: str | None = None,
        deadline_seconds: int | None = None,
        detached: bool | None = None,
        stdin: bool | None = None,
        tty: bool | None = None,
        idempotency_key: str | None = None,
    ) -> Process:
        """Start a process in the VM and return it (attach with :meth:`attach_process`).

        ``argv`` runs directly; a shell is never inferred (use
        ``["sh", "-c", "..."]``). A non-detached process is killed when its
        first attached stream closes before it exits, or when nothing attaches
        within 60 seconds.

        Args:
            vm: VM ID or name (must be ``ready`` or ``running``).
            argv: Program and arguments; a non-empty list of strings.
            cwd: Working directory in the guest.
            env: Extra environment variables.
            user: Guest user (default: the image's).
            deadline_seconds: Kill the process group after this many seconds.
            detached: Survive stream closes; re-attach any time.
            stdin: Keep stdin open for the first attached stream.
            tty: Allocate a terminal (servers may answer ``UnsupportedCapability``).
            idempotency_key: Same key and body returns the same process
                (generated once per call if omitted).
        """
        body = ExecRequest(
            argv=_check_argv(argv),
            cwd=cwd,
            env=dict(env) if env is not None else None,
            user=user,
            deadline_seconds=_check_deadline(deadline_seconds),
            detached=detached,
            stdin=stdin,
            tty=tty,
        )
        key = idempotency_key or _transport.new_idempotency_key()
        data = self._call(
            "execProcess", {"vmId": vm}, body=body.to_dict(), headers={"Idempotency-Key": key}
        )
        return _decode(Process, data)

    def list_processes(
        self, vm: str, *, limit: int | None = None, cursor: str | None = None
    ) -> ProcessList:
        """Return one page of the VM's processes, oldest first in ``(createdAt, id)`` order.

        A start interrupted by a daemon crash is reported with status ``lost``.
        """
        data = self._call("listProcesses", {"vmId": vm}, params=self._page_params(limit, cursor))
        return _decode(ProcessList, data)

    def iter_processes(self, vm: str, *, limit: int | None = None) -> Iterator[Process]:
        """Iterate over every process of the VM, oldest first, following ``nextCursor``."""

        def fetch(cursor: str | None) -> tuple[list[Process], str | None]:
            page = self.list_processes(vm, limit=limit, cursor=cursor)
            return page.items, page.next_cursor

        return self._follow(fetch)

    def get_process(self, vm: str, process_id: str) -> Process:
        """Get one process."""
        data = self._call("getProcess", {"vmId": vm, "processId": process_id})
        return _decode(Process, data)

    def kill_process(self, vm: str, process_id: str, *, signal: str | None = None) -> Process:
        """Signal a process group (default ``SIGTERM``; e.g. ``"KILL"`` or ``"SIGINT"``)."""
        params = [("signal", signal)] if signal else None
        data = self._call("killProcess", {"vmId": vm, "processId": process_id}, params=params)
        return _decode(Process, data)

    def attach_process(
        self,
        vm: str,
        process_id: str,
        *,
        output_cursor: int = 0,
        max_message_bytes: int = _websocket.DEFAULT_MAX_MESSAGE,
    ) -> ProcessStream:
        """Attach to a process's stdio over a WebSocket (operation ``attachProcess``).

        Output replays every retained chunk with ``seq > output_cursor``. See
        :class:`~davit.processes.ProcessStream` for events, input and
        reconnect behaviour. The stream uses the client's socket or
        ``base_url`` directly (a custom ``transport`` does not apply).
        """
        _, path = self._route("attachProcess", {"vmId": vm, "processId": process_id})

        def dial(cursor: int) -> _websocket.WebSocket:
            headers = {"X-Request-Id": _transport.new_request_id(), **self._headers}
            return _transport.open_websocket(
                socket_path=self.socket,
                base_url=self.base_url,
                path=f"{path}?outputCursor={cursor}",
                headers=headers,
                timeout=self.timeout,
                max_message=max_message_bytes,
                ssl_context=self._ssl_context,
            )

        return ProcessStream(dial, process_id=process_id, output_cursor=output_cursor)

    def run(
        self,
        vm: str,
        argv: Sequence[str],
        *,
        input: bytes | str | None = None,
        deadline_seconds: int | None = None,
        cwd: str | None = None,
        env: Mapping[str, str] | None = None,
        user: str | None = None,
        idempotency_key: str | None = None,
    ) -> RunResult:
        """Run a command to completion and collect its output, like ``davit exec``.

        Sends ``input`` (if given) and closes stdin. The deadline is enforced
        by the server; a process it kills reports exit code 124. A non-zero
        exit is not an exception.

        Raises:
            TransportError: The stream was lost and could not be re-attached.
        """
        proc = self.exec_process(
            vm,
            argv,
            cwd=cwd,
            env=env,
            user=user,
            deadline_seconds=deadline_seconds,
            stdin=True if input is not None else None,
            idempotency_key=idempotency_key,
        )
        out = {"stdout": bytearray(), "stderr": bytearray()}
        with self.attach_process(vm, proc.id) as stream:
            if input is not None:
                stream.send_stdin(input)
                stream.close_stdin()
            for event in stream:
                if event.type in out:
                    out[event.type] += event.data
            final = stream.process
        return RunResult(exit_status(final), bytes(out["stdout"]), bytes(out["stderr"]), final)

    # ---- job history and retained output ----------------------------------

    def list_process_history(
        self, vm: str, *, limit: int | None = None, cursor: str | None = None
    ) -> HistoryList:
        """Return one page of the VM's managed jobs across generations, newest first.

        Each :class:`~davit.models.HistoryEntry` carries the process and its
        retained ``stdout_bytes``/``stderr_bytes``. Reads host records, so it
        works while the VM is stopped.

        Raises:
            ServerNotImplemented: The host has no guest transport.
        """
        data = self._call(
            "listProcessHistory", {"vmId": vm}, params=self._page_params(limit, cursor)
        )
        return _decode(HistoryList, data)

    def list_guest_processes(self, vm: str) -> GuestProcessList:
        """Sample every process in the VM's guest (``ps``).

        A running VM is sampled live; managed jobs carry their ``process_id``.
        A VM that is not running answers with its last sample (``stale``).

        Raises:
            NotFound: The VM is not running and was never sampled.
            ServerNotImplemented: The host has no guest transport.
        """
        return _decode(GuestProcessList, self._call("listGuestProcesses", {"vmId": vm}))

    def iter_process_history(self, vm: str, *, limit: int | None = None) -> Iterator[HistoryEntry]:
        """Iterate over every managed job of the VM, newest first, following ``nextCursor``."""

        def fetch(cursor: str | None) -> tuple[list[HistoryEntry], str | None]:
            page = self.list_process_history(vm, limit=limit, cursor=cursor)
            return page.items, page.next_cursor

        return self._follow(fetch)

    def read_process_output(
        self,
        vm: str,
        process_id: str,
        *,
        after: str | int | None = None,
        stream: str | None = None,
        limit: int | None = None,
    ) -> ProcessOutput:
        """Return one page of a job's host-retained output (operation ``readProcessOutput``).

        Works while the VM is stopped. Items are ``stdout``/``stderr``
        :class:`~davit.models.ProcessStreamMessage` objects whose ``data`` is
        base64; :meth:`iter_process_output` decodes them. Pass ``next_cursor``
        as ``after`` to continue; ``complete`` is true once the process ended
        and every retained chunk was returned.

        Args:
            after: Cursor from a previous page (default: the beginning).
            stream: ``"stdout"``, ``"stderr"`` or ``"both"`` (server default).
            limit: Chunks per page (the server caps it).
        """
        params: list[tuple[str, str]] = []
        if after is not None and after != "":
            params.append(("after", str(after)))
        if stream:
            params.append(("stream", stream))
        if limit is not None:
            params.append(("limit", str(limit)))
        data = self._call("readProcessOutput", {"vmId": vm, "processId": process_id}, params=params)
        return _decode(ProcessOutput, data)

    def _output_pages(
        self,
        vm: str,
        process_id: str,
        after: str | int | None,
        stream: str | None,
        limit: int | None,
    ) -> Iterator[tuple[ProcessOutput, str | None]]:
        cursor = None if after is None or after == "" else str(after)
        while True:
            page = self.read_process_output(
                vm, process_id, after=cursor, stream=stream, limit=limit
            )
            cursor = page.next_cursor or cursor
            yield page, cursor
            if page.complete or not page.next_cursor:
                return

    def iter_process_output(
        self,
        vm: str,
        process_id: str,
        *,
        after: str | int | None = None,
        stream: str | None = None,
        limit: int | None = None,
    ) -> Iterator[StreamEvent]:
        """Yield a job's retained output chunks, decoded, until the retained end.

        Follows ``next_cursor`` until a page is ``complete`` or brings no new
        chunk (the process may still be running: read again later from the
        last ``seq``). Each item is a :class:`~davit.StreamEvent` with
        ``type`` ``"stdout"`` or ``"stderr"``; unknown message types are skipped.
        """
        for page, _ in self._output_pages(vm, process_id, after, stream, limit):
            for msg in page.items:
                if msg.type in ("stdout", "stderr"):
                    data = _b64(msg.data, f"process {process_id} output seq {msg.seq}")
                    yield StreamEvent(msg.type, msg.seq or 0, data)

    def collect_process_output(
        self,
        vm: str,
        process_id: str,
        *,
        after: str | int | None = None,
        stream: str | None = None,
        limit: int | None = None,
    ) -> RetainedOutput:
        """Read a job's retained output to the end and return its stdout and stderr bytes.

        Pages like :meth:`iter_process_output`. ``complete`` reports whether
        the process had ended and everything retained was read; ``cursor``
        resumes a later read. Works while the VM is stopped.
        """
        out = {"stdout": bytearray(), "stderr": bytearray()}
        complete, cursor = False, None
        for page, last in self._output_pages(vm, process_id, after, stream, limit):
            complete, cursor = page.complete, last
            for msg in page.items:
                if msg.type in out:
                    out[msg.type] += _b64(msg.data, f"process {process_id} output seq {msg.seq}")
        return RetainedOutput(bytes(out["stdout"]), bytes(out["stderr"]), complete, cursor)

    # ---- runs and push ------------------------------------------------------

    def create_run(
        self,
        argv: Sequence[str],
        *,
        image: str | None = None,
        snapshot: str | None = None,
        type: str | None = None,
        shape: ShapeOverride | None = None,
        environment: EnvironmentSpec | None = None,
        cwd: str | None = None,
        env: Mapping[str, str] | None = None,
        timeout_seconds: int | None = None,
        keep: bool = False,
        hold: bool = False,
        output_grace_seconds: int | None = None,
        idempotency_key: str | None = None,
    ) -> Run:
        """Create a VM, run ``argv`` in it, keep the exit code and clean up (``createRun``).

        Returns the run at once (state ``creating``); it advances on the host.
        Follow it with :meth:`wait_run`, or use :meth:`run_to_completion`.

        - ``outcome`` (with ``decided_at``) is the result as soon as it is known;
          ``state`` becomes it only after cleanup (output grace, then the VM is
          deleted unless ``keep``), when ``ended_at`` is set.
        - ``failed`` covers a non-zero exit (``exit_code``), a signal kill
          (``signal``, no error), the timeout (``error.code == "timeout"``) and
          setup failures (``error``).
        - After the command exits, the VM stays for an output grace so its
          output can still be read through ``process_id``
          (:meth:`attach_process`): ``output_grace_seconds`` (0..600, 0 none;
          omitted: the host default, 5 s unless configured). Once ``outcome``
          is set, :meth:`cancel_run` only ends that grace and keeps it.
        - With ``hold`` the run stops in ``waiting`` (VM ready) until
          :meth:`start_run`; push files first with :meth:`push_tree`.

        Args:
            argv: The command (no shell is inferred).
            image: Image digest or ref; exclusive with ``snapshot``.
            snapshot: Snapshot ID to start from.
            type: Administrator-defined profile.
            shape: Partial shape override.
            environment: ``EnvironmentRef`` or ``"NAME[@REVISION]"`` (as in
                :meth:`create_vm`).
            cwd: Working directory of the command.
            env: Environment of the command.
            timeout_seconds: Command deadline (0 or omitted: none).
            keep: Keep the VM after the run.
            hold: Stop in ``waiting`` until :meth:`start_run`.
            output_grace_seconds: How long the VM outlives the command (0..600).
            idempotency_key: Same key and body return the same run, another
                body raises :class:`~davit.IdempotencyConflict` (generated
                once per call if omitted).
        """
        if timeout_seconds is not None and (
            isinstance(timeout_seconds, bool)
            or not isinstance(timeout_seconds, int)
            or timeout_seconds < 0
        ):
            raise ValueError("timeout_seconds must be an integer >= 0")
        body = RunRequest(
            argv=_check_argv(argv),
            image=image,
            snapshot_id=snapshot,
            type=type,
            shape=shape,
            environment=_env_ref(environment),
            cwd=cwd,
            env=dict(env) if env is not None else None,
            timeout_seconds=timeout_seconds,
            keep=True if keep else None,
            hold=True if hold else None,
            output_grace_seconds=_check_grace(output_grace_seconds),
        )
        data = self._call(
            "createRun",
            body=body.to_dict(),
            headers={"Idempotency-Key": idempotency_key or _transport.new_idempotency_key()},
        )
        return _decode(Run, data)

    def list_runs(self, *, limit: int | None = None, cursor: str | None = None) -> RunList:
        """Return one page of the caller's runs, oldest first."""
        return _decode(RunList, self._call("listRuns", params=self._page_params(limit, cursor)))

    def iter_runs(self, *, limit: int | None = None) -> Iterator[Run]:
        """Iterate over every run, oldest first, following ``nextCursor``."""

        def fetch(cursor: str | None) -> tuple[list[Run], str | None]:
            page = self.list_runs(limit=limit, cursor=cursor)
            return page.items, page.next_cursor

        return self._follow(fetch)

    def get_run(self, run: str | Run) -> Run:
        """Get a run: its state, VM, process, exit code and error."""
        return _decode(Run, self._call("getRun", {"runId": _run_id(run)}))

    def start_run(self, run: str | Run) -> Run:
        """Start the command of a run created with ``hold``.

        Raises:
            InvalidState: The run is not ``waiting``.
        """
        return _decode(Run, self._call("startRun", {"runId": _run_id(run)}))

    def cancel_run(self, run: str | Run) -> Run:
        """Cancel a run, or end the output grace of a run whose outcome is decided.

        Before the outcome: kills the command and deletes the VM (unless
        ``keep``); ``outcome`` (and later ``state``) becomes ``cancelled``.
        After it (``outcome``/``decided_at`` set): keeps the outcome and starts
        cleanup at once. On an
        ended run it changes nothing, so a repeat is harmless. Returns the run
        as it is now; :meth:`wait_run` for the end.
        """
        return _decode(Run, self._call("cancelRun", {"runId": _run_id(run)}))

    def wait_run(self, run: str | Run, *, timeout: float | None = None) -> Run:
        """Poll :meth:`get_run` until the run has ended (``ended_at`` set) and return it.

        A failed or cancelled run is returned, not raised. Stopping early
        never cancels the run.

        Raises:
            WaitTimeout: The budget (default ``self.wait_timeout``) ran out;
                ``details["runId"]`` names the run.
        """
        return self._poll_run(_run_id(run), lambda r: r.ended_at is not None, timeout)

    def _poll_run(self, run_id: str, done: Callable[[Run], bool], timeout: float | None) -> Run:
        budget = self.wait_timeout if timeout is None else timeout
        deadline = time.monotonic() + budget
        while True:
            current = self.get_run(run_id)
            if done(current):
                return current
            if time.monotonic() >= deadline:
                raise WaitTimeout(
                    f"stopped waiting after {budget:g}s; run {run_id} continues on the host",
                    details={"runId": run_id},
                )
            time.sleep(min(RUN_POLL_INTERVAL, max(0.0, deadline - time.monotonic())))

    def push_tree(self, vm: str, path: str, data: WriteData, *, mode: str = "merge") -> PushResult:
        """Apply a tar of files under the guest directory ``path`` (operation ``pushTree``).

        ``data`` is the tar (``bytes``, a binary file object or an iterable of
        ``bytes``; see :meth:`write_file`), sent as ``application/x-tar``.
        ``mode="merge"`` adds and replaces files and keeps the others;
        ``"mirror"`` (also delete files the tree lacks) is passed through and
        this server answers :class:`~davit.ServerNotImplemented`. Naturally
        idempotent: sends no ``Idempotency-Key``. :meth:`push_dir` builds the
        tar from a local git work tree.
        """
        check_path(path)
        content, replayable = body_of(data)
        _, route = self._route("pushTree", {"vmId": vm})
        resp = _transport.open_stream(
            self._http,
            "PUT",
            self.base_url + route,
            headers={
                "X-Request-Id": _transport.new_request_id(),
                "Content-Type": "application/x-tar",
            },
            params=[("path", path), ("mode", mode)],
            content=content,
            retry=replayable,
        )
        try:
            body = resp.read()
        except httpx.HTTPError as exc:
            raise TransportError(f"pushing to {path}: {exc}", retryable=True) from exc
        finally:
            resp.close()
        try:
            parsed = json.loads(body)
        except ValueError as exc:
            raise Internal("decoding response: body is not JSON") from exc
        return _decode(PushResult, parsed)

    def push_dir(
        self,
        vm: str,
        local_dir: str | os.PathLike[str],
        path: str = DEFAULT_PUSH_PATH,
        *,
        mode: str = "merge",
    ) -> PushResult:
        """Push a local git work tree's files to ``path``, like ``davit push``.

        Selects tracked plus untracked-not-ignored files (submodules included,
        never ``.git``; see :func:`davit.push.git_selection`), builds the tar
        in a spooled temporary file and calls :meth:`push_tree`.

        Raises:
            ValueError: ``local_dir`` is not in a git work tree.
        """
        with self._tree_tar(local_dir) as spool:
            return self.push_tree(vm, path, spool, mode=mode)

    @staticmethod
    @contextlib.contextmanager
    def _tree_tar(local_dir: str | os.PathLike[str]) -> Iterator[IO[bytes]]:
        paths = git_selection(local_dir)
        with tempfile.SpooledTemporaryFile(max_size=8 << 20) as spool:
            write_tree_tar(local_dir, paths, spool)
            spool.seek(0)
            yield spool

    def run_to_completion(
        self,
        argv: Sequence[str],
        *,
        files: str | os.PathLike[str] | bytes | None = None,
        files_path: str = DEFAULT_PUSH_PATH,
        image: str | None = None,
        snapshot: str | None = None,
        type: str | None = None,
        shape: ShapeOverride | None = None,
        environment: EnvironmentSpec | None = None,
        cwd: str | None = None,
        env: Mapping[str, str] | None = None,
        timeout_seconds: int | None = None,
        keep: bool = False,
        output_grace_seconds: int | None = None,
        idempotency_key: str | None = None,
        timeout: float | None = None,
    ) -> RunOutcome:
        """Do a whole run and return its exit code and output, like ``davit run``.

        Steps: :meth:`create_run` (with ``hold`` when ``files`` are given);
        wait for the VM; :meth:`push_tree` the files to ``files_path`` and
        :meth:`start_run`; attach to the command and collect its output until
        it exits; once ``outcome``/``decided_at`` is set, :meth:`cancel_run` to
        end the output grace (skipped with ``output_grace_seconds=0``); then
        :meth:`wait_run` for cleanup. ``exit_code`` and ``signal`` report how
        the command ended.

        Args:
            files: A local directory (pushed as its git selection, see
                :meth:`push_dir`) or a ready tar as ``bytes``.
            files_path: Guest directory for ``files``; also the default ``cwd``
                when files are given.
            timeout: Client wait budget for each wait (default
                ``self.wait_timeout``); a timeout never cancels the run.

        The other arguments are those of :meth:`create_run`. A failed or
        cancelled command is returned (check ``exit_code`` and ``run``).

        Raises:
            DavitError: The typed error of a run that failed before its
                command started (``details["runId"]`` names it), or of a
                failed push (the run is then cancelled).
            WaitTimeout: A wait ran out of budget; the run continues.
        """
        with contextlib.ExitStack() as stack:
            tar: bytes | IO[bytes] | None = None
            if isinstance(files, (bytes, bytearray, memoryview)):
                tar = bytes(files)
            elif files is not None:  # selected before the VM exists: a bad dir costs nothing
                tar = stack.enter_context(self._tree_tar(files))
            if tar is not None and cwd is None:
                cwd = files_path
            run = self.create_run(
                argv,
                image=image,
                snapshot=snapshot,
                type=type,
                shape=shape,
                environment=environment,
                cwd=cwd,
                env=env,
                timeout_seconds=timeout_seconds,
                keep=keep,
                hold=tar is not None,
                output_grace_seconds=output_grace_seconds,
                idempotency_key=idempotency_key,
            )
            run = self._poll_run(
                run.id,
                lambda r: bool(r.ended_at or r.state == RunState.WAITING or r.process_id),
                timeout,
            )
            pushed = None
            if tar is not None and run.state == RunState.WAITING and not run.ended_at:
                try:
                    pushed = self.push_tree(run.vm_id or "", files_path, tar)
                except BaseException:
                    with contextlib.suppress(DavitError):  # nothing ran: give the VM back
                        self.cancel_run(run.id)
                    raise
                with contextlib.suppress(InvalidState):  # cancelled meanwhile
                    self.start_run(run.id)
                run = self._poll_run(run.id, lambda r: bool(r.ended_at or r.process_id), timeout)

        if not run.process_id:  # the command never started
            run = run if run.ended_at else self.wait_run(run.id, timeout=timeout)
            if run.error is not None and run.state != RunState.CANCELLED:
                raise error_for(
                    str(run.error.code),
                    run.error.message,
                    retryable=run.error.retryable,
                    details={**(run.error.details or {}), "runId": run.id},
                )
            return RunOutcome(run, _run_exit(run, None), b"", b"", pushed, run.signal)

        out = {"stdout": bytearray(), "stderr": bytearray()}
        final: Process | None = None
        cursor = 0
        # The stream can end with the VM (cleanup), or find it already
        # deleting (a short grace); the run says how it went.
        with (
            contextlib.suppress(TransportError, InvalidState, NotFound),
            self.attach_process(run.vm_id or "", run.process_id) as stream,
        ):
            for event in stream:
                if event.type in out:
                    out[event.type] += event.data
                    cursor = event.seq
            final = stream.process
        if final is None:  # read the rest from the host-retained output
            with contextlib.suppress(ServerNotImplemented, NotFound):
                for event in self.iter_process_output(
                    run.vm_id or "", run.process_id, after=cursor or None
                ):
                    out[event.type] += event.data
        run = self._poll_run(run.id, _decided, timeout)
        if run.ended_at is None and output_grace_seconds != 0:
            run = self.cancel_run(run.id)  # outcome decided: end the output grace
        run = self.wait_run(run.id, timeout=timeout)
        return RunOutcome(
            run,
            _run_exit(run, final),
            bytes(out["stdout"]),
            bytes(out["stderr"]),
            pushed,
            run.signal or (final.signal if final is not None else None),
        )

    # ---- tunnels ----------------------------------------------------------

    def open_tunnel(self, vm: str, port: int) -> Tunnel:
        """Connect to ``port`` on the guest's loopback and return the tunnel (``openTunnel``).

        The tunnel holds a live connection and must be attached once (with
        :meth:`attach_tunnel`) before ``expires_at`` (30 s), else the server
        closes it. :meth:`connect_tunnel` does both.

        Raises:
            Unavailable: Nothing listens on the guest port, or the guest agent
                cannot be reached.
        """
        if isinstance(port, bool) or not isinstance(port, int) or not 1 <= port <= 65535:
            raise ValueError("port must be an integer in 1..65535")
        body = OpenTunnelRequest(port=port).to_dict()
        return _decode(Tunnel, self._call("openTunnel", {"vmId": vm}, body=body))

    def close_tunnel(self, tunnel: str | Tunnel) -> None:
        """Close an unattached tunnel (``closeTunnel``).

        An attached tunnel ends when its stream closes and is then
        :class:`~davit.errors.NotFound` here, as is an expired one.
        """
        tunnel_id = tunnel.id if isinstance(tunnel, Tunnel) else tunnel
        self._call("closeTunnel", {"tunnelId": tunnel_id})

    def attach_tunnel(
        self,
        tunnel: str | Tunnel,
        *,
        max_message_bytes: int = _websocket.DEFAULT_MAX_MESSAGE,
    ) -> TunnelStream:
        """Attach to an opened tunnel's bytes over a WebSocket (``attachTunnel``; single use).

        See :class:`~davit.tunnels.TunnelStream`. Like :meth:`attach_process`,
        it uses the client's socket or ``base_url`` directly.
        """
        tunnel_id = tunnel.id if isinstance(tunnel, Tunnel) else tunnel
        _, path = self._route("attachTunnel", {"tunnelId": tunnel_id})
        ws = _transport.open_websocket(
            socket_path=self.socket,
            base_url=self.base_url,
            path=path,
            headers={"X-Request-Id": _transport.new_request_id(), **self._headers},
            timeout=self.timeout,
            max_message=max_message_bytes,
            ssl_context=self._ssl_context,
        )
        return TunnelStream(
            ws, tunnel_id=tunnel_id, tunnel=tunnel if isinstance(tunnel, Tunnel) else None
        )

    def connect_tunnel(self, vm: str, port: int) -> TunnelStream:
        """Open and attach a tunnel to ``port`` on the guest's loopback.

        ::

            with sw.connect_tunnel("web", 8080) as conn:
                conn.send(b"GET / HTTP/1.0\\r\\n\\r\\n")
                conn.close_write()
                reply = b"".join(conn)
        """
        tunnel = self.open_tunnel(vm, port)
        try:
            return self.attach_tunnel(tunnel)
        except BaseException:
            with contextlib.suppress(DavitError):
                self.close_tunnel(tunnel)  # do not hold the guest connection for 30 s
            raise

    # ---- reverse forwards ---------------------------------------------------

    def list_reverse_forwards(self, vm: str) -> ReverseForwardList:
        """List the VM's open reverse forwards (``listReverseForwards``; owner or admin)."""
        return _decode(ReverseForwardList, self._call("listReverseForwards", {"vmId": vm}))

    def create_reverse_forward(
        self, vm: str, guest_port: int, target: ReverseTarget
    ) -> ReverseForward:
        """Listen on ``127.0.0.1:guest_port`` in the VM and bridge to ``target``
        (``createReverseForward``; owner or admin).

        A ``session`` target must be attached (``attachReverseSession``)
        before ``session_expires_at`` (30 s); :meth:`reverse_forward` does
        that and the bridging. A ``host`` target is an ``ip:port`` the
        operator listed in ``network.reverseHostServices``
        (:meth:`forward_host_service`).

        Raises:
            Conflict: The guest port already has a forward.
            PermissionDenied: A host address the operator did not list.
            UnsupportedCapability: The guest agent cannot listen
                (capabilities feature ``reverseForward``).
        """
        _check_port(guest_port, "guest_port")
        body = CreateReverseForwardRequest(guest_port=guest_port, target=target).to_dict()
        return _decode(ReverseForward, self._call("createReverseForward", {"vmId": vm}, body=body))

    def revoke_reverse_forward(self, vm: str, forward: str | ReverseForward) -> None:
        """Revoke a reverse forward and cut its live connections (``revokeReverseForward``)."""
        forward_id = forward.id if isinstance(forward, ReverseForward) else forward
        self._call("revokeReverseForward", {"vmId": vm, "forwardId": forward_id})

    def forward_host_service(self, vm: str, guest_port: int, address: str) -> ReverseForward:
        """Bridge ``127.0.0.1:guest_port`` in the VM to the operator-listed host
        service ``address`` (``ip:port``); the daemon dials it. Revoke with
        :meth:`revoke_reverse_forward`.
        """
        target = ReverseTarget(kind=ReverseTargetKind.HOST, address=address)
        return self.create_reverse_forward(vm, guest_port, target)

    def reverse_forward(
        self, vm: str, guest_port: int, local_host: str, local_port: int
    ) -> ReverseSession:
        """Make ``local_host:local_port`` (reachable from this client) reachable
        from ``127.0.0.1:guest_port`` in the VM, like
        ``davit forward --reverse VM GUESTPORT:HOST:PORT``.

        Creates a ``session`` forward (labelled ``local_host:local_port``),
        attaches its session stream and bridges each guest connection on a
        background thread. Closing the returned session revokes the forward::

            with sw.reverse_forward("web", 5432, "127.0.0.1", 5432) as session:
                sw.run("web", ["psql", "-h", "127.0.0.1", "-c", "select 1"])
            session.reason  # "closed"

        If the attach fails, the forward is revoked (best effort).
        """
        _check_port(local_port, "local_port")
        bracketed = f"[{local_host}]" if ":" in local_host else local_host
        label = f"{bracketed}:{local_port}"
        forward = self.create_reverse_forward(
            vm, guest_port, ReverseTarget(kind=ReverseTargetKind.SESSION, address=label)
        )
        params = {"vmId": vm, "forwardId": forward.id}
        try:
            session = self._open_ws("attachReverseSession", params)
        except BaseException:
            with contextlib.suppress(DavitError):
                self.revoke_reverse_forward(vm, forward)
            raise

        def dial_connection(connection_id: str) -> _websocket.WebSocket:
            conn_params = {**params, "connectionId": connection_id}
            return self._open_ws("attachReverseConnection", conn_params)

        def dial_local() -> socket.socket:
            sock = socket.create_connection((local_host, local_port), timeout=LOCAL_DIAL_TIMEOUT)
            sock.settimeout(None)
            return sock

        return ReverseSession(
            forward, session, dial_connection=dial_connection, dial_local=dial_local
        )

    def _open_ws(self, operation_id: str, path_params: Mapping[str, str]) -> _websocket.WebSocket:
        _, path = self._route(operation_id, path_params)
        return _transport.open_websocket(
            socket_path=self.socket,
            base_url=self.base_url,
            path=path,
            headers={"X-Request-Id": _transport.new_request_id(), **self._headers},
            timeout=self.timeout,
            max_message=_websocket.DEFAULT_MAX_MESSAGE,
            ssl_context=self._ssl_context,
        )

    # ---- app endpoints ------------------------------------------------------

    def list_app_endpoints(self, vm: str) -> AppEndpointList:
        """List the VM's app endpoints with their tailnet URLs, oldest first
        (``listAppEndpoints``)."""
        return _decode(AppEndpointList, self._call("listAppEndpoints", {"vmId": vm}))

    def create_app_endpoint(
        self, vm: str, port: int, name: str | None = None, *, forward_identity: bool = False
    ) -> AppEndpoint:
        """Give the app on guest loopback ``port`` a stable tailnet-private URL
        (``createAppEndpoint``).

        The endpoint is served at ``https://<gateway>:<gateway_port>/`` on the
        shared tailnet gateway, to the owner or a bearer token with ``exec`` on
        the VM. It belongs to the VM: stopping it keeps the URL (requests get
        503 until it runs again); :meth:`delete_app_endpoint` removes it; forks
        inherit none. ``name`` defaults to ``port-<port>``. With
        ``forward_identity`` the app gets the caller's tailnet login
        (``Tailscale-User-Login``); off by default, the guest is untrusted.

        Raises:
            Conflict: The port or the name already has an endpoint.
            UnsupportedCapability: No tailnet gateway (capabilities feature
                ``appEndpoints``).
        """
        _check_port(port, "port")
        req = CreateAppEndpointRequest(
            port=port, name=name, forward_identity=forward_identity or None
        )
        body = req.to_dict()
        return _decode(AppEndpoint, self._call("createAppEndpoint", {"vmId": vm}, body=body))

    def expose(
        self, vm: str, port: int, name: str | None = None, *, forward_identity: bool = False
    ) -> AppEndpoint:
        """Alias of :meth:`create_app_endpoint`: ``sw.expose("web", 3000).url``."""
        return self.create_app_endpoint(vm, port, name, forward_identity=forward_identity)

    def update_app_endpoint(
        self,
        vm: str,
        endpoint: str | AppEndpoint,
        *,
        name: str | None = None,
        hidden: bool | None = None,
        forward_identity: bool | None = None,
    ) -> AppEndpoint:
        """Rename, hide or show an app endpoint (``updateAppEndpoint``); only the
        given fields change. ``endpoint`` is its ID, its name or the object.

        ``hidden=True`` closes its gateway port and cuts live connections;
        ``hidden=False`` serves it again at the same URL. A rename keeps the URL.

        Raises:
            Conflict: Another endpoint of the VM has ``name``.
        """
        req = UpdateAppEndpointRequest(name=name, hidden=hidden, forward_identity=forward_identity)
        body = req.to_dict()
        params = {"vmId": vm, "endpointId": _ref(endpoint, "endpoint")}
        return _decode(AppEndpoint, self._call("updateAppEndpoint", params, body=body))

    def hide_app_endpoint(self, vm: str, endpoint: str | AppEndpoint) -> AppEndpoint:
        """Stop serving an endpoint, keeping its URL (``update_app_endpoint(hidden=True)``)."""
        return self.update_app_endpoint(vm, endpoint, hidden=True)

    def show_app_endpoint(self, vm: str, endpoint: str | AppEndpoint) -> AppEndpoint:
        """Serve a hidden endpoint again at the same URL (``hidden=False``)."""
        return self.update_app_endpoint(vm, endpoint, hidden=False)

    def rename_app_endpoint(self, vm: str, endpoint: str | AppEndpoint, name: str) -> AppEndpoint:
        """Rename an endpoint; its URL (port-based) does not change."""
        return self.update_app_endpoint(vm, endpoint, name=name)

    def delete_app_endpoint(self, vm: str, endpoint: str | AppEndpoint) -> None:
        """Delete an endpoint, freeing its gateway port and cutting live
        connections (``deleteAppEndpoint``)."""
        self._call("deleteAppEndpoint", {"vmId": vm, "endpointId": _ref(endpoint, "endpoint")})

    # ---- desktop ------------------------------------------------------------

    def get_desktop(self, vm: str) -> AppEndpoint:
        """The VM's open desktop: its app endpoint of kind ``desktop``
        (``getDesktop``).

        Raises:
            NotFound: No desktop is open (or no such VM).
        """
        return _decode(AppEndpoint, self._call("getDesktop", {"vmId": vm}))

    def open_desktop(self, vm: str, port: int | None = None) -> AppEndpoint:
        """Serve the VM's VNC desktop as a tailnet-private web viewer
        (``openDesktop``); an already open desktop is returned as is.

        The desktop is the VM's app endpoint of kind ``desktop`` for the guest
        VNC server on loopback ``port`` (server default 5900, x11vnc in the
        desktop image profile). Its ``url`` serves the built-in viewer and
        ``<url>websockify`` a websockify-compatible RFB stream (noVNC works).
        ``view`` rights are view-only (the gateway drops input), ``exec`` adds
        input. A stop cuts the stream (503 at the same URL until it runs
        again); :meth:`close_desktop` removes it.

        Raises:
            UnsupportedCapability: No tailnet gateway (capabilities feature
                ``desktop``).
        """
        body = None
        if port is not None:
            _check_port(port, "port")
            body = OpenDesktopRequest(port=port).to_dict()
        return _decode(AppEndpoint, self._call("openDesktop", {"vmId": vm}, body=body))

    def close_desktop(self, vm: str) -> None:
        """Close the VM's desktop, cutting every viewer (``closeDesktop``)."""
        self._call("closeDesktop", {"vmId": vm})

    # ---- ssh ----------------------------------------------------------------

    def issue_ssh_credential(
        self,
        vm: str,
        public_key: str,
        *,
        guest_user: str | None = None,
        ttl_seconds: int | None = None,
    ) -> SSHCredential:
        """Sign ``public_key`` for SSH into the VM (operation ``issueSSHCredential``).

        ``public_key`` is the caller's OpenSSH ``authorized_keys`` line; the
        SDK never generates keys. The certificate is valid for the VM's
        current generation and ``guest_user`` (default ``user``) for
        ``ttl_seconds`` (1..600, default 600). Pin ``host_keys`` (they match
        ``vm.endpoint.ssh_host_key_fingerprints``); never disable host-key
        checking.

        Raises:
            InvalidArgument: ``public_key`` is not a valid public key.
            InvalidState: The VM is not ready/running or has no host keys.
            UnsupportedCapability: The runtime has no guest transport.
        """
        if ttl_seconds is not None and (
            isinstance(ttl_seconds, bool)
            or not isinstance(ttl_seconds, int)
            or not 1 <= ttl_seconds <= 600
        ):
            raise ValueError("ttl_seconds must be an integer in 1..600")
        body = IssueSSHCredentialRequest(
            public_key=public_key, guest_user=guest_user, ttl_seconds=ttl_seconds
        ).to_dict()
        return _decode(SSHCredential, self._call("issueSSHCredential", {"vmId": vm}, body=body))

    # ---- tokens -------------------------------------------------------------

    def create_token(
        self,
        name: str,
        *,
        actions: Sequence[str] | None = None,
        vm: str | Sequence[str] | None = None,
        scopes: Sequence[TokenScope] | None = None,
        ttl_seconds: int,
        idempotency_key: str | None = None,
    ) -> CreatedToken:
        """Create a scoped, expiring API token (operation ``createToken``).

        Give either ``actions`` (``"view"``, ``"exec"``, ``"admin"``; exec
        implies view, admin implies both) with an optional ``vm`` (a VM name or
        ID, or a list of them: one scope each; omitted means every VM), or
        explicit ``scopes``. Only the owner identity (local socket or tailnet
        WhoIs owner) may manage tokens; a client using a token gets
        :class:`~davit.PermissionDenied`.

        The returned :attr:`CreatedToken.secret` (``slw_...``) is shown only
        once and is kept out of the model's ``repr``. Use it as the ``token`` of
        an ``https`` :class:`Client` on another tailnet host. Always sends an
        ``Idempotency-Key`` (``idempotency_key`` or generated once per call):
        a replay within 10 minutes returns the same token.

        Args:
            name: Display name (1..64 printable bytes).
            actions: Actions granted on every VM in ``vm``.
            vm: Limit the token to this VM or these VMs.
            scopes: Explicit scopes instead of ``actions``/``vm``.
            ttl_seconds: Lifetime in seconds, 1..31536000 (one year).
            idempotency_key: Replay key for this create.

        Raises:
            InvalidArgument: Bad name, ttl or action.
            NotFound: A scoped VM does not exist.
            PermissionDenied: The caller is not the owner identity.
        """
        if (actions is None) == (scopes is None):
            raise TypeError("give exactly one of actions= or scopes=")
        if scopes is not None and vm is not None:
            raise TypeError("vm= goes with actions=; put vm_id in each scope instead")
        if isinstance(actions, str):
            raise TypeError("actions must be a list of action names")
        if (
            isinstance(ttl_seconds, bool)
            or not isinstance(ttl_seconds, int)
            or not 1 <= ttl_seconds <= 31_536_000
        ):
            raise ValueError("ttl_seconds must be an integer in 1..31536000")
        if scopes is None:
            acts = list(actions or [])
            vms = [vm] if isinstance(vm, str) else list(vm or [])
            scopes = [TokenScope(vm_id=v, actions=acts) for v in vms] or [TokenScope(actions=acts)]
        body = CreateTokenRequest(name=name, scopes=list(scopes), ttl_seconds=ttl_seconds)
        data = self._call(
            "createToken",
            body=body.to_dict(),
            headers={"Idempotency-Key": idempotency_key or _transport.new_idempotency_key()},
        )
        return _decode(CreatedToken, data)

    def list_tokens(self, *, limit: int | None = None, cursor: str | None = None) -> TokenList:
        """Return one page of the caller's active tokens (never their secrets)."""
        return _decode(TokenList, self._call("listTokens", params=self._page_params(limit, cursor)))

    def iter_tokens(self, *, limit: int | None = None) -> Iterator[Token]:
        """Iterate over every active token, following ``nextCursor``."""

        def fetch(cursor: str | None) -> tuple[list[Token], str | None]:
            page = self.list_tokens(limit=limit, cursor=cursor)
            return page.items, page.next_cursor

        return self._follow(fetch)

    def revoke_token(self, token: str | Token | CreatedToken) -> None:
        """Revoke a token by ID; it stops working on its next request.

        Raises:
            NotFound: No such active token (for example, already revoked).
        """
        token_id = token if isinstance(token, str) else token.id
        if not token_id:
            raise ValueError("token id must be non-empty")
        self._call("revokeToken", {"tokenId": token_id})

    # ---- members and shares -------------------------------------------------

    def list_members(self) -> MemberList:
        """List the tailnet users who may use shared resources (``listMembers``).

        Members and shares are owner-only (local socket or the owner's tailnet
        identity); tokens and members get :class:`~davit.PermissionDenied`.

        Raises:
            UnsupportedCapability: No tailnet gateway (capabilities feature
                ``members``).
        """
        return _decode(MemberList, self._call("listMembers"))

    def add_member(self, login: str, role: MemberRole | str) -> Member:
        """Add a tailnet user as a member with a default ``role`` (``addMember``).

        The login is pinned to the stable tailnet user ID on the member's
        first connection; from then on a recycled login is refused. A member
        sees and uses only what is shared with it (:meth:`create_share`).

        Raises:
            Conflict: The login is already a member.
            UnsupportedCapability: No tailnet gateway (feature ``members``).
        """
        if not login:
            raise ValueError("login must be non-empty")
        body = AddMemberRequest(login=login, role=MemberRole(role)).to_dict()
        return _decode(Member, self._call("addMember", body=body))

    def update_member(self, member: str | Member, role: MemberRole | str) -> Member:
        """Change a member's role (``updateMember``); its live sessions are cut.
        ``member`` is its ID, its login or the object."""
        body = UpdateMemberRequest(role=MemberRole(role)).to_dict()
        params = {"memberId": _ref(member, "member")}
        return _decode(Member, self._call("updateMember", params, body=body))

    def remove_member(self, member: str | Member) -> None:
        """Remove a member with its shares, cutting its live sessions
        (``removeMember``). ``member`` is its ID, its login or the object."""
        self._call("removeMember", {"memberId": _ref(member, "member")})

    def list_shares(
        self,
        kind: ShareKind | str | None = None,
        resource: str | VM | Snapshot | Environment | None = None,
    ) -> ShareList:
        """List shares (``listShares``), optionally of one ``kind`` and
        ``resource`` (an ID, a name or the object; give ``kind`` with it)."""
        params: list[tuple[str, str]] = []
        if kind is not None:
            params.append(("kind", ShareKind(kind).value))
        if resource is not None:
            params.append(("resource", _ref(resource, "resource")))
        return _decode(ShareList, self._call("listShares", params=params))

    def create_share(
        self,
        kind: ShareKind | str,
        resource: str | VM | Snapshot | Environment,
        member: str | Member,
        role: MemberRole | str | None = None,
        *,
        credentials: bool = False,
    ) -> Share:
        """Share a VM, snapshot or environment template (``createShare``).

        ``member`` is a member ID, its login, the object, or ``"*"`` for every
        member; ``role`` defaults to the member's own role. Roles nest as
        viewer < exec < admin. Sharing the same resource with the same member
        again replaces the role and ``credentials`` (a downgrade cuts that
        member's live sessions). Without ``credentials`` an exec or admin
        share pauses the VM's credential grants for everyone while it exists.

        Raises:
            NotFound: No such resource or member.
            UnsupportedCapability: No tailnet gateway (feature ``members``).
        """
        req = CreateShareRequest(
            kind=ShareKind(kind),
            resource=_ref(resource, "resource"),
            member=_ref(member, "member"),
            role=None if role is None else MemberRole(role),
            credentials=credentials or None,
        )
        return _decode(Share, self._call("createShare", body=req.to_dict()))

    def share_vm(
        self,
        vm: str | VM,
        member: str | Member,
        role: MemberRole | str | None = None,
        *,
        credentials: bool = False,
    ) -> Share:
        """:meth:`create_share` of a VM."""
        return self.create_share(ShareKind.VM, vm, member, role, credentials=credentials)

    def share_snapshot(
        self,
        snapshot: str | Snapshot,
        member: str | Member,
        role: MemberRole | str | None = None,
        *,
        credentials: bool = False,
    ) -> Share:
        """:meth:`create_share` of a snapshot."""
        return self.create_share(
            ShareKind.SNAPSHOT, snapshot, member, role, credentials=credentials
        )

    def share_environment(
        self,
        environment: str | Environment,
        member: str | Member,
        role: MemberRole | str | None = None,
        *,
        credentials: bool = False,
    ) -> Share:
        """:meth:`create_share` of an environment template."""
        return self.create_share(
            ShareKind.ENVIRONMENT, environment, member, role, credentials=credentials
        )

    def delete_share(self, share: str | Share) -> None:
        """Unshare by share ID or object (``deleteShare``); the members' live
        sessions on the resource (API streams, SSH, app endpoint connections,
        their reverse forwards) are cut."""
        self._call("deleteShare", {"shareId": _ref(share, "share")})

    def unshare(
        self,
        kind: ShareKind | str,
        resource: str | VM | Snapshot | Environment,
        member: str | Member,
    ) -> None:
        """Delete the share of ``resource`` with ``member`` (an ID, a login, the
        object or ``"*"``), found with :meth:`list_shares`.

        Raises:
            NotFound: The resource is not shared with that member.
        """
        kind = ShareKind(kind)
        ref = _ref(member, "member")
        shares = [s for s in self.list_shares(kind, resource).items if s.kind == kind]
        match = next((s for s in shares if s.member == ref), None)
        if match is None and ref != "*" and shares:
            # A share names its member by ID: resolve a login.
            ids = {m.id for m in self.list_members().items if ref in (m.id, m.login)}
            match = next((s for s in shares if s.member in ids), None)
        if match is None:
            raise NotFound(f"{kind.value} {_ref(resource, 'resource')} is not shared with {ref}")
        self.delete_share(match)

    def rotate_token(
        self,
        token: str | Token | CreatedToken,
        *,
        overlap: float | timedelta = 0,
        ttl_seconds: int | None = None,
        idempotency_key: str | None = None,
    ) -> CreatedToken:
        """Replace a token's secret, keeping its name and scopes (``rotateToken``).

        The old secret stops working at once, or after ``overlap`` (seconds or
        a ``timedelta``; the old token's expiry only ever moves earlier). The
        new token lives ``ttl_seconds``, else as long as the old one was issued
        for. Its :attr:`CreatedToken.secret` is shown once. Always sends an
        ``Idempotency-Key``: a replay within 10 minutes returns the same token.

        Raises:
            NotFound: No such active token.
            PermissionDenied: The caller is not the owner identity.
        """
        token_id = token if isinstance(token, str) else token.id
        if not token_id:
            raise ValueError("token id must be non-empty")
        secs = overlap.total_seconds() if isinstance(overlap, timedelta) else overlap
        if isinstance(secs, bool) or secs < 0:
            raise ValueError("overlap must be a non-negative number of seconds or a timedelta")
        body = RotateTokenRequest(overlap_seconds=int(secs) or None, ttl_seconds=ttl_seconds)
        data = self._call(
            "rotateToken",
            {"tokenId": token_id},
            body=body.to_dict(),
            headers={"Idempotency-Key": idempotency_key or _transport.new_idempotency_key()},
        )
        return _decode(CreatedToken, data)

    def whoami(self) -> Me:
        """Who this client is to the daemon (``whoami``): principal, auth
        method (``local``, ``tailnet`` or ``token``) and, for a token, its
        scopes. Never returns a secret."""
        return _decode(Me, self._call("whoami"))

    # ---- credentials --------------------------------------------------------

    def list_credential_profiles(
        self, *, limit: int | None = None, cursor: str | None = None
    ) -> CredentialProfileList:
        """Return one page of credential profiles (secrets are never returned)."""
        data = self._call("listCredentialProfiles", params=self._page_params(limit, cursor))
        return _decode(CredentialProfileList, data)

    def iter_credential_profiles(self, *, limit: int | None = None) -> Iterator[CredentialProfile]:
        """Iterate over every credential profile, following ``nextCursor``."""

        def fetch(cursor: str | None) -> tuple[list[CredentialProfile], str | None]:
            page = self.list_credential_profiles(limit=limit, cursor=cursor)
            return page.items, page.next_cursor

        return self._follow(fetch)

    def create_credential_profile(
        self,
        name: str,
        *,
        default: bool | None = None,
        idempotency_key: str | None = None,
    ) -> CredentialProfile:
        """Create an empty credential profile (operation ``createCredentialProfile``).

        ``default=True`` makes it the owner's default profile (the previous
        default is cleared). Always sends an ``Idempotency-Key``
        (``idempotency_key`` or generated once per call): a replay of the same
        body returns the same profile.
        """
        body = CreateCredentialProfileRequest(name=name, default=default)
        data = self._call(
            "createCredentialProfile",
            body=body.to_dict(),
            headers={"Idempotency-Key": idempotency_key or _transport.new_idempotency_key()},
        )
        return _decode(CredentialProfile, data)

    def get_credential_profile(self, profile: str | CredentialProfile) -> CredentialProfile:
        """Return a credential profile by ID or name."""
        data = self._call("getCredentialProfile", {"profileId": _ref(profile, "profile")})
        return _decode(CredentialProfile, data)

    def delete_credential_profile(self, profile: str | CredentialProfile) -> None:
        """Delete a profile and its secrets; its grants are revoked (``profile_deleted``)."""
        self._call("deleteCredentialProfile", {"profileId": _ref(profile, "profile")})

    def put_credential(
        self,
        profile: str | CredentialProfile,
        name: str,
        *,
        kind: CredentialKind | str,
        env_var: str,
        secret: str = "",
        provider: str | None = None,
        base_url: str | None = None,
    ) -> CredentialProfile:
        """Create or replace a credential in a profile; returns the profile.

        The ``secret`` is write-only: stored on the host only (never in a guest)
        and never returned; the SDK never logs it. An empty ``secret`` keeps the
        stored one (a new credential needs one). ``api_key`` credentials of
        providers ``anthropic`` and ``openai`` default their upstream to the
        provider's API; ``custom_api`` needs ``base_url`` (http or https).
        Existing grants use a new secret from their next request. Naturally
        idempotent: no ``Idempotency-Key`` is sent.

        Args:
            profile: Profile ID or name.
            name: Credential name within the profile.
            kind: ``"api_key"`` or ``"custom_api"`` (:class:`CredentialKind`).
            env_var: Guest variable that carries a grant's placeholder.
            secret: The secret value.
            provider: Provider (``anthropic``, ``openai``, ...).
            base_url: Upstream base URL.
        """
        body = PutCredentialRequest(
            kind=CredentialKind(kind),
            provider=provider,
            base_url=base_url,
            env_var=env_var,
            secret=secret,
        )
        data = self._call(
            "putCredential",
            {"profileId": _ref(profile, "profile"), "credentialName": _ref(name, "credential")},
            body=body.to_dict(),
        )
        return _decode(CredentialProfile, data)

    def delete_credential(self, profile: str | CredentialProfile, name: str) -> None:
        """Remove a credential and its secret; its grants are revoked (``credential_removed``)."""
        self._call(
            "deleteCredential",
            {"profileId": _ref(profile, "profile"), "credentialName": _ref(name, "credential")},
        )

    def list_credential_grants(
        self, vm: str, *, limit: int | None = None, cursor: str | None = None
    ) -> CredentialGrantList:
        """Return the VM's credential grants, active and revoked, oldest first."""
        data = self._call(
            "listCredentialGrants", {"vmId": vm}, params=self._page_params(limit, cursor)
        )
        return _decode(CredentialGrantList, data)

    def create_credential_grant(
        self,
        vm: str,
        profile: str | CredentialProfile,
        credential: str,
        *,
        idempotency_key: str | None = None,
    ) -> CredentialGrant:
        """Grant a VM proxied use of a credential (operation ``createCredentialGrant``).

        The guest sees only the grant's ``placeholder`` (in ``env_var``) and
        points its client at the host credential proxy,
        ``http://172.16.0.1:15080``; the proxy swaps the placeholder for the
        secret on requests to the credential's ``upstream``, for this VM only.
        Stop keeps the grant dormant, resume reuses it, a fork child inherits
        none, delete revokes it. Always sends an ``Idempotency-Key``
        (``idempotency_key`` or generated once per call): a replay of the same
        body returns the same grant.
        """
        body = CreateCredentialGrantRequest(profile=_ref(profile, "profile"), credential=credential)
        data = self._call(
            "createCredentialGrant",
            {"vmId": vm},
            body=body.to_dict(),
            headers={"Idempotency-Key": idempotency_key or _transport.new_idempotency_key()},
        )
        return _decode(CredentialGrant, data)

    def revoke_credential_grant(self, vm: str, grant: str | CredentialGrant) -> CredentialGrant:
        """Revoke a grant and return it; its placeholder stops working at once
        (requests in flight are cut)."""
        data = self._call("revokeCredentialGrant", {"vmId": vm, "grantId": _ref(grant, "grant")})
        return _decode(CredentialGrant, data)

    # ---- environments -------------------------------------------------------

    def list_environments(
        self, *, limit: int | None = None, cursor: str | None = None
    ) -> EnvironmentList:
        """Return one page of the caller's environments."""
        return _decode(
            EnvironmentList, self._call("listEnvironments", params=self._page_params(limit, cursor))
        )

    def iter_environments(self, *, limit: int | None = None) -> Iterator[Environment]:
        """Iterate over every environment, following ``nextCursor``."""

        def fetch(cursor: str | None) -> tuple[list[Environment], str | None]:
            page = self.list_environments(limit=limit, cursor=cursor)
            return page.items, page.next_cursor

        return self._follow(fetch)

    def create_environment(
        self, name: str, manifest: Manifest, *, idempotency_key: str | None = None
    ) -> Environment:
        """Create an environment with its first revision (``latest_revision == 1``).

        ``manifest`` is an :class:`~davit.models.EnvManifest` or its wire JSON
        object (camelCase keys; omitted lists and maps are empty, unknown keys
        raise ``ValueError``). It holds no secret values: ``secretFiles`` name
        host secrets. Always sends an ``Idempotency-Key`` (``idempotency_key``
        or generated once per call); the server also returns the existing
        environment for the same name and an identical first manifest.

        Raises:
            InvalidArgument: Bad name or manifest, or the name is taken.
            ServerNotImplemented: The manifest names a ``credentialProfile``
                and this server has no credential proxy.
        """
        body = CreateEnvironmentRequest(name=name, manifest=_manifest(manifest))
        data = self._call(
            "createEnvironment",
            body=body.to_dict(),
            headers={"Idempotency-Key": idempotency_key or _transport.new_idempotency_key()},
        )
        return _decode(Environment, data)

    def get_environment(self, environment: str) -> Environment:
        """Get an environment by ID (``env_...``) or name."""
        data = self._call("getEnvironment", {"environmentId": environment})
        return _decode(Environment, data)

    def delete_environment(self, environment: str | Environment) -> None:
        """Delete an environment and its revisions (VMs keep their pinned copy).

        Naturally idempotent: no ``Idempotency-Key`` is sent; a repeat raises
        :class:`~davit.NotFound`.
        """
        ref = environment.id if isinstance(environment, Environment) else environment
        self._call("deleteEnvironment", {"environmentId": ref})

    def list_environment_revisions(
        self, environment: str, *, limit: int | None = None, cursor: str | None = None
    ) -> EnvironmentRevisionList:
        """Return one page of an environment's revisions, oldest first."""
        data = self._call(
            "listEnvironmentRevisions",
            {"environmentId": environment},
            params=self._page_params(limit, cursor),
        )
        return _decode(EnvironmentRevisionList, data)

    def iter_environment_revisions(
        self, environment: str, *, limit: int | None = None
    ) -> Iterator[EnvironmentRevision]:
        """Iterate over every revision of an environment, oldest first."""

        def fetch(cursor: str | None) -> tuple[list[EnvironmentRevision], str | None]:
            page = self.list_environment_revisions(environment, limit=limit, cursor=cursor)
            return page.items, page.next_cursor

        return self._follow(fetch)

    def create_environment_revision(
        self,
        environment: str,
        manifest: Manifest,
        *,
        if_match: int | str | None = None,
        idempotency_key: str | None = None,
    ) -> EnvironmentRevision:
        """Mint the next immutable revision from a complete manifest.

        Args:
            environment: Environment ID or name.
            manifest: The whole new manifest (as in :meth:`create_environment`).
            if_match: Expected ``latest_revision``; a stale value raises
                :class:`~davit.RevisionMismatch` (``details["latestRevision"]``).
            idempotency_key: A retry with the same key returns the revision it
                minted (generated once per call if omitted).
        """
        body = CreateEnvironmentRevisionRequest(manifest=_manifest(manifest))
        headers = {"Idempotency-Key": idempotency_key or _transport.new_idempotency_key()}
        if if_match is not None:
            headers["If-Match"] = str(if_match)
        data = self._call(
            "createEnvironmentRevision",
            {"environmentId": environment},
            body=body.to_dict(),
            headers=headers,
        )
        return _decode(EnvironmentRevision, data)

    def get_environment_revision(self, environment: str, revision: int) -> EnvironmentRevision:
        """Get one revision (an integer >= 1) of an environment."""
        if isinstance(revision, bool) or not isinstance(revision, int) or revision < 1:
            raise ValueError("revision must be an integer >= 1")
        data = self._call(
            "getEnvironmentRevision",
            {"environmentId": environment, "revision": str(revision)},
        )
        return _decode(EnvironmentRevision, data)

    # ---- CI runners ---------------------------------------------------------

    def get_ci_status(self, *, limit: int | None = None) -> CIStatus:
        """The host's GitHub Actions runner jobs (``getCIStatus``), owner only.

        ``queued`` is in FIFO order, ``running`` holds the starting, running
        and stopping jobs, and ``recent`` the done and refused ones, newest
        first. ``enabled`` is false (and the lists empty) when ``ci.github``
        is off.

        Args:
            limit: At most this many ``recent`` jobs (1..200; server default 20).

        Raises:
            PermissionDenied: The caller is not the owner identity.
        """
        return _decode(CIStatus, self._call("getCIStatus", params=self._page_params(limit, None)))

    # ---- events and webhooks ------------------------------------------------

    def list_events(
        self,
        *,
        limit: int | None = None,
        cursor: str | None = None,
        types: str | Sequence[str] | None = None,
        vm: str | None = None,
        start: EventStart | None = None,
        wait_ms: int = 0,
    ) -> EventList:
        """Return one page of the host event stream, in ``seq`` order.

        ``next_cursor`` is always set: it continues after the last item, or
        where the stream was when the page is empty. Event types the client
        does not know may appear; skip them.

        Args:
            limit: Page size.
            cursor: ``next_cursor`` of a previous page.
            types: Event types or prefixes ending in ``.`` (``"vm."``,
                ``"operation.state"``), as a list or a comma-separated string.
            vm: Only events of this VM (sent as ``vmId``).
            start: Without a cursor, ``"oldest"`` (server default) or
                ``"latest"`` (only events after this request).
            wait_ms: Long-poll up to this long (0..30000) for the first
                matching event.
        """
        params = self._page_params(limit, cursor)
        if types is not None:
            joined = types if isinstance(types, str) else ",".join(types)
            if joined:
                params.append(("types", joined))
        if vm:
            params.append(("vmId", vm))
        if start is not None:
            params.append(("start", start))
        if wait_ms:
            params.append(("waitMs", str(wait_ms)))
        data = self._call("listEvents", params=params, timeout=wait_ms / 1000 + self.timeout)
        return _decode(EventList, data)

    def iter_events(
        self,
        *,
        cursor: str | None = None,
        types: str | Sequence[str] | None = None,
        vm: str | None = None,
        start: EventStart | None = None,
        limit: int | None = None,
        follow: bool = False,
        wait_ms: int = MAX_POLL_MS,
    ) -> Iterator[Event]:
        """Iterate over events in ``seq`` order, following ``next_cursor``.

        Without ``follow`` the iteration ends at the first empty page (the
        current end of the stream). With ``follow=True`` it never ends: each
        empty page is followed by another long-poll of up to ``wait_ms``
        (default 25000) milliseconds. Stop by breaking out of the loop. Resume
        later from :meth:`list_events` pages if you need a durable cursor.

        Args:
            cursor: Start after this ``next_cursor`` (``start`` is then ignored).
            types: As in :meth:`list_events`.
            vm: As in :meth:`list_events`.
            start: As in :meth:`list_events`; ``"latest"`` with ``follow``
                tails new events only.
            limit: Page size.
            follow: Keep long-polling for new events forever.
            wait_ms: Long-poll budget of each request while following (0..30000).
        """
        if not 0 <= wait_ms <= 30_000:
            raise ValueError("wait_ms must be in 0..30000")
        while True:
            page = self.list_events(
                limit=limit,
                cursor=cursor,
                types=types,
                vm=vm,
                start=None if cursor else start,
                wait_ms=wait_ms if follow else 0,
            )
            yield from page.items
            if not page.next_cursor:
                raise Internal("listEvents returned a page without nextCursor")
            cursor = page.next_cursor
            if not page.items and not follow:
                return

    def list_webhooks(self, *, limit: int | None = None, cursor: str | None = None) -> WebhookList:
        """Return one page of webhooks (never their secrets). Owner only."""
        data = self._call("listWebhooks", params=self._page_params(limit, cursor))
        return _decode(WebhookList, data)

    def iter_webhooks(self, *, limit: int | None = None) -> Iterator[Webhook]:
        """Iterate over every webhook, following ``nextCursor``."""

        def fetch(cursor: str | None) -> tuple[list[Webhook], str | None]:
            page = self.list_webhooks(limit=limit, cursor=cursor)
            return page.items, page.next_cursor

        return self._follow(fetch)

    def create_webhook(
        self,
        url: str,
        *,
        events: Sequence[str] | None = None,
        description: str | None = None,
        enabled: bool | None = None,
        idempotency_key: str | None = None,
    ) -> WebhookSecret:
        """Create a webhook that receives signed event deliveries. Owner only.

        The returned :attr:`WebhookSecret.secret` is shown only once (and kept
        out of the model's ``repr``); give it to the receiver for
        :func:`davit.verify_webhook`. Always sends an ``Idempotency-Key``
        (``idempotency_key`` or generated once per call).

        Args:
            url: http or https URL that receives POSTed deliveries.
            events: Event types or prefixes ending in ``.``; omitted or empty
                means every event.
            description: Free text.
            enabled: Default true.
            idempotency_key: Replay key for this create.

        Raises:
            InvalidArgument: Bad URL or event filter.
            PermissionDenied: The caller is not the owner identity.
        """
        if isinstance(events, str):
            raise TypeError("events must be a list of event types")
        body = CreateWebhookRequest(
            url=url,
            events=None if events is None else list(events),
            description=description,
            enabled=enabled,
        )
        data = self._call(
            "createWebhook",
            body=body.to_dict(),
            headers={"Idempotency-Key": idempotency_key or _transport.new_idempotency_key()},
        )
        return _decode(WebhookSecret, data)

    def get_webhook(self, webhook: str | Webhook | WebhookSecret) -> Webhook:
        """Get a webhook by ID (or the object returned by another call)."""
        data = self._call("getWebhook", {"webhookId": _webhook_ref(webhook)})
        return _decode(Webhook, data)

    def update_webhook(
        self,
        webhook: str | Webhook | WebhookSecret,
        *,
        url: str | None = None,
        events: Sequence[str] | None = None,
        description: str | None = None,
        enabled: bool | None = None,
    ) -> Webhook:
        """Change a webhook's URL, event filter, description or enabled state.

        Only the given fields change (``events=[]`` subscribes to every
        event). Naturally idempotent: no ``Idempotency-Key`` is sent.
        """
        if isinstance(events, str):
            raise TypeError("events must be a list of event types")
        body = UpdateWebhookRequest(
            url=url,
            events=None if events is None else list(events),
            description=description,
            enabled=enabled,
        )
        data = self._call(
            "updateWebhook", {"webhookId": _webhook_ref(webhook)}, body=body.to_dict()
        )
        return _decode(Webhook, data)

    def delete_webhook(self, webhook: str | Webhook | WebhookSecret) -> None:
        """Delete a webhook and its pending deliveries.

        Raises:
            NotFound: No such webhook (for example, already deleted).
        """
        self._call("deleteWebhook", {"webhookId": _webhook_ref(webhook)})

    def rotate_webhook_secret(
        self,
        webhook: str | Webhook | WebhookSecret,
        *,
        overlap_seconds: int | None = None,
        idempotency_key: str | None = None,
    ) -> WebhookSecret:
        """Replace a webhook's signing secret and return the new one (shown once).

        Until ``webhook.previous_secret_expires_at`` deliveries are signed with
        both secrets, so receivers can switch over (verify with both). Always
        sends an ``Idempotency-Key`` (``idempotency_key`` or generated once per
        call), so a retried rotation does not rotate twice.

        Args:
            webhook: The webhook.
            overlap_seconds: How long the old secret keeps signing, 0..604800
                (server default 86400; 0 drops it at once).
            idempotency_key: Replay key for this rotation.
        """
        body = RotateWebhookSecretRequest(overlap_seconds=overlap_seconds)
        data = self._call(
            "rotateWebhookSecret",
            {"webhookId": _webhook_ref(webhook)},
            body=body.to_dict(),
            headers={"Idempotency-Key": idempotency_key or _transport.new_idempotency_key()},
        )
        return _decode(WebhookSecret, data)

    def list_webhook_deliveries(
        self,
        webhook: str | Webhook | WebhookSecret,
        *,
        state: DeliveryState | None = None,
        limit: int | None = None,
        cursor: str | None = None,
    ) -> WebhookDeliveryList:
        """Return one page of a webhook's deliveries, newest first.

        Args:
            webhook: The webhook.
            state: Only ``"pending"``, ``"delivered"`` or ``"dead"`` deliveries.
            limit: Page size.
            cursor: ``next_cursor`` of a previous page.
        """
        params = self._page_params(limit, cursor)
        if state is not None:
            params.append(("state", state))
        data = self._call(
            "listWebhookDeliveries", {"webhookId": _webhook_ref(webhook)}, params=params
        )
        return _decode(WebhookDeliveryList, data)

    def iter_webhook_deliveries(
        self,
        webhook: str | Webhook | WebhookSecret,
        *,
        state: DeliveryState | None = None,
        limit: int | None = None,
    ) -> Iterator[WebhookDelivery]:
        """Iterate over a webhook's deliveries, newest first, following ``nextCursor``."""

        def fetch(cursor: str | None) -> tuple[list[WebhookDelivery], str | None]:
            page = self.list_webhook_deliveries(webhook, state=state, limit=limit, cursor=cursor)
            return page.items, page.next_cursor

        return self._follow(fetch)

    def retry_webhook_delivery(
        self, webhook: str | Webhook | WebhookSecret, delivery: str | WebhookDelivery
    ) -> WebhookDelivery:
        """Deliver a dead (or delivered) delivery again, now; returns it pending again."""
        params = {"webhookId": _webhook_ref(webhook), "deliveryId": _ref(delivery, "delivery")}
        return _decode(WebhookDelivery, self._call("retryWebhookDelivery", params))

    # ---- files ------------------------------------------------------------

    def _file_call(
        self,
        operation_id: str,
        vm: str,
        path: str,
        user: str | None,
        generation: int | None,
        archive: bool,
    ) -> tuple[str, list[tuple[str, str]]]:
        check_path(path)
        if generation is None:
            current = self.get_vm(vm)  # like `davit cp`: resolve once, then use the ID
            vm, generation = current.id, current.instance_generation
        elif isinstance(generation, bool) or not isinstance(generation, int) or generation < 1:
            raise ValueError("generation must be the VM's instanceGeneration (an int >= 1)")
        params = [("path", path), ("generation", str(generation))]
        if user:
            params.append(("user", user))
        if archive:
            params.append(("archive", "tar"))
        _, route = self._route(operation_id, {"vmId": vm})
        return self.base_url + route, params

    def read_file_stream(
        self,
        vm: str,
        path: str,
        *,
        user: str | None = None,
        generation: int | None = None,
        archive: bool = False,
    ) -> FileStream:
        """Open a streamed read of a guest file (or, with ``archive``, a directory as tar).

        Nothing is buffered; consume the returned :class:`~davit.files.FileStream`
        (iterate, ``read()`` or ``copy_to(fileobj)``) inside a ``with`` block.
        A truncated transfer raises :class:`~davit.TransportError` at the end.

        Args:
            vm: VM ID or name.
            path: Absolute guest path.
            user: Guest user whose permissions scope the access (default: the image's).
            generation: Expected ``instance_generation``; a different boot raises
                :class:`~davit.InvalidState`. When omitted the VM is fetched first
                and its current generation (and ID) are used.
            archive: Read a directory as a tar stream of its contents.
        """
        url, params = self._file_call("readFile", vm, path, user, generation, archive)
        headers = {"X-Request-Id": _transport.new_request_id()}
        resp = _transport.open_stream(self._http, "GET", url, headers=headers, params=params)
        return FileStream(resp, path=path, archive=archive)

    def read_file(
        self,
        vm: str,
        path: str,
        *,
        user: str | None = None,
        generation: int | None = None,
        archive: bool = False,
    ) -> FileContent:
        """Read a whole guest file into memory; see :meth:`read_file_stream`."""
        with self.read_file_stream(
            vm, path, user=user, generation=generation, archive=archive
        ) as stream:
            return FileContent(stream.read(), stream.mode)

    def write_file(
        self,
        vm: str,
        path: str,
        data: WriteData,
        *,
        user: str | None = None,
        generation: int | None = None,
        mode: int | str | None = None,
        archive: bool = False,
    ) -> FileInfo:
        """Atomically replace a guest file, or extract a tar into a directory.

        Args:
            vm: VM ID or name.
            path: Absolute guest path (the target directory with ``archive``).
            data: ``bytes`` or ``str`` (UTF-8; retried if the daemon was not
                reached), or a binary file object / iterable of ``bytes``
                (streamed once, chunked, never retried).
            user: Guest user whose permissions scope the write.
            generation: Expected ``instance_generation`` (see :meth:`read_file_stream`).
            mode: Permission bits, ``0o644`` or ``"0644"``. Files: default keeps
                an existing file's mode, else 0644. Archives: mode of a newly
                created directory (default 0755).
            archive: ``data`` is a tar stream to extract into ``path``.

        A write is a full replace, so it is naturally idempotent and sends no
        ``Idempotency-Key``.
        """
        wire_mode = format_mode(mode)
        content, replayable = body_of(data)
        url, params = self._file_call("writeFile", vm, path, user, generation, archive)
        if wire_mode is not None:
            params.append(("mode", wire_mode))
        headers = {
            "X-Request-Id": _transport.new_request_id(),
            "Content-Type": "application/x-tar" if archive else "application/octet-stream",
        }
        resp = _transport.open_stream(
            self._http,
            "PUT",
            url,
            headers=headers,
            params=params,
            content=content,
            retry=replayable,
        )
        try:
            body = resp.read()
        except httpx.HTTPError as exc:
            raise TransportError(f"writing {path}: {exc}", retryable=True) from exc
        finally:
            resp.close()
        try:
            parsed = json.loads(body)
        except ValueError as exc:
            raise Internal("decoding response: body is not JSON") from exc
        return _decode(FileInfo, parsed)

    def upload_dir(
        self,
        vm: str,
        local_dir: str | os.PathLike[str],
        remote_dir: str,
        *,
        user: str | None = None,
        generation: int | None = None,
        mode: int | str | None = None,
    ) -> FileInfo:
        """Copy a local directory's contents into ``remote_dir`` (a tar upload).

        Regular files, directories and symlinks (not followed) keep their
        permission bits and mtimes. ``mode`` defaults to the local directory's.
        """
        root = os.fspath(local_dir)
        if not os.path.isdir(root):
            raise NotADirectoryError(root)
        if mode is None:
            mode = os.stat(root).st_mode & 0o7777
        with tempfile.SpooledTemporaryFile(max_size=8 << 20) as spool:
            with tarfile.open(fileobj=spool, mode="w", format=tarfile.PAX_FORMAT) as tar:
                for name in sorted(os.listdir(root)):
                    tar.add(os.path.join(root, name), arcname=name)
            spool.seek(0)
            return self.write_file(
                vm, remote_dir, spool, user=user, generation=generation, mode=mode, archive=True
            )

    def download_dir(
        self,
        vm: str,
        remote_dir: str,
        local_dir: str | os.PathLike[str],
        *,
        user: str | None = None,
        generation: int | None = None,
    ) -> None:
        """Copy a guest directory's contents into ``local_dir`` (created if missing).

        The tar is verified complete before anything is extracted, and
        extraction refuses absolute or escaping paths, links leaving the
        directory and device files.
        """
        root = os.fspath(local_dir)
        with tempfile.TemporaryFile() as spool:
            with self.read_file_stream(
                vm, remote_dir, user=user, generation=generation, archive=True
            ) as stream:
                stream.copy_to(spool)
                root_mode = stream.mode
            spool.seek(0)
            os.makedirs(root, exist_ok=True)
            with tarfile.open(fileobj=spool, mode="r:") as tar:
                _safe_extract(tar, root)
        if root_mode and root_mode.isdigit():
            os.chmod(root, int(root_mode, 8) & 0o777)

    # ---- prompts and conversations ----------------------------------------

    def create_prompt(
        self,
        vm: str,
        text: str,
        *,
        harness: str | None = None,
        conversation_id: str | None = None,
        new: bool = False,
        model: str | None = None,
        cwd: str | None = None,
        env: Mapping[str, str] | None = None,
        secrets: Sequence[str] | None = None,
        attachments: Sequence[PromptAttachment] | None = None,
        reasoning_effort: str | None = None,
        fast: bool | None = None,
        turn_timeout_seconds: int | None = None,
        idempotency_key: str | None = None,
    ) -> Prompt:
        """Start one turn of a harness conversation (Claude Code, Codex, OpenCode, pi) in the VM.

        Returns at once; follow it with :meth:`follow_prompt` or
        :meth:`wait_prompt`. A conversation runs one turn at a time: a turn
        while another is active raises :class:`~davit.OperationInProgress`.

        Args:
            vm: VM ID or name.
            text: The prompt.
            harness: ``"claude"`` (server default; alias ``"claude-code"``), ``"codex"``,
                ``"opencode"`` or ``"pi"``.
            conversation_id: Continue this conversation (its native session resumes).
            new: Start a new conversation.
            model: Harness model name.
            cwd: Guest working directory of a new conversation.
            env: Environment for the harness process; values enter the guest
                and are not stored.
            secrets: Names of daemon-configured secrets to pass as
                same-named environment variables.
            attachments: Files for the turn: ``PromptAttachment(name=,
                media_type=, path=)`` for a guest file or ``data=`` with inline
                base64 bytes (exactly one). Accepted types are in
                ``capabilities.harnesses[].attachment_types``.
            reasoning_effort: Harness reasoning effort
                (``capabilities.harnesses[].reasoning_efforts``), sent as ``options``.
            fast: Harness fast mode (``capabilities.harnesses[].fast``), sent
                as ``options``.
            turn_timeout_seconds: Bound the turn (server default 3600, at most
                86400); it then fails with ``turn_timeout``.
            idempotency_key: Same key and body returns the existing turn; a
                different body raises :class:`~davit.IdempotencyConflict`
                (generated once per call if omitted, reused on retry).
        """
        if not isinstance(text, str) or not text.strip():
            raise ValueError("prompt text must be a non-empty string")
        if isinstance(secrets, str):
            raise TypeError("secrets must be a list of secret names")
        options = None
        if reasoning_effort is not None or fast is not None:
            options = PromptOptions(reasoning_effort=reasoning_effort, fast=fast)
        body = PromptRequest(
            text=text,
            harness=harness,
            conversation_id=conversation_id,
            new=True if new else None,
            model=model,
            cwd=cwd,
            env=dict(env) if env is not None else None,
            secrets=list(secrets) if secrets is not None else None,
            attachments=list(attachments) if attachments is not None else None,
            options=options,
            turn_timeout_seconds=turn_timeout_seconds,
        )
        return _decode(
            Prompt,
            self._call(
                "createPrompt",
                {"vmId": vm},
                body=body.to_dict(),
                headers={"Idempotency-Key": idempotency_key or _transport.new_idempotency_key()},
            ),
        )

    def get_prompt(self, vm: str, prompt_id: str) -> Prompt:
        """Get one turn."""
        data = self._call("getPrompt", {"vmId": vm, "promptId": prompt_id})
        return _decode(Prompt, data)

    def cancel_prompt(self, vm: str, prompt_id: str) -> Prompt:
        """Cancel a running turn (SIGTERM to the harness); it ends ``cancelled``."""
        data = self._call("cancelPrompt", {"vmId": vm, "promptId": prompt_id})
        return _decode(Prompt, data)

    def steer_prompt(
        self, vm: str, prompt_id: str, text: str, *, idempotency_key: str | None = None
    ) -> SteerResult:
        """Send ``text`` to a running turn.

        ``delivery`` ``native``: the same turn took it (``prompt`` is that
        turn). ``restart``: the turn was cancelled and ``prompt`` is the new
        turn of the same conversation carrying the message. Sends an
        ``Idempotency-Key`` (``idempotency_key`` or generated once per call):
        the message is delivered once, and a repeat returns the first result.

        Raises:
            InvalidState: The turn has ended.
            OperationInProgress: The turn is still starting (retryable).
            UnsupportedCapability: The harness takes no steering.
        """
        if not isinstance(text, str) or not text.strip():
            raise ValueError("steer text must be a non-empty string")
        data = self._call(
            "steerPrompt",
            {"vmId": vm, "promptId": prompt_id},
            body=SteerRequest(text=text).to_dict(),
            headers={"Idempotency-Key": idempotency_key or _transport.new_idempotency_key()},
        )
        return _decode(SteerResult, data)

    def interrupt_prompts(
        self, vm: str, *, conversation_id: str | None = None, all: bool = False
    ) -> InterruptResult:
        """Cancel one conversation's active turn, or (``all=True``) every active turn on the VM.

        Pass exactly one of ``conversation_id`` and ``all``. Returns the
        cancelled turns; sends no Idempotency-Key, and a repeat finds nothing
        active and returns an empty list.
        """
        if bool(conversation_id) == all:
            raise ValueError("interrupt_prompts needs exactly one of conversation_id and all=True")
        body = InterruptRequest(conversation_id=conversation_id or None, all=True if all else None)
        data = self._call("interruptPrompts", {"vmId": vm}, body=body.to_dict())
        return _decode(InterruptResult, data)

    def list_vm_prompt_events(
        self,
        vm: str,
        *,
        after: str | None = None,
        conversation_id: str | None = None,
        wait_ms: int = 0,
        limit: int | None = None,
    ) -> PromptFeed:
        """Return the VM's prompt events across turns after the feed cursor ``after``.

        ``after`` is opaque (``next_cursor`` or an item's ``cursor``); ``None``
        replays from the oldest event. ``next_cursor`` is set on every page,
        also an empty one: pass it back to continue. With ``wait_ms > 0`` (max
        30000) and nothing newer, the server waits for an event.
        """
        params: list[tuple[str, str]] = []
        if after:
            params.append(("after", after))
        if conversation_id is not None:
            params.append(("conversationId", conversation_id))
        params.append(("waitMs", str(wait_ms)))
        if limit is not None:
            params.append(("limit", str(limit)))
        data = self._call(
            "listVMPromptEvents",
            {"vmId": vm},
            params=params,
            timeout=wait_ms / 1000 + self.timeout,
        )
        return _decode(PromptFeed, data)

    def follow_vm_prompt_events(
        self,
        vm: str,
        *,
        after: str | None = None,
        conversation_id: str | None = None,
        limit: int | None = None,
    ) -> Iterator[PromptFeedItem]:
        """Yield the VM's prompt feed from ``after`` on, long-polling, without end.

        Stop by breaking out of the loop; resume later with the last item's
        ``cursor`` as ``after``.
        """
        cursor = after
        while True:
            page = self.list_vm_prompt_events(
                vm, after=cursor, conversation_id=conversation_id, wait_ms=MAX_POLL_MS, limit=limit
            )
            for item in page.items:
                cursor = item.cursor
                yield item
            if page.next_cursor:
                cursor = page.next_cursor

    def list_prompt_events(
        self, vm: str, prompt_id: str, *, after: int = 0, wait_ms: int = 0
    ) -> PromptEventList:
        """Return the turn and its events with ``seq > after``.

        With ``wait_ms > 0`` (max 30000) and no newer events, the server waits
        until one arrives, the turn ends, or the time elapses.
        """
        data = self._call(
            "listPromptEvents",
            {"vmId": vm, "promptId": prompt_id},
            params=[("after", str(after)), ("waitMs", str(wait_ms))],
            timeout=wait_ms / 1000 + self.timeout,
        )
        return _decode(PromptEventList, data)

    def follow_prompt(
        self, vm: str, prompt_id: str, *, after: int = 0, timeout: float | None = None
    ) -> Iterator[PromptEvent]:
        """Yield a turn's events in order until the turn has finished.

        Long-polls :meth:`list_prompt_events`; the last event of a finished
        turn is ``turn.completed``. Stopping early, or a timeout, never
        cancels the turn.

        Raises:
            WaitTimeout: The budget (default ``self.wait_timeout``) ran out.
        """
        budget = self.wait_timeout if timeout is None else timeout
        deadline = time.monotonic() + budget
        cursor = after
        while True:
            remaining = deadline - time.monotonic()
            wait_ms = int(max(0.0, min(remaining * 1000, MAX_POLL_MS)))
            try:
                page = self.list_prompt_events(vm, prompt_id, after=cursor, wait_ms=wait_ms)
            except TransportError as exc:
                timed_out = isinstance(exc.__cause__, httpx.TimeoutException)
                if timed_out and time.monotonic() >= deadline:
                    raise self._prompt_timeout(prompt_id, budget, cursor) from exc
                raise
            for event in page.items:
                cursor = max(cursor, event.seq)
                yield event
            if page.prompt.state not in _PROMPT_ACTIVE and not page.items:
                return
            if time.monotonic() >= deadline:
                raise self._prompt_timeout(prompt_id, budget, cursor)

    def wait_prompt(
        self,
        vm: str,
        prompt_id: str,
        *,
        after: int = 0,
        timeout: float | None = None,
        on_event: PromptCallback | None = None,
    ) -> Prompt:
        """Wait for a turn to finish and return it.

        A failed or cancelled turn is returned, not raised: check ``state``
        and ``error`` (codes such as ``harness_missing``, ``harness_failed``).
        """
        for event in self.follow_prompt(vm, prompt_id, after=after, timeout=timeout):
            if on_event is not None:
                on_event(event)
        return self.get_prompt(vm, prompt_id)

    def run_prompt(
        self,
        vm: str,
        text: str,
        *,
        harness: str | None = None,
        conversation_id: str | None = None,
        new: bool = False,
        model: str | None = None,
        cwd: str | None = None,
        env: Mapping[str, str] | None = None,
        secrets: Sequence[str] | None = None,
        attachments: Sequence[PromptAttachment] | None = None,
        reasoning_effort: str | None = None,
        fast: bool | None = None,
        turn_timeout_seconds: int | None = None,
        idempotency_key: str | None = None,
        timeout: float | None = None,
        on_event: PromptCallback | None = None,
    ) -> PromptResult:
        """Run one turn to completion: :meth:`create_prompt` then :meth:`wait_prompt`.

        Returns the final prompt, every event, and ``text``: the concatenated
        assistant ``message.delta`` texts. A failed turn is returned, not raised.
        """
        events: list[PromptEvent] = []

        def record(event: PromptEvent) -> None:
            events.append(event)
            if on_event is not None:
                on_event(event)

        started = self.create_prompt(
            vm,
            text,
            harness=harness,
            conversation_id=conversation_id,
            new=new,
            model=model,
            cwd=cwd,
            env=env,
            secrets=secrets,
            attachments=attachments,
            reasoning_effort=reasoning_effort,
            fast=fast,
            turn_timeout_seconds=turn_timeout_seconds,
            idempotency_key=idempotency_key,
        )
        final = self.wait_prompt(vm, started.id, timeout=timeout, on_event=record)
        text = "".join(
            e.text or ""
            for e in events
            if e.type == "message.delta" and e.role in (None, "", "assistant")
        )
        return PromptResult(prompt=final, text=text, events=events)

    def list_conversations(
        self, vm: str, *, limit: int | None = None, cursor: str | None = None
    ) -> ConversationList:
        """Return one page of the VM's harness conversations, ordered by ``(createdAt, id)``."""
        data = self._call(
            "listConversations", {"vmId": vm}, params=self._page_params(limit, cursor)
        )
        return _decode(ConversationList, data)

    def iter_conversations(self, vm: str, *, limit: int | None = None) -> Iterator[Conversation]:
        """Iterate over every conversation of the VM, oldest first, following ``nextCursor``."""

        def fetch(cursor: str | None) -> tuple[list[Conversation], str | None]:
            page = self.list_conversations(vm, limit=limit, cursor=cursor)
            return page.items, page.next_cursor

        return self._follow(fetch)

    def get_conversation(self, vm: str, conversation_id: str) -> Conversation:
        """Get one conversation."""
        data = self._call("getConversation", {"vmId": vm, "conversationId": conversation_id})
        return _decode(Conversation, data)

    @staticmethod
    def _prompt_timeout(prompt_id: str, budget: float, cursor: int) -> WaitTimeout:
        return WaitTimeout(
            f"stopped waiting after {budget:g}s; prompt {prompt_id} continues "
            f"(resume with follow_prompt(..., after={cursor}))",
            details={"promptId": prompt_id, "cursor": cursor},
        )

    # ---- operations -------------------------------------------------------

    def get_operation(self, operation_id: str) -> Operation:
        """Get an operation's current state and terminal result."""
        return _decode(Operation, self._call("getOperation", {"operationId": operation_id}))

    def list_operation_events(
        self, operation_id: str, *, after: int = 0, wait_ms: int = 0
    ) -> OperationEventList:
        """Return the operation and its events with ``seq > after``.

        With ``wait_ms > 0`` (max 30000) and no new events, the server blocks
        until an event arrives, the operation becomes terminal, or the time
        elapses.
        """
        data = self._call(
            "listOperationEvents",
            {"operationId": operation_id},
            params=[("after", str(after)), ("waitMs", str(wait_ms))],
            timeout=wait_ms / 1000 + self.timeout,
        )
        return _decode(OperationEventList, data)

    def wait(
        self,
        operation_id: str,
        *,
        timeout: float | None = None,
        on_progress: ProgressCallback | None = None,
    ) -> Operation:
        """Follow an operation until it is terminal and return it.

        Long-polls the events stream; ``on_progress`` sees every event once, in
        order. A client timeout never cancels the server operation.

        Args:
            operation_id: The operation to follow.
            timeout: Wait budget in seconds (default ``self.wait_timeout``).
            on_progress: Called for each event.

        Raises:
            OperationFailed: The operation failed; ``.error`` is the typed error.
            WaitTimeout: The budget ran out; the operation continues.
        """
        budget = self.wait_timeout if timeout is None else timeout
        deadline = time.monotonic() + budget
        after = 0
        while True:
            remaining = deadline - time.monotonic()
            wait_ms = int(max(0.0, min(remaining * 1000, MAX_POLL_MS)))
            try:
                page = self.list_operation_events(operation_id, after=after, wait_ms=wait_ms)
            except TransportError as exc:
                timed_out = isinstance(exc.__cause__, httpx.TimeoutException)
                if timed_out and time.monotonic() >= deadline:
                    raise self._wait_timeout(operation_id, budget) from exc
                raise
            for event in page.items:
                if on_progress is not None:
                    on_progress(event)
                after = event.seq
            op = page.operation
            expired = time.monotonic() >= deadline
            if op.state in _TERMINAL and (not page.items or expired):
                break
            if expired:
                raise self._wait_timeout(operation_id, budget)
        if op.state == OperationState.FAILED:
            error: DavitError = (
                error_from_info(op.error, operation_id=op.id)
                if op.error is not None
                else Internal("operation failed", operation_id=op.id)
            )
            raise OperationFailed(op, error)
        return op

    @staticmethod
    def _wait_timeout(operation_id: str, budget: float) -> WaitTimeout:
        return WaitTimeout(
            f"stopped waiting after {budget:g}s; operation {operation_id} continues "
            f"(resume with Client.wait({operation_id!r}))",
            operation_id=operation_id,
        )
