"""A scripted RFC 6455 server on a unix socket, for client tests."""

from __future__ import annotations

import base64
import json
import os
import shutil
import socket
import struct
import tempfile
import threading
from collections.abc import Callable
from typing import Any

from davit._websocket import accept_key


class Peer:
    """The server side of one accepted connection."""

    def __init__(self, sock: socket.socket) -> None:
        self.sock = sock
        self.buf = b""
        self.path = ""
        self.headers: dict[str, str] = {}

    def read_exact(self, n: int) -> bytes:
        while len(self.buf) < n:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise EOFError
            self.buf += chunk
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def read_request(self) -> None:
        while b"\r\n\r\n" not in self.buf:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise EOFError
            self.buf += chunk
        head, _, self.buf = self.buf.partition(b"\r\n\r\n")
        line, *rest = head.decode().split("\r\n")
        self.path = line.split(" ")[1]
        for h in rest:
            k, _, v = h.partition(":")
            self.headers[k.strip().lower()] = v.strip()

    def accept(self, accept: str | None = None) -> None:
        key = accept or accept_key(self.headers["sec-websocket-key"])
        self.sock.sendall(
            (
                "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"
                f"Connection: Upgrade\r\nSec-WebSocket-Accept: {key}\r\n\r\n"
            ).encode()
        )

    def reject(self, status: int, body: dict[str, Any]) -> None:
        data = json.dumps(body).encode()
        self.sock.sendall(
            f"HTTP/1.1 {status} Nope\r\nContent-Type: application/json\r\n"
            f"X-Request-Id: req_ws\r\nContent-Length: {len(data)}\r\n\r\n".encode()
            + data
        )

    def frame(self, op: int, payload: bytes, *, fin: bool = True, b0_extra: int = 0) -> None:
        n = len(payload)
        b0 = (0x80 if fin else 0) | op | b0_extra
        if n <= 125:
            head = struct.pack("!BB", b0, n)
        elif n <= 0xFFFF:
            head = struct.pack("!BBH", b0, 126, n)
        else:
            head = struct.pack("!BBQ", b0, 127, n)
        self.sock.sendall(head + payload)

    def text(self, obj: Any) -> None:
        self.frame(0x1, json.dumps(obj).encode())

    def recv_frame(self) -> tuple[int, bytes]:
        """Read one client frame; asserts it is masked."""
        b0, b1 = self.read_exact(2)
        assert b1 & 0x80, "client frames must be masked"
        n = b1 & 0x7F
        if n == 126:
            (n,) = struct.unpack("!H", self.read_exact(2))
        elif n == 127:
            (n,) = struct.unpack("!Q", self.read_exact(8))
        key = self.read_exact(4)
        data = bytes(b ^ key[i % 4] for i, b in enumerate(self.read_exact(n)))
        return b0 & 0x0F, data

    def recv_json(self) -> Any:
        op, data = self.recv_frame()
        assert op == 0x1
        return json.loads(data)


Script = Callable[[Peer, int], None]


class Server:
    """Accepts connections on a unix socket and runs ``script(peer, index)`` for each."""

    def __init__(self, script: Script) -> None:
        root = tempfile.gettempdir()
        self.dir = tempfile.mkdtemp(prefix="swpy-ws-", dir=root if len(root) <= 70 else "/tmp")
        self.path = os.path.join(self.dir, "ws.sock")
        self.script = script
        self.peers: list[Peer] = []
        self.errors: list[BaseException] = []
        self.listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.listener.bind(self.path)
        self.listener.listen(8)
        self.thread = threading.Thread(target=self._serve, daemon=True)
        self.thread.start()

    def _serve(self) -> None:
        index = 0
        while True:
            try:
                conn, _ = self.listener.accept()
            except OSError:
                return
            peer = Peer(conn)
            self.peers.append(peer)
            threading.Thread(target=self._run, args=(peer, index), daemon=True).start()
            index += 1

    def _run(self, peer: Peer, index: int) -> None:
        try:
            peer.read_request()
            self.script(peer, index)
        except EOFError:
            pass
        except BaseException as exc:  # surfaced by the test via .errors
            self.errors.append(exc)
        finally:
            peer.sock.close()

    def close(self) -> None:
        self.listener.close()
        shutil.rmtree(self.dir, ignore_errors=True)


def b64(data: bytes) -> str:
    return base64.b64encode(data).decode()
