"""Roundtrip against a real ``davit daemon`` with the fake runtime.

Skipped when bin/davit is missing (run ``make build`` or set DAVIT_BIN).
"""

from __future__ import annotations

import base64
import contextlib
import hashlib
import io
import os
import shutil
import socket
import stat
import subprocess
import tarfile
import tempfile
import threading
import time
import uuid
from collections.abc import Callable
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import TypeVar

import pytest

from conftest import run_daemon
from davit import (
    Client,
    Conflict,
    Egress,
    EgressMode,
    EnvManifest,
    FileContent,
    IdempotencyConflict,
    InvalidArgument,
    InvalidState,
    NotFound,
    OperationEvent,
    OperationInProgress,
    PromptAttachment,
    PromptEvent,
    PromptState,
    ReverseTargetKind,
    RevisionMismatch,
    ServerNotImplemented,
    StopRequired,
    Unavailable,
    UnsupportedCapability,
    VMState,
    WaitTimeout,
)
from davit.models import (
    BootMode,
    FeatureStatus,
    ProcessStatus,
    RunState,
    SetupAction,
    SteerSupport,
)
from davit.processes import exit_status

pytestmark = pytest.mark.integration

T = TypeVar("T")


def test_lifecycle_roundtrip(daemon: Client) -> None:
    sw = daemon
    assert sw.get_capabilities().runtime.engine == "fake"

    events: list[OperationEvent] = []
    web = sw.create_vm("web", on_progress=events.append)
    assert web.state == VMState.READY
    assert web.instance_generation == 1
    assert web.boot_mode == BootMode.COLD_BOOT
    assert events and [e.seq for e in events] == sorted(e.seq for e in events)

    with pytest.raises(StopRequired) as info:
        sw.fork_vm("web", "too-early")
    assert isinstance(info.value, Conflict) and info.value.status == 409
    assert info.value.request_id

    stopped = sw.stop_vm("web")
    assert stopped.state == VMState.STOPPED
    assert stopped.last_snapshot_id

    base = sw.create_snapshot("web", "base")
    assert base.name == "base" and base.source_vm_id == web.id
    assert sw.get_snapshot("base").id == base.id
    assert base.id in {s.id for s in sw.iter_vm_snapshots("web")}
    assert base.id in {s.id for s in sw.iter_snapshots(limit=1)}

    fork = sw.fork_vm("web", "web-fork")
    assert fork.state == VMState.READY and fork.source_snapshot_id
    from_base = sw.fork_vm("web", "web-base", snapshot="base")
    assert from_base.state == VMState.READY

    resumed = sw.resume_vm("web")
    assert resumed.state == VMState.READY and resumed.instance_generation == 2

    with pytest.raises(RevisionMismatch):
        sw.stop_vm("web", if_match=stopped.state_revision)

    live = {vm.name for vm in sw.iter_vms(limit=1)}
    assert live == {"web", "web-fork", "web-base"}

    for name in ("web-fork", "web-base", "web"):
        gone = sw.delete_vm(name)
        assert gone.state == VMState.DELETED
    with pytest.raises(NotFound):
        sw.get_vm("web")
    assert sw.get_vm(web.id).state == VMState.DELETED  # tombstone by ID
    assert list(sw.iter_vms()) == []

    sw.delete_snapshot("base")
    with pytest.raises(NotFound):
        sw.delete_snapshot("base")


def test_egress(daemon: Client) -> None:
    sw = daemon
    assert sw.create_vm("plain").egress.mode == EgressMode.FILTERED  # host default

    policy = Egress(
        mode=EgressMode.ALLOWLIST,
        allow=["GitHub.com", "10.9.1.2/16", "github.com", "*.Example.com"],
    )
    web = sw.create_vm("web", egress=policy)
    normalized = ["*.example.com", "10.9.0.0/16", "github.com"]
    assert web.egress == Egress(mode=EgressMode.ALLOWLIST, allow=normalized)

    sw.stop_vm("web")
    fork = sw.fork_vm("web", "web-fork")  # omitted: inherits the source's policy
    assert fork.egress == web.egress
    sealed = sw.fork_vm("web", "web-off", egress=Egress(mode=EgressMode.OFF))
    assert sealed.egress.mode == EgressMode.OFF and not sealed.egress.allow

    off = sw.set_vm_egress("web", "off")  # stopped: kept for the next boot
    assert off.id == web.id and off.egress.mode == EgressMode.OFF and not off.egress.allow
    assert sw.resume_vm("web").egress.mode == EgressMode.OFF
    back = sw.set_vm_egress("web", EgressMode.ALLOWLIST, allow=["github.com", "*.example.com"])
    assert back.egress.allow == ["*.example.com", "github.com"]  # running: live
    assert sw.get_vm("web").egress == back.egress

    with pytest.raises(InvalidArgument) as info:
        sw.set_vm_egress("web", "allowlist", allow=["not a domain!"])
    assert info.value.status == 400
    with pytest.raises(InvalidArgument):
        sw.set_vm_egress("web", "off", allow=["github.com"])  # allow needs allowlist
    with pytest.raises(InvalidArgument):
        sw.create_vm("bad", egress=Egress(mode=EgressMode.ALLOWLIST, allow=["300.1.1.1/8"]))
    with pytest.raises(TypeError):
        sw.set_vm_egress("web", "allowlist", allow="github.com")
    with pytest.raises(NotFound):
        sw.set_vm_egress("nope", "off")
    assert sw.get_vm("web").egress == back.egress  # rejected calls changed nothing


def test_idempotency(daemon: Client) -> None:
    sw = daemon
    key = str(uuid.uuid4())
    first = sw.create_vm("idem", idempotency_key=key, wait=False)
    again = sw.create_vm("idem", idempotency_key=key, wait=False)
    assert again.operation.id == first.operation.id
    with pytest.raises(IdempotencyConflict):
        sw.create_vm("idem-other", idempotency_key=key, wait=False)
    assert sw.wait(first.operation.id).state == "succeeded"
    assert sw.wait_ready("idem").state == VMState.READY


def test_tokens(daemon: Client) -> None:
    sw = daemon
    web = sw.create_vm("tok-web")
    key = str(uuid.uuid4())
    created = sw.create_token(
        "ci", actions=["exec"], vm="tok-web", ttl_seconds=3600, idempotency_key=key
    )
    assert created.secret.startswith("slw_") and created.secret not in repr(created)
    assert [(s.vm_id, s.actions) for s in created.scopes] == [(web.id, ["exec"])]
    replay = sw.create_token(
        "ci", actions=["exec"], vm="tok-web", ttl_seconds=3600, idempotency_key=key
    )
    assert (replay.id, replay.secret) == (created.id, created.secret)
    other = sw.create_token("all", actions=["view"], ttl_seconds=60)

    listed = {t.id: t for t in sw.iter_tokens(limit=1)}
    assert {created.id, other.id} <= set(listed)
    assert listed[created.id].name == "ci" and not hasattr(listed[created.id], "secret")
    assert listed[other.id].scopes[0].vm_id is None

    assert sw.revoke_token(created) is None
    with pytest.raises(NotFound):
        sw.revoke_token(created.id)
    assert created.id not in {t.id for t in sw.iter_tokens()}
    sw.revoke_token(other.id)

    with pytest.raises(InvalidArgument):
        sw.create_token("bad", actions=["root"], ttl_seconds=60)
    with pytest.raises(NotFound):
        sw.create_token("gone", actions=["view"], vm="no-such-vm", ttl_seconds=60)


def test_wait_timeout_never_cancels(slow_daemon: Client) -> None:
    sw = slow_daemon
    accepted = sw.create_vm("slow", wait=False)
    op_id = accepted.operation.id
    with pytest.raises(WaitTimeout) as info:
        sw.wait(op_id, timeout=0.2)
    assert info.value.operation_id == op_id
    assert op_id in str(info.value)
    done = sw.wait(op_id)
    assert done.state == "succeeded"
    assert sw.get_vm("slow").state == VMState.READY


def test_processes(guest_daemon: Client) -> None:
    sw = guest_daemon
    sw.create_vm("box")

    result = sw.run("box", ["sh", "-c", "echo hi; echo err >&2; exit 3"])
    assert result.exit_code == 3
    assert result.stdout == b"hi\n"
    assert result.stderr == b"err\n"
    assert result.process is not None
    assert result.process.status == ProcessStatus.EXITED

    echoed = sw.run("box", ["cat"], input=b"hello\x00world")
    assert (echoed.exit_code, echoed.stdout) == (0, b"hello\x00world")

    slow = sw.run("box", ["sleep", "30"], deadline_seconds=1)
    assert slow.exit_code == 124
    assert slow.timed_out

    script = "for i in 1 2 3; do echo line$i; sleep 0.1; done; exec sleep 60"
    proc = sw.exec_process("box", ["sh", "-c", script], detached=True)
    assert proc.detached
    assert proc.id in {p.id for p in sw.iter_processes("box")}
    assert sw.get_process("box", proc.id).status in ("starting", "running")

    with sw.attach_process("box", proc.id) as stream:
        seen = []
        for event in stream:
            seen.append(event)
            if b"".join(e.data for e in seen).count(b"\n") == 3:
                break
    assert b"".join(e.data for e in seen) == b"line1\nline2\nline3\n"
    first = seen[0].seq

    # Detached processes survive a closed stream; resume after the first chunk.
    assert sw.get_process("box", proc.id).status == "running"
    with sw.attach_process("box", proc.id, output_cursor=first) as stream:
        replay = []
        for event in stream:
            replay.append(event)
            if b"line3" in event.data or b"".join(e.data for e in replay).endswith(b"line3\n"):
                break
    assert all(e.seq > first for e in replay)
    assert b"".join(e.data for e in replay) == b"line2\nline3\n"

    killed = sw.kill_process("box", proc.id, signal="KILL")
    assert killed.id == proc.id
    with sw.attach_process("box", proc.id, output_cursor=replay[-1].seq) as stream:
        events = list(stream)
    assert [e.type for e in events] == ["exit"]
    final = events[-1].process
    assert final is not None
    assert final.status == ProcessStatus.KILLED
    assert exit_status(final) == 137


def _echo_server() -> socket.socket:
    """A TCP echo server on 127.0.0.1 (the fake guest's loopback is the host's)."""
    listener = socket.create_server(("127.0.0.1", 0))

    def serve() -> None:
        while True:
            try:
                conn, _ = listener.accept()
            except OSError:
                return
            with conn:
                while data := conn.recv(65536):
                    conn.sendall(data)
                conn.shutdown(socket.SHUT_WR)  # echo the half-close

    threading.Thread(target=serve, daemon=True).start()
    return listener


def test_tunnels(guest_daemon: Client) -> None:
    sw = guest_daemon
    sw.create_vm("box")
    listener = _echo_server()
    port = listener.getsockname()[1]
    try:
        payload = os.urandom(200_000)
        with sw.connect_tunnel("box", port) as conn:
            assert conn.tunnel is not None and conn.tunnel.port == port
            conn.send(payload)
            conn.close_write()
            assert b"".join(conn) == payload  # half-close reached the port; EOF came back

        idle = sw.open_tunnel("box", port)
        sw.close_tunnel(idle)
        with pytest.raises(NotFound):
            sw.attach_tunnel(idle)  # closed tunnels are gone; tunnels are single use
    finally:
        listener.close()

    with socket.create_server(("127.0.0.1", 0)) as unused:
        closed = unused.getsockname()[1]
    with pytest.raises(Unavailable):
        sw.open_tunnel("box", closed)  # nothing listens there


def _recv_all(sock: socket.socket) -> bytes:
    data = b""
    while chunk := sock.recv(65536):
        data += chunk
    return data


def test_reverse_forward(guest_daemon: Client) -> None:
    sw = guest_daemon
    features = {f.name: f.status for f in sw.get_capabilities().features}
    if features.get("reverseForward") != FeatureStatus.AVAILABLE:
        pytest.skip(f"reverseForward is {features.get('reverseForward')} on this daemon")
    sw.create_vm("box")
    listener = _echo_server()
    echo_port = listener.getsockname()[1]
    with socket.create_server(("127.0.0.1", 0)) as unused:
        guest_port = unused.getsockname()[1]
    try:
        with sw.reverse_forward("box", guest_port, "127.0.0.1", echo_port) as session:
            fwd = session.forward
            assert fwd.guest_port == guest_port and fwd.target.kind == ReverseTargetKind.SESSION
            assert fwd.target.address == f"127.0.0.1:{echo_port}"
            assert [f.id for f in sw.list_reverse_forwards("box").items] == [fwd.id]

            # A "guest process": the fake guest's loopback is the host's.
            payload = os.urandom(200_000)
            with socket.create_connection(("127.0.0.1", guest_port), timeout=10) as guest:
                guest.sendall(payload)
                guest.shutdown(socket.SHUT_WR)  # half-close reaches the echo server
                assert _recv_all(guest) == payload  # and its EOF comes back

            with socket.create_connection(("127.0.0.1", guest_port), timeout=10) as guest:
                guest.sendall(b"ping")
                got = b""
                while len(got) < 4:
                    chunk = guest.recv(4 - len(got))
                    assert chunk, "connection ended before the echo"
                    got += chunk
                assert got == b"ping"
                sw.revoke_reverse_forward("box", fwd)
                with contextlib.suppress(ConnectionResetError):
                    assert _recv_all(guest) == b""  # the revoke cut the live connection
            reason = session.wait(10)
            assert session.closed and reason and session.error is None
        assert sw.list_reverse_forwards("box").items == []
    finally:
        listener.close()


def _fingerprint(key_line: str) -> str:
    """OpenSSH ``SHA256:`` fingerprint of an authorized_keys-style line."""
    blob = base64.b64decode(key_line.split()[1])
    return "SHA256:" + base64.b64encode(hashlib.sha256(blob).digest()).decode().rstrip("=")


@pytest.mark.skipif(shutil.which("ssh-keygen") is None, reason="ssh-keygen not installed")
def test_ssh_credentials(guest_daemon: Client, tmp_path: Path) -> None:
    sw = guest_daemon
    vm = sw.create_vm("box")
    key = tmp_path / "id"
    subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(key)], check=True)
    public_key = (tmp_path / "id.pub").read_text().strip()

    cred = sw.issue_ssh_credential("box", public_key, ttl_seconds=60)
    assert cred.certificate.startswith("ssh-ed25519-cert-v01@openssh.com ")
    assert cred.generation == vm.instance_generation
    assert cred.host_keys
    pinned = sw.get_vm("box").endpoint.ssh_host_key_fingerprints or []
    assert {_fingerprint(k) for k in cred.host_keys} <= set(pinned)

    with pytest.raises(InvalidArgument):
        sw.issue_ssh_credential("box", "not a key")


def test_files(guest_daemon: Client) -> None:
    sw = guest_daemon
    vm = sw.create_vm("files")
    # The fake guest is the host: guest paths are host paths. Keep them private.
    with tempfile.TemporaryDirectory(prefix="swpy-f-") as tmp:
        target = os.path.join(tmp, "a.bin")
        info = sw.write_file("files", target, b"hello\x00world", mode=0o600)
        assert (info.path, info.size_bytes, info.mode, info.generation) == (target, 11, "0600", 1)
        assert info.sha256 == hashlib.sha256(b"hello\x00world").hexdigest()
        assert stat.S_IMODE(os.stat(target).st_mode) == 0o600
        got = sw.read_file("files", target, generation=vm.instance_generation)
        assert (got.data, got.mode) == (b"hello\x00world", "0600")

        sw.write_file("files", target, "replaced")  # no mode: keeps 0600
        assert sw.read_file("files", target) == FileContent(b"replaced", "0600")

        with pytest.raises(NotFound):
            sw.read_file("files", os.path.join(tmp, "missing"))
        with pytest.raises(InvalidState):
            sw.read_file("files", target, generation=vm.instance_generation + 1)

        # Streamed upload and streamed read of a few MiB.
        blob = os.path.join(tmp, "blob")
        chunk = bytes(range(256)) * 4096  # 1 MiB
        big = sw.write_file("files", blob, (chunk for _ in range(5)))
        assert big.size_bytes == 5 << 20
        digest = hashlib.sha256()
        with sw.read_file_stream("files", blob) as stream:
            assert stream.content_length == 5 << 20
            for part in stream:
                digest.update(part)
        assert stream.received == 5 << 20
        assert digest.hexdigest() == big.sha256

        # Directory round trip through tar.
        src = os.path.join(tmp, "src")
        os.makedirs(os.path.join(src, "sub"))
        with open(os.path.join(src, "sub", "f.txt"), "wb") as f:
            f.write(b"nested")
        os.chmod(os.path.join(src, "sub", "f.txt"), 0o640)
        os.symlink("sub/f.txt", os.path.join(src, "link"))
        dst = os.path.join(tmp, "dst")
        up = sw.upload_dir("files", src, dst)
        assert up.archive
        assert Path(dst, "sub", "f.txt").read_bytes() == b"nested"

        raw = sw.read_file("files", dst, archive=True).data
        assert raw.endswith(b"\0" * 1024)
        with tarfile.open(fileobj=io.BytesIO(raw)) as tar:
            names = set(tar.getnames())
        assert {"sub", "sub/f.txt", "link"} <= {n.removeprefix("./") for n in names}

        back = os.path.join(tmp, "back")
        sw.download_dir("files", dst, back)
        assert Path(back, "sub", "f.txt").read_bytes() == b"nested"
        assert stat.S_IMODE(os.stat(os.path.join(back, "sub", "f.txt")).st_mode) == 0o640
        assert os.readlink(os.path.join(back, "link")) == "sub/f.txt"


def test_prompts_unsupported_without_guest_transport(daemon: Client) -> None:
    daemon.create_vm("nog")
    with pytest.raises(UnsupportedCapability):
        daemon.create_prompt("nog", "hello")
    with pytest.raises(UnsupportedCapability):
        daemon.list_conversations("nog")


def test_app_endpoints_unsupported_without_gateway(daemon: Client) -> None:
    features = {f.name: f.status for f in daemon.get_capabilities().features}
    assert features.get("appEndpoints") == FeatureStatus.UNSUPPORTED
    daemon.create_vm("app")
    with pytest.raises(UnsupportedCapability) as exc:
        daemon.expose("app", 3000)
    assert exc.value.status == 422 and exc.value.code == "unsupported_capability"


def test_members_unsupported_without_gateway(daemon: Client) -> None:
    features = {f.name: f.status for f in daemon.get_capabilities().features}
    assert features.get("members") == FeatureStatus.UNSUPPORTED
    with pytest.raises(UnsupportedCapability) as exc:
        daemon.add_member("alice@example.com", "viewer")
    assert exc.value.status == 422 and exc.value.code == "unsupported_capability"


def test_desktop_unsupported_without_gateway(daemon: Client) -> None:
    features = {f.name: f.status for f in daemon.get_capabilities().features}
    assert features.get("desktop") == FeatureStatus.UNSUPPORTED
    daemon.create_vm("desk")
    with pytest.raises(UnsupportedCapability) as exc:
        daemon.open_desktop("desk")
    assert exc.value.status == 422 and exc.value.code == "unsupported_capability"


def test_prompts(prompt_daemon: Client) -> None:
    sw = prompt_daemon
    sw.create_vm("box")

    events: list[PromptEvent] = []
    result = sw.run_prompt("box", "hello", on_event=events.append)
    first = result.prompt
    assert first.state == PromptState.SUCCEEDED
    assert result.events == events
    assert "echo: hello resumed=none" in result.text
    assert first.usage is not None
    assert (first.usage.input_tokens, first.usage.output_tokens) == (3, 4)
    kinds = [e.type for e in events]
    assert kinds[-1] == "turn.completed"
    assert "conversation.started" in kinds
    assert [e.seq for e in events] == sorted({e.seq for e in events})
    text = "".join(e.text or "" for e in events if e.type == "message.delta")
    assert "echo: hello resumed=none" in text
    tool = next(e for e in events if e.type == "tool.call")
    assert (tool.tool_name, tool.input) == ("Bash", {"command": "ls"})

    # The next turn resumes the native session; env reaches the harness.
    events.clear()
    second = sw.run_prompt(
        "box",
        "again",
        conversation_id=first.conversation_id,
        env={"FAKE_TOKEN": "tok"},
        on_event=events.append,
    )
    assert second.prompt.conversation_id == first.conversation_id
    text = "".join(e.text or "" for e in events if e.type == "message.delta")
    assert "resumed=sess-" in text
    assert "token=tok" in text
    assert second.text == text

    failed = sw.run_prompt("box", "please fail", new=True).prompt
    assert failed.state == PromptState.FAILED
    assert failed.error is not None
    assert failed.error.code == "harness_failed"

    codex = sw.run_prompt("box", "hi", harness="codex").prompt
    assert codex.harness == "codex"

    # One turn at a time; a short budget times out without cancelling; cancel ends it.
    slow = sw.create_prompt("box", "slow", conversation_id=first.conversation_id)
    with pytest.raises(OperationInProgress):
        sw.create_prompt("box", "too soon", conversation_id=first.conversation_id)
    with pytest.raises(WaitTimeout) as info:
        list(sw.follow_prompt("box", slow.id, timeout=0.2))
    assert info.value.details["promptId"] == slow.id
    assert sw.get_prompt("box", slow.id).state in (PromptState.STARTING, PromptState.RUNNING)
    sw.cancel_prompt("box", slow.id)
    assert sw.wait_prompt("box", slow.id, timeout=30).state == PromptState.CANCELLED

    convs = sw.list_conversations("box").items
    assert len(convs) == 3
    assert convs[0].id == first.conversation_id  # (createdAt, id): oldest first
    main = sw.get_conversation("box", first.conversation_id)
    assert main.turns >= 3
    assert main.session_id


def _while_starting(call: Callable[[], T]) -> T:
    """Retry ``call`` while the turn is still starting (OperationInProgress)."""
    for _ in range(100):
        try:
            return call()
        except OperationInProgress:
            time.sleep(0.05)
    return call()


def test_vm_prompt_feed(prompt_daemon: Client) -> None:
    sw = prompt_daemon
    sw.create_vm("feed")
    one = sw.run_prompt("feed", "first", new=True)
    two = sw.run_prompt("feed", "second", new=True)
    full = sw.list_vm_prompt_events("feed")
    assert [i.event.seq for i in full.items] == [e.seq for e in one.events + two.events]
    assert full.next_cursor

    # Page with a small limit, resuming from next_cursor each time.
    paged: list[str] = []
    after: str | None = None
    for _ in range(50):
        page = sw.list_vm_prompt_events("feed", after=after, limit=3)
        assert len(page.items) <= 3
        assert page.next_cursor, "next_cursor is set on every page, also an empty one"
        paged += [i.cursor for i in page.items]
        after = page.next_cursor
        if not page.items:
            break
    assert paged == [i.cursor for i in full.items]
    empty = sw.list_vm_prompt_events("feed", after=full.next_cursor)
    assert empty.items == []
    assert empty.next_cursor

    only = sw.list_vm_prompt_events("feed", conversation_id=two.prompt.conversation_id)
    assert [i.event.seq for i in only.items] == [e.seq for e in two.events]

    # Follow from the end: a new turn's events arrive, ending with turn.completed.
    third = sw.create_prompt("feed", "third", conversation_id=one.prompt.conversation_id)
    kinds: list[str] = []
    for item in sw.follow_vm_prompt_events("feed", after=full.next_cursor):
        assert item.event.prompt_id == third.id
        kinds.append(item.event.type)
        if item.event.type == "turn.completed":
            break
    assert "message.delta" in kinds


def test_prompt_interrupt_steer_and_options(prompt_daemon: Client) -> None:
    sw = prompt_daemon
    sw.create_vm("ctl")

    # interrupt_prompts(all=True) cancels a slow turn; a repeat cancels nothing.
    with pytest.raises(ValueError, match="exactly one"):
        sw.interrupt_prompts("ctl")
    slow = sw.create_prompt("ctl", "slow interrupt", new=True)
    res = _while_starting(lambda: sw.interrupt_prompts("ctl", all=True))
    assert [p.id for p in res.cancelled] == [slow.id]
    assert sw.wait_prompt("ctl", slow.id, timeout=30).state == PromptState.CANCELLED
    assert sw.interrupt_prompts("ctl", all=True).cancelled == []
    assert sw.interrupt_prompts("ctl", conversation_id=slow.conversation_id).cancelled == []

    # A running claude turn takes a steer natively; the same turn answers it.
    slow = sw.create_prompt("ctl", "slow steer", new=True)
    steer = _while_starting(lambda: sw.steer_prompt("ctl", slow.id, "also this"))
    assert steer.delivery == SteerSupport.NATIVE
    assert steer.prompt.id == slow.id
    events: list[PromptEvent] = []
    done = sw.wait_prompt("ctl", slow.id, timeout=30, on_event=events.append)
    assert done.state == PromptState.SUCCEEDED
    assert "echo: also this" in "".join(e.text or "" for e in events if e.type == "message.delta")
    with pytest.raises(InvalidState):
        sw.steer_prompt("ctl", slow.id, "too late")

    # Inline attachment, reasoning effort and fast mode reach the harness.
    claude = next(h for h in sw.get_capabilities().harnesses if h.name == "claude")
    assert "text/plain" in claude.attachment_types
    note = PromptAttachment(
        name="note.txt",
        media_type="text/plain",
        data=base64.b64encode(b"hello note\n").decode(),
    )
    result = sw.run_prompt(
        "ctl",
        "look at the note",
        new=True,
        attachments=[note],
        reasoning_effort="high",
        fast=True,
        turn_timeout_seconds=600,
    )
    assert result.prompt.state == PromptState.SUCCEEDED, result.prompt.error
    assert "effort=high fast=yes" in result.text
    assert result.prompt.turn_deadline_at is not None


def test_prompt_idempotency_and_conversation_paging(prompt_daemon: Client) -> None:
    sw = prompt_daemon
    sw.create_vm("idem")
    key = str(uuid.uuid4())
    first = sw.create_prompt("idem", "hello", new=True, idempotency_key=key)
    again = sw.create_prompt("idem", "hello", new=True, idempotency_key=key)
    assert again.id == first.id
    assert again.conversation_id == first.conversation_id
    with pytest.raises(IdempotencyConflict):
        sw.create_prompt("idem", "different", new=True, idempotency_key=key)
    final = sw.wait_prompt("idem", first.id, timeout=30)
    assert final.state == PromptState.SUCCEEDED
    events = list(sw.follow_prompt("idem", first.id))
    assert [e.type for e in events].count("turn.completed") == 1
    assert sw.get_conversation("idem", first.conversation_id).turns == 1

    second = sw.run_prompt("idem", "another", new=True).prompt
    assert second.conversation_id != first.conversation_id
    page = sw.list_conversations("idem", limit=1)
    assert [c.id for c in page.items] == [first.conversation_id]
    assert page.next_cursor
    rest = sw.list_conversations("idem", limit=1, cursor=page.next_cursor)
    assert [c.id for c in rest.items] == [second.conversation_id]
    if rest.next_cursor:
        assert sw.list_conversations("idem", limit=1, cursor=rest.next_cursor).items == []
    ids = [c.id for c in sw.iter_conversations("idem", limit=1)]
    assert ids == [first.conversation_id, second.conversation_id]


PROFILES = [{"name": "big", "shape": {"cpus": 2, "memoryBytes": 1 << 30, "diskBytes": 0}}]


def test_update_extend_rename_and_profiles() -> None:
    with run_daemon(extra={"profiles": PROFILES, "ttlWarning": "5m"}) as sw:
        caps = sw.get_capabilities()
        assert [(p.name, p.shape.cpus, p.shape.memory_bytes) for p in caps.profiles] == [
            ("big", 2, 1 << 30)
        ]
        assert caps.ttl_warning_seconds == 300

        big = sw.create_vm("big-one", type="big")
        assert (big.shape.cpus, big.shape.memory_bytes) == (2, 1 << 30)
        with pytest.raises(InvalidArgument):
            sw.create_vm("nope", type="no-such-profile")

        vm = sw.create_vm("web", ttl=600)
        assert vm.expires_at is not None
        assert vm.auto_stop_warning_at is not None
        assert vm.expires_at - vm.auto_stop_warning_at == timedelta(seconds=300)

        time.sleep(1.1)
        extended = sw.extend_vm("web")
        assert extended.expires_at is not None
        assert extended.expires_at > vm.expires_at  # the countdown restarted from now
        longer = sw.extend_vm("web", timedelta(hours=3))
        assert longer.ttl_seconds == 3 * 3600
        assert longer.expires_at is not None
        assert longer.expires_at - datetime.now(timezone.utc) > timedelta(hours=2, minutes=59)

        with pytest.raises(RevisionMismatch):
            sw.rename_vm("web", "web-new", if_match=vm.state_revision - 1)
        renamed = sw.rename_vm("web", "web-new", if_match=longer.state_revision)
        assert renamed.name == "web-new"
        assert sw.get_vm("web-new").id == vm.id
        with pytest.raises(NotFound):
            sw.get_vm("web")


def test_reclaim_vms_and_capacity_wait(daemon: Client) -> None:
    sw = daemon
    idle = sw.create_vm("idle", labels={"team": "a"}, capacity_wait=5)  # room: no wait
    sw.create_vm("other", labels={"team": "b"})

    listed = sw.reclaim_vms(0, labels={"team": "a"}, dry_run=True)
    assert [(i.vm_id, i.name, i.operation_id) for i in listed.items] == [(idle.id, "idle", None)]
    assert listed.items[0].memory_bytes == idle.shape.memory_bytes
    assert sw.get_vm("idle").state == VMState.READY  # a dry run stops nothing
    assert sw.reclaim_vms(timedelta(hours=1), dry_run=True).items == []
    with pytest.raises(InvalidArgument):
        sw.reclaim_vms(-1)

    (stopped,) = sw.reclaim_vms(0, labels={"team": "a"}).items
    assert stopped.vm_id == idle.id and stopped.operation_id
    op = sw.wait(stopped.operation_id)
    assert op.state == "succeeded" and op.reason == "gc"
    assert sw.get_vm("idle").state == VMState.STOPPED
    assert sw.get_vm("other").state == VMState.READY  # other labels: kept
    assert sw.resume_vm("idle", capacity_wait=timedelta(seconds=5)).state == VMState.READY
    with pytest.raises(ValueError, match="capacity_wait"):
        sw.create_vm("too-long", capacity_wait=3601)


def test_list_processes_pages_oldest_first(guest_daemon: Client) -> None:
    sw = guest_daemon
    sw.create_vm("pp")
    started = [sw.exec_process("pp", ["true"], detached=True).id for _ in range(3)]
    page = sw.list_processes("pp", limit=2)
    assert [p.id for p in page.items] == started[:2]
    assert page.next_cursor
    rest = sw.list_processes("pp", limit=2, cursor=page.next_cursor)
    assert [p.id for p in rest.items] == started[2:]
    assert [p.id for p in sw.iter_processes("pp", limit=1)] == started


def test_environments(daemon: Client) -> None:
    sw = daemon
    manifest = EnvManifest(
        repos=[],
        variables={"A": "1"},
        secret_files=[],
        setup=[SetupAction(name="hello", argv=["true"])],
    )
    env = sw.create_environment("dev", manifest)
    assert (env.name, env.latest_revision) == ("dev", 1) and env.id.startswith("env_")
    assert sw.create_environment("dev", manifest).id == env.id  # same name + manifest replays
    with pytest.raises(InvalidArgument):
        sw.create_environment("dev", {"variables": {"A": "other"}})
    assert sw.get_environment(env.id).name == "dev"
    assert [e.id for e in sw.iter_environments(limit=1)] == [env.id]

    rev2 = sw.create_environment_revision("dev", {"variables": {"A": "2"}}, if_match=1)
    assert rev2.revision == 2 and rev2.manifest.variables == {"A": "2"}
    with pytest.raises(RevisionMismatch) as info:
        sw.create_environment_revision("dev", {"variables": {"A": "3"}}, if_match=1)
    assert info.value.status == 412 and info.value.details["latestRevision"] == 2

    key = str(uuid.uuid4())
    rev3 = sw.create_environment_revision(env.id, {"setup": []}, if_match=2, idempotency_key=key)
    again = sw.create_environment_revision("dev", {"setup": []}, idempotency_key=key)
    assert (rev3.revision, again.revision) == (3, 3)  # the key replays the revision it minted

    assert [r.revision for r in sw.iter_environment_revisions("dev", limit=2)] == [1, 2, 3]
    assert sw.get_environment_revision("dev", 1).manifest.setup[0].name == "hello"
    assert sw.get_environment("dev").latest_revision == 3

    # The test daemon has no workspace preparer: pinning an environment is typed 501.
    with pytest.raises(ServerNotImplemented) as nie:
        sw.create_vm("ws", environment="dev@2")
    assert nie.value.status == 501

    sw.delete_environment("dev")
    with pytest.raises(NotFound):
        sw.get_environment("dev")
    with pytest.raises(NotFound):
        sw.delete_environment(env)
    assert list(sw.iter_environments()) == []


def test_job_history_readable_while_stopped(guest_daemon: Client) -> None:
    sw = guest_daemon
    sw.create_vm("jobs")
    proc = sw.exec_process("jobs", ["sh", "-c", "echo hi"], detached=True)
    deadline = time.monotonic() + 30
    while sw.get_process("jobs", proc.id).status in (ProcessStatus.STARTING, ProcessStatus.RUNNING):
        assert time.monotonic() < deadline, "job never exited"
        time.sleep(0.05)
    assert sw.stop_vm("jobs").state == VMState.STOPPED

    (entry,) = [e for e in sw.iter_process_history("jobs", limit=1) if e.process.id == proc.id]
    assert entry.process.status == ProcessStatus.EXITED
    assert (entry.stdout_bytes, entry.stderr_bytes, entry.output_dropped) == (3, 0, False)

    out = sw.collect_process_output("jobs", proc.id)
    assert (out.stdout, out.stderr, out.complete) == (b"hi\n", b"", True)
    assert out.cursor
    chunks = list(sw.iter_process_output("jobs", proc.id, stream="stdout"))
    assert b"".join(c.data for c in chunks) == b"hi\n" and {c.type for c in chunks} == {"stdout"}
    assert list(sw.iter_process_output("jobs", proc.id, stream="stderr")) == []
    rest = sw.read_process_output("jobs", proc.id, after=out.cursor)
    assert rest.items == [] and rest.complete
    with pytest.raises(NotFound):
        sw.read_process_output("jobs", "proc_missing")


def _until_decided(sw: Client, run_id: str) -> None:
    deadline = time.monotonic() + 30
    while True:
        r = sw.get_run(run_id)
        if r.ended_at or r.exit_code is not None or r.error:
            return
        assert time.monotonic() < deadline, f"run {run_id} never decided: {r}"
        time.sleep(0.05)


def test_runs(guest_daemon: Client) -> None:
    sw = guest_daemon
    assert sw.socket is not None
    host_dir = os.path.join(os.path.dirname(sw.socket), "tree")  # fake guests run on the host

    # A failing command: the outcome is kept, cancel only ends the output grace.
    failed = sw.create_run(["sh", "-c", "exit 3"])
    assert failed.state == RunState.CREATING and not failed.keep
    _until_decided(sw, failed.id)
    sw.cancel_run(failed.id)
    done = sw.wait_run(failed.id, timeout=60)
    assert (done.state, done.exit_code) == (RunState.FAILED, 3)
    assert done.ended_at is not None and done.vm_id
    assert sw.get_vm(done.vm_id).state == VMState.DELETED
    assert sw.cancel_run(failed.id).state == RunState.FAILED  # repeating is harmless

    # keep leaves the VM (no grace without a delete).
    kept = sw.wait_run(sw.create_run(["true"], keep=True), timeout=60)
    assert (kept.state, kept.exit_code, kept.keep) == (RunState.SUCCEEDED, 0, True)
    assert kept.vm_id and sw.get_vm(kept.vm_id).state in (VMState.READY, VMState.RUNNING)
    with pytest.raises(ServerNotImplemented):
        sw.push_tree(kept.vm_id, host_dir, _tar({"x": b"x"}), mode="mirror")

    # hold, push, start: the command reads the pushed file.
    out = sw.run_to_completion(
        ["cat", "hello.txt"], files=_tar({"hello.txt": b"hi\n"}), files_path=host_dir
    )
    assert out.exit_code == 0 and out.stdout == b"hi\n" and out.stderr == b""
    assert out.push is not None and out.push.files == 1 and out.push.path == host_dir
    assert out.run.state == RunState.SUCCEEDED and out.run.ended_at is not None
    assert out.run.vm_id and sw.get_vm(out.run.vm_id).state == VMState.DELETED

    # The same steps by hand: a held run waits for start_run.
    held = sw.create_run(["cat", "again.txt"], hold=True, cwd=host_dir)
    deadline = time.monotonic() + 30
    while (held := sw.get_run(held.id)).state != RunState.WAITING:
        assert time.monotonic() < deadline and not held.ended_at, held
        time.sleep(0.05)
    assert held.vm_id
    pushed = sw.push_tree(held.vm_id, host_dir, _tar({"again.txt": b"again\n"}))
    assert pushed.files == 1 and pushed.sha256
    assert sw.start_run(held).state == RunState.RUNNING
    with pytest.raises(InvalidState):
        sw.start_run(held)
    _until_decided(sw, held.id)
    held = sw.get_run(held.id)
    assert held.process_id
    assert sw.collect_process_output(held.vm_id, held.process_id).stdout == b"again\n"
    sw.cancel_run(held)
    assert sw.wait_run(held, timeout=60).state == RunState.SUCCEEDED


def _tar(files: dict[str, bytes]) -> bytes:
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w", format=tarfile.PAX_FORMAT) as tar:
        for name, data in files.items():
            info = tarfile.TarInfo(name)
            info.size, info.mode = len(data), 0o644
            tar.addfile(info, io.BytesIO(data))
    return buf.getvalue()


def test_run_without_output_grace(guest_daemon: Client) -> None:
    sw = guest_daemon
    run = sw.create_run(["sh", "-c", "echo done"], output_grace_seconds=0)
    done = sw.wait_run(run, timeout=60)  # no cancel_run: nothing holds the VM
    assert (done.state, done.outcome, done.exit_code) == (RunState.SUCCEEDED,) * 2 + (0,)
    assert done.decided_at is not None and done.ended_at is not None
    assert (done.ended_at - done.decided_at).total_seconds() < 4  # below the 5 s default grace
    assert done.vm_id and sw.get_vm(done.vm_id).state == VMState.DELETED
    assert done.signal is None and done.error is None
    listed = {r.id: r for r in sw.iter_runs(limit=1)}
    assert listed[run.id].outcome == RunState.SUCCEEDED

    # A signal kill fails with no error; the helper reports 128+n.
    # With no grace the VM may be gone before the attach: the helper then reads
    # the host-retained output.
    script = "echo out; echo err >&2; kill -KILL $$"
    killed = sw.run_to_completion(["sh", "-c", script], output_grace_seconds=0)
    assert (killed.stdout, killed.stderr) == (b"out\n", b"err\n")
    assert killed.run.state == RunState.FAILED and killed.run.error is None
    assert killed.signal and killed.run.signal
    assert killed.exit_code == 137
    assert [r.id for r in sw.list_runs().items][-2:] == [run.id, killed.run.id]  # oldest first
