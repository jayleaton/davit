"""Remote endpoint configuration: $DAVIT_TOKEN and $DAVIT_CA_FILE (private-CA TLS)."""

from __future__ import annotations

import contextlib
import json
import os
import shutil
import socket
import ssl
import subprocess
import sys
import threading
from collections.abc import Iterator
from pathlib import Path

import httpx
import pytest

from davit import CA_FILE_ENV, TOKEN_ENV, Client, TransportError
from wsserver import Peer


@pytest.fixture(autouse=True)
def _clean_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("DAVIT_API_URL", raising=False)  # conftest clears the other two


def recording_client(**kwargs: object) -> tuple[Client, list[httpx.Request]]:
    seen: list[httpx.Request] = []

    def record(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        return httpx.Response(200, json={"items": []})

    return Client(transport=httpx.MockTransport(record), **kwargs), seen  # type: ignore[arg-type]


def test_env_names() -> None:
    assert (TOKEN_ENV, CA_FILE_ENV) == ("DAVIT_TOKEN", "DAVIT_CA_FILE")


def test_env_token_for_https_base_url(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv(TOKEN_ENV, "slw_env")
    client, seen = recording_client(base_url="https://davit-host.example.ts.net")
    with client:
        client.list_vms()
    assert seen[0].headers["Authorization"] == "Bearer slw_env"
    # From $DAVIT_API_URL as well.
    monkeypatch.setenv("DAVIT_API_URL", "https://davit-host:8443")
    client, seen = recording_client()
    with client:
        client.list_vms()
    assert seen[0].headers["Authorization"] == "Bearer slw_env"


def test_explicit_token_wins(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv(TOKEN_ENV, "slw_env")
    client, seen = recording_client(base_url="https://h", token="slw_arg")
    with client:
        client.list_vms()
    assert seen[0].headers["Authorization"] == "Bearer slw_arg"
    client, seen = recording_client(base_url="https://h", token="")
    with client:
        client.list_vms()
    assert "Authorization" not in seen[0].headers


def test_env_token_never_for_unix_socket(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv(TOKEN_ENV, "slw_env")
    client, seen = recording_client(socket="/run/x/davit.sock")
    with client:
        client.list_vms()
    assert "Authorization" not in seen[0].headers
    monkeypatch.setenv("DAVIT_API_URL", "unix:///run/x/davit.sock")
    client, seen = recording_client()
    with client:
        client.list_vms()
    assert "Authorization" not in seen[0].headers


def test_ca_file_errors(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    with pytest.raises(ValueError, match=r"ca_file .*missing\.pem.*cannot read"):
        Client(base_url="https://h", ca_file=str(tmp_path / "missing.pem"))
    junk = tmp_path / "junk.pem"
    junk.write_text("not a certificate\n")
    with pytest.raises(ValueError, match="no PEM certificate"):
        Client(base_url="https://h", ca_file=str(junk))
    broken = tmp_path / "broken.pem"
    broken.write_text("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")
    with pytest.raises(ValueError, match="invalid PEM"):
        Client(base_url="https://h", ca_file=str(broken))
    monkeypatch.setenv(CA_FILE_ENV, str(tmp_path / "missing.pem"))
    with pytest.raises(ValueError, match=r"DAVIT_CA_FILE .*cannot read"):
        Client(base_url="https://h")
    # The local socket ignores the environment and rejects an explicit CA file.
    Client(socket="/run/x/davit.sock").close()
    with pytest.raises(ValueError, match="not to the local socket"):
        Client(socket="/run/x/davit.sock", ca_file=str(junk))


# ---- a real TLS server with a throwaway private CA ------------------------


def _openssl(*args: str, cwd: Path) -> None:
    subprocess.run(["openssl", *args], cwd=cwd, check=True, capture_output=True, timeout=60)


@pytest.fixture(scope="module")
def certs(tmp_path_factory: pytest.TempPathFactory) -> Path:
    """``ca.pem`` and ``server.pem``/``server.key`` (SAN 127.0.0.1, localhost) via openssl."""
    if shutil.which("openssl") is None:
        pytest.skip("openssl not installed")
    d = tmp_path_factory.mktemp("tls")
    ec = ("-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes")
    _openssl(
        "req", "-x509", *ec, "-keyout", "ca.key", "-out", "ca.pem", "-days", "2",
        "-subj", "/CN=davit test CA",
        "-addext", "basicConstraints=critical,CA:TRUE",
        "-addext", "keyUsage=critical,keyCertSign,cRLSign",
        cwd=d,
    )  # fmt: skip
    _openssl(
        "req", "-new", *ec, "-keyout", "server.key", "-out", "server.csr",
        "-subj", "/CN=127.0.0.1",
        cwd=d,
    )  # fmt: skip
    (d / "ext.cnf").write_text(
        "basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\n"
        "extendedKeyUsage=serverAuth\nsubjectAltName=IP:127.0.0.1,DNS:localhost\n"
        "subjectKeyIdentifier=hash\nauthorityKeyIdentifier=keyid\n"
    )
    _openssl(
        "x509", "-req", "-in", "server.csr", "-CA", "ca.pem", "-CAkey", "ca.key",
        "-CAcreateserial", "-out", "server.pem", "-days", "2", "-extfile", "ext.cnf",
        cwd=d,
    )  # fmt: skip
    return d


REVERSE_FORWARD = {
    "id": "rev_1",
    "vmId": "vm_1",
    "generation": 1,
    "guestPort": 5432,
    "target": {"kind": "session", "address": "127.0.0.1:5432"},
    "state": "awaiting_session",
    "active": 0,
    "createdAt": "2026-10-22T04:22:34Z",
}


class TLSServer:
    """Answers plain requests with ``{"items": []}`` and upgrades WebSockets, then closes."""

    def __init__(self, certs: Path) -> None:
        self.context = ssl.create_default_context(ssl.Purpose.CLIENT_AUTH)
        self.context.load_cert_chain(certs / "server.pem", certs / "server.key")
        self.listener = socket.create_server(("127.0.0.1", 0))
        self.url = f"https://127.0.0.1:{self.listener.getsockname()[1]}"
        self.requests: list[Peer] = []
        self.thread = threading.Thread(target=self._serve, daemon=True)
        self.thread.start()

    def _serve(self) -> None:
        while True:
            try:
                conn, _ = self.listener.accept()
            except OSError:
                return
            threading.Thread(target=self._one, args=(conn,), daemon=True).start()

    def _one(self, conn: socket.socket) -> None:
        conn.settimeout(10)
        try:
            tls = self.context.wrap_socket(conn, server_side=True)
        except (OSError, ssl.SSLError):
            conn.close()  # the client rejected our certificate
            return
        peer = Peer(tls)
        try:
            peer.read_request()
            self.requests.append(peer)
            if peer.headers.get("upgrade", "").lower() == "websocket":
                peer.accept()
                if peer.path.endswith("/session"):  # a reverse session the server revokes
                    peer.text({"type": "revoked", "reason": "test over tls"})
                peer.frame(0x8, b"\x03\xe8")  # normal close: EOF
                peer.recv_frame()
                return
            body = b'{"items":[]}'
            status = "200 OK"
            if peer.headers.get("content-length"):
                peer.read_exact(int(peer.headers["content-length"]))
                if peer.path.endswith("/reverse-forwards"):
                    status, body = "201 Created", json.dumps(REVERSE_FORWARD).encode()
            tls.sendall(
                f"HTTP/1.1 {status}\r\nContent-Type: application/json\r\nConnection: close\r\n"
                f"Content-Length: {len(body)}\r\n\r\n".encode()
                + body
            )
        except (OSError, EOFError):
            pass
        finally:
            tls.close()

    def close(self) -> None:
        self.listener.close()


@pytest.fixture
def tls_server(certs: Path) -> Iterator[TLSServer]:
    server = TLSServer(certs)
    yield server
    server.close()


def test_ca_file_trusts_private_ca(
    certs: Path, tls_server: TLSServer, monkeypatch: pytest.MonkeyPatch
) -> None:
    ca = str(certs / "ca.pem")
    with Client(base_url=tls_server.url, token="slw_t", ca_file=ca) as client:
        assert client.list_vms().items == []
        stream = client.attach_tunnel("tun_1")  # WebSocket over the same TLS trust
        assert stream.recv() == b""
        stream.close()
    assert [p.path for p in tls_server.requests] == ["/api/v1/vms", "/api/v1/tunnels/tun_1/stream"]
    assert all(p.headers["authorization"] == "Bearer slw_t" for p in tls_server.requests)
    # The same from the environment alone.
    monkeypatch.setenv("DAVIT_API_URL", tls_server.url)
    monkeypatch.setenv(TOKEN_ENV, "slw_env")
    monkeypatch.setenv(CA_FILE_ENV, ca)
    with Client() as client:
        assert client.list_vms().items == []
    assert tls_server.requests[-1].headers["authorization"] == "Bearer slw_env"


def test_reverse_session_over_tls(certs: Path, tls_server: TLSServer) -> None:
    ca = str(certs / "ca.pem")
    with Client(base_url=tls_server.url, token="slw_t", ca_file=ca) as client:
        session = client.reverse_forward("web", 5432, "127.0.0.1", 5432)
        assert session.wait(10) == "test over tls" and session.error is None
        session.close()
    assert [p.path for p in tls_server.requests] == [
        "/api/v1/vms/web/reverse-forwards",
        "/api/v1/vms/web/reverse-forwards/rev_1/session",
    ]
    assert all(p.headers["authorization"] == "Bearer slw_t" for p in tls_server.requests)


def test_without_ca_file_tls_fails(tls_server: TLSServer) -> None:
    with Client(base_url=tls_server.url) as client:
        with pytest.raises(TransportError, match=r"(?i)certificate"):
            client.list_vms()
        with pytest.raises(TransportError, match=r"(?i)certificate"):
            client.attach_tunnel("tun_1")
    assert tls_server.requests == []


# ---- the remote_https example, end to end over TLS ------------------------


class TLSProxy:
    """Terminates TLS (throwaway CA) and pipes each connection to the daemon's unix socket."""

    def __init__(self, certs: Path, socket_path: str) -> None:
        self.context = ssl.create_default_context(ssl.Purpose.CLIENT_AUTH)
        self.context.load_cert_chain(certs / "server.pem", certs / "server.key")
        self.socket_path = socket_path
        self.listener = socket.create_server(("127.0.0.1", 0))
        self.url = f"https://127.0.0.1:{self.listener.getsockname()[1]}"
        threading.Thread(target=self._serve, daemon=True).start()

    def _serve(self) -> None:
        while True:
            try:
                conn, _ = self.listener.accept()
            except OSError:
                return
            threading.Thread(target=self._one, args=(conn,), daemon=True).start()

    def _one(self, conn: socket.socket) -> None:
        try:
            tls = self.context.wrap_socket(conn, server_side=True)
        except OSError:
            conn.close()
            return
        upstream = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            upstream.connect(self.socket_path)
        except OSError:
            tls.close()
            upstream.close()
            return

        def pump(src: socket.socket, dst: socket.socket) -> None:
            try:
                while data := src.recv(65536):
                    dst.sendall(data)
            except OSError:
                pass
            finally:
                for s in (src, dst):
                    with contextlib.suppress(OSError):
                        s.shutdown(socket.SHUT_RDWR)

        threading.Thread(target=pump, args=(upstream, tls), daemon=True).start()
        pump(tls, upstream)
        tls.close()
        upstream.close()

    def close(self) -> None:
        self.listener.close()


EXAMPLE = Path(__file__).resolve().parent.parent / "examples" / "remote_https.py"


@pytest.mark.integration
def test_remote_https_example(certs: Path, guest_daemon: Client) -> None:
    assert guest_daemon.socket is not None
    proxy = TLSProxy(certs, guest_daemon.socket)
    env = {k: v for k, v in os.environ.items() if not k.startswith("DAVIT_")}
    env.update(
        DAVIT_API_URL=proxy.url,
        DAVIT_TOKEN="slw_example",
        DAVIT_CA_FILE=str(certs / "ca.pem"),
    )
    try:
        out = subprocess.run(
            [sys.executable, str(EXAMPLE), "py-example"],
            env=env,
            capture_output=True,
            text=True,
            timeout=120,
        )
        assert out.returncode == 0, out.stdout + out.stderr
        assert "engine: fake" in out.stdout
        assert "created: py-example" in out.stdout
        assert os.uname().sysname in out.stdout  # uname -a ran (fake guests run on the host)
        assert "deleted: py-example" in out.stdout
        assert all(vm.name != "py-example" for vm in guest_daemon.iter_vms())
        # Without the CA the same configuration fails cleanly with exit 1.
        del env["DAVIT_CA_FILE"]
        out = subprocess.run(
            [sys.executable, str(EXAMPLE)], env=env, capture_output=True, text=True, timeout=60
        )
        assert out.returncode == 1 and "certificate" in out.stderr.lower(), out.stderr
    finally:
        proxy.close()
