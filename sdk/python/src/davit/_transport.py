"""Endpoint resolution and the low-level HTTP exchange (retries, error mapping)."""

from __future__ import annotations

import errno
import ipaddress
import json
import os
import secrets
import ssl
import sys
import time
import uuid
from collections.abc import Iterator, Mapping
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit

import httpx

from . import _websocket
from .errors import DavitError, Internal, TransportError, error_for

VERSION = "0.1.0"
PRODUCT = "davit"
"""Product name; package, socket and environment names derive from it."""
ENV_PREFIX = PRODUCT.upper() + "_"
API_URL_ENV = ENV_PREFIX + "API_URL"
TOKEN_ENV = ENV_PREFIX + "TOKEN"
CA_FILE_ENV = ENV_PREFIX + "CA_FILE"
BASE_PATH = "/api/v1"

MAX_ATTEMPTS = 3
_BACKOFF = 0.1  # seconds, doubled per attempt
_UNREACHED = {errno.ECONNREFUSED, errno.ENOENT}


def default_socket(
    env: Mapping[str, str] | None = None, platform: str | None = None, home: str | None = None
) -> str:
    """Return the daemon's default local socket path.

    Linux: ``$XDG_RUNTIME_DIR/davit/davit.sock`` when ``XDG_RUNTIME_DIR`` is
    an absolute path, else ``$XDG_DATA_HOME/davit/davit.sock`` or
    ``~/.local/share/davit/davit.sock``. macOS:
    ``~/Library/Application Support/davit/davit.sock``.
    """
    env = os.environ if env is None else env
    platform = sys.platform if platform is None else platform
    home = home if home is not None else str(Path.home())
    sock = PRODUCT + ".sock"
    if platform == "darwin":
        return os.path.join(home, "Library", "Application Support", PRODUCT, sock)
    runtime = env.get("XDG_RUNTIME_DIR", "")
    if os.path.isabs(runtime):
        return os.path.join(runtime, PRODUCT, sock)
    data = env.get("XDG_DATA_HOME", "")
    if os.path.isabs(data):
        return os.path.join(data, PRODUCT, sock)
    return os.path.join(home, ".local", "share", PRODUCT, sock)


def is_loopback(host: str) -> bool:
    """Report whether ``host`` names the local machine (``localhost`` or a loopback IP)."""
    host = host.strip("[]").lower()
    if host == "localhost" or host.endswith(".localhost"):
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


def resolve_target(
    socket: str | None, base_url: str | None, token: str | None
) -> tuple[str | None, str]:
    """Pick the endpoint: returns ``(socket path or None, API base URL)``.

    Explicit ``socket`` or ``base_url`` win; otherwise ``$DAVIT_API_URL``
    (``unix:///abs/path.sock`` or an http(s) URL), otherwise the default socket.
    """
    if socket and base_url:
        raise ValueError("pass either socket or base_url, not both")
    if socket is None and base_url is None:
        env_url = os.environ.get(API_URL_ENV, "")
        if env_url.startswith("unix:"):
            socket = urlsplit(env_url).path or env_url[len("unix:") :]
        elif env_url:
            base_url = env_url
        else:
            socket = default_socket()
    if socket is not None:
        if not os.path.isabs(socket):
            raise ValueError(f"socket must be an absolute path, got {socket!r}")
        return socket, "http://local" + BASE_PATH
    assert base_url is not None
    parts = urlsplit(base_url)
    if parts.scheme not in ("http", "https") or not parts.hostname:
        raise ValueError(f"base_url must be https://host (or http://loopback), got {base_url!r}")
    if parts.scheme == "http" and not is_loopback(parts.hostname):
        what = "a token" if token else "requests"
        raise ValueError(f"refusing to send {what} over plain http to non-loopback {base_url!r}")
    return None, base_url.rstrip("/") + BASE_PATH


def resolve_token(token: str | None, socket_path: str | None) -> str | None:
    """Pick the bearer token: an explicit ``token`` wins, else ``$DAVIT_TOKEN``.

    The environment token is used only for http(s) base URLs, never for the
    local unix socket (owner-only; it never gets a bearer). An explicit empty
    string means "no token" and also disables the environment fallback.
    """
    if token is not None:
        return token or None
    if socket_path is not None:
        return None
    return os.environ.get(TOKEN_ENV) or None


def tls_context(ca_file: str | None, socket_path: str | None) -> ssl.SSLContext | None:
    """Build the TLS context that trusts ``ca_file`` (or ``$DAVIT_CA_FILE``) as well.

    Returns ``None`` (library defaults) when no CA file is configured or the
    endpoint is the local socket; the environment variable is ignored for the
    socket, an explicit ``ca_file`` with a socket is an error. The context keeps
    the system roots and adds the PEM bundle; it is shared by HTTP requests and
    the WebSocket streams.

    Raises:
        ValueError: The CA file is missing, unreadable or holds no certificate.
    """
    what = "ca_file"
    if ca_file is None:
        if socket_path is not None:
            return None
        ca_file = os.environ.get(CA_FILE_ENV) or None
        what = CA_FILE_ENV
        if ca_file is None:
            return None
    elif socket_path is not None:
        raise ValueError("ca_file applies to https base URLs, not to the local socket")
    try:
        with open(ca_file, "rb") as f:
            pem = f.read()
    except OSError as exc:
        raise ValueError(f"{what} {ca_file!r}: cannot read: {exc.strerror or exc}") from exc
    if b"-----BEGIN CERTIFICATE-----" not in pem:
        raise ValueError(f"{what} {ca_file!r}: no PEM certificate found")
    context = ssl.create_default_context()
    try:
        context.load_verify_locations(cadata=pem.decode("ascii", errors="replace"))
    except (ssl.SSLError, ValueError) as exc:
        raise ValueError(f"{what} {ca_file!r}: invalid PEM bundle: {exc}") from exc
    return context


def new_request_id() -> str:
    """Return a fresh ``req_<hex>`` correlation ID."""
    return "req_" + secrets.token_hex(8)


def new_idempotency_key() -> str:
    """Return a fresh UUIDv4 idempotency key."""
    return str(uuid.uuid4())


def never_reached_server(exc: BaseException) -> bool:
    """Report whether a transport error certainly happened before the request was sent.

    Only connection refused and a missing socket file qualify; anything after
    the connection was established may have been applied.
    """
    if not isinstance(exc, httpx.ConnectError):
        return False
    seen: BaseException | None = exc
    while seen is not None:
        if isinstance(seen, OSError) and seen.errno in _UNREACHED:
            return True
        seen = seen.__cause__ or seen.__context__
    return False


def send(
    http: httpx.Client,
    method: str,
    url: str,
    *,
    headers: dict[str, str],
    params: list[tuple[str, str]] | None = None,
    body: Any = None,
    timeout: float | None = None,
) -> tuple[httpx.Response, Any]:
    """Send one logical request, retrying only if it never reached the server.

    Returns the response and its decoded JSON body (``None`` when empty).
    Raises the mapped :class:`DavitError` for non-2xx responses.
    """
    content = None
    if body is not None:
        content = json.dumps(body, separators=(",", ":")).encode()
        headers = {**headers, "Content-Type": "application/json"}
    for attempt in range(1, MAX_ATTEMPTS + 1):
        try:
            resp = http.request(
                method,
                url,
                params=tuple(params) if params else None,
                content=content,
                headers=headers,
                timeout=httpx.USE_CLIENT_DEFAULT if timeout is None else timeout,
            )
            break
        except httpx.TransportError as exc:
            if attempt < MAX_ATTEMPTS and never_reached_server(exc):
                time.sleep(_BACKOFF * 2 ** (attempt - 1))
                continue
            raise TransportError(
                f"cannot reach the daemon: {exc}",
                retryable=True,
                request_id=headers.get("X-Request-Id"),
            ) from exc
    request_id = resp.headers.get("X-Request-Id") or headers.get("X-Request-Id")
    data: Any = None
    if resp.content:
        try:
            data = resp.json()
        except ValueError:
            data = None
    if resp.is_success:
        if data is None and resp.content:
            raise Internal("decoding response: body is not JSON", request_id=request_id)
        return resp, data
    raise _http_error(resp.status_code, data, request_id)


def _http_error(status: int, data: Any, request_id: str | None) -> DavitError:
    err = data.get("error") if isinstance(data, dict) else None
    if not isinstance(err, dict) or not err.get("code"):
        return Internal(
            f"unexpected HTTP {status} from daemon", status=status, request_id=request_id
        )
    details = err.get("details")
    return error_for(
        str(err["code"]),
        str(err.get("message", "")),
        status=status,
        retryable=bool(err.get("retryable", False)),
        details=details if isinstance(details, dict) else None,
        request_id=err.get("requestId") or request_id,
        operation_id=err.get("operationId") or None,
    )


def open_websocket(
    *,
    socket_path: str | None,
    base_url: str,
    path: str,
    headers: Mapping[str, str],
    timeout: float | None,
    max_message: int,
    ssl_context: ssl.SSLContext | None = None,
) -> _websocket.WebSocket:
    """Open a WebSocket to ``path`` (relative to the API base) over the client's endpoint.

    Retries like :func:`send` (only when the server was certainly not reached);
    a non-101 answer raises the mapped API error.
    """
    parts = urlsplit(base_url)
    target: dict[str, Any]
    if socket_path is not None:
        target = {"unix_socket": socket_path}
    else:
        tls = parts.scheme == "https"
        target = {
            "host": parts.hostname or "",
            "port": parts.port or (443 if tls else 80),
            "tls": tls,
            "ssl_context": ssl_context,
        }
    request_id = headers.get("X-Request-Id")
    for attempt in range(1, MAX_ATTEMPTS + 1):
        try:
            return _websocket.connect(
                parts.path + path,
                headers=headers,
                timeout=timeout,
                max_message=max_message,
                **target,
            )
        except _websocket.HandshakeError as exc:
            if exc.status is None or exc.status == 101:
                raise TransportError(f"opening the stream: {exc}", request_id=request_id) from exc
            try:
                data: Any = json.loads(exc.body) if exc.body else None
            except ValueError:
                data = None
            rid = exc.headers.get("x-request-id") or request_id
            raise _http_error(exc.status, data, rid) from exc
        except OSError as exc:
            if attempt < MAX_ATTEMPTS and exc.errno in _UNREACHED:
                time.sleep(_BACKOFF * 2 ** (attempt - 1))
                continue
            raise TransportError(
                f"cannot reach the daemon: {exc}", retryable=True, request_id=request_id
            ) from exc
    raise AssertionError("unreachable")  # pragma: no cover


MAX_ERROR_BODY = 1 << 20


def open_stream(
    http: httpx.Client,
    method: str,
    url: str,
    *,
    headers: dict[str, str],
    params: list[tuple[str, str]] | None = None,
    content: bytes | Iterator[bytes] | None = None,
    retry: bool = True,
) -> httpx.Response:
    """Send a request whose response body is streamed; return the open 2xx response.

    The caller must close the response. ``retry`` must be false for bodies
    that cannot be replayed. Non-2xx answers raise the mapped API error.
    """
    for attempt in range(1, MAX_ATTEMPTS + 1):
        try:
            request = http.build_request(
                method,
                url,
                params=tuple(params) if params else None,
                content=content,
                headers=headers,
            )
            resp = http.send(request, stream=True)
            break
        except httpx.TransportError as exc:
            if retry and attempt < MAX_ATTEMPTS and never_reached_server(exc):
                time.sleep(_BACKOFF * 2 ** (attempt - 1))
                continue
            raise TransportError(
                f"cannot reach the daemon: {exc}",
                retryable=True,
                request_id=headers.get("X-Request-Id"),
            ) from exc
    if resp.is_success:
        return resp
    request_id = resp.headers.get("X-Request-Id") or headers.get("X-Request-Id")
    body = b""
    try:
        for chunk in resp.iter_bytes():
            body += chunk
            if len(body) > MAX_ERROR_BODY:
                break
    except httpx.HTTPError:
        pass
    finally:
        resp.close()
    try:
        data: Any = json.loads(body) if body else None
    except ValueError:
        data = None
    raise _http_error(resp.status_code, data, request_id)
