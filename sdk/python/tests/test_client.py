"""Client behaviour against an in-process mock transport."""

from __future__ import annotations

import base64
import errno
import io
import json
import os
import re
import shutil
import subprocess
import tarfile
import uuid
from collections.abc import Callable, Iterator
from datetime import timedelta
from pathlib import Path
from typing import Any

import httpx
import pytest

from davit import (
    DEFAULT_PUSH_PATH,
    VM,
    Client,
    Conflict,
    DavitError,
    EnvironmentRef,
    EnvManifest,
    Internal,
    NotFound,
    OperationAccepted,
    OperationFailed,
    PrivacyPolicy,
    ResourceExhausted,
    RetainedOutput,
    RevisionMismatch,
    ServerNotImplemented,
    Snapshot,
    StopRequired,
    TransportError,
    WaitTimeout,
    _transport,
    default_socket,
    git_selection,
    parse_environment_ref,
)
from davit.client import _run_exit
from davit.errors import ERROR_CLASSES, OperationInProgress
from davit.models import (
    CredentialKind,
    GrantState,
    OutputNotRetainedReason,
    PutCredentialRequest,
    Run,
    RunState,
    ShapeOverride,
    TokenScope,
)

NOW = "2026-10-22T04:22:34.243078072Z"


def vm_json(**over: Any) -> dict[str, Any]:
    vm = {
        "id": "vm_1",
        "owner": "local:1000",
        "name": "web",
        "state": "ready",
        "stateRevision": 2,
        "instanceGeneration": 1,
        "shape": {"cpus": 2, "memoryBytes": 1 << 31, "diskBytes": 1 << 33},
        "imageId": "img_1",
        "bootMode": "cold_boot",
        "ttlSeconds": 3600,
        "autoStop": True,
        "readiness": {"agent": False, "ssh": False, "services": {}},
        "endpoint": {"mode": "gateway", "state": "none"},
        "storage": {"mode": "copy", "fastClone": False},
        "privacy": {},
        "egress": {"mode": "filtered"},
        "createdAt": NOW,
        "updatedAt": NOW,
    }
    return {**vm, **over}


def op_json(state: str = "running", **over: Any) -> dict[str, Any]:
    op = {
        "id": "op_1",
        "kind": "create",
        "vmId": "vm_1",
        "owner": "local:1000",
        "state": state,
        "phase": state,
        "progress": 0.5,
        "createdAt": NOW,
        "updatedAt": NOW,
    }
    return {**op, **over}


def event(seq: int, state: str = "running") -> dict[str, Any]:
    return {
        "operationId": "op_1",
        "seq": seq,
        "time": NOW,
        "state": state,
        "phase": f"p{seq}",
        "progress": 0.5,
    }


def err_body(code: str, **over: Any) -> dict[str, Any]:
    return {"error": {"code": code, "message": "nope", "retryable": False, "requestId": "req_srv"}}


Handler = Callable[[httpx.Request], httpx.Response]


@pytest.fixture
def make_client() -> Iterator[Callable[..., tuple[Client, list[httpx.Request]]]]:
    clients: list[Client] = []

    def build(handler: Handler, **kwargs: Any) -> tuple[Client, list[httpx.Request]]:
        seen: list[httpx.Request] = []

        def record(request: httpx.Request) -> httpx.Response:
            seen.append(request)
            return handler(request)

        kwargs.setdefault("base_url", "http://127.0.0.1:9")
        client = Client(transport=httpx.MockTransport(record), **kwargs)
        clients.append(client)
        return client, seen

    yield build
    for client in clients:
        client.close()


@pytest.fixture(autouse=True)
def _no_backoff(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(_transport, "_BACKOFF", 0.0)


def refused() -> httpx.ConnectError:
    try:
        raise ConnectionRefusedError(errno.ECONNREFUSED, "Connection refused")
    except ConnectionRefusedError as cause:
        exc = httpx.ConnectError("[Errno 111] Connection refused")
        exc.__cause__ = cause
        return exc


def test_mutation_headers_and_body(make_client: Any) -> None:
    client, seen = make_client(
        lambda r: httpx.Response(202, json={"operation": op_json(), "vm": vm_json()}),
        token="s3cret",
    )
    acc = client.create_vm("web", ttl=timedelta(minutes=5), auto_stop=False, wait=False)
    assert isinstance(acc, OperationAccepted)
    assert acc.vm is not None and acc.vm.name == "web"
    (req,) = seen
    assert (req.method, req.url.path) == ("POST", "/api/v1/vms")
    assert re.fullmatch(r"req_[0-9a-f]{16}", req.headers["X-Request-Id"])
    assert uuid.UUID(req.headers["Idempotency-Key"]).version == 4
    assert req.headers["Authorization"] == "Bearer s3cret"
    assert req.headers["Content-Type"] == "application/json"
    assert "If-Match" not in req.headers
    assert json.loads(req.content) == {"name": "web", "ttlSeconds": 300, "autoStop": False}


def test_if_match_explicit_key_and_path_escaping(make_client: Any) -> None:
    client, seen = make_client(lambda r: httpx.Response(202, json={"operation": op_json()}))
    client.stop_vm("a/b", if_match=7, idempotency_key="k-1", wait=False)
    client.delete_vm("web", wait=False)
    stop, delete = seen
    assert stop.url.raw_path == b"/api/v1/vms/a%2Fb/stop"
    assert stop.headers["If-Match"] == "7"
    assert stop.headers["Idempotency-Key"] == "k-1"
    assert json.loads(stop.content) == {}
    assert delete.method == "DELETE" and delete.content == b""
    assert "Idempotency-Key" in delete.headers


def test_retries_only_unreached_requests_with_same_key(make_client: Any) -> None:
    calls = iter([refused(), refused(), None])

    def handler(request: httpx.Request) -> httpx.Response:
        exc = next(calls)
        if exc is not None:
            raise exc
        return httpx.Response(202, json={"operation": op_json()})

    client, seen = make_client(handler)
    client.fork_vm("web", "copy", wait=False)
    assert len(seen) == 3
    assert len({r.headers["Idempotency-Key"] for r in seen}) == 1
    assert len({r.headers["X-Request-Id"] for r in seen}) == 1


def test_gives_up_after_three_attempts(make_client: Any) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        raise refused()

    client, seen = make_client(handler)
    with pytest.raises(TransportError) as info:
        client.get_capabilities()
    assert len(seen) == 3
    assert info.value.status is None and info.value.retryable


@pytest.mark.parametrize(
    "exc", [httpx.ReadError("reset"), httpx.RemoteProtocolError("eof"), httpx.ConnectError("dns")]
)
def test_no_retry_after_possible_delivery(make_client: Any, exc: Exception) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        raise exc

    client, seen = make_client(handler)
    with pytest.raises(TransportError):
        client.create_vm("web", wait=False)
    assert len(seen) == 1


def test_missing_socket_is_transport_error(tmp_path: Path) -> None:
    with Client(socket=str(tmp_path / "missing.sock")) as client, pytest.raises(TransportError):
        client.get_capabilities()


@pytest.mark.parametrize("code", sorted(ERROR_CLASSES))
def test_http_errors_map_to_classes(make_client: Any, code: str) -> None:
    body = err_body(code)
    body["error"].update(details={"field": "x"}, operationId="op_9")
    client, _ = make_client(lambda r: httpx.Response(409, json=body))
    with pytest.raises(DavitError) as info:
        client.get_vm("web")
    err = info.value
    assert type(err) is ERROR_CLASSES[code]
    assert (err.status, err.code, err.message) == (409, code, "nope")
    assert err.request_id == "req_srv" and err.operation_id == "op_9"
    assert err.details == {"field": "x"}


def test_conflict_family_and_unknown_codes(make_client: Any) -> None:
    assert issubclass(StopRequired, Conflict) and issubclass(OperationInProgress, Conflict)
    assert not issubclass(RevisionMismatch, Conflict)
    client, _ = make_client(lambda r: httpx.Response(418, json=err_body("brand_new")))
    with pytest.raises(DavitError) as info:
        client.get_vm("web")
    assert type(info.value) is DavitError and info.value.code == "brand_new"


def test_non_json_error_is_internal(make_client: Any) -> None:
    client, _ = make_client(
        lambda r: httpx.Response(502, text="bad gateway", headers={"X-Request-Id": "req_h"})
    )
    with pytest.raises(Internal) as info:
        client.get_capabilities()
    assert info.value.status == 502 and info.value.request_id == "req_h"


def events_server(pages: list[dict[str, Any]], tail: dict[str, Any] | None = None) -> Handler:
    remaining = list(pages)

    def handler(request: httpx.Request) -> httpx.Response:
        path = request.url.path
        if request.method != "GET":
            return httpx.Response(202, json={"operation": op_json("queued")})
        if path.endswith("/events"):
            page = remaining.pop(0) if remaining else tail
            assert page is not None, "unexpected extra poll"
            return httpx.Response(200, json=page)
        if path.startswith("/api/v1/vms/"):
            return httpx.Response(200, json=vm_json())
        if path.startswith("/api/v1/snapshots/"):
            return httpx.Response(
                200,
                json={
                    "id": "snap_1",
                    "name": "base",
                    "owner": "o",
                    "sourceVmId": "vm_1",
                    "generation": 1,
                    "imageId": "img_1",
                    "arch": "amd64",
                    "kind": "disk",
                    "consistency": "clean",
                    "sizeBytes": 1,
                    "createdAt": NOW,
                },
            )
        raise AssertionError(f"unexpected {request.method} {path}")

    return handler


def test_wait_follows_events_until_terminal(make_client: Any) -> None:
    done = op_json("succeeded", result={"durationMs": 5, "vmId": "vm_1"})
    pages = [
        {"operation": op_json(), "items": [event(1)], "nextCursor": "1"},
        {"operation": done, "items": [event(2, "succeeded")], "nextCursor": "2"},
        {"operation": done, "items": [], "nextCursor": "2"},
    ]
    client, seen = make_client(events_server(pages))
    progress: list[int] = []
    vm = client.create_vm("web", on_progress=lambda ev: progress.append(ev.seq))
    assert isinstance(vm, VM) and vm.id == "vm_1"
    assert progress == [1, 2]
    polls = [r for r in seen if r.url.path.endswith("/events")]
    assert [r.url.params["after"] for r in polls] == ["0", "1", "2"]
    assert all(0 < int(r.url.params["waitMs"]) <= 25_000 for r in polls)
    assert seen[-1].url.path == "/api/v1/vms/vm_1"


def test_recover_vm_posts_keyed_and_waits(make_client: Any) -> None:
    client, seen = make_client(
        lambda r: httpx.Response(202, json={"operation": op_json(kind="recover")})
    )
    acc = client.recover_vm("a/b", if_match=4, idempotency_key="k-r", wait=False)
    assert isinstance(acc, OperationAccepted) and acc.operation.kind == "recover"
    (req,) = seen
    assert (req.method, req.url.raw_path) == ("POST", b"/api/v1/vms/a%2Fb/recover")
    assert req.headers["Idempotency-Key"] == "k-r" and req.headers["If-Match"] == "4"
    assert json.loads(req.content) == {}

    done = op_json(
        "succeeded",
        kind="recover",
        result={"durationMs": 5, "vmId": "vm_1", "bootMode": "cold_boot"},
    )
    tail = {"operation": done, "items": []}
    client, seen = make_client(
        events_server([{"operation": done, "items": [event(1, "succeeded")]}], tail)
    )
    progress: list[int] = []
    vm = client.recover_vm("web", on_progress=lambda ev: progress.append(ev.seq))
    assert isinstance(vm, VM) and vm.id == "vm_1" and progress == [1]
    assert uuid.UUID(seen[0].headers["Idempotency-Key"]).version == 4
    assert seen[0].url.path == "/api/v1/vms/web/recover" and seen[-1].url.path == "/api/v1/vms/vm_1"


def test_create_snapshot_returns_snapshot(make_client: Any) -> None:
    done = op_json("succeeded", kind="snapshot", result={"durationMs": 0, "snapshotId": "snap_1"})
    client, seen = make_client(events_server([{"operation": done, "items": []}]))
    snap = client.create_snapshot("web", "base")
    assert isinstance(snap, Snapshot) and snap.name == "base"
    assert json.loads(seen[0].content) == {"name": "base"}
    assert seen[-1].url.path == "/api/v1/snapshots/snap_1"


def test_wait_failed_raises_typed_operation_failed(make_client: Any) -> None:
    failed = op_json(
        "failed", error={"code": "resource_exhausted", "message": "full", "retryable": True}
    )
    client, _ = make_client(events_server([{"operation": failed, "items": []}]))
    with pytest.raises(OperationFailed) as info:
        client.wait("op_1")
    err = info.value
    assert isinstance(err.error, ResourceExhausted)
    assert err.code == "resource_exhausted" and err.retryable
    assert err.operation.id == "op_1" and err.operation_id == "op_1"


def test_wait_timeout_carries_operation_id(make_client: Any) -> None:
    running = {"operation": op_json(), "items": []}
    client, _ = make_client(events_server([], tail=running))
    with pytest.raises(WaitTimeout) as info:
        client.wait("op_1", timeout=0.05)
    assert info.value.operation_id == "op_1"
    assert info.value.code == "timeout" and info.value.status is None


def test_wait_ready(make_client: Any) -> None:
    done = op_json("succeeded", result={"durationMs": 1})
    client, seen = make_client(events_server([{"operation": done, "items": []}]))
    vm = client.wait_ready(VM.from_dict(vm_json(state="provisioning", activeOperationId="op_1")))
    assert vm.state == "ready"
    assert [r.url.path for r in seen] == ["/api/v1/operations/op_1/events", "/api/v1/vms/vm_1"]

    broken = vm_json(
        state="error", lastError={"code": "not_found", "message": "gone", "retryable": False}
    )
    client, _ = make_client(lambda r: httpx.Response(200, json=broken))
    with pytest.raises(NotFound, match="gone"):
        client.wait_ready("web")


def test_list_and_iterate_follow_cursor(make_client: Any) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        cursor = request.url.params.get("cursor")
        if cursor is None:
            return httpx.Response(200, json={"items": [vm_json(id="vm_1")], "nextCursor": "c1"})
        return httpx.Response(200, json={"items": [vm_json(id="vm_2")]})

    client, seen = make_client(handler)
    ids = [vm.id for vm in client.iter_vms(states=["ready", "stopped"], limit=1)]
    assert ids == ["vm_1", "vm_2"]
    assert seen[0].url.params.get_list("state") == ["ready", "stopped"]
    assert seen[0].url.params["limit"] == "1"
    assert seen[1].url.params["cursor"] == "c1"
    client.list_vms(all=True)
    assert seen[-1].url.params["all"] == "true"
    client.list_vms(labels={"davit.box": "proj"})
    assert seen[-1].url.params.get_list("label") == ["davit.box=proj"]


def test_create_vm_labels_and_idle_timeout(make_client: Any) -> None:
    client, seen = make_client(
        lambda r: httpx.Response(202, json={"operation": op_json("succeeded"), "vm": vm_json()})
    )
    client.create_vm(
        "proj", labels={"davit.box": "proj"}, idle_timeout=timedelta(minutes=15), wait=False
    )
    body = json.loads(seen[0].read())
    assert body["labels"] == {"davit.box": "proj"} and body["idleTimeoutSeconds"] == 900


def test_set_vm_egress_is_a_synchronous_patch(make_client: Any) -> None:
    egress = {"mode": "allowlist", "allow": ["github.com"]}
    client, seen = make_client(lambda r: httpx.Response(200, json=vm_json(egress=egress)))
    vm = client.set_vm_egress("a/b", "allowlist", allow=("github.com",))
    assert vm.egress.mode == "allowlist" and vm.egress.allow == ["github.com"]
    client.set_vm_egress("web", "off")
    put, off = seen
    assert (put.method, put.url.raw_path) == ("PATCH", b"/api/v1/vms/a%2Fb")
    assert json.loads(put.content) == {"egress": egress}
    assert json.loads(off.content) == {"egress": {"mode": "off"}}
    assert "Idempotency-Key" not in put.headers and "If-Match" not in put.headers


def test_delete_snapshot_is_synchronous(make_client: Any) -> None:
    client, seen = make_client(lambda r: httpx.Response(204))
    assert client.delete_snapshot("base") is None
    assert seen[0].method == "DELETE" and seen[0].url.path == "/api/v1/snapshots/base"
    assert "Idempotency-Key" not in seen[0].headers


@pytest.mark.parametrize(
    "url",
    ["http://example.com", "http://10.0.0.1:8080", "ftp://localhost", "localhost:8080"],
)
def test_rejects_unsafe_or_invalid_base_urls(url: str) -> None:
    with pytest.raises(ValueError, match=r"base_url|plain http"):
        Client(base_url=url, token="t")


@pytest.mark.parametrize(
    ("url", "base"),
    [
        ("https://davit-host", "https://davit-host/api/v1"),
        ("https://h.ts.net:8443/", "https://h.ts.net:8443/api/v1"),
        ("http://127.0.0.1:8080", "http://127.0.0.1:8080/api/v1"),
        ("http://localhost", "http://localhost/api/v1"),
        ("http://[::1]:9", "http://[::1]:9/api/v1"),
    ],
)
def test_accepts_https_and_loopback_http(url: str, base: str) -> None:
    with Client(base_url=url, token="t") as client:
        assert client.base_url == base and client.socket is None


def test_construction_errors() -> None:
    with pytest.raises(ValueError, match="not both"):
        Client(socket="/tmp/s.sock", base_url="https://h")
    with pytest.raises(ValueError, match="absolute"):
        Client(socket="rel.sock")


def test_endpoint_from_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("DAVIT_API_URL", "unix:///run/x/davit.sock")
    with Client() as client:
        assert client.socket == "/run/x/davit.sock"
        assert client.base_url == "http://local/api/v1"
    monkeypatch.setenv("DAVIT_API_URL", "https://davit-host")
    with Client() as client:
        assert client.socket is None and client.base_url == "https://davit-host/api/v1"
    monkeypatch.delenv("DAVIT_API_URL")
    monkeypatch.setenv("XDG_RUNTIME_DIR", "/run/user/1000")
    with Client() as client:
        assert client.socket == "/run/user/1000/davit/davit.sock"


@pytest.mark.parametrize(
    ("env", "platform", "expected"),
    [
        ({"XDG_RUNTIME_DIR": "/run/user/1"}, "linux", "/run/user/1/davit/davit.sock"),
        (
            {"XDG_RUNTIME_DIR": "rel", "XDG_DATA_HOME": "/d"},
            "linux",
            "/d/davit/davit.sock",
        ),
        ({}, "linux", "/h/.local/share/davit/davit.sock"),
        (
            {"XDG_RUNTIME_DIR": "/run/user/1"},
            "darwin",
            "/h/Library/Application Support/davit/davit.sock",
        ),
    ],
)
def test_default_socket(env: dict[str, str], platform: str, expected: str) -> None:
    assert default_socket(env, platform, home="/h") == expected


def test_malformed_success_body_is_internal(make_client: Any) -> None:
    client, _ = make_client(lambda r: httpx.Response(200, json={"id": "vm_1"}))
    with pytest.raises(Internal, match="decoding response"):
        client.get_vm("web")


def test_issue_ssh_credential_mapping(make_client: Any) -> None:
    cred = {
        "certificate": "ssh-ed25519-cert-v01@openssh.com AAAA",
        "guestUser": "root",
        "generation": 2,
        "hostKeys": ["ssh-ed25519 AAAAhost"],
        "sshTarget": "web.ts.net:22",
        "expiresAt": NOW,
    }
    client, seen = make_client(lambda _: httpx.Response(201, json=cred))
    got = client.issue_ssh_credential(
        "web", "ssh-ed25519 AAAAkey", guest_user="root", ttl_seconds=60
    )
    (req,) = seen
    assert (req.method, req.url.path) == ("POST", "/api/v1/vms/web/ssh-credentials")
    assert req.headers["x-request-id"].startswith("req_")
    assert "idempotency-key" not in req.headers
    body = {"publicKey": "ssh-ed25519 AAAAkey", "guestUser": "root", "ttlSeconds": 60}
    assert json.loads(req.content) == body
    assert got.certificate == cred["certificate"] and got.host_keys == cred["hostKeys"]
    assert (got.guest_user, got.generation, got.ssh_target) == ("root", 2, "web.ts.net:22")

    client.issue_ssh_credential("web", "ssh-ed25519 AAAAkey")
    assert json.loads(seen[1].content) == {"publicKey": "ssh-ed25519 AAAAkey"}
    with pytest.raises(ValueError, match="ttl_seconds"):
        client.issue_ssh_credential("web", "k", ttl_seconds=601)


def test_update_vm_is_patch_without_idempotency_key(make_client: Any) -> None:
    client, seen = make_client(lambda r: httpx.Response(200, json=vm_json(name="new")))
    vm = client.update_vm("web", name="new", ttl=timedelta(hours=2), auto_stop=True, if_match=3)
    assert vm.name == "new"
    client.update_vm("web")
    patch, empty = seen
    assert (patch.method, patch.url.path) == ("PATCH", "/api/v1/vms/web")
    assert json.loads(patch.content) == {"name": "new", "ttlSeconds": 7200, "autoStop": True}
    assert patch.headers["If-Match"] == "3"
    assert "Idempotency-Key" not in patch.headers
    assert json.loads(empty.content) == {}
    assert "If-Match" not in empty.headers


def test_extend_and_rename_mirror_the_cli(make_client: Any) -> None:
    client, seen = make_client(lambda r: httpx.Response(200, json=vm_json()))
    client.extend_vm("web")
    client.extend_vm("web", 1800, if_match=7)
    client.rename_vm("web", "web-2")
    bodies = [json.loads(r.content) for r in seen]
    assert bodies == [
        {"autoStop": True},
        {"ttlSeconds": 1800, "autoStop": True},
        {"name": "web-2"},
    ]
    assert seen[1].headers["If-Match"] == "7"
    for bad, exc in ((0, ValueError), (-5, ValueError), (True, TypeError), ("1h", TypeError)):
        with pytest.raises(exc):
            client.extend_vm("web", bad)
    assert len(seen) == 3


def test_create_and_fork_pass_type(make_client: Any) -> None:
    client, seen = make_client(lambda r: httpx.Response(202, json={"operation": op_json()}))
    client.create_vm("web", type="big", wait=False)
    client.fork_vm("web", "copy", type="big", wait=False)
    client.create_vm("plain", wait=False)
    assert json.loads(seen[0].content) == {"name": "web", "type": "big"}
    assert json.loads(seen[1].content) == {"name": "copy", "type": "big"}
    assert "type" not in json.loads(seen[2].content)


def batch_json() -> dict[str, Any]:
    accepted = {"operation": op_json("queued", id="op_1"), "vm": vm_json(name="web-1")}
    refused = {"code": "resource_exhausted", "message": "full", "retryable": True, "requestId": "r"}
    return {
        "items": [
            {"index": 1, "name": "web-1", "accepted": accepted},
            {"index": 2, "name": "web-2", "error": refused},
        ]
    }


def test_create_vms_sends_one_batch(make_client: Any) -> None:
    client, seen = make_client(lambda r: httpx.Response(200, json=batch_json()))
    res = client.create_vms(2, "web", type="big", ttl=60, idempotency_key="batch-1")
    (req,) = seen
    assert req.method == "POST" and req.url.path == "/api/v1/vms/batch"
    assert req.headers["Idempotency-Key"] == "batch-1"
    assert json.loads(req.content) == {
        "count": 2,
        "template": {"name": "web", "type": "big", "ttlSeconds": 60},
    }
    first, second = res.items
    assert first.index == 1 and first.accepted is not None
    assert first.accepted.operation.id == "op_1"
    assert second.accepted is None and second.error is not None
    assert second.error.code == "resource_exhausted"


def test_create_vms_wait_reports_each_item(make_client: Any) -> None:
    done = op_json("succeeded", result={"durationMs": 5, "vmId": "vm_1"})

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "POST":
            return httpx.Response(200, json=batch_json())
        if request.url.path.endswith("/events"):
            return httpx.Response(200, json={"operation": done, "items": []})
        return httpx.Response(200, json=vm_json(name="web-1"))

    client, seen = make_client(handler)
    res = client.create_vms(2, "web", wait=True)
    first, second = res.items
    assert first.accepted is not None and first.accepted.operation.state == "succeeded"
    assert first.accepted.vm is not None and first.accepted.vm.name == "web-1"
    assert second.error is not None
    paths = [r.url.path for r in seen][1:]
    assert paths == ["/api/v1/operations/op_1/events", "/api/v1/vms/vm_1"]


def test_reclaim_vms_posts_without_a_key(make_client: Any) -> None:
    item = {"vmId": "vm_1", "name": "web", "idleSeconds": 700, "memoryBytes": 1 << 30}
    client, seen = make_client(lambda r: httpx.Response(200, json={"items": [item]}))
    res = client.reclaim_vms(600, labels={"team": "a"}, dry_run=True)
    client.reclaim_vms(timedelta(minutes=5))
    first, second = seen
    assert first.method == "POST" and first.url.path == "/api/v1/vms/reclaim"
    assert "Idempotency-Key" not in first.headers
    assert json.loads(first.content) == {
        "idleSeconds": 600,
        "labels": {"team": "a"},
        "dryRun": True,
    }
    assert json.loads(second.content) == {"idleSeconds": 300}
    (got,) = res.items
    assert (got.vm_id, got.idle_seconds, got.memory_bytes, got.operation_id) == (
        "vm_1",
        700,
        1 << 30,
        None,
    )


def test_capacity_wait_sends_wait_seconds(make_client: Any) -> None:
    client, seen = make_client(lambda r: httpx.Response(202, json={"operation": op_json()}))
    client.create_vm("web", capacity_wait=90, wait=False)
    client.resume_vm("web", capacity_wait=timedelta(minutes=2), wait=False)
    client.create_vm("plain", capacity_wait=0, wait=False)
    client.resume_vm("web", wait=False)
    assert [r.url.params.get("waitSeconds") for r in seen] == ["90", "120", None, None]
    assert seen[0].url.path == "/api/v1/vms" and "waitSeconds" not in json.loads(seen[0].content)
    # The request itself may queue that long: its HTTP timeout grows by the wait.
    assert seen[0].extensions["timeout"]["read"] == client.timeout + 90
    assert seen[2].extensions["timeout"]["read"] == client.timeout
    for bad in (-1, 3601, timedelta(hours=2)):
        with pytest.raises(ValueError, match="capacity_wait"):
            client.create_vm("web", capacity_wait=bad, wait=False)
    with pytest.raises(TypeError):
        client.resume_vm("web", capacity_wait="60", wait=False)  # type: ignore[arg-type]
    assert len(seen) == 4  # rejected before sending


def test_privacy_policy_reaches_create_update_and_batch(make_client: Any) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "PATCH":
            return httpx.Response(200, json=vm_json())
        if request.url.path.endswith("/batch"):
            return httpx.Response(200, json=batch_json())
        return httpx.Response(202, json={"operation": op_json()})

    client, seen = make_client(handler)
    client.create_vm("web", privacy=PrivacyPolicy(ephemeral=True), wait=False)
    client.create_vm("plain", wait=False)
    client.create_vms(2, "b", privacy=PrivacyPolicy(no_process_samples=True))
    client.update_vm("web", privacy=PrivacyPolicy(no_snapshot=True, no_output_retention=True))
    bodies = [json.loads(r.content) for r in seen]
    assert bodies[0] == {"name": "web", "privacy": {"ephemeral": True}}
    assert "privacy" not in bodies[1]
    assert bodies[2] == {
        "count": 2,
        "template": {"name": "b", "privacy": {"noProcessSamples": True}},
    }
    assert bodies[3] == {"privacy": {"noSnapshot": True, "noOutputRetention": True}}


def test_resume_resizes(make_client: Any) -> None:
    client, seen = make_client(lambda r: httpx.Response(202, json={"operation": op_json()}))
    client.resume_vm("web", type="big", wait=False)
    client.resume_vm("web", shape=ShapeOverride(cpus=4, disk_bytes=20 << 30), wait=False)
    client.resume_vm("web", wait=False)
    assert seen[0].url.path.endswith("/vms/web/resume")
    assert json.loads(seen[0].content) == {"type": "big"}
    assert json.loads(seen[1].content) == {"shape": {"cpus": 4, "diskBytes": 20 << 30}}
    assert json.loads(seen[2].content) == {}


def token_json(**over: Any) -> dict[str, Any]:
    tok = {
        "id": "tok_1",
        "name": "ci",
        "scopes": [{"vmId": "vm_1", "actions": ["exec"]}],
        "createdAt": NOW,
        "expiresAt": NOW,
        "useCount": 0,
    }
    return {**tok, **over}


def test_create_token_mapping(make_client: Any) -> None:
    client, seen = make_client(lambda _: httpx.Response(201, json=token_json(secret="slw_s3cret")))
    got = client.create_token("ci", actions=["exec"], vm="web", ttl_seconds=3600)
    (req,) = seen
    assert (req.method, req.url.path) == ("POST", "/api/v1/tokens")
    assert uuid.UUID(req.headers["Idempotency-Key"]).version == 4
    body = {"name": "ci", "scopes": [{"vmId": "web", "actions": ["exec"]}], "ttlSeconds": 3600}
    assert json.loads(req.content) == body
    assert (got.id, got.secret, got.scopes[0].vm_id) == ("tok_1", "slw_s3cret", "vm_1")
    assert "slw_s3cret" not in repr(got) and "slw_s3cret" not in str(got)

    # Several VMs: one scope each; no VM: one unscoped scope; explicit key.
    client.create_token("a", actions=["view"], vm=["web", "db"], ttl_seconds=1)
    assert json.loads(seen[1].content)["scopes"] == [
        {"vmId": "web", "actions": ["view"]},
        {"vmId": "db", "actions": ["view"]},
    ]
    client.create_token("b", actions=["admin"], ttl_seconds=60, idempotency_key="k-1")
    assert json.loads(seen[2].content)["scopes"] == [{"actions": ["admin"]}]
    assert seen[2].headers["Idempotency-Key"] == "k-1"
    scopes = [TokenScope(actions=["view"]), TokenScope(vm_id="web", actions=["exec"])]
    client.create_token("c", scopes=scopes, ttl_seconds=60)
    assert json.loads(seen[3].content)["scopes"] == [
        {"actions": ["view"]},
        {"vmId": "web", "actions": ["exec"]},
    ]

    with pytest.raises(TypeError, match="exactly one"):
        client.create_token("x", ttl_seconds=60)
    with pytest.raises(TypeError, match="exactly one"):
        client.create_token("x", actions=["view"], scopes=scopes, ttl_seconds=60)
    with pytest.raises(TypeError, match="vm="):
        client.create_token("x", scopes=scopes, vm="web", ttl_seconds=60)
    with pytest.raises(TypeError, match="list"):
        client.create_token("x", actions="view", ttl_seconds=60)
    with pytest.raises(ValueError, match="ttl_seconds"):
        client.create_token("x", actions=["view"], ttl_seconds=31_536_001)
    assert len(seen) == 4


def test_list_iter_and_revoke_tokens(make_client: Any) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "DELETE":
            return httpx.Response(204)
        if request.url.params.get("cursor") is None:
            return httpx.Response(200, json={"items": [token_json(id="tok_1")], "nextCursor": "c1"})
        return httpx.Response(200, json={"items": [token_json(id="tok_2")]})

    client, seen = make_client(handler)
    assert [t.id for t in client.iter_tokens(limit=1)] == ["tok_1", "tok_2"]
    assert (seen[0].method, seen[0].url.path) == ("GET", "/api/v1/tokens")
    assert seen[0].url.params["limit"] == "1" and "cursor" not in seen[0].url.params
    assert seen[1].url.params["cursor"] == "c1"
    page = client.list_tokens(cursor="c1")
    assert page.next_cursor is None and page.items[0].last_used_at is None

    assert client.revoke_token("tok_1") is None
    client.revoke_token(page.items[0])
    rev1, rev2 = seen[-2:]
    assert (rev1.method, rev1.url.path) == ("DELETE", "/api/v1/tokens/tok_1")
    assert rev2.url.path == "/api/v1/tokens/tok_2"
    assert "Idempotency-Key" not in rev1.headers and rev1.content == b""


def profile_json(**over: Any) -> dict[str, Any]:
    prof = {
        "id": "cp_1",
        "name": "work",
        "default": False,
        "credentials": [
            {
                "name": "anthropic",
                "kind": "api_key",
                "provider": "anthropic",
                "envVar": "ANTHROPIC_API_KEY",
                "hasSecret": True,
                "updatedAt": NOW,
            }
        ],
        "createdAt": NOW,
        "updatedAt": NOW,
    }
    return {**prof, **over}


def grant_json(**over: Any) -> dict[str, Any]:
    grant = {
        "id": "cg_1",
        "vmId": "vm_1",
        "profileId": "cp_1",
        "credential": "anthropic",
        "state": "active",
        "placeholder": "slwp_placeholder",
        "envVar": "ANTHROPIC_API_KEY",
        "upstream": "https://api.anthropic.com",
        "createdAt": NOW,
    }
    return {**grant, **over}


def test_credential_profiles(make_client: Any) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "DELETE":
            return httpx.Response(204)
        if request.method == "POST":
            return httpx.Response(201, json=profile_json(default=True))
        if request.url.path.endswith("/credential-profiles"):
            if request.url.params.get("cursor") is None:
                return httpx.Response(200, json={"items": [profile_json()], "nextCursor": "c1"})
            return httpx.Response(200, json={"items": [profile_json(id="cp_2")]})
        return httpx.Response(200, json=profile_json())

    client, seen = make_client(handler)
    assert [p.id for p in client.iter_credential_profiles(limit=1)] == ["cp_1", "cp_2"]
    assert (seen[0].method, seen[0].url.path) == ("GET", "/api/v1/credential-profiles")
    assert seen[0].url.params["limit"] == "1" and "cursor" not in seen[0].url.params
    assert seen[1].url.params["cursor"] == "c1"

    created = client.create_credential_profile("work", default=True)
    assert created.default is True
    req = seen[2]
    assert (req.method, req.url.path) == ("POST", "/api/v1/credential-profiles")
    assert uuid.UUID(req.headers["Idempotency-Key"]).version == 4
    assert json.loads(req.content) == {"name": "work", "default": True}
    client.create_credential_profile("plain", idempotency_key="k-1")
    assert seen[3].headers["Idempotency-Key"] == "k-1"
    assert json.loads(seen[3].content) == {"name": "plain"}

    got = client.get_credential_profile("my work/x")
    assert got.credentials[0].kind == CredentialKind.API_KEY
    assert got.credentials[0].has_secret is True
    assert seen[4].method == "GET"
    assert seen[4].url.raw_path == b"/api/v1/credential-profiles/my%20work%2Fx"

    assert client.delete_credential_profile(got) is None
    client.delete_credential_profile("a/b")
    d1, d2 = seen[5:7]
    assert (d1.method, d1.url.path) == ("DELETE", "/api/v1/credential-profiles/cp_1")
    assert d2.url.raw_path == b"/api/v1/credential-profiles/a%2Fb"
    assert "Idempotency-Key" not in d1.headers and d1.content == b""
    for r in seen[:2] + seen[4:]:
        assert "Idempotency-Key" not in r.headers
    with pytest.raises(ValueError, match="profile"):
        client.get_credential_profile("")
    assert len(seen) == 7


def test_put_and_delete_credential(make_client: Any) -> None:
    secret = "sk-ant-s3cret"

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "DELETE":
            return httpx.Response(204)
        return httpx.Response(200, json=profile_json())

    client, seen = make_client(handler)
    prof = client.put_credential(
        "work",
        "anthropic",
        kind="api_key",
        env_var="ANTHROPIC_API_KEY",
        secret=secret,
        provider="anthropic",
    )
    assert prof.id == "cp_1"
    put = seen[0]
    assert put.method == "PUT"
    assert put.url.raw_path == b"/api/v1/credential-profiles/work/credentials/anthropic"
    assert json.loads(put.content) == {
        "kind": "api_key",
        "provider": "anthropic",
        "envVar": "ANTHROPIC_API_KEY",
        "secret": secret,
    }
    assert "Idempotency-Key" not in put.headers
    assert secret not in repr(prof) and secret not in str(prof)

    # Custom upstream, escaped names; an omitted secret is sent empty (keeps the stored one).
    client.put_credential(
        prof,
        "my api/1",
        kind=CredentialKind.CUSTOM_API,
        env_var="X_KEY",
        base_url="https://api.example.com",
    )
    assert seen[1].url.raw_path == b"/api/v1/credential-profiles/cp_1/credentials/my%20api%2F1"
    assert json.loads(seen[1].content) == {
        "kind": "custom_api",
        "baseUrl": "https://api.example.com",
        "envVar": "X_KEY",
        "secret": "",
    }

    req = PutCredentialRequest(kind=CredentialKind.API_KEY, env_var="K", secret=secret)
    assert secret not in repr(req) and secret not in str(req)

    assert client.delete_credential("work", "a/b") is None
    rm = seen[2]
    assert rm.method == "DELETE" and rm.content == b""
    assert rm.url.raw_path == b"/api/v1/credential-profiles/work/credentials/a%2Fb"
    assert "Idempotency-Key" not in rm.headers


def test_put_credential_error_does_not_echo_secret(make_client: Any) -> None:
    secret = "sk-ant-s3cret"
    client, _ = make_client(lambda _: httpx.Response(400, json=err_body("invalid_argument")))
    with pytest.raises(DavitError) as info:
        client.put_credential("work", "c", kind="api_key", env_var="K", secret=secret)
    assert secret not in str(info.value) and secret not in repr(info.value)


def test_credential_grants(make_client: Any) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "DELETE":
            return httpx.Response(
                200, json=grant_json(state="revoked", revokeReason="revoked", revokedAt=NOW)
            )
        if request.method == "POST":
            return httpx.Response(201, json=grant_json())
        if "/credential-profiles/" in request.url.path:
            return httpx.Response(200, json=profile_json())
        return httpx.Response(200, json={"items": [grant_json()]})

    client, seen = make_client(handler)
    page = client.list_credential_grants("web", limit=10, cursor="c1")
    assert page.items[0].state == GrantState.ACTIVE and page.next_cursor is None
    ls = seen[0]
    assert (ls.method, ls.url.path) == ("GET", "/api/v1/vms/web/credential-grants")
    assert ls.url.params["limit"] == "10" and ls.url.params["cursor"] == "c1"

    grant = client.create_credential_grant("a/b", "work", "anthropic")
    assert grant.placeholder == "slwp_placeholder"
    mk = seen[1]
    assert (mk.method, mk.url.raw_path) == ("POST", b"/api/v1/vms/a%2Fb/credential-grants")
    assert uuid.UUID(mk.headers["Idempotency-Key"]).version == 4
    assert json.loads(mk.content) == {"profile": "work", "credential": "anthropic"}
    client.create_credential_grant(
        "web", client.get_credential_profile("work"), "anthropic", idempotency_key="k-2"
    )
    assert seen[3].headers["Idempotency-Key"] == "k-2"
    assert json.loads(seen[3].content) == {"profile": "cp_1", "credential": "anthropic"}

    revoked = client.revoke_credential_grant("web", grant)
    assert revoked.state == GrantState.REVOKED and revoked.revoked_at is not None
    client.revoke_credential_grant("web", "g/1")
    r1, r2 = seen[4:6]
    assert (r1.method, r1.url.path) == ("DELETE", "/api/v1/vms/web/credential-grants/cg_1")
    assert r2.url.raw_path == b"/api/v1/vms/web/credential-grants/g%2F1"
    assert "Idempotency-Key" not in r1.headers and r1.content == b""
    assert "Idempotency-Key" not in ls.headers


def env_json(**over: Any) -> dict[str, Any]:
    env = {"id": "env_1", "name": "dev", "latestRevision": 1, "createdAt": NOW, "updatedAt": NOW}
    return {**env, **over}


MANIFEST = {
    "repos": [{"url": "https://example.com/r.git", "path": "/work/r"}],
    "variables": {"A": "1"},
    "secretFiles": [],
    "setup": [{"name": "deps", "argv": ["make", "deps"]}],
}


def rev_json(revision: int = 1, **over: Any) -> dict[str, Any]:
    rev = {"environmentId": "env_1", "revision": revision, "manifest": MANIFEST, "createdAt": NOW}
    return {**rev, **over}


def test_environment_calls(make_client: Any) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        path, method = request.url.path, request.method
        if method == "DELETE":
            return httpx.Response(204)
        if path.endswith("/revisions") and method == "POST":
            return httpx.Response(201, json=rev_json(2))
        if path.endswith("/revisions"):
            if request.url.params.get("cursor") is None:
                return httpx.Response(200, json={"items": [rev_json(1)], "nextCursor": "c1"})
            return httpx.Response(200, json={"items": [rev_json(2)]})
        if "/revisions/" in path:
            return httpx.Response(200, json=rev_json(int(path.rsplit("/", 1)[1])))
        if path == "/api/v1/environments" and method == "POST":
            return httpx.Response(201, json=env_json())
        if path == "/api/v1/environments":
            if request.url.params.get("cursor") is None:
                return httpx.Response(200, json={"items": [env_json()], "nextCursor": "c1"})
            return httpx.Response(200, json={"items": [env_json(id="env_2", name="ci")]})
        return httpx.Response(200, json=env_json())

    client, seen = make_client(handler)
    env = client.create_environment("dev", MANIFEST)
    (create,) = seen
    assert (create.method, create.url.path) == ("POST", "/api/v1/environments")
    assert uuid.UUID(create.headers["Idempotency-Key"]).version == 4
    assert "If-Match" not in create.headers
    assert json.loads(create.content) == {"name": "dev", "manifest": MANIFEST}
    assert (env.id, env.latest_revision) == ("env_1", 1)

    # A model manifest serializes the same; an explicit key is sent as given.
    model = EnvManifest.from_dict(MANIFEST)
    client.create_environment("dev", model, idempotency_key="k-1")
    assert seen[-1].headers["Idempotency-Key"] == "k-1"
    assert json.loads(seen[-1].content)["manifest"] == MANIFEST
    # Omitted collections are empty; unknown keys are refused before sending.
    client.create_environment("bare", {"setup": []})
    assert json.loads(seen[-1].content)["manifest"] == {
        "repos": [],
        "variables": {},
        "secretFiles": [],
        "setup": [],
    }
    n = len(seen)
    with pytest.raises(ValueError, match="secret_files"):
        client.create_environment("bad", {"secret_files": []})
    with pytest.raises(TypeError):
        client.create_environment("bad", ["not", "a", "manifest"])  # type: ignore[arg-type]
    assert len(seen) == n

    assert [e.name for e in client.iter_environments(limit=1)] == ["dev", "ci"]
    first, second = seen[-2:]
    assert (first.method, first.url.path) == ("GET", "/api/v1/environments")
    assert first.url.params["limit"] == "1" and second.url.params["cursor"] == "c1"

    assert client.get_environment("a/b").id == "env_1"
    assert seen[-1].url.raw_path == b"/api/v1/environments/a%2Fb"

    rev = client.create_environment_revision("dev", MANIFEST, if_match=1)
    post = seen[-1]
    assert (post.method, post.url.path) == ("POST", "/api/v1/environments/dev/revisions")
    assert post.headers["If-Match"] == "1"
    assert uuid.UUID(post.headers["Idempotency-Key"]).version == 4
    assert json.loads(post.content) == {"manifest": MANIFEST}
    assert rev.revision == 2 and rev.manifest.setup[0].argv == ["make", "deps"]
    client.create_environment_revision("dev", model, idempotency_key="k-2")
    assert "If-Match" not in seen[-1].headers and seen[-1].headers["Idempotency-Key"] == "k-2"

    assert [r.revision for r in client.iter_environment_revisions("dev", limit=1)] == [1, 2]
    assert seen[-2].url.path == "/api/v1/environments/dev/revisions"
    assert seen[-1].url.params["cursor"] == "c1"

    assert client.get_environment_revision("dev", 2).revision == 2
    assert (seen[-1].method, seen[-1].url.path) == ("GET", "/api/v1/environments/dev/revisions/2")
    for bad in (0, -1, True, "2"):
        with pytest.raises(ValueError, match="revision"):
            client.get_environment_revision("dev", bad)  # type: ignore[arg-type]

    assert client.delete_environment(env) is None
    client.delete_environment("ci")
    d1, d2 = seen[-2:]
    assert (d1.method, d1.url.path) == ("DELETE", "/api/v1/environments/env_1")
    assert d2.url.path == "/api/v1/environments/ci"
    assert "Idempotency-Key" not in d1.headers and d1.content == b""


def test_stale_environment_if_match_is_revision_mismatch(make_client: Any) -> None:
    body = err_body("revision_mismatch")
    body["error"]["details"] = {"latestRevision": 3}
    client, _ = make_client(lambda r: httpx.Response(412, json=body))
    with pytest.raises(RevisionMismatch) as info:
        client.create_environment_revision("dev", MANIFEST, if_match=1)
    assert info.value.details == {"latestRevision": 3}


@pytest.mark.parametrize(
    ("spec", "expected"),
    [
        ("dev", {"environment": "dev"}),
        ("dev@3", {"environment": "dev", "revision": 3}),
        ("env_01J@12", {"environment": "env_01J", "revision": 12}),
        (EnvironmentRef(environment="dev", revision=0), {"environment": "dev", "revision": 0}),
    ],
)
def test_parse_environment_ref(spec: Any, expected: dict[str, Any]) -> None:
    assert parse_environment_ref(spec).to_dict() == expected


@pytest.mark.parametrize("bad", ["", "@3", "dev@", "dev@0", "dev@-1", "dev@x", "dev@1@2", "dev@٣"])
def test_parse_environment_ref_rejects(bad: str) -> None:
    with pytest.raises(ValueError, match="NAME@REVISION"):
        parse_environment_ref(bad)


def test_environment_option_on_create_fork_resume(make_client: Any) -> None:
    client, seen = make_client(lambda r: httpx.Response(202, json={"operation": op_json()}))
    client.create_vm("web", environment="dev@2", wait=False)
    client.fork_vm("web", "copy", environment="dev", wait=False)
    client.resume_vm("web", environment=EnvironmentRef(environment="env_1"), wait=False)
    client.create_vm("plain", wait=False)
    assert json.loads(seen[0].content) == {
        "name": "web",
        "environment": {"environment": "dev", "revision": 2},
    }
    assert json.loads(seen[1].content) == {"name": "copy", "environment": {"environment": "dev"}}
    assert json.loads(seen[2].content) == {"environment": {"environment": "env_1"}}
    assert "environment" not in json.loads(seen[3].content)
    with pytest.raises(ValueError, match="REVISION"):
        client.create_vm("bad", environment="dev@zero", wait=False)
    assert len(seen) == 4


def history_entry(pid: str) -> dict[str, Any]:
    proc = {
        "id": pid,
        "vmId": "vm_1",
        "generation": 1,
        "argv": ["sh"],
        "status": "exited",
        "exitCode": 0,
        "detached": True,
        "outputCursor": 1,
        "createdAt": NOW,
    }
    return {
        "process": proc,
        "handle": pid,
        "stdoutBytes": 3,
        "stderrBytes": 0,
        "outputDropped": False,
    }


def test_process_history_pages(make_client: Any) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.params.get("cursor") is None:
            return httpx.Response(200, json={"items": [history_entry("p2")], "nextCursor": "c1"})
        return httpx.Response(200, json={"items": [history_entry("p1")]})

    client, seen = make_client(handler)
    entries = list(client.iter_process_history("a/b", limit=1))
    assert [e.process.id for e in entries] == ["p2", "p1"]
    assert entries[0].stdout_bytes == 3 and entries[0].output_dropped is False
    assert entries[0].output_not_retained is None
    assert (seen[0].method, seen[0].url.raw_path) == ("GET", b"/api/v1/vms/a%2Fb/history?limit=1")
    assert seen[1].url.params["cursor"] == "c1"


def test_process_history_output_not_retained(make_client: Any) -> None:
    withheld = {**history_entry("p1"), "stdoutBytes": 0, "outputNotRetained": "policy"}
    client, _ = make_client(lambda r: httpx.Response(200, json={"items": [withheld]}))
    (entry,) = client.iter_process_history("web")
    assert entry.output_not_retained == OutputNotRetainedReason.POLICY
    assert entry.output_dropped is False  # a policy is not truncation


def test_list_guest_processes(make_client: Any) -> None:
    sample = {
        "sampledAt": NOW,
        "stale": True,
        "truncated": False,
        "items": [
            {
                "pid": 42,
                "ppid": 1,
                "user": "davit",
                "args": ["sleep", "9"],
                "state": "S",
                "cpuTimeMillis": 5,
                "rssBytes": 4096,
                "startedAt": NOW,
                "processId": "proc_1",
            }
        ],
    }
    client, seen = make_client(lambda request: httpx.Response(200, json=sample))
    sample_list = client.list_guest_processes("a/b")
    assert (
        sample_list.stale
        and sample_list.items[0].process_id == "proc_1"
        and sample_list.items[0].args == ["sleep", "9"]
    )
    assert (seen[0].method, seen[0].url.raw_path) == ("GET", b"/api/v1/vms/a%2Fb/ps")


def b64(data: bytes) -> str:
    return base64.b64encode(data).decode()


def test_read_process_output_pages_and_decodes(make_client: Any) -> None:
    pages = {
        None: {
            "items": [
                {"type": "stdout", "seq": 1, "data": b64(b"he")},
                {"type": "stderr", "seq": 2, "data": b64(b"oops\n")},
            ],
            "nextCursor": "2",
            "complete": False,
        },
        "2": {
            "items": [
                {"type": "future", "seq": 3},
                {"type": "stdout", "seq": 4, "data": b64(b"llo\x00\n")},
            ],
            "nextCursor": "4",
            "complete": True,
        },
    }
    client, seen = make_client(lambda r: httpx.Response(200, json=pages[r.url.params.get("after")]))
    page = client.read_process_output("web", "p1", stream="both", limit=2)
    assert page.next_cursor == "2" and not page.complete and page.items[0].data == b64(b"he")
    req = seen[0]
    assert (req.method, req.url.path) == ("GET", "/api/v1/vms/web/processes/p1/output")
    assert dict(req.url.params) == {"stream": "both", "limit": "2"}

    events = list(client.iter_process_output("web", "p1"))
    assert [(e.type, e.seq, e.data) for e in events] == [
        ("stdout", 1, b"he"),
        ("stderr", 2, b"oops\n"),
        ("stdout", 4, b"llo\x00\n"),
    ]
    assert [r.url.params.get("after") for r in seen[1:]] == [None, "2"]

    out = client.collect_process_output("web", "p1")
    assert out == RetainedOutput(b"hello\x00\n", b"oops\n", True, "4")
    client.read_process_output("web", "p1", after=2)
    assert seen[-1].url.params["after"] == "2"


def test_collect_process_output_stops_without_new_chunks(make_client: Any) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.params.get("after") is None:
            item = {"type": "stdout", "seq": 5, "data": b64(b"x")}
            return httpx.Response(200, json={"items": [item], "nextCursor": "5", "complete": False})
        # Still running and nothing new: no nextCursor, not complete.
        return httpx.Response(200, json={"items": [], "complete": False})

    client, seen = make_client(handler)
    out = client.collect_process_output("web", "p1", stream="stdout")
    assert out == RetainedOutput(b"x", b"", False, "5")
    assert [r.url.params.get("after") for r in seen] == [None, "5"]
    assert all(r.url.params["stream"] == "stdout" for r in seen)
    # Resuming from the cursor with nothing new keeps the cursor.
    assert client.collect_process_output("web", "p1", after="5").cursor == "5"


def test_malformed_output_data_is_internal(make_client: Any) -> None:
    page = {"items": [{"type": "stdout", "seq": 1, "data": "not base64!"}], "complete": True}
    client, _ = make_client(lambda r: httpx.Response(200, json=page))
    with pytest.raises(Internal, match="base64"):
        client.collect_process_output("web", "p1")


def run_json(state: str = "creating", **over: Any) -> dict[str, Any]:
    return {"id": "run_1", "state": state, "keep": False, "createdAt": NOW, **over}


def test_run_calls(make_client: Any) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "POST" and request.url.path == "/api/v1/runs":
            return httpx.Response(201, json=run_json())
        if request.url.path.endswith("/start"):
            return httpx.Response(200, json=run_json("running", vmId="vm_1"))
        return httpx.Response(200, json=run_json("failed", exitCode=3, endedAt=NOW))

    client, seen = make_client(handler)
    run = client.create_run(
        ["sh", "-c", "exit 3"],
        image="base",
        environment="dev@2",
        cwd="/w",
        env={"A": "1"},
        timeout_seconds=30,
        keep=True,
        hold=True,
    )
    assert (run.id, run.state) == ("run_1", "creating")
    (create,) = seen
    assert (create.method, create.url.path) == ("POST", "/api/v1/runs")
    assert uuid.UUID(create.headers["Idempotency-Key"]).version == 4
    assert json.loads(create.content) == {
        "image": "base",
        "environment": {"environment": "dev", "revision": 2},
        "argv": ["sh", "-c", "exit 3"],
        "cwd": "/w",
        "env": {"A": "1"},
        "timeoutSeconds": 30,
        "keep": True,
        "hold": True,
    }
    client.create_run(["true"], snapshot="snap_1", idempotency_key="k-1")
    assert seen[-1].headers["Idempotency-Key"] == "k-1"
    assert json.loads(seen[-1].content) == {"snapshotId": "snap_1", "argv": ["true"]}
    n = len(seen)
    with pytest.raises(TypeError):
        client.create_run("true")  # type: ignore[arg-type]
    with pytest.raises(ValueError, match="timeout_seconds"):
        client.create_run(["true"], timeout_seconds=-1)
    assert len(seen) == n

    assert client.get_run("run_1").exit_code == 3
    assert client.start_run(run).state == "running"
    assert client.cancel_run("a/b").state == "failed"
    get, start, cancel = seen[-3:]
    assert (get.method, get.url.path) == ("GET", "/api/v1/runs/run_1")
    assert (start.method, start.url.path, start.content) == (
        "POST",
        "/api/v1/runs/run_1/start",
        b"",
    )
    assert (cancel.method, cancel.url.raw_path) == ("DELETE", b"/api/v1/runs/a%2Fb")
    for req in (get, start, cancel):
        assert "Idempotency-Key" not in req.headers


def test_wait_run_polls_until_ended(make_client: Any, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("davit.client.RUN_POLL_INTERVAL", 0.0)
    states = iter(
        [run_json("running"), run_json("running", exitCode=0), run_json("succeeded", endedAt=NOW)]
    )
    client, seen = make_client(lambda r: httpx.Response(200, json=next(states)))
    done = client.wait_run("run_1")
    assert done.state == "succeeded" and done.ended_at is not None
    assert len(seen) == 3 and all(r.url.path == "/api/v1/runs/run_1" for r in seen)

    client2, _ = make_client(lambda r: httpx.Response(200, json=run_json("running")))
    with pytest.raises(WaitTimeout) as info:
        client2.wait_run("run_1", timeout=0)
    assert info.value.details == {"runId": "run_1"}


def push_json(**over: Any) -> dict[str, Any]:
    res = {"path": "/w", "files": 1, "bytes": 1024, "removed": 0, "sha256": "ab", "generation": 1}
    return {**res, **over}


def tiny_tar() -> bytes:
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w") as tar:
        info = tarfile.TarInfo("hello.txt")
        info.size = 3
        tar.addfile(info, io.BytesIO(b"hi\n"))
    return buf.getvalue()


def test_push_tree_request(make_client: Any) -> None:
    client, seen = make_client(lambda r: httpx.Response(200, json=push_json()))
    data = tiny_tar()
    res = client.push_tree("a/b", "/w", data)
    assert (res.files, res.bytes, res.generation) == (1, 1024, 1)
    (req,) = seen
    assert (req.method, req.url.raw_path.split(b"?")[0]) == ("PUT", b"/api/v1/vms/a%2Fb/tree")
    assert list(req.url.params.multi_items()) == [("path", "/w"), ("mode", "merge")]
    assert req.headers["Content-Type"] == "application/x-tar"
    assert "Idempotency-Key" not in req.headers
    assert req.content == data
    # A stream is sent as is; mirror is passed through (the server decides).
    client.push_tree("web", "/w", io.BytesIO(data), mode="mirror")
    assert seen[-1].url.params["mode"] == "mirror" and seen[-1].read() == data
    with pytest.raises(ValueError, match="absolute"):
        client.push_tree("web", "rel", data)
    assert len(seen) == 2


def test_push_tree_mirror_not_implemented_is_typed(make_client: Any) -> None:
    client, _ = make_client(lambda r: httpx.Response(501, json=err_body("not_implemented")))
    with pytest.raises(ServerNotImplemented):
        client.push_tree("web", "/w", tiny_tar(), mode="mirror")


def git(root: Path, *args: str) -> None:
    subprocess.run(["git", "-C", str(root), *args], check=True, capture_output=True)


@pytest.fixture
def hermetic_git(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    """Isolate every git the test runs from the caller's config, excludes and
    repository variables; the test's own gits never look above ``tmp_path``
    (the selector ignores that ceiling)."""
    for k in [k for k in os.environ if k.startswith("GIT_")]:
        monkeypatch.delenv(k)
    home = tmp_path / "home"
    home.mkdir()
    for k, v in {
        "HOME": str(home),
        "XDG_CONFIG_HOME": str(home / ".config"),
        "LC_ALL": "C",
        "GIT_CONFIG_GLOBAL": os.devnull,
        "GIT_CONFIG_NOSYSTEM": "1",
        "GIT_CEILING_DIRECTORIES": str(tmp_path),
        "GIT_AUTHOR_NAME": "t",
        "GIT_AUTHOR_EMAIL": "t@example.com",
        "GIT_COMMITTER_NAME": "t",
        "GIT_COMMITTER_EMAIL": "t@example.com",
        # No automatic maintenance: git detaches it after a commit, and it
        # can still be writing into .git when tmp_path is removed.
        "GIT_CONFIG_COUNT": "2",
        "GIT_CONFIG_KEY_0": "maintenance.auto",
        "GIT_CONFIG_VALUE_0": "false",
        "GIT_CONFIG_KEY_1": "gc.auto",
        "GIT_CONFIG_VALUE_1": "0",
    }.items():
        monkeypatch.setenv(k, v)
    return tmp_path


@pytest.mark.skipif(shutil.which("git") is None, reason="git not installed")
def test_git_selection_and_push_dir(make_client: Any, hermetic_git: Path) -> None:
    tmp_path = hermetic_git
    repo = tmp_path / "repo"
    (repo / "src").mkdir(parents=True)
    git(repo, "init", "-q", "-b", "main")
    (repo / ".gitignore").write_text("*.log\nbuild/\n")
    (repo / "src" / "main.py").write_text("print(1)\n")
    (repo / "tracked-gone.txt").write_text("x")
    (repo / "debug.log").write_text("ignored")
    (repo / "build").mkdir()
    (repo / "build" / "out.bin").write_text("ignored")
    script = repo / "run.sh"
    script.write_text("#!/bin/sh\n")
    script.chmod(0o755)
    (repo / "link").symlink_to("src/main.py")
    git(repo, "add", ".gitignore", "src/main.py", "tracked-gone.txt")
    (repo / "tracked-gone.txt").unlink()
    (repo / "untracked.txt").write_text("u")

    assert sorted(git_selection(repo)) == [
        ".gitignore",
        "link",
        "run.sh",
        "src/main.py",
        "untracked.txt",
    ]
    assert git_selection(repo / "src") == ["main.py"]  # a subdirectory of the work tree
    plain = tmp_path / "plain"
    plain.mkdir()
    probe = subprocess.run(
        ["git", "-C", str(plain), "rev-parse"],
        capture_output=True,
        env={k: v for k, v in os.environ.items() if k != "GIT_CEILING_DIRECTORIES"},
    )
    if probe.returncode != 0:  # else the test's TMPDIR is inside a work tree
        with pytest.raises(ValueError, match="git"):
            git_selection(plain)  # not a work tree

    client, seen = make_client(lambda r: httpx.Response(200, json=push_json(files=5)))
    assert client.push_dir("web", repo).files == 5
    (req,) = seen
    assert req.url.params["path"] == DEFAULT_PUSH_PATH
    with tarfile.open(fileobj=io.BytesIO(req.read())) as tar:
        members = {m.name: m for m in tar.getmembers()}
    assert sorted(members) == [".gitignore", "link", "run.sh", "src/main.py", "untracked.txt"]
    assert members["link"].issym() and members["link"].linkname == "src/main.py"
    assert members["run.sh"].mode & 0o777 == 0o755
    assert all(m.uid == 0 and m.uname == "" for m in members.values())


@pytest.mark.skipif(shutil.which("git") is None, reason="git not installed")
def test_git_selection_ignores_repo_env(
    hermetic_git: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    # Repository-locating variables from the caller (here pointing at another
    # throwaway repository) do not redirect the selection.
    mine, other = hermetic_git / "mine", hermetic_git / "other"
    for repo, rel in ((mine, "src/mine.txt"), (other, "other.txt")):
        (repo / rel).parent.mkdir(parents=True)
        git(repo, "init", "-q", "-b", "main")
        (repo / rel).write_text(rel)
        git(repo, "add", ".")
        git(repo, "commit", "-q", "-m", "init")
    other_git = other / ".git"
    for k, v in {
        "GIT_DIR": other_git,
        "GIT_WORK_TREE": other,
        "GIT_INDEX_FILE": other_git / "index",
        "GIT_OBJECT_DIRECTORY": other_git / "objects",
        "GIT_ALTERNATE_OBJECT_DIRECTORIES": other_git / "objects",
        "GIT_COMMON_DIR": other_git,
        "GIT_CEILING_DIRECTORIES": mine,
    }.items():
        monkeypatch.setenv(k, str(v))
    assert git_selection(mine) == ["src/mine.txt"]
    assert git_selection(mine / "src") == ["mine.txt"]


def test_run_to_completion_setup_failure_raises_typed(
    make_client: Any, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr("davit.client.RUN_POLL_INTERVAL", 0.0)
    failed = run_json(
        "failed",
        endedAt=NOW,
        error={"code": "not_found", "message": "no image", "retryable": False, "requestId": "r"},
    )

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "POST":
            return httpx.Response(201, json=run_json())
        return httpx.Response(200, json=failed)

    client, seen = make_client(handler)
    with pytest.raises(NotFound) as info:
        client.run_to_completion(["true"], image="nope")
    assert info.value.details == {"runId": "run_1"}
    assert [r.method for r in seen] == ["POST", "GET"]


def test_list_and_iter_runs(make_client: Any) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.params.get("cursor") is None:
            return httpx.Response(200, json={"items": [run_json(id="run_1")], "nextCursor": "c1"})
        return httpx.Response(200, json={"items": [run_json(id="run_2")]})

    client, seen = make_client(handler)
    assert [r.id for r in client.iter_runs(limit=1)] == ["run_1", "run_2"]
    assert (seen[0].method, seen[0].url.path) == ("GET", "/api/v1/runs")
    assert seen[0].url.params["limit"] == "1" and "cursor" not in seen[0].url.params
    assert seen[1].url.params["cursor"] == "c1"
    page = client.list_runs(cursor="c1")
    assert page.next_cursor is None and page.items[0].id == "run_2"


def test_create_run_output_grace(make_client: Any) -> None:
    client, seen = make_client(lambda r: httpx.Response(201, json=run_json()))
    client.create_run(["true"], output_grace_seconds=0)
    client.create_run(["true"], output_grace_seconds=600)
    client.create_run(["true"])
    assert json.loads(seen[0].content)["outputGraceSeconds"] == 0
    assert json.loads(seen[1].content)["outputGraceSeconds"] == 600
    assert "outputGraceSeconds" not in json.loads(seen[2].content)
    for bad in (-1, 601, True, 1.5):
        with pytest.raises(ValueError, match="output_grace_seconds"):
            client.create_run(["true"], output_grace_seconds=bad)  # type: ignore[arg-type]
    with pytest.raises(ValueError, match="output_grace_seconds"):
        client.run_to_completion(["true"], output_grace_seconds=601)
    assert len(seen) == 3


def test_run_fields_and_exit_status() -> None:
    signalled = Run.from_dict(
        run_json("failed", outcome="failed", decidedAt=NOW, endedAt=NOW, signal="SIGKILL")
    )
    assert signalled.signal == "SIGKILL" and signalled.outcome == RunState.FAILED
    assert signalled.decided_at is not None and signalled.error is None
    assert _run_exit(signalled, None) == 137
    # Before cleanup the outcome already says cancelled.
    assert _run_exit(Run.from_dict(run_json("running", outcome="cancelled")), None) == 130
    assert _run_exit(Run.from_dict(run_json("failed", exitCode=3, signal="TERM")), None) == 3
    timeout = {"code": "timeout", "message": "t", "retryable": False, "requestId": "r"}
    assert _run_exit(Run.from_dict(run_json("failed", error=timeout)), None) == 124
    assert _run_exit(Run.from_dict(run_json("failed", signal="SIGWEIRD")), None) == 255


def test_whoami_and_rotate_token(make_client: Any) -> None:
    me = {
        "principal": "tailnet:o",
        "auth": "token",
        "host": "box",
        "version": "v1",
        "token": token_json(),
    }
    created = {**token_json(id="tok_2"), "secret": "slw_new"}

    def handler(r: httpx.Request) -> httpx.Response:
        if r.url.path.endswith("/me"):
            return httpx.Response(200, json=me)
        return httpx.Response(201, json=created)

    client, seen = make_client(handler)
    got = client.whoami()
    assert got.auth == "token" and got.token is not None and got.token.use_count == 0
    new = client.rotate_token("tok_1", overlap=timedelta(minutes=10), idempotency_key="k")
    assert new.id == "tok_2" and new.secret == "slw_new" and "slw_new" not in repr(new)
    rot = seen[1]
    assert (rot.method, rot.url.path) == ("POST", "/api/v1/tokens/tok_1/rotate")
    assert json.loads(rot.content) == {"overlapSeconds": 600}
    assert rot.headers["Idempotency-Key"] == "k"
    client.rotate_token("tok_1")
    assert json.loads(seen[2].content) == {}
    assert "Idempotency-Key" in seen[2].headers
