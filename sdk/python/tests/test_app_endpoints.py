"""App endpoint (G11) and desktop (G15) wrappers against a mocked HTTP API."""

from __future__ import annotations

import json
from typing import Any

import httpx
import pytest

from davit import (
    AppEndpoint,
    AppEndpointKind,
    AppEndpointState,
    Client,
    Conflict,
    InvalidState,
    NotFound,
    UnsupportedCapability,
)

NOW = "2026-10-22T04:22:34Z"
BASE = "/api/v1/vms/web/app-endpoints"


def endpoint(
    name: str = "port-3000", port: int = 3000, hidden: bool = False, kind: str = "http"
) -> Any:
    return {
        "id": "app_1",
        "vmId": "vm_1",
        "name": name,
        "port": port,
        "kind": kind,
        "url": "https://gw.example.ts.net:20001/",
        "gatewayPort": 20001,
        "hidden": hidden,
        "forwardIdentity": False,
        "state": "hidden" if hidden else "active",
        "createdAt": NOW,
    }


class Api:
    """A mock HTTP API recording calls: method, raw (still escaped) path and JSON body."""

    def __init__(self) -> None:
        self.calls: list[tuple[str, str, Any]] = []

    def __call__(self, request: httpx.Request) -> httpx.Response:
        body = json.loads(request.content) if request.content else None
        self.calls.append((request.method, request.url.raw_path.decode(), body))
        if request.method == "POST":
            return httpx.Response(
                201, json=endpoint(body.get("name", f"port-{body['port']}"), body["port"])
            )
        if request.method == "PATCH":
            return httpx.Response(
                200, json=endpoint(body.get("name", "port-3000"), hidden=body.get("hidden", False))
            )
        if request.method == "GET":
            return httpx.Response(200, json={"items": [endpoint()]})
        return httpx.Response(204)


def test_forward_identity_is_opt_in() -> None:
    api = Api()
    client = Client(socket="/nonexistent.sock", transport=httpx.MockTransport(api))
    client.expose("web", 3000)
    client.expose("web", 3001, forward_identity=True)
    client.update_app_endpoint("web", "port-3000", forward_identity=False)
    bodies = [c[2] for c in api.calls]
    assert bodies == [
        {"port": 3000},
        {"port": 3001, "forwardIdentity": True},
        {"forwardIdentity": False},
    ]


def test_wrappers_send_the_contract_shapes() -> None:
    api = Api()
    client = Client(socket="/nonexistent.sock", transport=httpx.MockTransport(api))

    listed = client.list_app_endpoints("web").items
    assert [e.id for e in listed] == ["app_1"]
    assert listed[0].gateway_port == 20001 and listed[0].state == AppEndpointState.ACTIVE

    ep = client.create_app_endpoint("web", 3000)
    assert isinstance(ep, AppEndpoint) and ep.url == "https://gw.example.ts.net:20001/"
    assert ep.kind == AppEndpointKind.HTTP
    assert client.create_app_endpoint("web", 8080, "api").name == "api"
    assert client.expose("web", 5173, name="vite").port == 5173

    assert client.update_app_endpoint("web", ep, name="app", hidden=True).hidden
    assert client.hide_app_endpoint("web", "app").state == AppEndpointState.HIDDEN
    assert not client.show_app_endpoint("web", "app").hidden
    assert client.rename_app_endpoint("web", "app_1", "site").name == "site"
    client.delete_app_endpoint("web", ep)
    client.delete_app_endpoint("web", "site")

    assert api.calls == [
        ("GET", BASE, None),
        ("POST", BASE, {"port": 3000}),
        ("POST", BASE, {"port": 8080, "name": "api"}),
        ("POST", BASE, {"port": 5173, "name": "vite"}),
        ("PATCH", f"{BASE}/app_1", {"name": "app", "hidden": True}),
        ("PATCH", f"{BASE}/app", {"hidden": True}),
        ("PATCH", f"{BASE}/app", {"hidden": False}),
        ("PATCH", f"{BASE}/app_1", {"name": "site"}),
        ("DELETE", f"{BASE}/app_1", None),
        ("DELETE", f"{BASE}/site", None),
    ]


def test_names_are_escaped_and_checked() -> None:
    api = Api()
    client = Client(socket="/nonexistent.sock", transport=httpx.MockTransport(api))
    client.hide_app_endpoint("my vm", "a/b c?d")
    client.delete_app_endpoint("web", "%2e%2e")
    assert [path for _, path, _ in api.calls] == [
        "/api/v1/vms/my%20vm/app-endpoints/a%2Fb%20c%3Fd",
        f"{BASE}/%252e%252e",
    ]
    with pytest.raises(ValueError, match="endpoint"):
        client.delete_app_endpoint("web", "")
    for bad in (0, 65536, True):
        with pytest.raises(ValueError, match="port"):
            client.create_app_endpoint("web", bad)
    assert len(api.calls) == 2


def error(status: int, code: str, message: str) -> httpx.Response:
    info = {"code": code, "message": message, "retryable": False, "requestId": "req_srv"}
    return httpx.Response(status, json={"error": info})


def test_errors_map_to_typed_exceptions() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "POST":
            return error(422, "unsupported_capability", "no tailnet gateway")
        return error(409, "invalid_state", "name taken")

    client = Client(socket="/nonexistent.sock", transport=httpx.MockTransport(handler))
    with pytest.raises(UnsupportedCapability) as unsupported:
        client.expose("web", 3000)
    assert unsupported.value.status == 422 and unsupported.value.code == "unsupported_capability"
    with pytest.raises(Conflict) as conflict:
        client.rename_app_endpoint("web", "app_1", "taken")
    assert isinstance(conflict.value, InvalidState) and conflict.value.status == 409
    assert conflict.value.request_id == "req_srv"


DESKTOP = "/api/v1/vms/web/desktop"


def test_desktop_wrappers_send_the_contract_shapes() -> None:
    calls: list[tuple[str, str, Any, str | None]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        body = json.loads(request.content) if request.content else None
        ctype = request.headers.get("Content-Type")
        calls.append((request.method, request.url.raw_path.decode(), body, ctype))
        if request.method == "DELETE":
            return httpx.Response(204)
        port = (body or {}).get("port", 5900)
        status = 201 if request.method == "POST" and port == 5900 else 200
        return httpx.Response(status, json=endpoint("desktop", port, kind="desktop"))

    client = Client(socket="/nonexistent.sock", transport=httpx.MockTransport(handler))
    opened = client.open_desktop("web")
    assert isinstance(opened, AppEndpoint)
    assert opened.kind == AppEndpointKind.DESKTOP and opened.name == "desktop"
    assert opened.port == 5900
    assert client.open_desktop("web", 5901).port == 5901
    assert client.get_desktop("web").kind == AppEndpointKind.DESKTOP
    assert client.close_desktop("web") is None
    client.get_desktop("my vm")

    assert [c[:3] for c in calls] == [
        ("POST", DESKTOP, None),
        ("POST", DESKTOP, {"port": 5901}),
        ("GET", DESKTOP, None),
        ("DELETE", DESKTOP, None),
        ("GET", "/api/v1/vms/my%20vm/desktop", None),
    ]
    # Without a port, open sends no body at all (the server default applies).
    assert calls[0][3] is None and calls[1][3] == "application/json"
    for bad in (0, 65536, True):
        with pytest.raises(ValueError, match="port"):
            client.open_desktop("web", bad)
    assert len(calls) == 5


def test_desktop_errors_map_to_typed_exceptions() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "POST":
            return error(422, "unsupported_capability", "no tailnet gateway")
        return error(404, "not_found", "no desktop is open")

    client = Client(socket="/nonexistent.sock", transport=httpx.MockTransport(handler))
    with pytest.raises(NotFound) as missing:
        client.get_desktop("web")
    assert missing.value.status == 404 and missing.value.code == "not_found"
    assert missing.value.request_id == "req_srv"
    with pytest.raises(NotFound):
        client.close_desktop("web")
    with pytest.raises(UnsupportedCapability) as unsupported:
        client.open_desktop("web")
    assert unsupported.value.status == 422 and unsupported.value.code == "unsupported_capability"
