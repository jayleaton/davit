"""Tunnel streams against a scripted local WebSocket server."""

from __future__ import annotations

import json
import struct
from collections.abc import Iterator
from typing import Any

import httpx
import pytest

from davit import Client, NotFound, TransportError
from davit.tunnels import TUNNEL_CHUNK
from wsserver import Peer, Script, Server

NOW = "2026-10-22T04:22:34Z"
TUNNEL = {
    "id": "tun_1",
    "vmId": "vm_1",
    "generation": 1,
    "port": 8080,
    "createdAt": NOW,
    "expiresAt": NOW,
}


@pytest.fixture
def serve() -> Iterator[Any]:
    servers: list[Server] = []

    def start(script: Script) -> Server:
        server = Server(script)
        servers.append(server)
        return server

    yield start
    for server in servers:
        server.close()
        assert not server.errors, server.errors


def close_frame(peer: Peer, code: int) -> None:
    peer.frame(0x8, struct.pack("!H", code))


def test_relay_half_close_and_eof(serve: Any) -> None:
    got: dict[str, Any] = {}

    def script(peer: Peer, _: int) -> None:
        peer.accept()
        sizes, data = [], b""
        while True:
            op, chunk = peer.recv_frame()
            assert op == 0x2
            if not chunk:
                break  # the half-close
            sizes.append(len(chunk))
            data += chunk
        got["sizes"], got["data"] = sizes, data
        peer.frame(0x1, b"ignored")  # text messages are not tunnel bytes
        peer.frame(0x2, data[::-1][:1000])
        peer.frame(0x2, b"!")
        close_frame(peer, 1000)
        peer.recv_frame()  # the echoed close

    server = serve(script)
    payload = bytes(range(256)) * 300  # 76800 bytes: three messages
    with Client(socket=server.path).attach_tunnel("tun_1") as conn:
        conn.send(payload)
        conn.close_write()
        conn.close_write()  # idempotent
        with pytest.raises(TransportError, match="closed for writing"):
            conn.send(b"late")
        assert b"".join(conn) == payload[::-1][:1000] + b"!"
        assert conn.recv() == b""  # EOF stays EOF
        assert conn.tunnel is None and conn.tunnel_id == "tun_1"
    peer = server.peers[0]
    assert peer.path == "/api/v1/tunnels/tun_1/stream"
    assert peer.headers["x-request-id"].startswith("req_")
    assert got["data"] == payload
    assert max(got["sizes"]) <= TUNNEL_CHUNK and len(got["sizes"]) == 3


def test_abnormal_end_is_a_transport_error(serve: Any) -> None:
    def script(peer: Peer, index: int) -> None:
        peer.accept()
        if index == 0:
            close_frame(peer, 1001)  # the guest side failed
            peer.recv_frame()
        # index 1: drop the connection without a close frame

    server = serve(script)
    client = Client(socket=server.path)
    with client.attach_tunnel("tun_1") as conn, pytest.raises(TransportError, match="1001"):
        conn.recv()
    with client.attach_tunnel("tun_2") as conn, pytest.raises(TransportError) as info:
        conn.recv()
    assert info.value.details == {"tunnelId": "tun_2"}


def test_connect_tunnel_opens_then_attaches(serve: Any) -> None:
    calls: list[httpx.Request] = []

    def api(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        return httpx.Response(201, json=TUNNEL)

    def script(peer: Peer, _: int) -> None:
        peer.accept()
        assert peer.recv_frame() == (0x2, b"ping")
        peer.frame(0x2, b"pong")
        close_frame(peer, 1000)
        peer.recv_frame()

    server = serve(script)
    client = Client(socket=server.path, transport=httpx.MockTransport(api))
    with client.connect_tunnel("web", 8080) as conn:
        conn.send(b"ping")
        assert conn.recv() == b"pong"
        assert conn.recv() == b""
        assert conn.tunnel is not None and conn.tunnel.port == 8080
    (req,) = calls
    assert (req.method, req.url.path) == ("POST", "/api/v1/vms/web/tunnels")
    assert json.loads(req.content) == {"port": 8080}
    assert server.peers[0].path == "/api/v1/tunnels/tun_1/stream"
    with pytest.raises(ValueError, match="port"):
        client.open_tunnel("web", 0)


def test_attach_error_is_typed_and_releases_the_tunnel(serve: Any) -> None:
    calls: list[tuple[str, str]] = []

    def api(request: httpx.Request) -> httpx.Response:
        calls.append((request.method, request.url.path))
        if request.method == "POST":
            return httpx.Response(201, json=TUNNEL)
        return httpx.Response(204)

    def script(peer: Peer, _: int) -> None:
        peer.reject(404, {"error": {"code": "not_found", "message": "tunnel gone"}})

    server = serve(script)
    client = Client(socket=server.path, transport=httpx.MockTransport(api))
    with pytest.raises(NotFound, match="tunnel gone") as info:
        client.connect_tunnel("web", 8080)
    assert info.value.status == 404 and info.value.request_id == "req_ws"
    assert calls == [("POST", "/api/v1/vms/web/tunnels"), ("DELETE", "/api/v1/tunnels/tun_1")]
