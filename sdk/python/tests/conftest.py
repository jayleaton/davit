"""Shared fixtures: OpenAPI spec loading and a real-daemon harness (fake runtime)."""

from __future__ import annotations

import importlib.util
import json
import os
import shutil
import socket
import subprocess
import tempfile
import time
from collections.abc import Iterator
from contextlib import contextmanager
from pathlib import Path
from types import ModuleType
from typing import Any

import pytest

from davit import Client

SDK_DIR = Path(__file__).resolve().parent.parent
REPO_ROOT = SDK_DIR.parent.parent
SPEC_PATH = REPO_ROOT / "api" / "openapi.yaml"


@pytest.fixture(autouse=True)
def _no_remote_env(monkeypatch: pytest.MonkeyPatch) -> None:
    """Keep the caller's remote configuration ($DAVIT_TOKEN, $DAVIT_CA_FILE) out of tests."""
    for name in ("DAVIT_TOKEN", "DAVIT_CA_FILE"):
        monkeypatch.delenv(name, raising=False)


@pytest.fixture(scope="session")
def spec(gen_models: ModuleType) -> dict[str, Any]:
    """The API spec, read with YAML 1.2 rules (``off`` is a string) like the generator."""
    data: dict[str, Any] = gen_models.load_spec(SPEC_PATH)
    return data


@pytest.fixture(scope="session")
def gen_models() -> ModuleType:
    """The model generator script, imported as a module."""
    path = SDK_DIR / "scripts" / "gen_models.py"
    module_spec = importlib.util.spec_from_file_location("gen_models", path)
    assert module_spec is not None and module_spec.loader is not None
    module = importlib.util.module_from_spec(module_spec)
    module_spec.loader.exec_module(module)
    return module


def short_tmp_root() -> str:
    """$TMPDIR (tempfile's choice) unless unix socket paths under it would be too long."""
    root = tempfile.gettempdir()
    # <root>/swpy-XXXXXXXX/s.sock must stay well under the ~107-byte sun_path limit.
    return root if len(root) <= 70 else "/tmp"


def _daemon_binary() -> Path:
    return Path(os.environ.get("DAVIT_BIN") or REPO_ROOT / "bin" / "davit")


def _socket_accepts(path: str) -> bool:
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
        try:
            s.connect(path)
        except OSError:
            return False
        return True


@contextmanager
def run_daemon(
    fake: dict[str, Any] | None = None,
    startup: float = 15.0,
    extra: dict[str, Any] | None = None,
) -> Iterator[Client]:
    """Run ``davit daemon`` (fake runtime) in a private tmp dir; yield a connected Client.

    The daemon is killed and the tmp dir removed on exit, also on failure.
    """
    binary = _daemon_binary()
    if not binary.is_file():
        pytest.skip(f"daemon binary {binary} not found: run `make build` (or set DAVIT_BIN)")
    tmp = tempfile.mkdtemp(prefix="swpy-", dir=short_tmp_root())
    proc: subprocess.Popen[bytes] | None = None
    log_path = os.path.join(tmp, "daemon.log")
    log = open(log_path, "wb")  # noqa: SIM115 - closed in finally
    try:
        sock = os.path.join(tmp, "s.sock")
        config: dict[str, Any] = {
            "dataDir": os.path.join(tmp, "data"),
            "socket": sock,
            "runtime": "fake",
            "logLevel": "warn",
        }
        if fake:
            config["fake"] = fake
        config.update(extra or {})
        config_path = os.path.join(tmp, "config.json")
        with open(config_path, "w", encoding="utf-8") as f:
            json.dump(config, f)
        env = {k: v for k, v in os.environ.items() if not k.startswith("DAVIT_")}
        for var, sub in (
            ("HOME", "home"),
            ("XDG_CONFIG_HOME", "config"),
            ("XDG_DATA_HOME", "share"),
            ("XDG_RUNTIME_DIR", "run"),
        ):
            env[var] = os.path.join(tmp, sub)
            os.makedirs(env[var], mode=0o700)
        env["DAVIT_TS_AUTHKEY"] = ""
        proc = subprocess.Popen(
            [str(binary), "daemon", "--config", config_path],
            env=env,
            stdin=subprocess.DEVNULL,
            stdout=log,
            stderr=subprocess.STDOUT,
        )
        deadline = time.monotonic() + startup
        while not _socket_accepts(sock):
            if proc.poll() is not None or time.monotonic() > deadline:
                log.flush()
                with open(log_path, encoding="utf-8", errors="replace") as f:
                    pytest.fail(f"daemon did not start (exit {proc.poll()}):\n{f.read()}")
            time.sleep(0.02)
        with Client(socket=sock, wait_timeout=60) as client:
            yield client
    finally:
        if proc is not None and proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
        log.close()
        shutil.rmtree(tmp, ignore_errors=True)


@pytest.fixture
def daemon() -> Iterator[Client]:
    """A fresh daemon per test with the default (instant) fake runtime."""
    with run_daemon() as client:
        yield client


@pytest.fixture
def slow_daemon() -> Iterator[Client]:
    """A fresh daemon whose fake runtime takes 2s to boot."""
    with run_daemon(fake={"bootDelay": "2s"}) as client:
        yield client


@pytest.fixture
def guest_daemon() -> Iterator[Client]:
    """A fresh daemon whose fake VMs run an in-process guest agent (exec runs on the host)."""
    with run_daemon(fake={"guestAgent": True}) as client:
        yield client


HARNESS_FAKES = REPO_ROOT / "internal" / "harness" / "testdata"


@pytest.fixture
def prompt_daemon() -> Iterator[Client]:
    """A guest-agent daemon whose `claude`/`codex` harnesses are the repo's fake scripts."""
    for name in ("fake-claude", "fake-codex"):
        if not (HARNESS_FAKES / name).is_file():
            pytest.skip(f"{HARNESS_FAKES / name} not found")
    harness = {
        "commands": {
            "claude": str(HARNESS_FAKES / "fake-claude"),
            "codex": str(HARNESS_FAKES / "fake-codex"),
        }
    }
    with run_daemon(fake={"guestAgent": True}, extra={"harness": harness}) as client:
        yield client
