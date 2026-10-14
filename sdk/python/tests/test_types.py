"""B1: the public request types follow the spec: every ChatRequest /
MessagesRequest property in spec/openapi.yaml is a key of the exported
TypedDict (the client accepts them as keyword arguments)."""
import pathlib
import typing

import pytest

import lmbp_server

SPEC = pathlib.Path(__file__).resolve().parents[3] / "spec" / "openapi.yaml"


def openapi_props(schema):
    out, in_s, props = [], False, False
    for line in SPEC.read_text().split("\n"):
        ind = len(line) - len(line.lstrip(" "))
        if line == f"    {schema}:":
            in_s = True
            continue
        if not in_s:
            continue
        if line.strip() and ind <= 4:
            break
        if line == "      properties:":
            props = True
            continue
        if props and line.strip() and ind <= 6:
            props = False
        elif props and ind == 8 and ":" in line:
            out.append(line.strip().split(":")[0])
    assert out, schema
    return out


@pytest.mark.parametrize("schema", ["ChatRequest", "MessagesRequest"])
def test_request_types_cover_spec(schema):
    td = getattr(lmbp_server, schema)
    have = set(typing.get_type_hints(td))
    missing = [p for p in openapi_props(schema) if p not in have]
    assert missing == [], f"{schema} lacks spec properties"
