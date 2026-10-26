"""Reverse forwards against a scripted local server (HTTP mocked, WebSockets on a unix socket)."""

from __future__ import annotations

import json
import socket
import struct
import threading
import time
from collections.abc import Callable, Iterator
from typing import Any

import httpx
import pytest

from davit import (
    Client,
    NotFound,
    ReverseTarget,
    ReverseTargetKind,
    TransportError,
)
from davit.reverse import CLOSED_REASON
from wsserver import Peer, Script, Server

NOW = "2026-10-22T04:22:34Z"
BASE = "/api/v1/vms/web/reverse-forwards"


def forward(kind: str = "session", address: str = "127.0.0.1:1", port: int = 5432) -> Any:
    return {
        "id": "rev_1",
        "vmId": "vm_1",
        "generation": 1,
        "guestPort": port,
        "target": {"kind": kind, "address": address},
        "state": "awaiting_session" if kind == "session" else "active",
        "active": 0,
        "createdAt": NOW,
        "sessionExpiresAt": NOW,
    }


@pytest.fixture
def serve() -> Iterator[Callable[[Script], Server]]:
    servers: list[Server] = []

    def start(script: Script) -> Server:
        server = Server(script)
        servers.append(server)
        return server

    yield start
    for server in servers:
        server.close()
        assert not server.errors, server.errors


class Api:
    """A mock HTTP API recording calls; createReverseForward answers ``forward``."""

    def __init__(self) -> None:
        self.calls: list[tuple[str, str, Any]] = []

    def __call__(self, request: httpx.Request) -> httpx.Response:
        body = json.loads(request.content) if request.content else None
        self.calls.append((request.method, request.url.path, body))
        if request.method == "POST":
            target = body["target"]
            return httpx.Response(
                201, json=forward(target["kind"], target.get("address", ""), body["guestPort"])
            )
        if request.method == "GET":
            return httpx.Response(200, json={"items": [forward()]})
        return httpx.Response(204)


def close_frame(peer: Peer, code: int) -> None:
    peer.frame(0x8, struct.pack("!H", code))


def local_server(handler: Callable[[socket.socket], None]) -> tuple[socket.socket, int]:
    """A one-connection local TCP service running ``handler`` on a thread."""
    listener = socket.create_server(("127.0.0.1", 0))

    def run() -> None:
        try:
            conn, _ = listener.accept()
        except OSError:
            return
        with conn:
            handler(conn)

    threading.Thread(target=run, daemon=True).start()
    return listener, listener.getsockname()[1]


def eventually(cond: Callable[[], bool], timeout: float = 5.0) -> None:
    deadline = time.monotonic() + timeout
    while not cond():
        assert time.monotonic() < deadline, "condition not met in time"
        time.sleep(0.02)


def recv_all(sock: socket.socket) -> bytes:
    data = b""
    while chunk := sock.recv(65536):
        data += chunk
    return data


def test_wrappers(serve: Callable[[Script], Server]) -> None:
    api = Api()
    client = Client(socket="/nonexistent.sock", transport=httpx.MockTransport(api))
    assert [f.id for f in client.list_reverse_forwards("web").items] == ["rev_1"]
    target = ReverseTarget(kind=ReverseTargetKind.SESSION, address="db:5432")
    fwd = client.create_reverse_forward("web", 5432, target)
    assert fwd.id == "rev_1" and fwd.target.kind == "session" and fwd.session_expires_at
    host = client.forward_host_service("web", 8080, "10.0.0.5:8080")
    assert host.target.kind == ReverseTargetKind.HOST
    client.revoke_reverse_forward("web", fwd)
    client.revoke_reverse_forward("web", "rev_2")
    assert api.calls == [
        ("GET", BASE, None),
        ("POST", BASE, {"guestPort": 5432, "target": {"kind": "session", "address": "db:5432"}}),
        ("POST", BASE, {"guestPort": 8080, "target": {"kind": "host", "address": "10.0.0.5:8080"}}),
        ("DELETE", f"{BASE}/rev_1", None),
        ("DELETE", f"{BASE}/rev_2", None),
    ]
    for bad in (0, 65536, True):
        with pytest.raises(ValueError, match="guest_port"):
            client.create_reverse_forward("web", bad, target)
    with pytest.raises(ValueError, match="local_port"):
        client.reverse_forward("web", 5432, "127.0.0.1", 0)


def test_bridge_half_close_both_ways_and_revoked_reason(
    serve: Callable[[Script], Server],
) -> None:
    to_local = bytes(range(256)) * 300  # 76800 bytes from the guest
    got: dict[str, Any] = {}
    bridged = threading.Event()

    def local(conn: socket.socket) -> None:
        data = recv_all(conn)  # until the guest's half-close
        got["local"] = data
        conn.sendall(data[::-1])  # answer after EOF: the half-close kept us writable
        conn.shutdown(socket.SHUT_WR)

    listener, port = local_server(local)

    def script(peer: Peer, _: int) -> None:
        peer.accept()
        if peer.path.endswith("/session"):
            peer.text({"type": "ping"})
            peer.frame(0x2, b"ignored")  # not an event
            peer.frame(0x1, b"not json")
            peer.text({"type": "connection", "connectionId": "conn_1"})
            assert bridged.wait(10)
            peer.text({"type": "revoked", "reason": "vm stopped"})
            close_frame(peer, 1000)
            assert peer.recv_frame()[0] == 0x8  # the echoed close
            return
        peer.frame(0x1, b"text is not connection bytes")
        for i in range(0, len(to_local), 30000):
            peer.frame(0x2, to_local[i : i + 30000])
        peer.frame(0x2, b"")  # the guest half-closes
        back = b""
        while True:
            op, chunk = peer.recv_frame()
            assert op == 0x2
            if not chunk:
                break  # the local half-close
            assert len(chunk) <= 32 << 10
            back += chunk
        got["back"] = back
        # Both directions ended: the client closes the connection itself.
        assert peer.recv_frame()[0] == 0x8
        bridged.set()

    server = serve(script)
    api = Api()
    client = Client(socket=server.path, transport=httpx.MockTransport(api))
    try:
        with client.reverse_forward("web", 5432, "127.0.0.1", port) as session:
            assert session.forward.target.address == f"127.0.0.1:{port}"
            assert session.wait(10) == "vm stopped"
            assert session.closed and session.error is None and session.active == 0
        assert session.reason == "vm stopped"  # close() after the end keeps the reason
    finally:
        listener.close()
    assert got["local"] == to_local and got["back"] == to_local[::-1]
    label = {"kind": "session", "address": f"127.0.0.1:{port}"}
    assert api.calls == [("POST", BASE, {"guestPort": 5432, "target": label})]
    paths = sorted(p.path for p in server.peers)
    assert paths == [f"{BASE}/rev_1/connections/conn_1", f"{BASE}/rev_1/session"]
    assert all(p.headers["x-request-id"].startswith("req_") for p in server.peers)


def test_unreachable_local_service_closes_the_connection(
    serve: Callable[[Script], Server],
) -> None:
    with socket.create_server(("127.0.0.1", 0)) as unused:
        dead = unused.getsockname()[1]
    conn_closed = threading.Event()
    session_close: list[int] = []

    def script(peer: Peer, _: int) -> None:
        peer.accept()
        if peer.path.endswith("/session"):
            peer.text({"type": "connection", "connectionId": "conn_1"})
            op, _payload = peer.recv_frame()  # the client closes the session
            session_close.append(op)
            return
        op, _payload = peer.recv_frame()
        assert op == 0x8  # closed without bytes
        conn_closed.set()

    server = serve(script)
    client = Client(socket=server.path, transport=httpx.MockTransport(Api()))
    session = client.reverse_forward("web", 5432, "127.0.0.1", dead)
    assert conn_closed.wait(10)
    assert not session.closed
    session.close()
    session.close()  # idempotent
    assert session.wait(1) == CLOSED_REASON and session.error is None
    eventually(lambda: bool(session_close))
    assert session_close == [0x8]


def test_close_cuts_live_connections_and_a_dropped_stream_is_an_error(
    serve: Callable[[Script], Server],
) -> None:
    local_eof = threading.Event()
    received = threading.Event()
    release = threading.Event()

    def local(conn: socket.socket) -> None:
        assert conn.recv(100) == b"hi"
        received.set()
        if recv_all(conn) == b"":
            local_eof.set()  # cut by close()

    listener, port = local_server(local)

    def script(peer: Peer, index: int) -> None:
        peer.accept()
        if peer.path.endswith("/session"):
            if index == 0:
                peer.text({"type": "connection", "connectionId": "conn_1"})
                peer.recv_frame()  # the client's close
            else:
                assert release.wait(10)
                peer.sock.shutdown(socket.SHUT_RDWR)  # dropped without a close frame
            return
        peer.frame(0x2, b"hi")
        try:
            while peer.recv_frame()[0] != 0x8:
                pass
        except EOFError:
            pass

    server = serve(script)
    client = Client(socket=server.path, transport=httpx.MockTransport(Api()))
    try:
        session = client.reverse_forward("web", 5432, "127.0.0.1", port)
        assert received.wait(10)
        eventually(lambda: session.active == 1)
        session.close()
        assert local_eof.wait(10)
        assert session.reason == CLOSED_REASON
    finally:
        listener.close()

    dropped = client.reverse_forward("web", 5433, "127.0.0.1", port)
    release.set()
    reason = dropped.wait(10)
    assert reason is not None and reason.startswith("reverse session stream ended")
    assert isinstance(dropped.error, TransportError)
    assert dropped.error.details == {"forwardId": "rev_1"}


def test_attach_error_is_typed_and_revokes_the_forward(
    serve: Callable[[Script], Server],
) -> None:
    def script(peer: Peer, _: int) -> None:
        peer.reject(404, {"error": {"code": "not_found", "message": "forward gone"}})

    server = serve(script)
    api = Api()
    client = Client(socket=server.path, transport=httpx.MockTransport(api))
    with pytest.raises(NotFound, match="forward gone"):
        client.reverse_forward("web", 5432, "127.0.0.1", 9)
    assert [(m, p) for m, p, _ in api.calls] == [("POST", BASE), ("DELETE", f"{BASE}/rev_1")]
