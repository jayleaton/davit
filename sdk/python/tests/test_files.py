"""File transfer behaviour against an in-process mock transport."""

from __future__ import annotations

import errno
import hashlib
import io
import tarfile
from collections.abc import Callable
from typing import Any

import httpx
import pytest

from davit import Client, FileInfo, InvalidState, NotFound, TransportError, _transport

NOW = "2026-10-22T04:22:34Z"
VM = {
    "id": "vm_1",
    "owner": "o",
    "name": "web",
    "state": "ready",
    "stateRevision": 2,
    "instanceGeneration": 4,
    "shape": {"cpus": 1, "memoryBytes": 1, "diskBytes": 1},
    "imageId": "img",
    "bootMode": "cold_boot",
    "ttlSeconds": 0,
    "autoStop": False,
    "readiness": {"agent": True, "ssh": False, "services": {}},
    "endpoint": {"mode": "gateway", "state": "none"},
    "storage": {"mode": "copy", "fastClone": False},
    "privacy": {},
    "egress": {"mode": "filtered"},
    "createdAt": NOW,
    "updatedAt": NOW,
}
INFO = {"path": "/a", "sizeBytes": 3, "mode": "0640", "sha256": "ab", "generation": 4}

Handler = Callable[[httpx.Request], httpx.Response]


@pytest.fixture(autouse=True)
def _no_backoff(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(_transport, "_BACKOFF", 0.0)


def client_for(handler: Handler) -> tuple[Client, list[httpx.Request]]:
    seen: list[httpx.Request] = []

    def record(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        if request.url.path == "/api/v1/vms/web":
            return httpx.Response(200, json=VM)
        return handler(request)

    client = Client(base_url="http://127.0.0.1:9", transport=httpx.MockTransport(record))
    return client, seen


def tar_of(files: dict[str, bytes]) -> bytes:
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w") as tar:
        for name, data in files.items():
            info = tarfile.TarInfo(name)
            info.size = len(data)
            tar.addfile(info, io.BytesIO(data))
    return buf.getvalue()


def test_read_resolves_generation_and_returns_mode() -> None:
    client, seen = client_for(
        lambda r: httpx.Response(200, content=b"hello", headers={"X-Davit-File-Mode": "0640"})
    )
    got = client.read_file("web", "/etc/motd", user="root")
    assert (got.data, got.mode) == (b"hello", "0640")
    assert [r.url.path for r in seen] == ["/api/v1/vms/web", "/api/v1/vms/vm_1/files"]
    params = seen[1].url.params
    assert dict(params) == {"path": "/etc/motd", "generation": "4", "user": "root"}
    assert seen[1].headers["X-Request-Id"].startswith("req_")


def test_explicit_generation_skips_lookup_and_archive_param() -> None:
    body = tar_of({"f": b"x"})
    client, seen = client_for(lambda r: httpx.Response(200, content=body))
    got = client.read_file("web", "/d", generation=2, archive=True)
    with tarfile.open(fileobj=io.BytesIO(got.data)) as tar:
        assert tar.getnames() == ["f"]
    assert got.mode is None
    (req,) = seen
    assert req.url.path == "/api/v1/vms/web/files"
    assert dict(req.url.params) == {"path": "/d", "generation": "2", "archive": "tar"}


def test_short_regular_file_is_truncation() -> None:
    client, _ = client_for(
        lambda r: httpx.Response(200, content=b"abc", headers={"Content-Length": "10"})
    )
    with pytest.raises(TransportError, match="got 3 of 10 bytes") as info:
        client.read_file("web", "/f", generation=1)
    assert info.value.details == {"path": "/f", "received": 3, "expected": 10}


def test_tar_without_end_marker_is_truncation() -> None:
    body = tar_of({"f": b"x" * 600})[:1536]  # header + data, no end blocks
    client, _ = client_for(lambda r: httpx.Response(200, content=body))
    with pytest.raises(TransportError, match="end marker"):
        client.read_file("web", "/d", generation=1, archive=True)


def test_mid_stream_failure_is_transport_error() -> None:
    class Broken(httpx.SyncByteStream):
        def __iter__(self) -> Any:
            yield b"part"
            raise httpx.ReadError("connection reset")

    client, _ = client_for(lambda r: httpx.Response(200, stream=Broken()))
    with client.read_file_stream("web", "/f", generation=1) as stream:
        chunks = []
        with pytest.raises(TransportError, match="transfer failed"):  # noqa: PT012
            for chunk in stream:
                chunks.append(chunk)
    assert chunks == [b"part"]
    assert stream.received == 4


def test_stream_copy_to_and_single_use() -> None:
    data = bytes(range(256)) * 1000
    client, _ = client_for(
        lambda r: httpx.Response(200, content=data, headers={"Content-Length": str(len(data))})
    )
    out = io.BytesIO()
    with client.read_file_stream("web", "/f", generation=1) as stream:
        assert stream.content_length == len(data)
        assert stream.copy_to(out) == len(data)
        with pytest.raises(RuntimeError):
            stream.read()
    assert out.getvalue() == data


def test_read_errors_map_to_api_errors() -> None:
    body = {"error": {"code": "invalid_state", "message": "generation 3 != 4", "retryable": False}}
    client, _ = client_for(lambda r: httpx.Response(409, json=body))
    with pytest.raises(InvalidState) as info:
        client.read_file("web", "/f", generation=3)
    assert info.value.status == 409
    nf = {"error": {"code": "not_found", "message": "no such file", "retryable": False}}
    client, _ = client_for(lambda r: httpx.Response(404, json=nf))
    with pytest.raises(NotFound):
        client.read_file("web", "/missing", generation=1)


def test_write_bytes_headers_and_result() -> None:
    client, seen = client_for(lambda r: httpx.Response(200, json=INFO))
    info = client.write_file("web", "/a", "hé", mode=0o640, generation=4)
    assert isinstance(info, FileInfo)
    assert info.size_bytes == 3
    req = seen[0]
    assert req.method == "PUT"
    assert req.content == "hé".encode()
    assert req.headers["Content-Type"] == "application/octet-stream"
    assert "Idempotency-Key" not in req.headers  # naturally idempotent: no key
    assert dict(req.url.params) == {"path": "/a", "generation": "4", "mode": "0640"}


def test_write_streams_file_objects_and_iterables_chunked() -> None:
    bodies: list[tuple[str | None, bytes]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        bodies.append((request.headers.get("Transfer-Encoding"), request.read()))
        return httpx.Response(200, json={**INFO, "archive": True})

    client, seen = client_for(handler)
    client.write_file("web", "/a", io.BytesIO(b"x" * 200_000), generation=1)
    client.write_file("web", "/d", iter([b"ab", b"", b"cd"]), generation=1, archive=True)
    assert bodies[0] == ("chunked", b"x" * 200_000)
    assert bodies[1] == ("chunked", b"abcd")
    assert seen[1].headers["Content-Type"] == "application/x-tar"
    assert seen[1].url.params["archive"] == "tar"


def refused() -> httpx.ConnectError:
    exc = httpx.ConnectError("refused")
    exc.__cause__ = ConnectionRefusedError(errno.ECONNREFUSED, "refused")
    return exc


def test_write_retries_only_replayable_bodies() -> None:
    attempts: list[int] = []

    def flaky(request: httpx.Request) -> httpx.Response:
        attempts.append(1)
        if len(attempts) < 3:
            raise refused()
        return httpx.Response(200, json=INFO)

    client, seen = client_for(flaky)
    client.write_file("web", "/a", b"abc", generation=1)
    assert len(seen) == 3
    assert all("Idempotency-Key" not in r.headers for r in seen)

    attempts.clear()
    client, seen = client_for(flaky)
    with pytest.raises(TransportError):
        client.write_file("web", "/a", iter([b"abc"]), generation=1)
    assert len(seen) == 1


@pytest.mark.parametrize(
    ("kwargs", "exc"),
    [
        ({"path": "relative"}, ValueError),
        ({"path": "/a", "mode": "rwx"}, ValueError),
        ({"path": "/a", "mode": 0o17777}, ValueError),
        ({"path": "/a", "mode": True}, TypeError),
        ({"path": "/a", "generation": 0}, ValueError),
        ({"path": "/a", "data": 42}, TypeError),
    ],
)
def test_write_validates_before_any_request(kwargs: dict[str, Any], exc: type[Exception]) -> None:
    client, seen = client_for(lambda r: httpx.Response(500))
    args = {"data": b"x", **kwargs}
    with pytest.raises(exc):
        client.write_file("web", args.pop("path"), args.pop("data"), **args)
    assert seen == []


def test_mode_strings_pass_through() -> None:
    client, seen = client_for(lambda r: httpx.Response(200, json=INFO))
    client.write_file("web", "/a", b"x", mode="755", generation=1)
    assert seen[0].url.params["mode"] == "755"
    client.write_file("web", "/a", b"x", mode=0o4755, generation=1)
    client.write_file("web", "/a", b"x", mode=0o7, generation=1)
    assert [r.url.params["mode"] for r in seen[1:]] == ["04755", "0007"]


def header(name: str, size: int, typeflag: bytes = b"0", size_field: bytes | None = None) -> bytes:
    block = bytearray(512)
    block[: len(name)] = name.encode()
    block[100:108] = b"0000644\0"
    block[124:136] = size_field if size_field is not None else b"%011o\0" % size
    block[156:157] = typeflag
    block[257:263] = b"ustar\0"
    block[148:156] = b" " * 8
    block[148:156] = b"%06o\0 " % sum(block)
    return bytes(block)


TAR_CASES = {
    "tarfile-padded": (tar_of({"a": b"x" * 700, "b": b""}), True),
    "dir-header-only": (header("d", 0, b"5") + b"\0" * 1024, True),
    "base256-size": (
        header("big", 3, size_field=b"\x80" + b"\0" * 10 + b"\x03")
        + b"abc".ljust(512, b"\0")
        + b"\0" * 1024,
        True,
    ),
    "empty-archive": (b"\0" * 1024, True),
    "zero-data-then-marker": (header("f", 3) + b"\0" * 1536, True),
    "single-zero-block": (header("f", 3) + b"abc".ljust(512, b"\0") + b"\0" * 512, False),
    "marker-eaten-by-data": (header("f", 3) + b"\0" * 1024, False),
    "bad-checksum": (
        header("f", 0)[:148] + b"0000000\0" + header("f", 0)[156:] + b"\0" * 1024,
        False,
    ),
    "trailing-garbage": (tar_of({"a": b"x"}) + b"garbage", False),
    "cut-mid-data": (header("f", 2000) + b"x" * 700, False),
}


@pytest.mark.parametrize(("body", "ok"), list(TAR_CASES.values()), ids=list(TAR_CASES))
def test_tar_structure_check(body: bytes, ok: bool) -> None:
    client, _ = client_for(lambda r: httpx.Response(200, content=body))
    if ok:
        assert client.read_file("web", "/d", generation=1, archive=True).data == body
    else:
        with pytest.raises(TransportError):
            client.read_file("web", "/d", generation=1, archive=True)


# ---- snapshot files ---------------------------------------------------------


def entry(path: str, kind: str = "file", **over: Any) -> dict[str, Any]:
    return {
        "path": path,
        "type": kind,
        "sizeBytes": 3,
        "mode": "0644",
        "modifiedAt": NOW,
        **over,
    }


def test_snapshot_tree_pages_follow_cursor() -> None:
    pages = {
        None: {"path": "/etc", "items": [entry("/etc/a"), entry("/etc/b")], "nextCursor": "c2"},
        "c2": {
            "path": "/etc",
            "items": [entry("/etc/l", "symlink", linkTarget="/etc/a")],
            "nextCursor": "",
        },
    }
    client, seen = client_for(lambda r: httpx.Response(200, json=pages[r.url.params.get("cursor")]))
    got = list(client.iter_snapshot_tree("base-snap", path="/etc", limit=2))
    assert [e.path for e in got] == ["/etc/a", "/etc/b", "/etc/l"]
    assert got[2].type == "symlink" and got[2].link_target == "/etc/a"
    assert got[0].mode == "0644" and got[0].size_bytes == 3
    assert [r.url.path for r in seen] == ["/api/v1/snapshots/base-snap/tree"] * 2
    assert [dict(r.url.params) for r in seen] == [
        {"path": "/etc", "limit": "2"},
        {"path": "/etc", "limit": "2", "cursor": "c2"},
    ]


def test_snapshot_tree_defaults_to_root_and_checks_path() -> None:
    client, seen = client_for(lambda r: httpx.Response(200, json={"path": "/", "items": []}))
    page = client.list_snapshot_tree("snap_1")
    assert (page.path, page.items, page.next_cursor) == ("/", [], None)
    assert dict(seen[0].url.params) == {"path": "/"}
    with pytest.raises(ValueError, match="must be absolute"):
        client.list_snapshot_tree("snap_1", path="etc")
    assert len(seen) == 1


def file_response(body: bytes, digest: str | None = None) -> httpx.Response:
    headers = {
        "Content-Length": str(len(body)),
        "X-Davit-File-Mode": "0600",
        "X-Davit-Content-Sha256": digest or hashlib.sha256(body).hexdigest(),
    }
    return httpx.Response(200, content=body, headers=headers)


def test_read_snapshot_file_verifies_digest() -> None:
    body = b"root:x:0:0\n"
    client, seen = client_for(lambda r: file_response(body))
    got = client.read_snapshot_file("snap_1", "/etc/passwd")
    assert (got.data, got.mode, got.sha256) == (body, "0600", hashlib.sha256(body).hexdigest())
    (req,) = seen  # no VM lookup, no generation
    assert req.url.path == "/api/v1/snapshots/snap_1/files"
    assert dict(req.url.params) == {"path": "/etc/passwd"}
    with client.read_snapshot_file_stream("snap_1", "/etc/passwd") as stream:
        assert stream.sha256 == hashlib.sha256(body).hexdigest()
        assert stream.read() == body


def test_read_snapshot_file_digest_mismatch_raises() -> None:
    wrong = hashlib.sha256(b"other").hexdigest()
    client, _ = client_for(lambda r: file_response(b"tampered", wrong))
    with pytest.raises(TransportError, match="digest mismatch") as info:
        client.read_snapshot_file("snap_1", "/f")
    assert "in snapshot snap_1" in str(info.value)
    assert info.value.details["expected_sha256"] == wrong
    assert info.value.details["sha256"] == hashlib.sha256(b"tampered").hexdigest()
    with (
        client.read_snapshot_file_stream("snap_1", "/f") as stream,
        pytest.raises(TransportError, match="digest mismatch"),
    ):
        stream.read()


def test_read_snapshot_file_without_digest_header_raises() -> None:
    client, _ = client_for(lambda r: httpx.Response(200, content=b"abc"))
    with pytest.raises(TransportError, match="X-Davit-Content-Sha256"):
        client.read_snapshot_file("snap_1", "/f")


def test_read_snapshot_dir_as_tar() -> None:
    body = tar_of({"a": b"x" * 700, "sub/b": b"y"})
    client, seen = client_for(
        lambda r: httpx.Response(
            200,
            content=body,
            headers={"Content-Type": "application/x-tar", "X-Davit-File-Mode": "0755"},
        )
    )
    got = client.read_snapshot_file("snap_1", "/srv", archive=True)
    assert (got.mode, got.sha256) == ("0755", None)
    with tarfile.open(fileobj=io.BytesIO(got.data)) as tar:
        assert tar.getnames() == ["a", "sub/b"]
    assert dict(seen[0].url.params) == {"path": "/srv", "archive": "tar"}
    client, _ = client_for(lambda r: httpx.Response(200, content=body[:1024]))
    with pytest.raises(TransportError, match="end marker"):
        client.read_snapshot_file("snap_1", "/srv", archive=True)


def test_export_snapshot_streams_tar_and_raw() -> None:
    tar_body = tar_of({"etc/hostname": b"web\n"})
    raw_body = bytes(range(256)) * 64

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.params["format"] == "tar":
            return httpx.Response(200, content=tar_body)
        return httpx.Response(200, content=raw_body, headers={"Content-Length": str(len(raw_body))})

    client, seen = client_for(handler)
    with client.export_snapshot("base-snap") as stream:
        out = io.BytesIO()
        assert stream.archive and stream.copy_to(out) == len(tar_body)
    assert out.getvalue() == tar_body
    with client.export_snapshot("base-snap", format="raw") as stream:
        assert not stream.archive and stream.content_length == len(raw_body)
        assert b"".join(stream.iter_chunks(1000)) == raw_body
    assert [(r.url.path, dict(r.url.params)) for r in seen] == [
        ("/api/v1/snapshots/base-snap/export", {"format": "tar"}),
        ("/api/v1/snapshots/base-snap/export", {"format": "raw"}),
    ]
    client, _ = client_for(
        lambda r: httpx.Response(200, content=b"abc", headers={"Content-Length": "9"})
    )
    with (
        client.export_snapshot("s", format="raw") as stream,
        pytest.raises(TransportError, match="raw export of snapshot s: got 3 of 9 bytes"),
    ):
        stream.read()
    with pytest.raises(ValueError, match="'tar' or 'raw'"):
        client.export_snapshot("s", format="zip")  # type: ignore[arg-type]
