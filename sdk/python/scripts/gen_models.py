#!/usr/bin/env python3
"""Generate ``src/davit/models.py`` from ``api/openapi.yaml``.

Usage (from the repository root)::

    .build/py-venv/bin/python sdk/python/scripts/gen_models.py          # write
    .build/py-venv/bin/python sdk/python/scripts/gen_models.py --check  # verify

Every schema in ``components.schemas`` that is not marked
``x-davit-implemented: false`` becomes either an open string enum or a frozen,
keyword-only dataclass whose ``_wire_`` table drives parsing and serialization
(see ``davit._model``). Offline; the only dependency is PyYAML.
"""

from __future__ import annotations

import argparse
import json
import keyword
import re
import sys
import textwrap
from pathlib import Path
from typing import Any

import yaml

SDK_DIR = Path(__file__).resolve().parent.parent
SPEC_PATH = SDK_DIR.parent.parent / "api" / "openapi.yaml"
OUTPUT_PATH = SDK_DIR / "src" / "davit" / "models.py"
REGENERATE = ".build/py-venv/bin/python sdk/python/scripts/gen_models.py"

# Wire names whose acronyms a generic camelCase split would get wrong or that
# deserve an explicit, reviewed mapping.
ATTR_OVERRIDES = {
    "vmFQDN": "vm_fqdn",
    "gatewayFQDN": "gateway_fqdn",
    "sourceVmId": "source_vm_id",
}

# Fields that hold a credential: kept out of the dataclass ``repr`` (and so out
# of logs and tracebacks that print the model); the attribute itself is unchanged.
SECRET_FIELDS = {
    ("CreatedToken", "secret"),
    ("PutCredentialRequest", "secret"),
    ("WebhookSecret", "secret"),
}

# Python names for enum values that are not identifiers.
ENUM_MEMBER_OVERRIDES = {"": "EMPTY"}

LINE = 100

# Keywords allowed on an untyped schema that still means "any JSON value".
ANY_KEYS = {"description", "title"}


def attr_name(wire: str) -> str:
    """Map a camelCase wire name to a snake_case Python attribute."""
    if wire in ATTR_OVERRIDES:
        return ATTR_OVERRIDES[wire]
    name = re.sub(r"([A-Z]+)([A-Z][a-z])", r"\1_\2", wire)
    name = re.sub(r"([a-z0-9])([A-Z])", r"\1_\2", name).lower()
    return name + "_" if keyword.iskeyword(name) else name


def q(text: str) -> str:
    """Quote a string as a double-quoted Python literal (ruff format style)."""
    return json.dumps(text)


def member_name(value: str) -> str:
    if value in ENUM_MEMBER_OVERRIDES:
        return ENUM_MEMBER_OVERRIDES[value]
    name = re.sub(r"[^0-9A-Za-z]+", "_", value).strip("_").upper()
    return "_" + name if name[:1].isdigit() else name


def ref_name(schema: dict[str, Any]) -> str | None:
    ref = schema.get("$ref")
    return ref.rsplit("/", 1)[-1] if isinstance(ref, str) else None


def type_of(schema: dict[str, Any]) -> tuple[str, str]:
    """Return ``(wire type spec, Python annotation)`` for a property schema."""
    name = ref_name(schema)
    if name is not None:
        return name, name
    kind = schema.get("type")
    if kind is None and set(schema) <= ANY_KEYS:
        return "any", "Any"  # `{}`: any JSON value
    if kind == "string":
        if schema.get("format") == "date-time":
            return "datetime", "datetime"
        return "str", "str"
    if kind == "integer":
        return "int", "int"
    if kind == "number":
        return "float", "float"
    if kind == "boolean":
        return "bool", "bool"
    if kind == "array":
        spec, ann = type_of(schema.get("items", {}))
        return f"list[{spec}]", f"list[{ann}]"
    if kind == "object":
        extra = schema.get("additionalProperties", True)
        if isinstance(extra, dict):
            spec, ann = type_of(extra)
        else:
            spec, ann = "any", "Any"
        return f"map[{spec}]", f"dict[str, {ann}]"
    raise ValueError(f"unsupported schema: {schema!r}")


def docstring(text: str | None, indent: str) -> list[str]:
    if not text:
        return []
    text = " ".join(text.split()).replace("\\", "\\\\").replace('"""', '\\"\\"\\"')
    width = LINE - len(indent) - 6
    lines = textwrap.wrap(text, width=width)
    if len(lines) == 1:
        return [f'{indent}"""{lines[0]}"""']
    return [f'{indent}"""{lines[0]}', *(f"{indent}{line}" for line in lines[1:]), f'{indent}"""']


def gen_enum(name: str, schema: dict[str, Any]) -> list[str]:
    out = [f"class {name}(OpenEnum):"]
    out += docstring(schema.get("description") or f"Open enum of {name} values.", "    ")
    out.append("")
    for value in schema["enum"]:
        out.append(f"    {member_name(value)} = {q(value)}")
    return out


def gen_model(name: str, schema: dict[str, Any]) -> list[str]:
    required = set(schema.get("required", []))
    out = ["@dataclass(frozen=True, kw_only=True)", f"class {name}(Model):"]
    out += docstring(schema.get("description") or f"The ``{name}`` wire object.", "    ")
    out.append("")
    wire: list[str] = []
    for prop, pschema in schema.get("properties", {}).items():
        attr = attr_name(prop)
        spec, ann = type_of(pschema)
        doc = docstring(pschema.get("description"), "    ")
        if doc and out[-1] != "":
            out.append("")
        default = "" if prop in required else " | None = None"
        if (name, prop) in SECRET_FIELDS:
            default = (
                " = field(repr=False)"
                if prop in required
                else " | None = field(default=None, repr=False)"
            )
        out += [f"    {attr}: {ann}{default}", *doc, *([""] if doc else [])]
        wire.append(f"        ({q(attr)}, {q(prop)}, {q(spec)}, {prop in required}),")
    if out[-1] != "":
        out.append("")
    if not wire:  # an object with no properties
        out.append("    _wire_ = ()")
    elif len(wire) == 1:  # ruff format keeps a one-element tuple on one line
        out.append(f"    _wire_ = ({wire[0].strip()})")
    else:
        out += ["    _wire_ = (", *wire, "    )"]
    return out


def implemented_schemas(spec: dict[str, Any]) -> dict[str, dict[str, Any]]:
    schemas: dict[str, dict[str, Any]] = spec["components"]["schemas"]
    return {n: s for n, s in schemas.items() if s.get("x-davit-implemented", True) is not False}


def generate(spec: dict[str, Any]) -> str:
    """Return the full text of ``models.py`` for an OpenAPI document."""
    schemas = implemented_schemas(spec)
    blocks: list[list[str]] = []
    for name, schema in schemas.items():
        if "enum" in schema:
            blocks.append(gen_enum(name, schema))
        elif schema.get("type") == "object":
            blocks.append(gen_model(name, schema))
        else:
            raise ValueError(f"schema {name}: unsupported shape for the generator")
    # ruff's isort-style order (RUF022): SCREAMING_CASE, then CamelCase, then the rest.
    names = sorted(schemas, key=lambda n: (0 if n.isupper() else 1 if n[:1].isupper() else 2, n))
    header = [
        '"""Wire models generated from api/openapi.yaml by scripts/gen_models.py.',
        "",
        f"DO NOT EDIT. Regenerate with ``{REGENERATE}``.",
        '"""',
        "",
        "from __future__ import annotations",
        "",
        "from dataclasses import dataclass, field",
        "from datetime import datetime",
        "from typing import Any",
        "",
        "from ._model import Model, OpenEnum",
        "",
        "__all__ = [",
        *(f'    "{n}",' for n in names),
        "]",
    ]
    lines = header
    for block in blocks:
        lines += ["", "", *block]
    text = "\n".join(lines) + "\n"
    if "field(" not in text:
        text = text.replace("dataclass, field\n", "dataclass\n")
    if "Any]" not in text:
        text = text.replace("from typing import Any\n", "")
    return text


class Yaml12Loader(yaml.SafeLoader):
    """``SafeLoader`` with YAML 1.2 booleans, as Go and the TypeScript generator read the spec.

    PyYAML implements YAML 1.1, where ``off``/``on``/``yes``/``no`` are booleans;
    in 1.2 they are strings (e.g. ``EgressMode`` value ``off``).
    """


_BOOL = "tag:yaml.org,2002:bool"
Yaml12Loader.yaml_implicit_resolvers = {
    first: [r for r in resolvers if r[0] != _BOOL]
    for first, resolvers in yaml.SafeLoader.yaml_implicit_resolvers.items()
}
Yaml12Loader.add_implicit_resolver(
    _BOOL, re.compile(r"^(?:true|True|TRUE|false|False|FALSE)$"), list("tTfF")
)


def load_spec(path: Path = SPEC_PATH) -> dict[str, Any]:
    with path.open(encoding="utf-8") as f:
        data: dict[str, Any] = yaml.load(f, Loader=Yaml12Loader)  # a SafeLoader subclass
    return data


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--check", action="store_true", help="fail if models.py is stale")
    args = parser.parse_args(argv)
    text = generate(load_spec())
    if args.check:
        if OUTPUT_PATH.read_text(encoding="utf-8") != text:
            print(f"{OUTPUT_PATH} is stale; run `{REGENERATE}`", file=sys.stderr)
            return 1
        return 0
    OUTPUT_PATH.write_text(text, encoding="utf-8")
    return 0


if __name__ == "__main__":
    sys.exit(main())
