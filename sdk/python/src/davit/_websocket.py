"""A small, bounded RFC 6455 WebSocket client (no extensions, no subprotocols).

Used for the process attach stream. It runs over a unix socket, plain TCP
(loopback only, enforced by the client) or TLS, verifies the handshake, masks
every client frame, answers pings, echoes close frames, reassembles fragmented
messages and bounds every message by ``max_message`` bytes.
"""

from __future__ import annotations

import base64
import contextlib
import hashlib
import os
import socket
import ssl
import struct
import threading
from collections.abc import Mapping

__all__ = [
    "BINARY",
    "TEXT",
    "ConnectionClosed",
    "HandshakeError",
    "MessageTooBig",
    "ProtocolError",
    "WebSocket",
    "WebSocketError",
    "accept_key",
    "connect",
]

TEXT = 0x1
BINARY = 0x2
_CONT, _CLOSE, _PING, _PONG = 0x0, 0x8, 0x9, 0xA
_OPCODES = {_CONT, TEXT, BINARY, _CLOSE, _PING, _PONG}
_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
_MAX_CONTROL = 125
_MAX_HEAD = 64 << 10
_MAX_ERROR_BODY = 64 << 10
DEFAULT_MAX_MESSAGE = 1 << 20

CLOSE_NORMAL = 1000
CLOSE_PROTOCOL_ERROR = 1002
CLOSE_NO_STATUS = 1005
CLOSE_ABNORMAL = 1006
CLOSE_TOO_BIG = 1009


class WebSocketError(Exception):
    """Base class of WebSocket failures."""


class HandshakeError(WebSocketError):
    """The server did not accept the upgrade; ``status`` and ``body`` hold its answer."""

    def __init__(
        self,
        message: str,
        status: int | None = None,
        headers: Mapping[str, str] | None = None,
        body: bytes = b"",
    ) -> None:
        super().__init__(message)
        self.status = status
        self.headers = dict(headers or {})
        self.body = body


class ProtocolError(WebSocketError):
    """The server violated the protocol; the connection was failed with 1002."""


class MessageTooBig(WebSocketError):
    """A message exceeded the size bound; the connection was failed with 1009."""


class ConnectionClosed(WebSocketError):
    """The connection ended: by a close frame (``code``) or by EOF (1006)."""

    def __init__(self, code: int, reason: str = "") -> None:
        super().__init__(f"websocket closed ({code}){': ' + reason if reason else ''}")
        self.code = code
        self.reason = reason


def accept_key(key: str) -> str:
    """Return the ``Sec-WebSocket-Accept`` value for a ``Sec-WebSocket-Key``."""
    digest = hashlib.sha1((key + _GUID).encode(), usedforsecurity=False).digest()
    return base64.b64encode(digest).decode()


def _mask(data: bytes, key: bytes) -> bytes:
    n = len(data)
    if n == 0:
        return b""
    pad = (key * (n // 4 + 1))[:n]
    return (int.from_bytes(data, "big") ^ int.from_bytes(pad, "big")).to_bytes(n, "big")


def _has_token(headers: Mapping[str, str], name: str, token: str) -> bool:
    return any(p.strip().lower() == token for p in headers.get(name, "").split(","))


def _valid_close_code(code: int) -> bool:
    return 1000 <= code <= 1003 or 1007 <= code <= 1014 or 3000 <= code <= 4999


class WebSocket:
    """An open client connection. One thread may read; sends are thread-safe."""

    def __init__(self, sock: socket.socket, buffered: bytes, max_message: int) -> None:
        self._sock = sock
        self._buf = bytearray(buffered)
        self.max_message = max_message
        self._wlock = threading.Lock()
        self._close_sent = False
        self._closed = False

    # ---- reading ----------------------------------------------------------

    def _read_exact(self, n: int) -> bytes:
        while len(self._buf) < n:
            chunk = self._sock.recv(max(65536, n - len(self._buf)))
            if not chunk:
                raise ConnectionClosed(CLOSE_ABNORMAL, "connection dropped")
            self._buf += chunk
        data = bytes(self._buf[:n])
        del self._buf[:n]
        return data

    def _fail(self, code: int, exc: WebSocketError) -> WebSocketError:
        self.close(code, str(exc)[:100])
        return exc

    def recv(self) -> tuple[int, bytes]:
        """Return the next data message as ``(TEXT|BINARY, payload)``.

        Raises:
            ConnectionClosed: The peer closed (its close frame is echoed) or dropped.
            ProtocolError: The peer violated RFC 6455 (connection failed, 1002).
            MessageTooBig: A message exceeded ``max_message`` (failed, 1009).
            OSError: The socket failed.
        """
        if self._closed:
            raise ConnectionClosed(CLOSE_ABNORMAL, "connection is closed")
        msg_op: int | None = None
        parts = bytearray()
        while True:
            b0, b1 = self._read_exact(2)
            fin, op = bool(b0 & 0x80), b0 & 0x0F
            if b0 & 0x70:
                raise self._fail(CLOSE_PROTOCOL_ERROR, ProtocolError("reserved bits set"))
            if op not in _OPCODES:
                raise self._fail(CLOSE_PROTOCOL_ERROR, ProtocolError(f"unknown opcode {op}"))
            if b1 & 0x80:
                raise self._fail(CLOSE_PROTOCOL_ERROR, ProtocolError("masked server frame"))
            length = b1 & 0x7F
            if length == 126:
                (length,) = struct.unpack("!H", self._read_exact(2))
            elif length == 127:
                (length,) = struct.unpack("!Q", self._read_exact(8))
                if length >> 63:
                    raise self._fail(CLOSE_PROTOCOL_ERROR, ProtocolError("length has MSB set"))
            if op >= _CLOSE:
                if not fin or length > _MAX_CONTROL:
                    err = ProtocolError("fragmented or oversize control frame")
                    raise self._fail(CLOSE_PROTOCOL_ERROR, err)
                payload = self._read_exact(length)
                if op == _PING:
                    self._send_frame(_PONG, payload)
                elif op == _CLOSE:
                    raise self._handle_close(payload)
                continue
            if op == _CONT:
                if msg_op is None:
                    err = ProtocolError("continuation without a message")
                    raise self._fail(CLOSE_PROTOCOL_ERROR, err)
            elif msg_op is not None:
                err = ProtocolError("new message inside a fragmented message")
                raise self._fail(CLOSE_PROTOCOL_ERROR, err)
            else:
                msg_op = op
            if len(parts) + length > self.max_message:
                err2 = MessageTooBig(f"message exceeds {self.max_message} bytes")
                raise self._fail(CLOSE_TOO_BIG, err2)
            parts += self._read_exact(length)
            if fin:
                return msg_op, bytes(parts)

    def _handle_close(self, payload: bytes) -> WebSocketError:
        if len(payload) == 1:
            return self._fail(CLOSE_PROTOCOL_ERROR, ProtocolError("close payload of 1 byte"))
        code, reason = CLOSE_NO_STATUS, ""
        if len(payload) >= 2:
            (code,) = struct.unpack("!H", payload[:2])
            if not _valid_close_code(code):
                err = ProtocolError(f"invalid close code {code}")
                return self._fail(CLOSE_PROTOCOL_ERROR, err)
            reason = payload[2:].decode("utf-8", "replace")
        self.close(code)  # echo the peer's code, then drop the socket
        return ConnectionClosed(code, reason)

    # ---- writing ----------------------------------------------------------

    def _send_frame(self, op: int, payload: bytes) -> None:
        n = len(payload)
        if n <= _MAX_CONTROL:
            head = struct.pack("!BB", 0x80 | op, 0x80 | n)
        elif n <= 0xFFFF:
            head = struct.pack("!BBH", 0x80 | op, 0x80 | 126, n)
        else:
            head = struct.pack("!BBQ", 0x80 | op, 0x80 | 127, n)
        key = os.urandom(4)
        frame = head + key + _mask(payload, key)
        with self._wlock:
            if self._close_sent:
                raise ConnectionClosed(CLOSE_NORMAL, "close already sent")
            if op == _CLOSE:
                self._close_sent = True
            self._sock.sendall(frame)

    def send_text(self, text: str) -> None:
        """Send one text message (a single frame)."""
        self._send_frame(TEXT, text.encode())

    def send_binary(self, data: bytes) -> None:
        """Send one binary message (a single frame)."""
        self._send_frame(BINARY, data)

    def close(self, code: int = CLOSE_NORMAL, reason: str = "") -> None:
        """Send a close frame (best effort, 1s bound) and close the socket. Idempotent."""
        if self._closed:
            return
        self._closed = True
        payload = b""
        if code not in (0, CLOSE_NO_STATUS):
            payload = struct.pack("!H", code) + reason.encode()[: _MAX_CONTROL - 2]
        try:
            self._sock.settimeout(1.0)
            self._send_frame(_CLOSE, payload)
        except (OSError, ConnectionClosed):
            pass
        self.abort()

    def abort(self) -> None:
        """Close the socket without a close frame.

        The socket is shut down first, so a thread blocked in :meth:`recv`
        wakes up (closing alone does not interrupt it).
        """
        self._closed = True
        with contextlib.suppress(OSError):
            self._sock.shutdown(socket.SHUT_RDWR)
        with contextlib.suppress(OSError):
            self._sock.close()


def _read_head(sock: socket.socket) -> tuple[bytes, bytes]:
    buf = b""
    while b"\r\n\r\n" not in buf:
        if len(buf) > _MAX_HEAD:
            raise HandshakeError("handshake response headers too large")
        chunk = sock.recv(4096)
        if not chunk:
            raise HandshakeError("connection closed during the handshake")
        buf += chunk
    head, _, rest = buf.partition(b"\r\n\r\n")
    return head, rest


def _read_body(sock: socket.socket, headers: Mapping[str, str], rest: bytes) -> bytes:
    """Read a bounded error body (Content-Length, chunked, or until EOF)."""
    buf = bytearray(rest)

    def more() -> bool:
        if len(buf) > 2 * _MAX_ERROR_BODY:
            return False
        try:
            chunk = sock.recv(65536)
        except OSError:
            return False
        buf.extend(chunk)
        return bool(chunk)

    if "chunked" in headers.get("transfer-encoding", "").lower():
        out = bytearray()
        while len(out) < _MAX_ERROR_BODY:
            while (end := buf.find(b"\r\n")) < 0:
                if not more():
                    return bytes(out)
            try:
                size = int(bytes(buf[:end]).split(b";")[0] or b"0", 16)
            except ValueError:
                return bytes(out)
            del buf[: end + 2]
            if size == 0:
                break
            while len(buf) < size and more():
                pass
            out += buf[:size]
            del buf[: size + 2]
        return bytes(out[:_MAX_ERROR_BODY])
    try:
        want = int(headers.get("content-length", "-1"))
    except ValueError:
        want = -1
    limit = _MAX_ERROR_BODY if want < 0 else min(want, _MAX_ERROR_BODY)
    while len(buf) < limit and more():
        pass
    return bytes(buf[:limit])


def connect(
    path: str,
    *,
    headers: Mapping[str, str] | None = None,
    unix_socket: str | None = None,
    host: str = "local",
    port: int = 0,
    tls: bool = False,
    ssl_context: ssl.SSLContext | None = None,
    timeout: float | None = 30.0,
    max_message: int = DEFAULT_MAX_MESSAGE,
) -> WebSocket:
    """Dial and complete the client handshake for ``GET path``.

    ``timeout`` bounds the dial and the handshake; afterwards reads block.

    Raises:
        HandshakeError: The server answered without a valid 101.
        OSError: The connection could not be established.
    """
    if not path.startswith("/"):
        raise ValueError(f"path {path!r} must start with /")
    sock: socket.socket
    if unix_socket is not None:
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.settimeout(timeout)
        try:
            sock.connect(unix_socket)
        except OSError:
            sock.close()
            raise
        host_header = "local"
    else:
        sock = socket.create_connection((host, port), timeout=timeout)
        if tls:
            context = ssl_context or ssl.create_default_context()
            try:
                sock = context.wrap_socket(sock, server_hostname=host)
            except OSError:
                sock.close()
                raise
        bracketed = f"[{host}]" if ":" in host else host
        default_port = 443 if tls else 80
        host_header = bracketed if port == default_port else f"{bracketed}:{port}"
    try:
        key = base64.b64encode(os.urandom(16)).decode()
        lines = [
            f"GET {path} HTTP/1.1",
            f"Host: {host_header}",
            "Upgrade: websocket",
            "Connection: Upgrade",
            f"Sec-WebSocket-Key: {key}",
            "Sec-WebSocket-Version: 13",
            *(f"{k}: {v}" for k, v in (headers or {}).items()),
        ]
        sock.sendall(("\r\n".join(lines) + "\r\n\r\n").encode())
        head, rest = _read_head(sock)
        status_line, *header_lines = head.decode("iso-8859-1").split("\r\n")
        parts = status_line.split(" ", 2)
        if len(parts) < 2 or not parts[0].startswith("HTTP/") or not parts[1].isdigit():
            raise HandshakeError(f"invalid status line {status_line!r}")
        status = int(parts[1])
        resp_headers: dict[str, str] = {}
        for line in header_lines:
            name, sep, value = line.partition(":")
            if sep:
                k = name.strip().lower()
                v = value.strip()
                resp_headers[k] = f"{resp_headers[k]}, {v}" if k in resp_headers else v
        if status != 101:
            body = _read_body(sock, resp_headers, rest)
            raise HandshakeError(f"server answered HTTP {status}", status, resp_headers, body)
        if not _has_token(resp_headers, "upgrade", "websocket"):
            raise HandshakeError("missing Upgrade: websocket", status, resp_headers)
        if not _has_token(resp_headers, "connection", "upgrade"):
            raise HandshakeError("missing Connection: upgrade", status, resp_headers)
        if resp_headers.get("sec-websocket-accept") != accept_key(key):
            raise HandshakeError("bad Sec-WebSocket-Accept", status, resp_headers)
        sock.settimeout(None)
        return WebSocket(sock, rest, max_message)
    except BaseException:
        sock.close()
        raise
