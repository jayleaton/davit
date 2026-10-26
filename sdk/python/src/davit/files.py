"""File transfers: streamed reads with truncation and digest checks, and upload bodies."""

from __future__ import annotations

import hashlib
import re
from collections.abc import Iterable, Iterator
from dataclasses import dataclass
from typing import IO, Any, Protocol, cast

import httpx

from .errors import TransportError

__all__ = ["CHUNK_SIZE", "FileContent", "FileStream", "WriteData"]

CHUNK_SIZE = 64 << 10
"""Chunk size for streamed reads and file-object uploads."""
_BLOCK = 512
_ZERO = b"\0" * _BLOCK
_HEADER_ONLY = set(b"123456")  # hard/sym links, devices, dirs, fifos carry no data
_MODE = re.compile(r"^0?[0-7]{3,4}$")


class TarChecker:
    """Incrementally checks that a byte stream is a structurally complete tar.

    Walks the 512-byte headers (checksum, size in octal or base-256), skips
    entry data, and requires two consecutive zero blocks at a header position,
    with only zero bytes after them.
    """

    def __init__(self) -> None:
        self._buf = bytearray()
        self._skip = 0  # data bytes still to skip
        self._zeros = 0  # consecutive zero header blocks seen
        self.error: str | None = None

    @property
    def complete(self) -> bool:
        return self.error is None and self._zeros >= 2

    def feed(self, data: bytes) -> None:
        if self.error is not None:
            return
        if self._zeros >= 2:
            if data.strip(b"\0"):
                self.error = "non-zero data after the tar end marker"
            return
        self._buf += data
        while self.error is None and self._zeros < 2:
            if self._skip:
                n = min(self._skip, len(self._buf))
                del self._buf[:n]
                self._skip -= n
                if self._skip:
                    return
            if len(self._buf) < _BLOCK:
                return
            block = bytes(self._buf[:_BLOCK])
            del self._buf[:_BLOCK]
            self._header(block)
        if self._zeros >= 2 and self._buf.strip(b"\0"):
            self.error = "non-zero data after the tar end marker"

    def _header(self, block: bytes) -> None:
        if block == _ZERO:
            self._zeros += 1
            return
        if self._zeros:
            self.error = "tar end marker is a single zero block"
            return
        try:
            stored = int(block[148:156].strip(b" \0") or b"0", 8)
        except ValueError:
            self.error = "corrupt tar header checksum field"
            return
        if stored != sum(block[:148]) + 8 * 32 + sum(block[156:]):
            self.error = "corrupt tar header (checksum mismatch)"
            return
        field = block[124:136]
        if field[0] & 0x80:  # base-256
            size = int.from_bytes(bytes([field[0] & 0x7F]) + field[1:], "big")
        else:
            try:
                size = int(field.strip(b" \0") or b"0", 8)
            except ValueError:
                self.error = "corrupt tar header size field"
                return
        if block[156] not in _HEADER_ONLY:
            self._skip = -(-size // _BLOCK) * _BLOCK


class _Readable(Protocol):
    def read(self, size: int = ..., /) -> bytes: ...


WriteData = bytes | bytearray | memoryview | str | IO[bytes] | Iterable[bytes]
"""Accepted upload bodies: bytes or str (UTF-8) are replayable; file objects and
iterables of bytes are streamed once (chunked)."""


@dataclass(frozen=True)
class FileContent:
    """A fully read file.

    Attributes:
        data: The file bytes (a tar stream when read with ``archive=True``).
        mode: Octal permission bits from ``X-Davit-File-Mode`` (e.g. ``"0644"``),
            or ``None`` when the server did not send them.
        sha256: Hex SHA-256 of ``data`` from ``X-Davit-Content-Sha256`` (snapshot
            file reads, already verified), or ``None`` when the server did not send it.
    """

    data: bytes
    mode: str | None
    sha256: str | None = None


class FileStream:
    """A streamed file read; iterate for byte chunks, or use :meth:`read` / :meth:`copy_to`.

    The transfer is verified when fully consumed: a regular file must deliver
    exactly ``Content-Length`` bytes and a tar stream must end with its end
    marker, otherwise :class:`~davit.TransportError` is raised. When opened
    with ``verify_sha256`` (snapshot file reads), a regular file's bytes must
    also hash to its ``X-Davit-Content-Sha256`` header. Single use; use as a
    context manager (or call :meth:`close`).

    Attributes:
        path: The guest (or snapshot) path.
        mode: Octal permission bits (``X-Davit-File-Mode``), or ``None``.
        sha256: Hex SHA-256 from the ``X-Davit-Content-Sha256`` header, or ``None``
            (archives and exports carry it as a trailer, which is not read).
        content_length: Expected size for a regular file, when the server sent it.
        archive: Whether this is a tar stream of a directory.
        received: Bytes delivered so far.
    """

    def __init__(
        self,
        response: httpx.Response,
        *,
        path: str,
        archive: bool,
        verify_sha256: bool = False,
        what: str | None = None,
    ) -> None:
        self._resp = response
        self.path = path
        self.archive = archive
        self.mode: str | None = response.headers.get("X-Davit-File-Mode")
        self.sha256: str | None = response.headers.get("X-Davit-Content-Sha256")
        length = response.headers.get("Content-Length")
        self.content_length: int | None = int(length) if length and length.isdigit() else None
        self.received = 0
        self._tar = TarChecker() if archive else None
        self._hash = hashlib.sha256() if verify_sha256 and not archive else None
        self._what = what or path
        self._started = False

    def __enter__(self) -> FileStream:
        return self

    def __exit__(self, *exc_info: object) -> None:
        self.close()

    def close(self) -> None:
        """Release the connection (an unfinished transfer is abandoned)."""
        self._resp.close()

    def __iter__(self) -> Iterator[bytes]:
        return self.iter_chunks()

    def iter_chunks(self, chunk_size: int | None = None) -> Iterator[bytes]:
        """Yield the body as it arrives (or re-chunked to ``chunk_size``), then verify it."""
        if self._started:
            raise RuntimeError("a FileStream can be consumed only once")
        self._started = True
        try:
            for chunk in self._resp.iter_bytes(chunk_size):
                self.received += len(chunk)
                if self._tar is not None:
                    self._tar.feed(chunk)
                if self._hash is not None:
                    self._hash.update(chunk)
                yield chunk
        except httpx.HTTPError as exc:
            raise self._truncated(f"transfer failed: {exc}") from exc
        finally:
            self._resp.close()
        self._verify()

    def read(self) -> bytes:
        """Read and verify the whole body."""
        return b"".join(self.iter_chunks())

    def copy_to(self, fileobj: IO[bytes], chunk_size: int | None = None) -> int:
        """Write the whole body into a binary file object; return the byte count."""
        for chunk in self.iter_chunks(chunk_size):
            fileobj.write(chunk)
        return self.received

    def _verify(self) -> None:
        if self._tar is not None:
            if self._tar.error is not None:
                raise self._truncated(self._tar.error)
            if not self._tar.complete:
                raise self._truncated("tar stream ended without its end marker")
        elif self.content_length is not None and self.received != self.content_length:
            raise self._truncated(f"got {self.received} of {self.content_length} bytes")
        if self._hash is not None:
            if not self.sha256:
                raise self._truncated("the server sent no X-Davit-Content-Sha256 header")
            got = self._hash.hexdigest()
            if got != self.sha256.strip().lower():
                err = self._truncated(
                    f"content digest mismatch: body hashes to sha256 {got}, "
                    f"X-Davit-Content-Sha256 says {self.sha256}"
                )
                err.details.update(sha256=got, expected_sha256=self.sha256)
                raise err

    def _truncated(self, message: str) -> TransportError:
        details: dict[str, Any] = {"path": self.path, "received": self.received}
        if self.content_length is not None and not self.archive:
            details["expected"] = self.content_length
        return TransportError(
            f"reading {self._what}: {message}",
            retryable=True,
            details=details,
            request_id=self._resp.headers.get("X-Request-Id"),
        )


def check_path(path: str) -> str:
    """Validate an absolute guest path."""
    if not isinstance(path, str) or not path.startswith("/"):
        raise ValueError(f"guest path must be absolute, got {path!r}")
    return path


def format_mode(mode: int | str | None) -> str | None:
    """Return the wire ``mode`` octal string for an int (``0o644``) or a str (``"0644"``)."""
    if mode is None:
        return None
    if isinstance(mode, bool):
        raise TypeError("mode must be an int like 0o644 or a string like '0644'")
    if isinstance(mode, int):
        if not 0 <= mode <= 0o7777:
            raise ValueError(f"mode {mode:#o} is outside 0..0o7777")
        return "0" + format(mode, "o").zfill(3)
    if isinstance(mode, str) and _MODE.match(mode):
        return mode
    raise ValueError(f"mode must be octal permission bits like '0644', got {mode!r}")


def body_of(data: WriteData) -> tuple[bytes | Iterator[bytes], bool]:
    """Return ``(httpx content, replayable)`` for an upload body."""
    if isinstance(data, str):
        return data.encode(), True
    if isinstance(data, (bytes, bytearray, memoryview)):
        return bytes(data), True
    reader = getattr(data, "read", None)
    if callable(reader):
        return _read_chunks(cast(_Readable, data)), False
    if isinstance(data, Iterable):
        return _iter_chunks(data), False
    raise TypeError(f"unsupported upload body: {type(data).__name__}")


def _read_chunks(fileobj: _Readable) -> Iterator[bytes]:
    while chunk := fileobj.read(CHUNK_SIZE):
        if not isinstance(chunk, (bytes, bytearray)):
            raise TypeError("file objects must be opened in binary mode")
        yield bytes(chunk)


def _iter_chunks(chunks: Iterable[Any]) -> Iterator[bytes]:
    for chunk in chunks:
        if not isinstance(chunk, (bytes, bytearray, memoryview)):
            raise TypeError("upload iterables must yield bytes")
        if chunk:
            yield bytes(chunk)
