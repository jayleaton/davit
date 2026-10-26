"""Reverse session background threads never die with an unhandled exception.

A reader or bridge thread that raises outside its handled cases prints a
traceback and, for the reader, leaves the session neither running nor
finished: ``wait()`` blocks forever. Every failure must end the affected
connection, or the session with ``error`` set.
"""

from __future__ import annotations

import threading
import time
import types
import urllib.parse
from collections.abc import Iterator
from typing import Any, cast

import pytest

from davit import _websocket, errors, reverse
from davit.models import ReverseForward
from wsserver import Peer, Server


@pytest.fixture
def thread_errors(monkeypatch: pytest.MonkeyPatch) -> list[BaseException]:
    seen: list[BaseException] = []

    def hook(args: threading.ExceptHookArgs) -> None:
        seen.append(cast(BaseException, args.exc_value))

    monkeypatch.setattr(threading, "excepthook", hook)
    return seen


def hold(peer: Peer) -> None:
    """Keep the stream open until the client closes it."""
    try:
        while True:
            peer.recv_frame()
    except (EOFError, OSError):
        pass


@pytest.fixture
def session_server() -> Iterator[Server]:
    def script(peer: Peer, index: int) -> None:
        peer.accept()
        # A connection ID of the wrong type (the server's bug, or a proxy's).
        peer.text({"type": "connection", "connectionId": 7})
        peer.text({"type": "connection", "connectionId": "c1"})
        hold(peer)

    server = Server(script)
    yield server
    server.close()


def open_session(server: Server, dialed: list[Any]) -> reverse.ReverseSession:
    def dial_connection(connection_id: str) -> _websocket.WebSocket:
        dialed.append(connection_id)
        urllib.parse.quote(connection_id)  # what the client does with it
        raise errors.NotFound("claimed elsewhere")

    ws = _websocket.connect("/session", unix_socket=server.path)
    forward = cast(ReverseForward, types.SimpleNamespace(id="rf_1"))
    return reverse.ReverseSession(
        forward,
        ws,
        dial_connection=dial_connection,
        dial_local=lambda: (_ for _ in ()).throw(OSError("unused")),
    )


def test_bad_connection_event_is_contained(
    session_server: Server, thread_errors: list[BaseException]
) -> None:
    dialed: list[Any] = []
    session = open_session(session_server, dialed)
    deadline = time.monotonic() + 5
    while not dialed and time.monotonic() < deadline:
        time.sleep(0.01)
    time.sleep(0.1)
    assert dialed == ["c1"]  # the malformed one is not dialed
    assert thread_errors == []  # the bad one ended its connection, nothing else
    assert not session.closed  # and the session keeps serving
    session.close()
    assert session.reason == reverse.CLOSED_REASON


def test_reader_failure_finishes_the_session(
    session_server: Server, thread_errors: list[BaseException], monkeypatch: pytest.MonkeyPatch
) -> None:
    def broken(data: Any) -> Any:
        raise KeyError("decoder bug")

    monkeypatch.setattr(reverse.ReverseSessionEvent, "from_dict", broken)
    session = open_session(session_server, [])
    assert session.wait(5) is not None, "the session hung after its reader failed"
    assert session.closed and session.error is not None
    assert "decoder bug" in str(session.error)
    assert thread_errors == []
