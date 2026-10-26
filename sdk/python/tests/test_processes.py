"""WebSocket client and process stream behaviour against a scripted local server."""

from __future__ import annotations

import base64
import json
import struct
import threading
from collections.abc import Iterator
from typing import Any

import httpx
import pytest

from davit import Client, InvalidState, NotFound, TransportError, _websocket
from davit.models import Process, RunState
from davit.processes import ProcessStream, StreamEvent, exit_status
from wsserver import Peer, Script, Server, b64

NOW = "2026-10-22T04:22:34Z"


def proc_json(**over: Any) -> dict[str, Any]:
    p = {
        "id": "proc_1",
        "vmId": "vm_1",
        "generation": 1,
        "status": "exited",
        "argv": ["sh"],
        "detached": False,
        "exitCode": 3,
        "outputCursor": 2,
        "createdAt": NOW,
    }
    return {**p, **over}


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


def attach(server: Server, **kwargs: Any) -> ProcessStream:
    client = Client(socket=server.path)
    return client.attach_process("web", "proc_1", **kwargs)


def raw(server: Server) -> _websocket.WebSocket:
    return _websocket.connect("/x", unix_socket=server.path, max_message=1024)


# ---- RFC 6455 client ---------------------------------------------------------


def test_handshake_masking_and_headers(serve: Any) -> None:
    def script(peer: Peer, _: int) -> None:
        peer.accept()
        op, data = peer.recv_frame()  # masked, asserted by Peer
        peer.frame(op, data)  # echo

    server = serve(script)
    with attach(server, output_cursor=7) as stream:
        stream._ws.send_text("hello")
        assert stream._ws.recv() == (_websocket.TEXT, b"hello")
    peer = server.peers[0]
    assert peer.path == "/api/v1/vms/web/processes/proc_1/stream?outputCursor=7"
    assert peer.headers["upgrade"] == "websocket"
    assert peer.headers["sec-websocket-version"] == "13"
    assert peer.headers["x-request-id"].startswith("req_")
    assert peer.headers["host"] == "local"


def test_bad_accept_is_rejected(serve: Any) -> None:
    server = serve(lambda peer, _: peer.accept(accept="d3Jvbmc="))
    with pytest.raises(TransportError, match="Sec-WebSocket-Accept"):
        attach(server)


def test_non_101_maps_to_api_error(serve: Any) -> None:
    body = {"error": {"code": "not_found", "message": "no proc", "retryable": False}}
    server = serve(lambda peer, _: peer.reject(404, body))
    with pytest.raises(NotFound, match="no proc") as info:
        attach(server)
    assert info.value.status == 404
    assert info.value.request_id == "req_ws"


def test_ping_fragmentation_and_long_frames(serve: Any) -> None:
    got: dict[str, Any] = {}

    def script(peer: Peer, _: int) -> None:
        peer.accept()
        peer.frame(0x1, b"he", fin=False)
        peer.frame(0x9, b"ping!")  # control frame between fragments
        peer.frame(0x0, b"ll", fin=False)
        peer.frame(0x0, b"o", fin=True)
        got["pong"] = peer.recv_frame()
        peer.frame(0x2, b"x" * 70_000)  # 64-bit length

    server = serve(script)
    ws = _websocket.connect("/x", unix_socket=server.path, max_message=1 << 20)
    assert ws.recv() == (_websocket.TEXT, b"hello")
    assert ws.recv() == (_websocket.BINARY, b"x" * 70_000)
    ws.close()
    assert got["pong"] == (0xA, b"ping!")


@pytest.mark.parametrize(
    ("send", "match"),
    [
        (lambda p: p.frame(0x1, b"x", b0_extra=0x40), "reserved bits"),
        (lambda p: p.frame(0x3, b"x"), "unknown opcode"),
        (lambda p: p.sock.sendall(b"\x81\x81abcd\x00"), "masked server frame"),
        (lambda p: p.frame(0x9, b"x", fin=False), "control frame"),
        (lambda p: p.frame(0x0, b"x"), "continuation without"),
        (lambda p: (p.frame(0x1, b"a", fin=False), p.frame(0x1, b"b")), "inside a fragmented"),
        (lambda p: p.frame(0x8, b"\x03"), "1 byte"),
        (lambda p: p.frame(0x8, struct.pack("!H", 1005)), "invalid close code"),
    ],
)
def test_protocol_errors_fail_with_1002(serve: Any, send: Any, match: str) -> None:
    got: dict[str, Any] = {}

    def script(peer: Peer, _: int) -> None:
        peer.accept()
        send(peer)
        got["close"] = peer.recv_frame()

    server = serve(script)
    ws = raw(server)
    with pytest.raises(_websocket.ProtocolError, match=match):
        ws.recv()
    for _ in range(100):
        if "close" in got:
            break
        threading.Event().wait(0.01)
    op, payload = got["close"]
    assert op == 0x8
    assert struct.unpack("!H", payload[:2])[0] == 1002


def test_oversize_message_fails_with_1009(serve: Any) -> None:
    got: dict[str, Any] = {}

    def script(peer: Peer, _: int) -> None:
        peer.accept()
        peer.frame(0x1, b"a" * 600, fin=False)
        peer.frame(0x0, b"b" * 600)
        got["close"] = peer.recv_frame()

    server = serve(script)
    with pytest.raises(_websocket.MessageTooBig):
        raw(server).recv()
    server.thread.join(0.2)
    for _ in range(100):
        if "close" in got:
            break
        threading.Event().wait(0.01)
    assert struct.unpack("!H", got["close"][1][:2])[0] == 1009


def test_server_close_is_echoed(serve: Any) -> None:
    got: dict[str, Any] = {}

    def script(peer: Peer, _: int) -> None:
        peer.accept()
        peer.frame(0x8, struct.pack("!H", 4000) + b"bye")
        got["echo"] = peer.recv_frame()

    server = serve(script)
    with pytest.raises(_websocket.ConnectionClosed) as info:
        raw(server).recv()
    assert (info.value.code, info.value.reason) == (4000, "bye")
    for _ in range(100):
        if "echo" in got:
            break
        threading.Event().wait(0.01)
    assert got["echo"] == (0x8, struct.pack("!H", 4000))


def test_missing_socket_is_transport_error(tmp_path: Any) -> None:
    client = Client(socket=str(tmp_path / "none.sock"))
    with pytest.raises(TransportError, match="cannot reach"):
        client.attach_process("web", "proc_1")


# ---- process streams ---------------------------------------------------------


def test_stream_decodes_dedupes_and_ends_at_exit(serve: Any) -> None:
    def script(peer: Peer, _: int) -> None:
        peer.accept()
        peer.text({"type": "stdout", "seq": 1, "data": b64(b"hi\n")})
        peer.text({"type": "stdout", "seq": 1, "data": b64(b"hi\n")})  # duplicate
        peer.text({"type": "brand.new", "seq": 9})  # unknown: ignored
        peer.frame(0x2, b"binary is ignored")
        peer.text({"type": "stderr", "seq": 2, "data": b64(b"err\n")})
        peer.text({"type": "exit", "seq": 2, "process": proc_json()})

    stream = attach(serve(script))
    events = list(stream)
    assert [(e.type, e.seq, e.data) for e in events] == [
        ("stdout", 1, b"hi\n"),
        ("stderr", 2, b"err\n"),
        ("exit", 2, b""),
    ]
    assert stream.process is not None
    assert stream.process.exit_code == 3
    assert events[-1].process == stream.process
    assert stream.cursor == 2
    with pytest.raises(TransportError):
        stream.send_stdin(b"late")


def test_stream_error_message_raises_typed_error(serve: Any) -> None:
    def script(peer: Peer, _: int) -> None:
        peer.accept()
        peer.text({"type": "stdout", "seq": 1, "data": b64(b"x")})
        peer.text({"type": "error", "error": {"code": "invalid_state", "message": "nope"}})

    stream = attach(serve(script))
    assert next(stream).data == b"x"
    with pytest.raises(InvalidState, match="nope") as info:
        next(stream)
    assert info.value.details["processId"] == "proc_1"
    assert list(stream) == []


def test_stream_reattaches_from_cursor_and_dedupes(serve: Any) -> None:
    def script(peer: Peer, index: int) -> None:
        peer.accept()
        if index == 0:
            peer.text({"type": "stdout", "seq": 1, "data": b64(b"a")})
            peer.text({"type": "stdout", "seq": 2, "data": b64(b"b")})
            return  # drop without exit
        # a replay that overlaps what was delivered
        peer.text({"type": "stdout", "seq": 2, "data": b64(b"b")})
        peer.text({"type": "stdout", "seq": 3, "data": b64(b"c")})
        peer.text({"type": "exit", "seq": 3, "process": proc_json(outputCursor=3)})

    server = serve(script)
    data = b"".join(e.data for e in attach(server))
    assert data == b"abc"
    assert [p.path.rsplit("=", 1)[1] for p in server.peers] == ["0", "2"]


def test_stream_gives_up_after_three_reattaches(serve: Any) -> None:
    def script(peer: Peer, index: int) -> None:
        if index == 0:
            peer.accept()
            peer.text({"type": "stdout", "seq": 5, "data": b64(b"a")})
            return
        peer.sock.close()  # handshake fails

    server = serve(script)
    stream = attach(server)
    assert next(stream).seq == 5
    with pytest.raises(TransportError, match="lost the process stream") as info:
        next(stream)
    assert info.value.details == {"processId": "proc_1", "cursor": 5}
    assert len(server.peers) == 4


def test_stream_input_messages(serve: Any) -> None:
    got: list[Any] = []

    def script(peer: Peer, _: int) -> None:
        peer.accept()
        for _ in range(4):
            got.append(peer.recv_json())
        peer.text({"type": "exit", "seq": 0, "process": proc_json(exitCode=0)})

    stream = attach(serve(script))
    big = b"z" * (256 << 10) + b"tail"
    stream.send_stdin(big)
    stream.close_stdin()
    stream.signal("INT")
    assert [e.type for e in stream] == ["exit"]
    stdin = [m for m in got if m["type"] == "stdin"]
    assert len(stdin) == 2
    assert b"".join(base64.b64decode(m["data"]) for m in stdin) == big
    assert got[2] == {"type": "stdin.close"}
    assert got[3] == {"type": "signal", "signal": "INT"}


def test_run_collects_output(serve: Any) -> None:
    def script(peer: Peer, _: int) -> None:
        peer.accept()
        assert peer.recv_json() == {"type": "stdin", "data": b64(b"in")}
        assert peer.recv_json() == {"type": "stdin.close"}
        peer.text({"type": "stdout", "seq": 1, "data": b64(b"out")})
        peer.text({"type": "stderr", "seq": 2, "data": b64(b"err")})
        peer.text({"type": "exit", "seq": 2, "process": proc_json(timedOut=True)})

    server = serve(script)
    seen: list[httpx.Request] = []

    def http(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        return httpx.Response(201, json=proc_json(status="running", exitCode=None))

    client = Client(socket=server.path, transport=httpx.MockTransport(http))
    result = client.run("web", ["cat"], input="in", deadline_seconds=5)
    assert (result.exit_code, result.stdout, result.stderr) == (124, b"out", b"err")
    assert result.timed_out
    assert json.loads(seen[0].content) == {"argv": ["cat"], "deadlineSeconds": 5, "stdin": True}
    assert seen[0].headers["Idempotency-Key"]


# ---- process HTTP calls and helpers -------------------------------------------


def test_process_http_calls() -> None:
    seen: list[httpx.Request] = []

    def http(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        if request.url.path.endswith("/processes") and request.method == "GET":
            return httpx.Response(200, json={"items": [proc_json()]})
        return httpx.Response(200 if request.method != "POST" else 201, json=proc_json())

    client = Client(base_url="http://127.0.0.1:9", transport=httpx.MockTransport(http))
    proc = client.exec_process(
        "web", ["sh", "-c", "echo hi"], cwd="/w", env={"A": "1"}, detached=True, tty=False
    )
    assert isinstance(proc, Process)
    assert json.loads(seen[0].content) == {
        "argv": ["sh", "-c", "echo hi"],
        "cwd": "/w",
        "env": {"A": "1"},
        "detached": True,
        "tty": False,
    }
    assert [p.id for p in client.iter_processes("web")] == ["proc_1"]
    client.get_process("web", "proc_1")
    client.kill_process("web", "proc_1")
    client.kill_process("web", "proc_1", signal="KILL")
    paths = [(r.method, r.url.path, r.url.query) for r in seen[1:]]
    assert paths == [
        ("GET", "/api/v1/vms/web/processes", b""),
        ("GET", "/api/v1/vms/web/processes/proc_1", b""),
        ("DELETE", "/api/v1/vms/web/processes/proc_1", b""),
        ("DELETE", "/api/v1/vms/web/processes/proc_1", b"signal=KILL"),
    ]


@pytest.mark.parametrize(
    ("argv", "deadline", "exc"),
    [
        ("echo hi", None, TypeError),
        ([1, 2], None, TypeError),
        ([], None, ValueError),
        (["true"], 0, ValueError),
        (["true"], True, ValueError),
    ],
)
def test_exec_validates_arguments(argv: Any, deadline: Any, exc: type[Exception]) -> None:
    client = Client(base_url="http://127.0.0.1:9", transport=httpx.MockTransport(lambda r: 1 / 0))
    with pytest.raises(exc):
        client.exec_process("web", argv, deadline_seconds=deadline)


@pytest.mark.parametrize(
    ("over", "code"),
    [
        ({"exitCode": 3}, 3),
        ({"exitCode": 256 + 7}, 7),
        ({"exitCode": 1, "timedOut": True}, 124),
        ({"exitCode": None, "signal": "KILL"}, 137),
        ({"exitCode": None, "signal": "SIGTERM"}, 143),
        ({"exitCode": None, "signal": "WINCH"}, 255),
        ({"exitCode": None}, 255),
    ],
)
def test_exit_status(over: dict[str, Any], code: int) -> None:
    raw_proc = {k: v for k, v in proc_json(**over).items() if v is not None}
    assert exit_status(Process.from_dict(raw_proc)) == code
    assert exit_status(None) == 255


def test_stream_event_is_plain_data() -> None:
    assert StreamEvent("stdout", 1, b"x") == StreamEvent("stdout", 1, b"x")


# ---- run_to_completion over a scripted daemon -----------------------------


def run_body(state: str, **over: Any) -> dict[str, Any]:
    return {"id": "run_1", "state": state, "keep": False, "createdAt": NOW, **over}


def _run_client(serve: Any, polls: list[dict[str, Any]]) -> tuple[Client, list[httpx.Request]]:
    """A client whose HTTP calls follow ``polls`` and whose attach stream is scripted."""

    def script(peer: Peer, _: int) -> None:
        peer.accept()
        peer.text({"type": "stdout", "seq": 1, "data": b64(b"out\n")})
        killed = proc_json(status="killed", exitCode=None, signal="KILL", detached=True)
        peer.text({"type": "exit", "seq": 1, "process": killed})

    server = serve(script)
    seen: list[httpx.Request] = []
    gets = iter(polls)

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        if request.method == "POST":
            return httpx.Response(201, json=run_body("creating"))
        if request.method == "DELETE":
            return httpx.Response(200, json=run_body("running", outcome="failed", decidedAt=NOW))
        return httpx.Response(200, json=next(gets))

    return Client(socket=server.path, transport=httpx.MockTransport(handler)), seen


def test_run_to_completion_cancels_once_decided_and_reports_signal(
    serve: Any, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr("davit.client.RUN_POLL_INTERVAL", 0.0)
    started = run_body("running", vmId="vm_1", processId="proc_1")
    decided = {**started, "outcome": "failed", "decidedAt": NOW, "signal": "KILL"}
    ended = {**decided, "state": "failed", "endedAt": NOW}
    # Still undecided after the exit message: keep polling; exitCode alone is not "decided".
    polls = [started, {**started, "exitCode": 0}, decided, ended]
    client, seen = _run_client(serve, polls)
    with client:
        out = client.run_to_completion(["sh"])
    assert (out.exit_code, out.signal, out.stdout) == (137, "KILL", b"out\n")
    assert out.run.state == RunState.FAILED
    methods = [r.method for r in seen]
    assert methods == ["POST", "GET", "GET", "GET", "DELETE", "GET"]
    assert seen[4].url.path == "/api/v1/runs/run_1"


def test_run_to_completion_without_grace_never_cancels(
    serve: Any, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr("davit.client.RUN_POLL_INTERVAL", 0.0)
    started = run_body("running", vmId="vm_1", processId="proc_1")
    decided = {**started, "outcome": "failed", "decidedAt": NOW, "signal": "KILL"}
    polls = [started, decided, {**decided, "state": "failed", "endedAt": NOW}]
    client, seen = _run_client(serve, polls)
    with client:
        out = client.run_to_completion(["sh"], output_grace_seconds=0)
    assert out.exit_code == 137
    assert json.loads(seen[0].content)["outputGraceSeconds"] == 0
    assert "DELETE" not in [r.method for r in seen]


def test_run_to_completion_reads_retained_output_when_the_vm_is_gone(
    serve: Any, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr("davit.client.RUN_POLL_INTERVAL", 0.0)

    def script(peer: Peer, _: int) -> None:
        err = {"code": "invalid_state", "message": "vm is deleting", "retryable": False}
        peer.reject(409, {"error": {**err, "requestId": "r"}})

    server = serve(script)
    started = run_body("running", vmId="vm_1", processId="proc_1")
    ended = {
        **started,
        "state": "succeeded",
        "outcome": "succeeded",
        "decidedAt": NOW,
        "endedAt": NOW,
        "exitCode": 0,
    }
    gets = iter([started, ended, ended])
    seen: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        if request.method == "POST":
            return httpx.Response(201, json=run_body("creating"))
        if request.url.path.endswith("/output"):
            items = [{"type": "stdout", "seq": 1, "data": b64(b"late\n")}]
            return httpx.Response(200, json={"items": items, "nextCursor": "1", "complete": True})
        return httpx.Response(200, json=next(gets))

    with Client(socket=server.path, transport=httpx.MockTransport(handler)) as client:
        out = client.run_to_completion(["true"], output_grace_seconds=0)
    assert (out.exit_code, out.stdout, out.signal) == (0, b"late\n", None)
    assert "/api/v1/vms/vm_1/processes/proc_1/output" in [r.url.path for r in seen]
