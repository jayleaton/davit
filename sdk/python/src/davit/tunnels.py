"""Tunnel streams: a guest loopback TCP connection as a byte stream over a WebSocket."""

from __future__ import annotations

import threading
from collections.abc import Iterator

from . import _websocket
from .errors import TransportError
from .models import Tunnel

__all__ = ["TUNNEL_CHUNK", "TunnelStream"]

TUNNEL_CHUNK = 32 << 10
"""Largest binary message the client sends (as the Go CLI and the server)."""

_EOF_CODES = (_websocket.CLOSE_NORMAL, _websocket.CLOSE_NO_STATUS)


class TunnelStream:
    """An attached tunnel (operation ``attachTunnel``): raw TCP bytes both ways.

    :meth:`recv` returns the next chunk from the guest port, or ``b""`` once
    the guest side ended (the server closed the WebSocket normally); any
    other end raises :class:`~davit.TransportError`. :meth:`send` writes
    bytes in chunks of at most 32 KiB, :meth:`close_write` sends EOF to the
    guest port (the guest may keep answering), :meth:`close` ends both
    directions. Iterating yields chunks until EOF. One thread may read while
    another writes.

    Use as a context manager, or call :meth:`close`.
    """

    def __init__(self, ws: _websocket.WebSocket, *, tunnel_id: str, tunnel: Tunnel | None) -> None:
        self._ws = ws
        self.tunnel_id = tunnel_id
        self.tunnel = tunnel
        """The tunnel resource, when it was opened by this client call."""
        self._wlock = threading.Lock()
        self._eof = False
        self._write_closed = False
        self._closed = False

    def __enter__(self) -> TunnelStream:
        return self

    def __exit__(self, *exc_info: object) -> None:
        self.close()

    def __iter__(self) -> Iterator[bytes]:
        while chunk := self.recv():
            yield chunk

    def recv(self) -> bytes:
        """Return the next bytes from the guest port, or ``b""`` at EOF.

        Raises:
            TransportError: The stream was closed locally, dropped, or ended
                abnormally.
        """
        while not self._eof:
            if self._closed:
                raise TransportError("the tunnel stream is closed")
            try:
                op, data = self._ws.recv()
            except _websocket.ConnectionClosed as exc:
                if self._closed:
                    raise TransportError("the tunnel stream is closed") from exc
                self._eof = True
                if exc.code in _EOF_CODES:
                    break
                raise TransportError(
                    f"tunnel stream ended: {exc}", details={"tunnelId": self.tunnel_id}
                ) from exc
            except (_websocket.WebSocketError, OSError) as exc:
                self._eof = True
                if self._closed:
                    raise TransportError("the tunnel stream is closed") from exc
                self._ws.abort()
                raise TransportError(
                    f"tunnel stream failed: {exc}", details={"tunnelId": self.tunnel_id}
                ) from exc
            if op == _websocket.BINARY and data:
                return data
        return b""

    def _send(self, data: bytes) -> None:
        try:
            self._ws.send_binary(data)
        except (_websocket.WebSocketError, OSError) as exc:
            raise TransportError(
                f"tunnel stream send failed: {exc}", details={"tunnelId": self.tunnel_id}
            ) from exc

    def send(self, data: bytes | bytearray | memoryview) -> None:
        """Write bytes to the guest port, in messages of at most 32 KiB."""
        raw = bytes(data)
        with self._wlock:
            if self._closed or self._write_closed:
                raise TransportError("the tunnel stream is closed for writing")
            for i in range(0, len(raw), TUNNEL_CHUNK):
                self._send(raw[i : i + TUNNEL_CHUNK])

    def close_write(self) -> None:
        """Half-close: the guest port reads EOF; :meth:`recv` keeps working. Idempotent."""
        with self._wlock:
            if self._closed or self._write_closed:
                return
            self._write_closed = True
            self._send(b"")  # an empty binary message is the half-close

    def close(self) -> None:
        """Close both directions and the WebSocket (idempotent)."""
        self._closed = True
        self._ws.close()
