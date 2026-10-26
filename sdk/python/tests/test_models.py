"""Generated model parsing and serialization."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
from types import ModuleType
from typing import Any

import pytest

from davit._model import format_datetime, parse_datetime
from davit.models import (
    VM,
    BootMode,
    CreateVMRequest,
    Egress,
    EgressMode,
    ForkVMRequest,
    HostCapabilities,
    Operation,
    ShapeOverride,
    Snapshot,
    StopVMRequest,
    VMState,
)

VM_JSON: dict[str, Any] = {
    "id": "vm_3h2ap4c5mkmmvjen",
    "owner": "local:1000",
    "name": "web",
    "state": "ready",
    "stateRevision": 2,
    "instanceGeneration": 1,
    "shape": {"cpus": 2, "memoryBytes": 2147483648, "diskBytes": 8589934592},
    "imageId": "img_08187b0fb0c7b64f",
    "bootMode": "cold_boot",
    "ttlSeconds": 3600,
    "autoStop": True,
    "expiresAt": "2026-10-22T05:22:34.245884111Z",
    "readiness": {"agent": False, "ssh": False, "services": {"web": True}},
    "endpoint": {"mode": "gateway", "state": "ready", "vmFQDN": "web.ts.net", "gatewayFQDN": "gw"},
    "storage": {"mode": "copy", "fastClone": False},
    "privacy": {"noOutputRetention": True},
    "egress": {"mode": "allowlist", "allow": ["github.com", "10.0.0.0/8"]},
    "lastError": {"code": "timeout", "message": "slow", "retryable": True},
    "createdAt": "2026-10-22T04:22:34.243078072Z",
    "updatedAt": "2026-10-22T04:22:34.245884111Z",
}


def test_vm_parses_wire_json() -> None:
    vm = VM.from_dict(VM_JSON)
    assert vm.state is VMState.READY
    assert vm.state == "ready"
    assert vm.state_revision == 2
    assert vm.boot_mode is BootMode.COLD_BOOT
    assert vm.shape.memory_bytes == 2 << 30
    assert vm.readiness.services == {"web": True}
    assert vm.endpoint.vm_fqdn == "web.ts.net"
    assert vm.endpoint.gateway_fqdn == "gw"
    assert vm.last_error is not None and vm.last_error.retryable
    assert vm.created_at == datetime(2026, 10, 22, 4, 22, 34, 243078, tzinfo=timezone.utc)
    assert vm.deleted_at is None and vm.active_operation_id is None
    assert vm.egress.mode is EgressMode.ALLOWLIST
    assert vm.egress.allow == ["github.com", "10.0.0.0/8"]
    assert vm.privacy.no_output_retention is True and vm.privacy.ephemeral is None


def test_unknown_fields_and_enum_values_are_tolerated() -> None:
    data = {**VM_JSON, "state": "suspended", "bootMode": "", "brandNewField": {"x": 1}}
    vm = VM.from_dict(data)
    assert vm.state == "suspended"
    assert isinstance(vm.state, VMState)
    assert str(vm.state) == "suspended"
    assert vm.boot_mode is BootMode.EMPTY
    assert vm.to_dict()["state"] == "suspended"


def test_null_lists_become_empty_and_missing_required_fails() -> None:
    caps = {
        "version": "dev",
        "runtime": {
            "engine": "fake",
            "arch": ["amd64", "riscv64"],
            "coldBoot": True,
            "goldenRestore": False,
            "diskFork": True,
            "ramSnapshot": False,
            "ramCheckpointFormats": None,
            "liveFork": False,
            "guestTransport": "none",
            "shapeConstraints": {
                "maxCpus": 64,
                "minMemoryBytes": 1,
                "maxMemoryBytes": 2,
                "memoryAlignBytes": 1,
                "minDiskBytes": 1,
            },
            "fingerprintRules": [],
        },
        "bootModes": ["cold_boot"],
        "storage": {"mode": "copy", "fastClone": False},
        "egress": {"mode": "filtered"},
        "network": {"listeners": ["unix"], "endpointModes": None, "forwarding": False},
        "states": ["ready"],
        "ttlWarningSeconds": 600,
    }
    parsed = HostCapabilities.from_dict(caps)
    assert parsed.services == []
    assert parsed.profiles == []  # missing required list -> empty
    assert parsed.runtime.ram_checkpoint_formats == []
    assert parsed.runtime.arch == ["amd64", "riscv64"]
    with pytest.raises(ValueError, match="version"):
        HostCapabilities.from_dict({k: v for k, v in caps.items() if k != "version"})


def test_snapshot_and_operation_attrs() -> None:
    snap = Snapshot.from_dict(
        {
            "id": "snap_1",
            "owner": "o",
            "sourceVmId": "vm_1",
            "generation": 1,
            "imageId": "img",
            "arch": "amd64",
            "kind": "disk",
            "consistency": "clean",
            "sizeBytes": 10,
            "createdAt": "2026-10-22T04:22:34Z",
        }
    )
    assert snap.source_vm_id == "vm_1"
    assert snap.name is None
    op = Operation.from_dict(
        {
            "id": "op_1",
            "kind": "snapshot",
            "vmId": "vm_1",
            "owner": "o",
            "state": "succeeded",
            "phase": "done",
            "progress": 1,
            "result": {"snapshotId": "snap_1", "durationMs": 3, "noop": False},
            "createdAt": "2026-10-22T04:22:34Z",
            "updatedAt": "2026-10-22T04:22:34Z",
        }
    )
    assert op.vm_id == "vm_1"
    assert op.progress == 1.0 and isinstance(op.progress, float)
    assert op.result is not None and op.result.snapshot_id == "snap_1"


def test_request_serialization_uses_wire_names_and_omits_unset() -> None:
    body = CreateVMRequest(
        name="web",
        snapshot_id="snap_1",
        shape=ShapeOverride(cpus=2, memory_bytes=1 << 30),
        env={"A": "1"},
        ttl_seconds=60,
        auto_stop=False,
        require_golden=True,
        egress=Egress(mode=EgressMode.ALLOWLIST, allow=["github.com"]),
    )
    assert body.to_dict() == {
        "name": "web",
        "snapshotId": "snap_1",
        "shape": {"cpus": 2, "memoryBytes": 1 << 30},
        "env": {"A": "1"},
        "ttlSeconds": 60,
        "autoStop": False,
        "requireGolden": True,
        "egress": {"mode": "allowlist", "allow": ["github.com"]},
    }
    assert ForkVMRequest().to_dict() == {}
    assert ForkVMRequest(egress=Egress(mode=EgressMode("off"))).to_dict() == {
        "egress": {"mode": "off"}
    }
    assert StopVMRequest(force=True).to_dict() == {"force": True}


@pytest.mark.parametrize(
    ("text", "expected"),
    [
        ("2026-10-22T04:22:34Z", datetime(2026, 10, 22, 4, 22, 34, tzinfo=timezone.utc)),
        (
            "2026-10-22T11:22:34.1+07:00",
            datetime(2026, 10, 22, 11, 22, 34, 100000, tzinfo=timezone(timedelta(hours=7))),
        ),
        (
            "2026-10-22t04:22:34.123456789-01:30",
            datetime(
                2026, 10, 22, 4, 22, 34, 123456, tzinfo=timezone(-timedelta(hours=1, minutes=30))
            ),
        ),
    ],
)
def test_parse_datetime(text: str, expected: datetime) -> None:
    assert parse_datetime(text) == expected


def test_datetime_roundtrip_and_errors() -> None:
    when = datetime(2026, 1, 2, 3, 4, 5, 6, tzinfo=timezone.utc)
    assert format_datetime(when) == "2026-01-02T03:04:05.000006Z"
    assert parse_datetime(format_datetime(when)) == when
    with pytest.raises(ValueError, match="RFC 3339"):
        parse_datetime("yesterday")


@pytest.mark.parametrize(
    ("wire", "attr"),
    [
        ("vmFQDN", "vm_fqdn"),
        ("gatewayFQDN", "gateway_fqdn"),
        ("sourceVmId", "source_vm_id"),
        ("vmId", "vm_id"),
        ("ramCheckpointFormats", "ram_checkpoint_formats"),
        ("stateRevision", "state_revision"),
        ("nextCursor", "next_cursor"),
    ],
)
def test_attr_names(gen_models: ModuleType, wire: str, attr: str) -> None:
    assert gen_models.attr_name(wire) == attr


def test_spec_loader_uses_yaml_1_2_booleans(gen_models: ModuleType, tmp_path: Any) -> None:
    path = tmp_path / "spec.yaml"
    path.write_text("enum: [off, on, yes, no, true, False]\n", encoding="utf-8")
    assert gen_models.load_spec(path) == {"enum": ["off", "on", "yes", "no", True, False]}
