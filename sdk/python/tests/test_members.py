"""Members and shares (G13) wrappers against a mocked HTTP API."""

from __future__ import annotations

import json
from typing import Any

import httpx
import pytest

from davit import (
    Client,
    Member,
    MemberRole,
    NotFound,
    PermissionDenied,
    Share,
    ShareKind,
    UnsupportedCapability,
)

NOW = "2026-10-22T04:22:34Z"


def member(login: str = "alice@example.com", role: str = "viewer") -> Any:
    return {"id": "mem_1", "login": login, "userId": 42, "role": role, "addedAt": NOW}


def share(
    member_id: str = "mem_1", kind: str = "vm", role: str | None = "exec", sid: str = "shr_1"
) -> Any:
    out = {
        "id": sid,
        "kind": kind,
        "resourceId": "vm_1",
        "member": member_id,
        "credentials": False,
        "createdAt": NOW,
    }
    if role is not None:
        out["role"] = role
    return out


class Api:
    """A mock HTTP API recording method, raw (still escaped) path with query, and JSON body."""

    def __init__(self, shares: list[Any] | None = None) -> None:
        self.calls: list[tuple[str, str, Any]] = []
        self.shares = shares if shares is not None else [share()]

    def __call__(self, request: httpx.Request) -> httpx.Response:
        body = json.loads(request.content) if request.content else None
        path = request.url.raw_path.decode()
        self.calls.append((request.method, path, body))
        if request.method == "DELETE":
            return httpx.Response(204)
        if path.startswith("/api/v1/members"):
            if request.method == "GET":
                return httpx.Response(200, json={"items": [member()]})
            role = body["role"]
            if request.method == "POST":
                return httpx.Response(201, json=member(body["login"], role))
            return httpx.Response(200, json=member(role=role))
        if request.method == "GET":
            return httpx.Response(200, json={"items": self.shares})
        reply = share(kind=body["kind"], role=body.get("role"))
        reply["credentials"] = body.get("credentials", False)
        return httpx.Response(201, json=reply)


def client(api: Api) -> Client:
    return Client(socket="/nonexistent.sock", transport=httpx.MockTransport(api))


def test_member_wrappers_send_the_contract_shapes() -> None:
    api = Api()
    sw = client(api)
    listed = sw.list_members().items
    assert [(m.id, m.login, m.user_id, m.role) for m in listed] == [
        ("mem_1", "alice@example.com", 42, MemberRole.VIEWER)
    ]
    added = sw.add_member("alice@example.com", "exec")
    assert isinstance(added, Member) and added.role == MemberRole.EXEC
    assert sw.update_member(added, MemberRole.ADMIN).role == MemberRole.ADMIN
    sw.update_member("alice@example.com", "viewer")
    sw.remove_member(added)
    sw.remove_member("alice@example.com")
    assert api.calls == [
        ("GET", "/api/v1/members", None),
        ("POST", "/api/v1/members", {"login": "alice@example.com", "role": "exec"}),
        ("PATCH", "/api/v1/members/mem_1", {"role": "admin"}),
        ("PATCH", "/api/v1/members/alice%40example.com", {"role": "viewer"}),
        ("DELETE", "/api/v1/members/mem_1", None),
        ("DELETE", "/api/v1/members/alice%40example.com", None),
    ]


def test_share_wrappers_send_the_contract_shapes() -> None:
    api = Api()
    sw = client(api)
    alice = Member.from_dict(member())
    assert [s.id for s in sw.list_shares().items] == ["shr_1"]
    sw.list_shares("vm", "web")
    sw.list_shares(ShareKind.SNAPSHOT)
    created = sw.create_share("vm", "web", alice, "exec", credentials=True)
    assert isinstance(created, Share) and created.credentials and created.role == MemberRole.EXEC
    assert sw.share_vm("web", "*").role is None
    sw.share_snapshot("snap", "alice@example.com", MemberRole.VIEWER)
    sw.share_environment("py", "mem_1", "admin", credentials=False)
    sw.delete_share(created)
    sw.delete_share("shr/2")
    assert api.calls == [
        ("GET", "/api/v1/shares", None),
        ("GET", "/api/v1/shares?kind=vm&resource=web", None),
        ("GET", "/api/v1/shares?kind=snapshot", None),
        (
            "POST",
            "/api/v1/shares",
            {
                "kind": "vm",
                "resource": "web",
                "member": "mem_1",
                "role": "exec",
                "credentials": True,
            },
        ),
        ("POST", "/api/v1/shares", {"kind": "vm", "resource": "web", "member": "*"}),
        (
            "POST",
            "/api/v1/shares",
            {
                "kind": "snapshot",
                "resource": "snap",
                "member": "alice@example.com",
                "role": "viewer",
            },
        ),
        (
            "POST",
            "/api/v1/shares",
            {"kind": "environment", "resource": "py", "member": "mem_1", "role": "admin"},
        ),
        ("DELETE", "/api/v1/shares/shr_1", None),
        ("DELETE", "/api/v1/shares/shr%2F2", None),
    ]


def test_query_parameters_are_escaped() -> None:
    api = Api()
    client(api).list_shares("vm", "my vm&x=1")
    assert api.calls[0][1] == "/api/v1/shares?kind=vm&resource=my+vm%26x%3D1"


def test_empty_references_are_refused() -> None:
    api = Api()
    sw = client(api)
    with pytest.raises(ValueError, match="member"):
        sw.remove_member("")
    with pytest.raises(ValueError, match="login"):
        sw.add_member("", "viewer")
    with pytest.raises(ValueError, match="share"):
        sw.delete_share("")
    with pytest.raises(ValueError, match="resource"):
        sw.share_vm("", "*")
    assert api.calls == []


def test_unshare_finds_the_share_by_member_id_or_star() -> None:
    api = Api([share("*", sid="shr_all"), share("mem_1", sid="shr_1")])
    sw = client(api)
    sw.unshare("vm", "web", Member.from_dict(member()))
    sw.unshare(ShareKind.VM, "web", "*")
    assert api.calls == [
        ("GET", "/api/v1/shares?kind=vm&resource=web", None),
        ("DELETE", "/api/v1/shares/shr_1", None),
        ("GET", "/api/v1/shares?kind=vm&resource=web", None),
        ("DELETE", "/api/v1/shares/shr_all", None),
    ]


def test_unshare_resolves_a_login_through_list_members() -> None:
    api = Api([share("mem_1", kind="snapshot", sid="shr_9")])
    sw = client(api)
    sw.unshare("snapshot", "snap", "alice@example.com")
    assert api.calls == [
        ("GET", "/api/v1/shares?kind=snapshot&resource=snap", None),
        ("GET", "/api/v1/members", None),
        ("DELETE", "/api/v1/shares/shr_9", None),
    ]


def test_unshare_without_a_matching_share_is_not_found() -> None:
    api = Api([share("mem_2")])
    sw = client(api)
    with pytest.raises(NotFound, match="not shared with bob"):
        sw.unshare("vm", "web", "bob")
    with pytest.raises(NotFound):
        sw.unshare("vm", "web", "*")
    assert not any(m == "DELETE" for m, _, _ in api.calls)


def error(status: int, code: str, message: str) -> httpx.Response:
    info = {"code": code, "message": message, "retryable": False, "requestId": "req_srv"}
    return httpx.Response(status, json={"error": info})


def test_errors_map_to_typed_exceptions() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "POST":
            return error(422, "unsupported_capability", "needs the tailnet gateway")
        return error(403, "permission_denied", "owner only")

    sw = Client(socket="/nonexistent.sock", transport=httpx.MockTransport(handler))
    with pytest.raises(UnsupportedCapability) as unsupported:
        sw.add_member("alice@example.com", "viewer")
    assert unsupported.value.status == 422
    with pytest.raises(PermissionDenied) as denied:
        sw.list_shares()
    assert denied.value.status == 403 and denied.value.request_id == "req_srv"
