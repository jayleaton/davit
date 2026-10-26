"""Exceptions raised by the davit client.

Server errors map to one subclass per stable ``Error.code``; match on the class
(or on :attr:`DavitError.code`), never on the message. Codes this client does
not know yet raise the base :class:`DavitError`.
"""

from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, ClassVar

if TYPE_CHECKING:
    from .models import ErrorInfo, Operation

__all__ = [
    "ERROR_CLASSES",
    "Conflict",
    "DavitError",
    "IdempotencyConflict",
    "Internal",
    "InvalidArgument",
    "InvalidState",
    "NotFound",
    "OperationFailed",
    "OperationInProgress",
    "PayloadTooLarge",
    "PermissionDenied",
    "ResourceExhausted",
    "RevisionMismatch",
    "ServerNotImplemented",
    "StopRequired",
    "Timeout",
    "TransportError",
    "Unauthenticated",
    "Unavailable",
    "UnsupportedCapability",
    "WaitTimeout",
    "error_for",
    "error_from_info",
]


class DavitError(Exception):
    """Base class of every error raised by this package.

    Attributes:
        status: HTTP status of the response, or ``None`` for client-side errors.
        code: Stable machine-readable error code (e.g. ``"not_found"``).
        message: Human-readable text; may change between versions.
        retryable: Whether the server says retrying may succeed.
        details: Structured extra information from the server.
        request_id: The ``X-Request-Id`` of the failed request, when known.
        operation_id: The operation this error is tied to, when known.
    """

    default_code: ClassVar[str] = "internal"

    def __init__(
        self,
        message: str,
        *,
        code: str | None = None,
        status: int | None = None,
        retryable: bool = False,
        details: Mapping[str, Any] | None = None,
        request_id: str | None = None,
        operation_id: str | None = None,
    ) -> None:
        super().__init__(message)
        self.message = message
        self.code = code or self.default_code
        self.status = status
        self.retryable = retryable
        self.details: dict[str, Any] = dict(details or {})
        self.request_id = request_id
        self.operation_id = operation_id

    def __str__(self) -> str:
        text = f"{self.code}: {self.message}"
        extra = [
            f"{label} {value}"
            for label, value in (("operation", self.operation_id), ("request", self.request_id))
            if value
        ]
        return f"{text} ({', '.join(extra)})" if extra else text

    def __repr__(self) -> str:
        return f"{type(self).__name__}(code={self.code!r}, message={self.message!r})"


class InvalidArgument(DavitError):
    """400 ``invalid_argument``: the request was malformed."""

    default_code = "invalid_argument"


class Unauthenticated(DavitError):
    """401 ``unauthenticated``: no valid identity."""

    default_code = "unauthenticated"


class PermissionDenied(DavitError):
    """403 ``permission_denied``: the identity lacks the required grant."""

    default_code = "permission_denied"


class NotFound(DavitError):
    """404 ``not_found``: absent or invisible to the caller."""

    default_code = "not_found"


class Conflict(DavitError):
    """Common parent of the 409 conflict errors."""

    default_code = "invalid_state"


class OperationInProgress(Conflict):
    """409 ``operation_in_progress``: another operation holds the VM (see ``operation_id``)."""

    default_code = "operation_in_progress"


class IdempotencyConflict(Conflict):
    """409 ``idempotency_conflict``: the key was reused with a different body."""

    default_code = "idempotency_conflict"


class InvalidState(Conflict):
    """409 ``invalid_state``: the resource is in the wrong state for this request."""

    default_code = "invalid_state"


class StopRequired(Conflict):
    """409 ``stop_required``: stop the VM first (e.g. to snapshot or fork it)."""

    default_code = "stop_required"


class RevisionMismatch(DavitError):
    """412 ``revision_mismatch``: ``If-Match`` did not equal the current ``stateRevision``."""

    default_code = "revision_mismatch"


class PayloadTooLarge(DavitError):
    """413 ``payload_too_large``: the request or object exceeds a size bound."""

    default_code = "payload_too_large"


class UnsupportedCapability(DavitError):
    """422 ``unsupported_capability``: unsupported shape, architecture or feature."""

    default_code = "unsupported_capability"


class ResourceExhausted(DavitError):
    """429 ``resource_exhausted``: quota or host allocation limit."""

    default_code = "resource_exhausted"


class Unavailable(DavitError):
    """503 ``unavailable``: the host or runtime is temporarily unavailable."""

    default_code = "unavailable"


class ServerNotImplemented(DavitError):
    """501 ``not_implemented``: specified by the API but not implemented by this server."""

    default_code = "not_implemented"


class Timeout(DavitError):
    """504 ``timeout``: the server gave up waiting on the host or guest."""

    default_code = "timeout"


class Internal(DavitError):
    """500 ``internal``: unexpected server error (message redacted)."""

    default_code = "internal"


ERROR_CLASSES: dict[str, type[DavitError]] = {
    cls.default_code: cls
    for cls in (
        InvalidArgument,
        Unauthenticated,
        PermissionDenied,
        NotFound,
        OperationInProgress,
        IdempotencyConflict,
        InvalidState,
        StopRequired,
        RevisionMismatch,
        PayloadTooLarge,
        UnsupportedCapability,
        ResourceExhausted,
        Unavailable,
        ServerNotImplemented,
        Timeout,
        Internal,
    )
}
"""Server ``Error.code`` → exception class."""


def error_for(code: str, message: str, **kwargs: Any) -> DavitError:
    """Build the exception for a server error code (base class for unknown codes)."""
    cls = ERROR_CLASSES.get(code, DavitError)
    return cls(message, code=code, **kwargs)


def error_from_info(info: ErrorInfo, **kwargs: Any) -> DavitError:
    """Build the exception for an ``ErrorInfo`` (``Operation.error`` or ``VM.lastError``)."""
    kwargs.setdefault("operation_id", info.operation_id)
    return error_for(
        str(info.code), info.message, retryable=info.retryable, details=info.details, **kwargs
    )


class TransportError(DavitError):
    """The daemon could not be reached, or the connection failed mid-request.

    The request may or may not have been applied when the failure happened
    after sending; replay a mutation with the same idempotency key to find out.
    """

    default_code = "unavailable"


class WaitTimeout(DavitError):
    """The client's wait budget ran out before the operation finished.

    The operation keeps running on the server; resume with
    ``Client.wait(err.operation_id)``.
    """

    default_code = "timeout"


class OperationFailed(DavitError):
    """An awaited operation finished in state ``failed``.

    Attributes:
        operation: The terminal operation.
        error: The typed error built from ``operation.error`` (same class
            mapping as HTTP errors).
    """

    def __init__(self, operation: Operation, error: DavitError) -> None:
        super().__init__(
            error.message,
            code=error.code,
            retryable=error.retryable,
            details=error.details,
            operation_id=operation.id,
        )
        self.operation = operation
        self.error = error
