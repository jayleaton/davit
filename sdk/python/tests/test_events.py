"""Events, webhooks and delivery verification against an in-process mock transport."""

from __future__ import annotations

import hashlib
import hmac
import itertools
import json
import uuid
from collections.abc import Callable, Iterator
from typing import Any

import httpx
import pytest

from davit import (
    WEBHOOK_SIGNATURE_HEADER,
    Client,
    DavitError,
    Internal,
    Webhook,
    WebhookSecret,
    WebhookVerificationError,
    verify_webhook,
)

NOW = "2026-10-22T04:22:34Z"

Handler = Callable[[httpx.Request], httpx.Response]


@pytest.fixture
def make_client() -> Iterator[Callable[[Handler], tuple[Client, list[httpx.Request]]]]:
    clients: list[Client] = []

    def build(handler: Handler) -> tuple[Client, list[httpx.Request]]:
        seen: list[httpx.Request] = []

        def record(request: httpx.Request) -> httpx.Response:
            seen.append(request)
            return handler(request)

        client = Client(base_url="http://127.0.0.1:9", transport=httpx.MockTransport(record))
        clients.append(client)
        return client, seen

    yield build
    for client in clients:
        client.close()


def ev(seq: int, type_: str = "vm.state") -> dict[str, Any]:
    return {
        "seq": seq,
        "type": type_,
        "time": NOW,
        "vmId": "vm_1",
        "subject": "vm_1",
        "data": {"state": "ready"},
    }


def hook_json(**over: Any) -> dict[str, Any]:
    hook = {
        "id": "wh_1",
        "url": "https://example.test/hook",
        "events": ["vm."],
        "enabled": True,
        "createdAt": NOW,
        "updatedAt": NOW,
        "pending": 0,
        "dead": 1,
    }
    return {**hook, **over}


def delivery_json(**over: Any) -> dict[str, Any]:
    d = {
        "id": "dl_1",
        "webhookId": "wh_1",
        "eventSeq": 7,
        "eventType": "vm.state",
        "state": "dead",
        "attempts": 5,
        "lastStatus": 500,
        "createdAt": NOW,
    }
    return {**d, **over}


# ---- events ----------------------------------------------------------------


def test_list_events_query(make_client: Any) -> None:
    client, seen = make_client(
        lambda _: httpx.Response(200, json={"items": [ev(1)], "nextCursor": "c1"})
    )
    page = client.list_events(
        limit=10, types=["vm.", "operation.state"], vm="web", start="latest", wait_ms=5000
    )
    assert [e.seq for e in page.items] == [1] and page.next_cursor == "c1"
    assert page.items[0].data == {"state": "ready"} and page.items[0].vm_id == "vm_1"
    (req,) = seen
    assert (req.method, req.url.path) == ("GET", "/api/v1/events")
    assert dict(req.url.params) == {
        "limit": "10",
        "types": "vm.,operation.state",
        "vmId": "web",
        "start": "latest",
        "waitMs": "5000",
    }
    client.list_events(types="vm.", cursor="c1")
    assert dict(seen[1].url.params) == {"cursor": "c1", "types": "vm."}
    client.list_events()
    assert dict(seen[2].url.params) == {}


def test_iter_events_pages_until_empty(make_client: Any) -> None:
    pages = iter(
        [
            {"items": [ev(1), ev(2)], "nextCursor": "c2"},
            {"items": [ev(3)], "nextCursor": "c3"},
            {"items": [], "nextCursor": "c3"},
        ]
    )
    client, seen = make_client(lambda _: httpx.Response(200, json=next(pages)))
    got = [e.seq for e in client.iter_events(types=["vm."], start="oldest", limit=2)]
    assert got == [1, 2, 3]
    assert [r.url.params.get("cursor") for r in seen] == [None, "c2", "c3"]
    # start only goes with the first (cursor-less) request; no long-poll without follow.
    assert [r.url.params.get("start") for r in seen] == ["oldest", None, None]
    assert all("waitMs" not in r.url.params for r in seen)
    assert all(r.url.params["types"] == "vm." and r.url.params["limit"] == "2" for r in seen)


def test_iter_events_follow_long_polls_through_empty_pages(make_client: Any) -> None:
    pages = iter(
        [
            {"items": [], "nextCursor": "c0"},
            {"items": [], "nextCursor": "c0"},
            {"items": [ev(5, "run.state")], "nextCursor": "c5"},
            {"items": [], "nextCursor": "c5"},
            {"items": [ev(6), ev(7)], "nextCursor": "c7"},
        ]
    )
    client, seen = make_client(lambda _: httpx.Response(200, json=next(pages)))
    it = client.iter_events(start="latest", follow=True)
    got = [e.seq for e in itertools.islice(it, 3)]
    assert got == [5, 6, 7]
    assert len(seen) == 5
    assert [r.url.params.get("cursor") for r in seen] == [None, "c0", "c0", "c5", "c5"]
    assert seen[0].url.params["start"] == "latest"
    assert all("start" not in r.url.params for r in seen[1:])
    assert all(r.url.params["waitMs"] == "25000" for r in seen)


def test_iter_events_resumes_from_cursor_and_checks_args(make_client: Any) -> None:
    client, seen = make_client(
        lambda _: httpx.Response(200, json={"items": [], "nextCursor": "c9"})
    )
    assert list(client.iter_events(cursor="c9", start="latest")) == []
    assert dict(seen[0].url.params) == {"cursor": "c9"}
    with pytest.raises(ValueError, match="wait_ms"):
        next(client.iter_events(follow=True, wait_ms=30_001))


def test_iter_events_without_cursor_is_internal(make_client: Any) -> None:
    client, _ = make_client(
        lambda _: httpx.Response(200, json={"items": [ev(1)], "nextCursor": ""})
    )
    it = client.iter_events()
    assert next(it).seq == 1
    with pytest.raises(Internal, match="nextCursor"):
        next(it)


# ---- webhooks --------------------------------------------------------------


def test_create_webhook_body_and_idempotency_key(make_client: Any) -> None:
    secret = {"webhook": hook_json(), "secret": "whs_s3cret"}
    client, seen = make_client(lambda _: httpx.Response(201, json=secret))
    got = client.create_webhook("https://example.test/hook", events=["vm."], description="ci")
    assert isinstance(got, WebhookSecret) and got.secret == "whs_s3cret"
    assert isinstance(got.webhook, Webhook) and got.webhook.dead == 1
    assert "whs_s3cret" not in repr(got) and "whs_s3cret" not in str(got)
    req = seen[0]
    assert (req.method, req.url.path) == ("POST", "/api/v1/webhooks")
    assert uuid.UUID(req.headers["Idempotency-Key"]).version == 4
    assert json.loads(req.content) == {
        "url": "https://example.test/hook",
        "events": ["vm."],
        "description": "ci",
    }

    client.create_webhook("https://e.test/", enabled=False, idempotency_key="k-1")
    assert seen[1].headers["Idempotency-Key"] == "k-1"
    assert json.loads(seen[1].content) == {"url": "https://e.test/", "enabled": False}
    with pytest.raises(TypeError, match="events"):
        client.create_webhook("https://e.test/", events="vm.")
    assert len(seen) == 2


def test_webhook_crud_paths_and_bodies(make_client: Any) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "DELETE":
            return httpx.Response(204)
        if request.url.path == "/api/v1/webhooks":
            if request.url.params.get("cursor") is None:
                return httpx.Response(200, json={"items": [hook_json()], "nextCursor": "w1"})
            return httpx.Response(200, json={"items": [hook_json(id="wh/2")]})
        if request.url.path.endswith("/rotate-secret"):
            return httpx.Response(200, json={"webhook": hook_json(), "secret": "whs_new"})
        return httpx.Response(200, json=hook_json(enabled=False))

    client, seen = make_client(handler)
    hooks = list(client.iter_webhooks(limit=1))
    assert [h.id for h in hooks] == ["wh_1", "wh/2"]
    assert seen[0].url.params["limit"] == "1" and seen[1].url.params["cursor"] == "w1"
    assert client.list_webhooks().next_cursor == "w1"

    seen.clear()
    assert client.get_webhook("wh_1").enabled is False
    client.update_webhook(hooks[1], enabled=False, events=[])
    client.update_webhook("wh_1", url="https://new.test/", description="")
    assert client.delete_webhook(hooks[0]) is None
    rotated = client.rotate_webhook_secret("wh_1", overlap_seconds=3600)
    client.rotate_webhook_secret("wh_1", idempotency_key="rot-1")
    assert rotated.secret == "whs_new" and "whs_new" not in repr(rotated)

    get, upd1, upd2, delete, rot1, rot2 = seen
    assert (get.method, get.url.path) == ("GET", "/api/v1/webhooks/wh_1")
    assert (upd1.method, upd1.url.raw_path) == ("PATCH", b"/api/v1/webhooks/wh%2F2")
    assert json.loads(upd1.content) == {"events": [], "enabled": False}
    assert json.loads(upd2.content) == {"url": "https://new.test/", "description": ""}
    assert "Idempotency-Key" not in upd1.headers
    assert (delete.method, delete.url.path, delete.content) == (
        "DELETE",
        "/api/v1/webhooks/wh_1",
        b"",
    )
    assert "Idempotency-Key" not in delete.headers
    assert (rot1.method, rot1.url.path) == ("POST", "/api/v1/webhooks/wh_1/rotate-secret")
    assert json.loads(rot1.content) == {"overlapSeconds": 3600}
    assert uuid.UUID(rot1.headers["Idempotency-Key"]).version == 4
    assert json.loads(rot2.content) == {} and rot2.headers["Idempotency-Key"] == "rot-1"


def test_webhook_deliveries_filter_and_retry(make_client: Any) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "POST":
            return httpx.Response(200, json=delivery_json(state="pending", attempts=0))
        if request.url.params.get("cursor") is None:
            return httpx.Response(200, json={"items": [delivery_json()], "nextCursor": "d1"})
        return httpx.Response(200, json={"items": [delivery_json(id="dl_2")]})

    client, seen = make_client(handler)
    secret = WebhookSecret.from_dict({"webhook": hook_json(), "secret": "s"})
    dead = list(client.iter_webhook_deliveries(secret, state="dead", limit=5))
    assert [d.id for d in dead] == ["dl_1", "dl_2"] and dead[0].last_status == 500
    assert seen[0].url.path == "/api/v1/webhooks/wh_1/deliveries"
    assert dict(seen[0].url.params) == {"limit": "5", "state": "dead"}
    assert dict(seen[1].url.params) == {"limit": "5", "cursor": "d1", "state": "dead"}
    client.list_webhook_deliveries("wh_1")
    assert dict(seen[2].url.params) == {}

    again = client.retry_webhook_delivery("wh_1", dead[0])
    assert again.state == "pending" and again.attempts == 0
    req = seen[-1]
    assert (req.method, req.url.path) == ("POST", "/api/v1/webhooks/wh_1/deliveries/dl_1/retry")
    assert req.content == b"" and "Idempotency-Key" not in req.headers


# ---- signature verification ------------------------------------------------

BODY = b'{"deliveryId":"dl_1","webhookId":"wh_1","event":{"seq":7}}'
T = 1_792_000_000


def sign(secret: str, t: int = T, body: bytes = BODY) -> str:
    mac = hmac.new(secret.encode(), str(t).encode() + b"." + body, hashlib.sha256)
    return mac.hexdigest()


def test_verify_valid_signature() -> None:
    header = f"t={T},v1={sign('whs_a')}"
    assert verify_webhook(BODY, header, "whs_a", now=T) is None
    assert verify_webhook(BODY.decode(), header, ["whs_a"], now=T + 300) is None
    assert (
        verify_webhook(bytearray(BODY), f" t={T} , v1={sign('whs_a').upper()} ", "whs_a", now=T)
        is None
    )
    # Unknown schemes are ignored.
    assert verify_webhook(BODY, f"t={T},v0=zz,v1={sign('whs_a')}", "whs_a", now=T) is None
    assert WEBHOOK_SIGNATURE_HEADER == "X-Davit-Signature"


def test_verify_wrong_secret_and_tampered_body() -> None:
    header = f"t={T},v1={sign('whs_a')}"
    with pytest.raises(WebhookVerificationError) as exc:
        verify_webhook(BODY, header, "whs_b", now=T)
    assert exc.value.reason == "signature" and isinstance(exc.value, DavitError)
    with pytest.raises(WebhookVerificationError, match="v1") as exc:
        verify_webhook(BODY.replace(b"7", b"8"), header, "whs_a", now=T)
    assert exc.value.reason == "signature"
    # The timestamp is signed too.
    with pytest.raises(WebhookVerificationError) as exc:
        verify_webhook(BODY, f"t={T + 1},v1={sign('whs_a')}", "whs_a", now=T)
    assert exc.value.reason == "signature"


def test_verify_rotation_pair() -> None:
    header = f"t={T},v1={sign('whs_old')},v1={sign('whs_new')}"
    verify_webhook(BODY, header, "whs_old", now=T)
    verify_webhook(BODY, header, "whs_new", now=T)
    verify_webhook(BODY, header, ["whs_other", "whs_new"], now=T)
    # A receiver holding both secrets accepts a single-secret delivery either way.
    verify_webhook(BODY, f"t={T},v1={sign('whs_old')}", ["whs_new", "whs_old"], now=T)
    with pytest.raises(WebhookVerificationError):
        verify_webhook(BODY, header, ["whs_x", "whs_y"], now=T)


@pytest.mark.parametrize("now", [T + 301, T - 301])
def test_verify_rejects_stale_and_future_timestamps(now: int) -> None:
    header = f"t={T},v1={sign('whs_a')}"
    with pytest.raises(WebhookVerificationError, match="tolerance") as exc:
        verify_webhook(BODY, header, "whs_a", now=now)
    assert exc.value.reason == "timestamp"
    verify_webhook(BODY, header, "whs_a", now=now, tolerance=301)


@pytest.mark.parametrize(
    "header",
    [
        None,
        "",
        "garbage",
        f"v1={sign('whs_a')}",
        f"t={T}",
        f"t=abc,v1={sign('whs_a')}",
        f"t=-{T},v1={sign('whs_a')}",
        f"t={T},t={T},v1={sign('whs_a')}",
        f"t={T},v1=xyz",
        f"t={T},v1={sign('whs_a')[:-2]}",
        f"t={T},v1",
    ],
)
def test_verify_malformed_header(header: str | None) -> None:
    with pytest.raises(WebhookVerificationError) as exc:
        verify_webhook(BODY, header, "whs_a", now=T)
    assert exc.value.reason == "malformed"


def test_verify_argument_errors() -> None:
    header = f"t={T},v1={sign('whs_a')}"
    for bad in ([], "", ["whs_a", ""]):
        with pytest.raises(ValueError, match="secrets"):
            verify_webhook(BODY, header, bad, now=T)
    with pytest.raises(ValueError, match="tolerance"):
        verify_webhook(BODY, header, "whs_a", now=T, tolerance=-1)
