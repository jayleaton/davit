"""Process streams: attach to a guest process's output, send it input, collect results."""

from __future__ import annotations

import base64
import binascii
import json
import time
from collections.abc import Callable, Iterator
from dataclasses import dataclass
from typing import Any

from . import _websocket
from .errors import DavitError, TransportError, error_for
from .models import Process

__all__ = [
    "EXIT_DEADLINE",
    "EXIT_LOST",
    "STDIN_CHUNK",
    "ProcessStream",
    "RetainedOutput",
    "RunResult",
    "StreamEvent",
    "exit_status",
    "signal_status",
]

EXIT_DEADLINE = 124
"""Exit status when the process deadline killed it (as ``davit exec``)."""
EXIT_LOST = 255
"""Exit status when the outcome is unknown."""
STDIN_CHUNK = 256 << 10
"""Largest raw stdin chunk per message (base64 must stay under the 1 MiB bound)."""

REATTACH_ATTEMPTS = 3
_REATTACH_BACKOFF = 0.1  # seconds, doubled per attempt

_SIGNALS = {
    "HUP": 1,
    "INT": 2,
    "QUIT": 3,
    "ILL": 4,
    "TRAP": 5,
    "ABRT": 6,
    "BUS": 7,
    "FPE": 8,
    "KILL": 9,
    "USR1": 10,
    "SEGV": 11,
    "USR2": 12,
    "PIPE": 13,
    "ALRM": 14,
    "TERM": 15,
    "SYS": 31,
}


def exit_status(process: Process | None) -> int:
    """Map a final process to a shell-style exit status, like ``davit exec``.

    Deadline → 124; exit code → ``code & 0xff``; signal → ``128 + n``;
    otherwise 255.
    """
    if process is None:
        return EXIT_LOST
    if process.timed_out:
        return EXIT_DEADLINE
    if process.exit_code is not None:
        return process.exit_code & 0xFF
    if process.signal:
        return signal_status(process.signal) or EXIT_LOST
    return EXIT_LOST


def signal_status(signal: str) -> int | None:
    """``128 + n`` for a signal name (``"KILL"`` or ``"SIGKILL"``), or ``None`` if unknown."""
    name = signal.upper().removeprefix("SIG")
    return 128 + _SIGNALS[name] if name in _SIGNALS else None


@dataclass(frozen=True)
class StreamEvent:
    """One decoded stream event.

    Attributes:
        type: ``"stdout"``, ``"stderr"`` or ``"exit"``.
        seq: Output sequence number (for ``exit``: the last output seq).
        data: Output bytes (empty for ``exit``).
        process: The final process (``exit`` only).
    """

    type: str
    seq: int
    data: bytes = b""
    process: Process | None = None


@dataclass(frozen=True)
class RunResult:
    """Result of :meth:`davit.Client.run`.

    Attributes:
        exit_code: Shell-style status (see :func:`exit_status`); non-zero is
            not an exception.
        stdout: Everything the process wrote to stdout.
        stderr: Everything the process wrote to stderr.
        process: The final process from the ``exit`` message.
    """

    exit_code: int
    stdout: bytes
    stderr: bytes
    process: Process | None

    @property
    def timed_out(self) -> bool:
        """Whether the process deadline killed it."""
        return bool(self.process is not None and self.process.timed_out)


@dataclass(frozen=True)
class RetainedOutput:
    """Result of :meth:`davit.Client.collect_process_output`.

    Attributes:
        stdout: The retained stdout bytes, in order.
        stderr: The retained stderr bytes, in order.
        complete: The process had ended and every retained chunk was read.
            ``False`` while it still runs (read again from ``cursor`` later).
        cursor: Resume point (``after``) for a later read; ``None`` when no
            chunk was read.
    """

    stdout: bytes
    stderr: bytes
    complete: bool
    cursor: str | None = None


Dial = Callable[[int], _websocket.WebSocket]
"""Opens the stream WebSocket with a given ``outputCursor``."""


class ProcessStream:
    """An attached process stream; iterate it for :class:`StreamEvent` values.

    Output replays from ``output_cursor``; chunks with ``seq <= cursor`` are
    dropped, so replays after a reconnect are never delivered twice. Iteration
    ends after the ``exit`` event (``.process`` then holds the final process).
    A server ``error`` message raises the typed :class:`~davit.DavitError`.
    If the connection drops before exit, the stream re-attaches with
    ``outputCursor = cursor`` (up to 3 attempts), else raises
    :class:`~davit.TransportError`. Stdin is not replayed, and a re-attached
    stream no longer owns it.

    Use as a context manager, or call :meth:`close`.
    """

    def __init__(self, dial: Dial, *, process_id: str, output_cursor: int = 0) -> None:
        self._dial = dial
        self.process_id = process_id
        self.cursor = output_cursor
        """Seq of the last output chunk delivered."""
        self.process: Process | None = None
        """The final process, once ``exit`` was received."""
        self._done = False
        self._ws = dial(output_cursor)

    def __enter__(self) -> ProcessStream:
        return self

    def __exit__(self, *exc_info: object) -> None:
        self.close()

    def __iter__(self) -> Iterator[StreamEvent]:
        return self

    def __next__(self) -> StreamEvent:
        while not self._done:
            try:
                op, data = self._ws.recv()
            except (_websocket.WebSocketError, OSError) as exc:
                self._reattach(exc)
                continue
            event = self._handle(op, data)
            if event is not None:
                return event
        raise StopIteration

    def _handle(self, op: int, data: bytes) -> StreamEvent | None:
        if op != _websocket.TEXT:
            return None
        try:
            msg: Any = json.loads(data)
        except ValueError:
            return None
        if not isinstance(msg, dict):
            return None
        kind = msg.get("type")
        seq = msg.get("seq") or 0
        seq = seq if isinstance(seq, int) else 0
        if kind in ("stdout", "stderr"):
            if seq <= self.cursor:
                return None
            try:
                chunk = base64.b64decode(msg.get("data") or "", validate=True)
            except (binascii.Error, ValueError):
                return None
            self.cursor = seq
            return StreamEvent(kind, seq, chunk)
        if kind == "exit":
            raw = msg.get("process")
            try:
                self.process = Process.from_dict(raw) if isinstance(raw, dict) else None
            except ValueError:
                self.process = None
            self._finish()
            return StreamEvent("exit", seq, b"", self.process)
        if kind == "error":
            self._finish()
            raise _stream_error(msg.get("error"), self.process_id)
        return None  # unknown message types are ignored

    def _reattach(self, cause: BaseException) -> None:
        self._ws.abort()
        last: BaseException = cause
        for attempt in range(REATTACH_ATTEMPTS):
            if attempt:
                time.sleep(_REATTACH_BACKOFF * 2 ** (attempt - 1))
            try:
                self._ws = self._dial(self.cursor)
                return
            except TransportError as exc:
                last = exc
        self._done = True
        raise TransportError(
            f"lost the process stream: {last}; query it with get_process",
            retryable=True,
            details={"processId": self.process_id, "cursor": self.cursor},
        ) from last

    def _finish(self) -> None:
        self._done = True
        self._ws.close()

    def _send(self, message: dict[str, str]) -> None:
        if self._done:
            raise TransportError("the process stream is closed")
        try:
            self._ws.send_text(json.dumps(message, separators=(",", ":")))
        except (_websocket.WebSocketError, OSError) as exc:
            raise TransportError(f"process stream send failed: {exc}") from exc

    def send_stdin(self, data: bytes | str) -> None:
        """Send input (``str`` is UTF-8 encoded), in chunks of at most 256 KiB."""
        raw = data.encode() if isinstance(data, str) else bytes(data)
        for i in range(0, len(raw), STDIN_CHUNK):
            chunk = base64.b64encode(raw[i : i + STDIN_CHUNK]).decode()
            self._send({"type": "stdin", "data": chunk})

    def close_stdin(self) -> None:
        """Signal end of input."""
        self._send({"type": "stdin.close"})

    def signal(self, name: str) -> None:
        """Signal the process group (e.g. ``"INT"``, ``"KILL"``)."""
        self._send({"type": "signal", "signal": name})

    def close(self) -> None:
        """Close the stream (idempotent).

        Closing the first stream of a non-detached process before it exits
        makes the server kill the process.
        """
        self._done = True
        self._ws.close()


def _stream_error(raw: Any, process_id: str) -> DavitError:
    err = raw if isinstance(raw, dict) else {}
    details = err.get("details")
    return error_for(
        str(err.get("code") or "internal"),
        str(err.get("message") or "process stream failed"),
        retryable=bool(err.get("retryable", False)),
        details={"processId": process_id, **(details if isinstance(details, dict) else {})},
        request_id=err.get("requestId") or None,
    )
