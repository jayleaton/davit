"""Reverse forward sessions: guest loopback connections bridged to a service the client reaches."""

from __future__ import annotations

import contextlib
import json
import socket
import threading
from collections.abc import Callable

from . import _websocket
from .errors import TransportError
from .models import ReverseForward, ReverseSessionEvent, ReverseSessionEventType
from .tunnels import TUNNEL_CHUNK

__all__ = ["CLOSED_REASON", "ReverseSession"]

CLOSED_REASON = "closed"
""":attr:`ReverseSession.reason` after the client closed the session itself."""

ConnectionDialer = Callable[[str], _websocket.WebSocket]
"""Opens ``attachReverseConnection`` for one announced connection ID."""
LocalDialer = Callable[[], socket.socket]
"""Opens the TCP connection to the local service."""


def _shutdown(sock: socket.socket, how: int) -> None:
    with contextlib.suppress(OSError):
        sock.shutdown(how)


class _Bridge:
    """One guest connection: an ``attachReverseConnection`` WebSocket spliced to a local socket."""

    def __init__(self, ws: _websocket.WebSocket, sock: socket.socket) -> None:
        self._ws = ws
        self._sock = sock
        self._lock = threading.Lock()
        self._closed = False
        self._local_eof = False
        self._remote_eof = False

    def run(self) -> None:
        """Splice both directions; returns once the connection ended."""
        writer = threading.Thread(target=self._local_to_ws, daemon=True)
        writer.start()
        self._ws_to_local()
        writer.join()

    def _ws_to_local(self) -> None:
        try:
            while True:
                op, data = self._ws.recv()
                if op != _websocket.BINARY:
                    continue
                if data:
                    self._sock.sendall(data)
                    continue
                _shutdown(self._sock, socket.SHUT_WR)  # the guest half-closed
                if self._half_closed(remote=True):
                    return
        except Exception:
            pass  # a close frame (or any failure) ends the connection
        finally:
            self.close()

    def _local_to_ws(self) -> None:
        try:
            while True:
                chunk = self._sock.recv(TUNNEL_CHUNK)
                if self._closed:
                    return
                self._ws.send_binary(chunk)  # an empty message is the half-close
                if not chunk:
                    if self._half_closed(remote=False):
                        self.close()
                    return
        except Exception:  # its own thread: any failure ends the connection
            self.close()

    def _half_closed(self, *, remote: bool) -> bool:
        """Record one direction's EOF; True once both directions ended."""
        with self._lock:
            if remote:
                self._remote_eof = True
            else:
                self._local_eof = True
            return self._remote_eof and self._local_eof

    def close(self) -> None:
        """End both directions (idempotent)."""
        with self._lock:
            if self._closed:
                return
            self._closed = True
        self._ws.close()
        _shutdown(self._sock, socket.SHUT_RDWR)  # wakes the writer thread
        self._sock.close()


class ReverseSession:
    """A session reverse forward held open by this client (``davit forward --reverse``).

    Every connection a guest process makes to ``127.0.0.1:<guest_port>`` in
    the VM is announced on the session stream; the session claims it
    (``attachReverseConnection``), dials the local service and splices the
    two with half-close in both directions. A local service that cannot be
    reached closes that one connection. Bridging runs on background threads.

    :meth:`close` (or leaving the ``with`` block) closes the session stream,
    which revokes the forward, and cuts every live connection. :meth:`wait`
    blocks until the session ended and returns :attr:`reason`.
    """

    def __init__(
        self,
        forward: ReverseForward,
        session: _websocket.WebSocket,
        *,
        dial_connection: ConnectionDialer,
        dial_local: LocalDialer,
    ) -> None:
        self.forward = forward
        """The forward as created (``id``, ``guest_port``, ``target``...)."""
        self.reason: str | None = None
        """Why the session ended: the server's ``revoked`` reason, :data:`CLOSED_REASON`
        after :meth:`close`, or a description of a dropped stream; ``None`` while it runs."""
        self.error: TransportError | None = None
        """Set when the session stream ended without a ``revoked`` message."""
        self._ws = session
        self._dial_connection = dial_connection
        self._dial_local = dial_local
        self._lock = threading.Lock()
        self._bridges: set[_Bridge] = set()
        self._closed = False
        self._done = threading.Event()
        self._reader = threading.Thread(
            target=self._read, name=f"davit-reverse-{forward.id}", daemon=True
        )
        self._reader.start()

    def __enter__(self) -> ReverseSession:
        return self

    def __exit__(self, *exc_info: object) -> None:
        self.close()

    @property
    def closed(self) -> bool:
        """True once the session ended (closed locally, revoked or dropped)."""
        return self._done.is_set()

    @property
    def active(self) -> int:
        """Guest connections this client is bridging right now."""
        with self._lock:
            return len(self._bridges)

    def wait(self, timeout: float | None = None) -> str | None:
        """Block until the session ended (or ``timeout`` seconds passed); return :attr:`reason`."""
        self._done.wait(timeout)
        return self.reason

    def close(self) -> None:
        """Close the session stream (revoking the forward) and every live connection.

        Idempotent; :attr:`reason` stays the server's if it ended first.
        """
        with self._lock:
            if self._closed:
                return
            self._closed = True
        self._ws.close()
        self._finish(CLOSED_REASON, None)
        self._reader.join(timeout=2.0)

    # ---- internals ----------------------------------------------------------

    def _read(self) -> None:
        revoked: str | None = None
        try:
            while True:
                op, data = self._ws.recv()
                if op != _websocket.TEXT:
                    continue
                try:
                    event = ReverseSessionEvent.from_dict(json.loads(data))
                except ValueError:
                    continue  # not a session event: ignore, as unknown types
                if event.type == ReverseSessionEventType.CONNECTION:
                    cid = event.connection_id
                    if not isinstance(cid, str) or not cid:
                        continue  # malformed: the server drops that guest connection
                    try:
                        threading.Thread(target=self._serve, args=(cid,), daemon=True).start()
                    except RuntimeError:
                        continue  # no thread to spare: this connection goes unclaimed
                elif event.type == ReverseSessionEventType.REVOKED:
                    revoked = event.reason or "revoked"
        except _websocket.ConnectionClosed as exc:
            if revoked is not None or self._closed:
                self._finish(revoked or CLOSED_REASON, None)
            elif exc.code in (_websocket.CLOSE_NORMAL, _websocket.CLOSE_NO_STATUS):
                self._finish("session closed by the server", None)
            else:
                self._dropped(exc)
        except (_websocket.WebSocketError, OSError) as exc:
            self._ws.abort()
            if revoked is not None or self._closed:
                self._finish(revoked or CLOSED_REASON, None)
            else:
                self._dropped(exc)
        except Exception as exc:  # anything else: the session ends, never hangs
            self._ws.abort()
            self._dropped(exc)

    def _dropped(self, exc: BaseException) -> None:
        reason = f"reverse session stream ended: {exc}"
        err = TransportError(reason, details={"forwardId": self.forward.id})
        self._finish(reason, err)

    def _finish(self, reason: str, error: TransportError | None) -> None:
        with self._lock:
            first = not self._done.is_set()
            if first:
                self.reason, self.error = reason, error
                self._closed = True
                self._done.set()
            bridges = list(self._bridges)
            self._bridges.clear()
        if first:
            for bridge in bridges:  # the revoke cut them on the server side too
                bridge.close()

    def _serve(self, connection_id: str) -> None:
        # A background thread: every failure ends this one connection.
        try:
            ws = self._dial_connection(connection_id)
        except Exception:
            return  # unclaimed: the server drops the guest connection
        try:
            sock = self._dial_local()
        except Exception:
            ws.close()  # the local service is unreachable: end this connection
            return
        bridge = _Bridge(ws, sock)
        with self._lock:
            if self._done.is_set():
                bridge.close()
                return
            self._bridges.add(bridge)
        try:
            bridge.run()
        finally:
            with self._lock:
                self._bridges.discard(bridge)
