"""Prompt and conversation calls against an in-process mock transport."""

from __future__ import annotations

import errno
import json
import uuid
from collections.abc import Callable
from typing import Any

import httpx
import pytest

from davit import (
    Client,
    OperationInProgress,
    PromptAttachment,
    PromptEvent,
    PromptState,
    WaitTimeout,
    _transport,
)

NOW = "2026-10-22T04:22:34Z"


def prompt_json(state: str = "running", **over: Any) -> dict[str, Any]:
    p = {
        "id": "pr_1",
        "conversationId": "conv_1",
        "vmId": "vm_1",
        "harness": "claude",
        "state": state,
        "eventCursor": 0,
        "createdAt": NOW,
    }
    return {**p, **over}


def event_json(seq: int, kind: str = "message.delta", **over: Any) -> dict[str, Any]:
    e = {
        "seq": seq,
        "time": NOW,
        "promptId": "pr_1",
        "conversationId": "conv_1",
        "type": kind,
    }
    return {**e, **over}


def client_for(handler: Callable[[httpx.Request], httpx.Response]) -> tuple[Client, list[Any]]:
    seen: list[httpx.Request] = []

    def record(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        return handler(request)

    return Client(base_url="http://127.0.0.1:9", transport=httpx.MockTransport(record)), seen


def test_create_prompt_shapes_request() -> None:
    client, seen = client_for(lambda r: httpx.Response(201, json=prompt_json("starting")))
    p = client.create_prompt(
        "box", "hello", harness="codex", new=True, env={"K": "v"}, secrets=["TOKEN"], model="m"
    )
    assert p.state is PromptState.STARTING
    req = seen[0]
    assert (req.method, req.url.path) == ("POST", "/api/v1/vms/box/prompts")
    assert json.loads(req.content) == {
        "text": "hello",
        "harness": "codex",
        "new": True,
        "model": "m",
        "env": {"K": "v"},
        "secrets": ["TOKEN"],
    }
    assert uuid.UUID(req.headers["Idempotency-Key"]).version == 4
    client.create_prompt("box", "again", conversation_id="conv_1", idempotency_key="k-2")
    assert seen[1].headers["Idempotency-Key"] == "k-2"
    assert seen[0].headers["Idempotency-Key"] != "k-2"
    assert json.loads(seen[1].content) == {"text": "again", "conversationId": "conv_1"}


@pytest.mark.parametrize(
    ("text", "secrets", "exc"),
    [("", None, ValueError), ("  ", None, ValueError), ("x", "S", TypeError)],
)
def test_create_prompt_validates(text: str, secrets: Any, exc: type[Exception]) -> None:
    client, seen = client_for(lambda r: httpx.Response(500))
    with pytest.raises(exc):
        client.create_prompt("box", text, secrets=secrets)
    assert seen == []


def test_busy_conversation_is_operation_in_progress() -> None:
    body = {"error": {"code": "operation_in_progress", "message": "busy", "retryable": True}}
    client, _ = client_for(lambda r: httpx.Response(409, json=body))
    with pytest.raises(OperationInProgress) as info:
        client.create_prompt("box", "x", conversation_id="conv_1")
    assert info.value.retryable


def test_events_parse_any_json_fields() -> None:
    page = {
        "prompt": prompt_json(),
        "items": [
            event_json(1, "tool.call", toolName="Bash", input={"command": "ls"}, raw=[1, "a"]),
            event_json(2, "brand.new.type", raw=None),
        ],
        "nextCursor": "2",
    }
    client, seen = client_for(lambda r: httpx.Response(200, json=page))
    got = client.list_prompt_events("box", "pr_1", after=0, wait_ms=500)
    assert got.items[0].input == {"command": "ls"}
    assert got.items[0].raw == [1, "a"]
    assert got.items[1].type == "brand.new.type"
    assert got.items[1].raw is None
    assert dict(seen[0].url.params) == {"after": "0", "waitMs": "500"}


def test_follow_until_terminal_and_wait_returns_failed_turn() -> None:
    done = prompt_json("failed", error={"code": "harness_failed", "message": "bad key"})
    pages = [
        {"prompt": prompt_json(), "items": [event_json(1), event_json(2)]},
        {"prompt": prompt_json(), "items": []},  # long-poll expired, still running
        {"prompt": done, "items": [event_json(3, "turn.completed", status="failed")]},
        {"prompt": done, "items": []},
    ]

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path.endswith("/events"):
            return httpx.Response(200, json=pages.pop(0))
        return httpx.Response(200, json=done)

    client, seen = client_for(handler)
    events: list[PromptEvent] = []
    final = client.wait_prompt("box", "pr_1", on_event=events.append)
    assert [e.seq for e in events] == [1, 2, 3]
    assert final.state is PromptState.FAILED
    assert final.error is not None
    assert final.error.code == "harness_failed"
    polls = [r for r in seen if r.url.path.endswith("/events")]
    assert [r.url.params["after"] for r in polls] == ["0", "2", "2", "3"]
    assert all(0 < int(r.url.params["waitMs"]) <= 25_000 for r in polls)


def test_unknown_state_counts_as_terminal() -> None:
    page = {"prompt": prompt_json("paused-by-future-server"), "items": []}
    client, _ = client_for(lambda r: httpx.Response(200, json=page))
    assert list(client.follow_prompt("box", "pr_1")) == []


def test_follow_timeout_never_cancels() -> None:
    client, seen = client_for(
        lambda r: httpx.Response(200, json={"prompt": prompt_json(), "items": []})
    )
    with pytest.raises(WaitTimeout) as info:
        list(client.follow_prompt("box", "pr_1", after=7, timeout=0.05))
    assert info.value.details == {"promptId": "pr_1", "cursor": 7}
    assert info.value.operation_id is None  # a prompt is not an operation
    assert all(r.method == "GET" for r in seen)


def test_run_prompt_and_conversations() -> None:
    done = prompt_json("succeeded", usage={"inputTokens": 3, "outputTokens": 4, "costUsd": 0.5})
    conv = {
        "id": "conv_1",
        "vmId": "vm_1",
        "harness": "claude",
        "turns": 2,
        "createdAt": NOW,
        "updatedAt": NOW,
    }

    def handler(request: httpx.Request) -> httpx.Response:
        path = request.url.path
        if request.method == "POST":
            return httpx.Response(201, json=prompt_json("starting"))
        if path.endswith("/events"):
            return httpx.Response(200, json={"prompt": done, "items": []})
        if path.endswith("/conversations"):
            return httpx.Response(200, json={"items": [conv]})
        if "/conversations/" in path:
            return httpx.Response(200, json=conv)
        if request.method == "DELETE":
            return httpx.Response(200, json=prompt_json("cancelled"))
        return httpx.Response(200, json=done)

    client, seen = client_for(handler)
    result = client.run_prompt("box", "hi")
    final = result.prompt
    assert (result.text, result.events) == ("", [])
    assert final.usage is not None
    assert (final.usage.input_tokens, final.usage.cost_usd) == (3, 0.5)
    assert client.cancel_prompt("box", "pr_1").state == "cancelled"
    assert [c.id for c in client.list_conversations("box", limit=5).items] == ["conv_1"]
    assert client.get_conversation("box", "conv_1").turns == 2
    tail = [(r.method, r.url.path, r.url.query) for r in seen[-3:]]
    assert tail == [
        ("DELETE", "/api/v1/vms/box/prompts/pr_1", b""),
        ("GET", "/api/v1/vms/box/conversations", b"limit=5"),
        ("GET", "/api/v1/vms/box/conversations/conv_1", b""),
    ]


def test_create_prompt_reuses_key_on_unreached_retry(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(_transport, "_BACKOFF", 0.0)
    calls: list[int] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(1)
        if len(calls) < 3:
            exc = httpx.ConnectError("refused")
            exc.__cause__ = ConnectionRefusedError(errno.ECONNREFUSED, "refused")
            raise exc
        return httpx.Response(201, json=prompt_json("starting"))

    client, seen = client_for(handler)
    client.create_prompt("box", "hi")
    assert len(seen) == 3
    assert len({r.headers["Idempotency-Key"] for r in seen}) == 1


def test_run_prompt_passes_key_through() -> None:
    done = prompt_json("succeeded")

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "POST":
            return httpx.Response(201, json=prompt_json("starting"))
        if request.url.path.endswith("/events"):
            return httpx.Response(200, json={"prompt": done, "items": []})
        return httpx.Response(200, json=done)

    client, seen = client_for(handler)
    client.run_prompt("box", "hi", idempotency_key="turn-1")
    assert seen[0].headers["Idempotency-Key"] == "turn-1"


def test_conversations_page_with_cursor() -> None:
    def conv(i: int) -> dict[str, Any]:
        return {
            "id": f"conv_{i}",
            "vmId": "vm_1",
            "harness": "claude",
            "turns": 1,
            "createdAt": NOW,
            "updatedAt": NOW,
        }

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.params.get("cursor") == "c1":
            return httpx.Response(200, json={"items": [conv(2)]})
        return httpx.Response(200, json={"items": [conv(1)], "nextCursor": "c1"})

    client, seen = client_for(handler)
    page = client.list_conversations("box", limit=1, cursor=None)
    assert (page.next_cursor, [c.id for c in page.items]) == ("c1", ["conv_1"])
    assert [c.id for c in client.iter_conversations("box", limit=1)] == ["conv_1", "conv_2"]
    queries = [r.url.query for r in seen]
    assert queries == [b"limit=1", b"limit=1", b"limit=1&cursor=c1"]


def test_prompt_controls_shape_requests() -> None:
    def handler(r: httpx.Request) -> httpx.Response:
        if r.url.path.endswith("/steer"):
            return httpx.Response(200, json={"delivery": "restart", "prompt": prompt_json()})
        if r.url.path.endswith("/interrupt"):
            return httpx.Response(200, json={"cancelled": [prompt_json("cancelled")]})
        if r.url.path.endswith("/prompt-events"):
            return httpx.Response(200, json={"items": [], "nextCursor": "c2"})
        return httpx.Response(201, json=prompt_json("starting"))

    client, seen = client_for(handler)
    note = PromptAttachment(name="a.png", media_type="image/png", data="iVBO")
    client.create_prompt(
        "box", "hi", attachments=[note], reasoning_effort="high", turn_timeout_seconds=60
    )
    client.create_prompt("box", "hi", fast=False)
    assert json.loads(seen[0].content) == {
        "text": "hi",
        "attachments": [{"name": "a.png", "mediaType": "image/png", "data": "iVBO"}],
        "options": {"reasoningEffort": "high"},
        "turnTimeoutSeconds": 60,
    }
    assert json.loads(seen[1].content) == {"text": "hi", "options": {"fast": False}}

    assert client.steer_prompt("box", "pr_1", "more").delivery == "restart"
    assert client.interrupt_prompts("box", all=True).cancelled[0].state is PromptState.CANCELLED
    client.interrupt_prompts("box", conversation_id="conv_1")
    assert client.list_vm_prompt_events("box").next_cursor == "c2"
    client.list_vm_prompt_events("box", after="c1", conversation_id="conv_1", wait_ms=10, limit=2)
    calls = seen[2:]
    assert [(r.method, r.url.path) for r in calls] == [
        ("POST", "/api/v1/vms/box/prompts/pr_1/steer"),
        ("POST", "/api/v1/vms/box/interrupt"),
        ("POST", "/api/v1/vms/box/interrupt"),
        ("GET", "/api/v1/vms/box/prompt-events"),
        ("GET", "/api/v1/vms/box/prompt-events"),
    ]
    assert [json.loads(r.content) for r in calls[:3]] == [
        {"text": "more"},
        {"all": True},
        {"conversationId": "conv_1"},
    ]
    assert uuid.UUID(calls[0].headers["Idempotency-Key"]).version == 4
    assert all("Idempotency-Key" not in r.headers for r in calls[1:3])
    assert dict(calls[3].url.params) == {"waitMs": "0"}
    assert dict(calls[4].url.params) == {
        "after": "c1",
        "conversationId": "conv_1",
        "waitMs": "10",
        "limit": "2",
    }
    client.steer_prompt("box", "pr_1", "more", idempotency_key="steer-1")
    assert seen[-1].headers["Idempotency-Key"] == "steer-1"
    with pytest.raises(ValueError, match="exactly one"):
        client.interrupt_prompts("box")
    with pytest.raises(ValueError, match="exactly one"):
        client.interrupt_prompts("box", conversation_id="conv_1", all=True)
    with pytest.raises(ValueError, match="non-empty"):
        client.steer_prompt("box", "pr_1", "")


def test_follow_vm_prompt_events_keeps_the_cursor() -> None:
    pages = [
        {
            "items": [{"cursor": "c1", "event": event_json(1, "conversation.started")}],
            "nextCursor": "c1",
        },
        {"items": [], "nextCursor": "c1b"},
        {"items": [{"cursor": "c2", "event": event_json(2, "turn.completed")}], "nextCursor": "c2"},
    ]
    client, seen = client_for(lambda r: httpx.Response(200, json=pages[len(seen) - 1]))
    cursors = []
    for item in client.follow_vm_prompt_events("box", after="c0"):
        cursors.append(item.cursor)
        if item.event.type == "turn.completed":
            break
    assert cursors == ["c1", "c2"]
    assert [r.url.params.get("after") for r in seen] == ["c0", "c1", "c1b"]
    assert all(r.url.params["waitMs"] == "25000" for r in seen)
