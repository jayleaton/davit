"""Receiver-side verification of signed webhook deliveries."""

from __future__ import annotations

import hashlib
import hmac
import re
import time
from collections.abc import Sequence

from .errors import DavitError

__all__ = [
    "DEFAULT_WEBHOOK_TOLERANCE",
    "WEBHOOK_SIGNATURE_HEADER",
    "WebhookVerificationError",
    "verify_webhook",
]

WEBHOOK_SIGNATURE_HEADER = "X-Davit-Signature"
"""Header carrying ``t=<unix seconds>,v1=<hex>[,v1=<hex>...]`` on every delivery."""
DEFAULT_WEBHOOK_TOLERANCE = 300
"""Largest accepted distance, in seconds, between the signed timestamp and now."""

_HEX64 = re.compile(r"[0-9a-fA-F]{64}")
_DIGITS = re.compile(r"[0-9]{1,15}")


class WebhookVerificationError(DavitError):
    """A webhook delivery failed signature verification; reject it (for example with 400).

    Attributes:
        reason: ``"malformed"`` (missing or unparsable header), ``"timestamp"``
            (outside the tolerance, too old or too far in the future) or
            ``"signature"`` (no ``v1`` matches any of the secrets, or the body
            was changed).
    """

    default_code = "invalid_signature"

    def __init__(self, message: str, *, reason: str) -> None:
        super().__init__(message)
        self.reason = reason


def verify_webhook(
    body: bytes | bytearray | memoryview | str,
    signature: str | None,
    secrets: str | Sequence[str],
    *,
    tolerance: float = DEFAULT_WEBHOOK_TOLERANCE,
    now: float | None = None,
) -> None:
    """Check the ``X-Davit-Signature`` of a delivery; return ``None`` when valid.

    Each ``v1`` is ``hex(HMAC-SHA256(secret, f"{t}." + raw_body))``. During a
    secret rotation a delivery carries one ``v1`` per live secret; it verifies
    when any ``v1`` matches any of ``secrets`` (pass the old and the new secret
    while you switch over). Comparison is constant-time.

    Args:
        body: The raw request body exactly as received (not re-serialized JSON;
            a ``str`` is encoded as UTF-8).
        signature: The ``X-Davit-Signature`` header value (``None`` when
            absent, which fails as ``"malformed"``).
        secrets: The webhook's signing secret, or several of them.
        tolerance: Seconds the signed timestamp may differ from ``now`` in
            either direction (default 300).
        now: Current unix time in seconds (default: ``time.time()``).

    Raises:
        WebhookVerificationError: The delivery is not authentic or not fresh;
            ``reason`` says why.
        ValueError: No secret, an empty secret, or a negative tolerance.
    """
    keys = [secrets] if isinstance(secrets, str) else list(secrets)
    if not keys or any(not isinstance(k, str) or not k for k in keys):
        raise ValueError("secrets must be one or more non-empty strings")
    if tolerance < 0:
        raise ValueError("tolerance must be >= 0")
    raw = body.encode("utf-8") if isinstance(body, str) else bytes(body)

    timestamp, candidates = _parse(signature)
    current = time.time() if now is None else now
    if abs(current - timestamp) > tolerance:
        raise WebhookVerificationError(
            f"signature timestamp {timestamp} is outside the {tolerance:g}s tolerance",
            reason="timestamp",
        )
    signed = str(timestamp).encode("ascii") + b"." + raw
    matched = False
    for key in keys:
        expected = hmac.new(key.encode("utf-8"), signed, hashlib.sha256).digest()
        for candidate in candidates:
            # No early exit: every pair is compared.
            matched |= hmac.compare_digest(expected, candidate)
    if not matched:
        raise WebhookVerificationError("no v1 signature matches the secret", reason="signature")


def _parse(header: str | None) -> tuple[int, list[bytes]]:
    if not header:
        raise WebhookVerificationError("missing signature header", reason="malformed")
    timestamp: int | None = None
    candidates: list[bytes] = []
    for part in header.split(","):
        key, sep, value = part.strip().partition("=")
        if not sep:
            raise WebhookVerificationError("malformed signature header", reason="malformed")
        if key == "t":
            if timestamp is not None or not _DIGITS.fullmatch(value):
                raise WebhookVerificationError("malformed signature timestamp", reason="malformed")
            timestamp = int(value)
        elif key == "v1":
            if not _HEX64.fullmatch(value):
                raise WebhookVerificationError("malformed v1 signature", reason="malformed")
            candidates.append(bytes.fromhex(value))
        # Other schemes (a future v2) are ignored.
    if timestamp is None or not candidates:
        raise WebhookVerificationError(
            "signature header needs t= and at least one v1=", reason="malformed"
        )
    return timestamp, candidates
