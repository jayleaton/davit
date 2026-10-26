"""Runtime support for the generated wire models in :mod:`davit.models`.

Generated dataclasses declare their wire layout in a ``_wire_`` class variable:
a tuple of ``(attribute, wire_name, type_spec, required)``. A type spec is one
of ``str``, ``int``, ``float``, ``bool``, ``datetime``, ``any``, the name of a
model or enum class in the same module, or ``list[<spec>]`` / ``map[<spec>]``.
Parsing and serialization are driven only by that table, so wire names never
need to be repeated by hand.
"""

from __future__ import annotations

import enum
import re
import sys
from collections.abc import Mapping
from datetime import datetime, timedelta, timezone
from typing import Any, ClassVar, TypeVar, cast

__all__ = ["Model", "OpenEnum", "format_datetime", "parse_datetime"]

_T = TypeVar("_T", bound="Model")

WireField = tuple[str, str, str, bool]

_RFC3339 = re.compile(
    r"^(\d{4})-(\d{2})-(\d{2})[Tt ](\d{2}):(\d{2}):(\d{2})(?:\.(\d+))?"
    r"(?:([Zz])|([+-])(\d{2}):(\d{2}))$"
)


def parse_datetime(value: str) -> datetime:
    """Parse an RFC 3339 timestamp into an aware :class:`datetime`.

    Fractional seconds beyond microseconds (the daemon sends nanoseconds) are
    truncated.
    """
    m = _RFC3339.match(value)
    if m is None:
        raise ValueError(f"invalid RFC 3339 timestamp: {value!r}")
    year, month, day, hour, minute, second = (int(g) for g in m.groups()[:6])
    frac = m.group(7) or ""
    micro = int((frac + "000000")[:6])
    if m.group(8):
        tz = timezone.utc
    else:
        offset = timedelta(hours=int(m.group(10)), minutes=int(m.group(11)))
        tz = timezone(-offset if m.group(9) == "-" else offset)
    return datetime(year, month, day, hour, minute, second, micro, tzinfo=tz)


def format_datetime(value: datetime) -> str:
    """Format a datetime as RFC 3339 (UTC with ``Z`` when the offset is zero)."""
    if value.tzinfo is None:
        value = value.replace(tzinfo=timezone.utc)
    text = value.isoformat()
    return text[:-6] + "Z" if text.endswith("+00:00") else text


class OpenEnum(str, enum.Enum):
    """A string enum that accepts values it does not know.

    Servers may add enum values in later versions; an unknown value parses to
    a pseudo-member that still compares equal to its string.
    """

    @classmethod
    def _missing_(cls, value: object) -> Any:
        if not isinstance(value, str):
            return None
        member = str.__new__(cls, value)
        member._name_ = value.upper() or "EMPTY"
        member._value_ = value
        return member

    def __str__(self) -> str:
        return str(self.value)


def _resolve(owner: type, name: str) -> Any:
    return getattr(sys.modules[owner.__module__], name)


def _decode(owner: type, spec: str, value: Any) -> Any:
    if spec.startswith("list["):
        inner = spec[5:-1]
        return [_decode(owner, inner, v) for v in (value or [])]
    if spec.startswith("map["):
        inner = spec[4:-1]
        return {str(k): _decode(owner, inner, v) for k, v in (value or {}).items()}
    if spec == "datetime":
        return parse_datetime(value)
    if spec == "float":
        return float(value)
    if spec in ("str", "int", "bool", "any"):
        return value
    target = _resolve(owner, spec)
    if issubclass(target, Model):
        return target.from_dict(value)
    return target(value)


def _encode(value: Any) -> Any:
    if isinstance(value, Model):
        return value.to_dict()
    if isinstance(value, enum.Enum):
        return value.value
    if isinstance(value, datetime):
        return format_datetime(value)
    if isinstance(value, list):
        return [_encode(v) for v in value]
    if isinstance(value, dict):
        return {k: _encode(v) for k, v in value.items()}
    return value


class Model:
    """Base class of generated wire models (frozen dataclasses)."""

    _wire_: ClassVar[tuple[WireField, ...]] = ()

    @classmethod
    def from_dict(cls: type[_T], data: Mapping[str, Any]) -> _T:
        """Build a model from decoded JSON, ignoring unknown fields.

        Missing or null lists and maps become empty; any other missing
        required field raises :class:`ValueError`.
        """
        if not isinstance(data, Mapping):
            raise ValueError(f"{cls.__name__}: expected an object, got {type(data).__name__}")
        kwargs: dict[str, Any] = {}
        for attr, wire, spec, required in cls._wire_:
            value = data.get(wire)
            if value is None:
                if spec.startswith(("list[", "map[")) and required:
                    kwargs[attr] = [] if spec.startswith("list[") else {}
                elif required:
                    raise ValueError(f"{cls.__name__}: missing required field {wire!r}")
                continue
            try:
                kwargs[attr] = _decode(cls, spec, value)
            except (TypeError, ValueError, AttributeError) as exc:
                raise ValueError(f"{cls.__name__}.{wire}: {exc}") from exc
        return cast(_T, cast(Any, cls)(**kwargs))

    def to_dict(self) -> dict[str, Any]:
        """Serialize to wire JSON, omitting fields that are ``None``."""
        out: dict[str, Any] = {}
        for attr, wire, _spec, _required in self._wire_:
            value = getattr(self, attr)
            if value is not None:
                out[wire] = _encode(value)
        return out
