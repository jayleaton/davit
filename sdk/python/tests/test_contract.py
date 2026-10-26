"""Contract tests: fail when api/openapi.yaml changes in a way the SDK does not cover."""

from __future__ import annotations

from types import ModuleType
from typing import Any

from davit import NOT_YET_SUPPORTED, OPERATIONS, models
from davit.errors import ERROR_CLASSES, DavitError, error_for

HTTP_METHODS = {"get", "put", "post", "delete", "patch", "head", "options"}


def spec_operations(spec: dict[str, Any]) -> dict[str, tuple[str, str, bool]]:
    ops: dict[str, tuple[str, str, bool]] = {}
    for path, item in spec["paths"].items():
        for method, op in item.items():
            if method in HTTP_METHODS:
                implemented = op.get("x-davit-implemented")
                assert isinstance(implemented, bool), f"{op['operationId']}: x-davit-implemented"
                ops[op["operationId"]] = (method.upper(), path, implemented)
    return ops


def test_every_operation_is_covered(spec: dict[str, Any]) -> None:
    ops = spec_operations(spec)
    for op_id, (method, path, implemented) in ops.items():
        if op_id in OPERATIONS:
            assert OPERATIONS[op_id] == (method, path), f"{op_id}: method/path changed"
        else:
            assert op_id in NOT_YET_SUPPORTED, f"{op_id} is new in openapi.yaml: wrap it or list it"
            assert not implemented, f"{op_id} is implemented by the daemon but not by the SDK"
    stale = (set(OPERATIONS) | NOT_YET_SUPPORTED) - set(ops)
    assert not stale, f"operations no longer in openapi.yaml: {sorted(stale)}"
    assert not set(OPERATIONS) & NOT_YET_SUPPORTED


def test_generated_models_are_up_to_date(spec: dict[str, Any], gen_models: ModuleType) -> None:
    expected = gen_models.generate(spec)
    actual = gen_models.OUTPUT_PATH.read_text(encoding="utf-8")
    assert actual == expected, f"models.py is stale: run `{gen_models.REGENERATE}` to regenerate"


def test_every_error_code_maps_to_a_class(spec: dict[str, Any]) -> None:
    codes = spec["components"]["schemas"]["ErrorCode"]["enum"]
    assert set(codes) == set(ERROR_CLASSES), "Error.code enum and ERROR_CLASSES differ"
    assert {m.value for m in models.ErrorCode} == set(codes)
    for code in codes:
        err = error_for(code, "boom")
        assert type(err) is ERROR_CLASSES[code]
        assert type(err) is not DavitError
        assert err.code == code
