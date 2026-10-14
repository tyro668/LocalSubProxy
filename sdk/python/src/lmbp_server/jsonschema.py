"""Minimal JSON Schema (draft 2020-12) validator for the LMBP schemas
(spec/schemas). It implements exactly the keywords those files use and FAILS
CLOSED: compiling a schema with any other keyword raises, so a constraint is
never silently ignored. Lengths count Unicode code points. validate() returns
None or the first error (path, message)."""
from __future__ import annotations

import re
from typing import Any, Optional
from urllib.parse import urljoin, unquote

ANNOTATIONS = {"$schema", "$id", "$defs", "title", "description", "default", "$comment"}
KEYWORDS = {"$ref", "type", "required", "properties", "additionalProperties", "items", "minItems", "maxItems",
            "minimum", "maximum", "minLength", "maxLength", "enum", "const", "pattern", "oneOf", "anyOf", "allOf",
            "if", "then", "else", "uniqueItems"}


def _is_int(v: Any) -> bool:
    return (isinstance(v, int) and not isinstance(v, bool)) or (isinstance(v, float) and v.is_integer())


def _is_type(v: Any, t: str) -> bool:
    if t == "null":
        return v is None
    if t == "boolean":
        return isinstance(v, bool)
    if t == "integer":
        return _is_int(v)
    if t == "number":
        return isinstance(v, (int, float)) and not isinstance(v, bool)
    if t == "string":
        return isinstance(v, str)
    if t == "array":
        return isinstance(v, list)
    if t == "object":
        return isinstance(v, dict)
    raise ValueError(f"jsonschema: unknown type {t}")


def _eq(a: Any, b: Any) -> bool:
    if isinstance(a, bool) or isinstance(b, bool):
        return type(a) is type(b) and a == b
    return a == b


class SchemaSet:
    def __init__(self, docs: list[dict]) -> None:
        self.docs = {}
        for d in docs:
            self._check(d)
            self.docs[d["$id"]] = d
        self._re: dict[str, re.Pattern] = {}

    def _check(self, s: Any) -> None:
        if not isinstance(s, dict):
            return
        for k, v in s.items():
            if k in ANNOTATIONS:
                if k == "$defs":
                    for x in v.values():
                        self._check(x)
                continue
            if k not in KEYWORDS:
                raise ValueError(f"jsonschema: unsupported keyword {k}")
            if k == "properties":
                for x in v.values():
                    self._check(x)
            elif k in ("items", "additionalProperties", "if", "then", "else"):
                self._check(v)
            elif k in ("oneOf", "anyOf", "allOf"):
                for x in v:
                    self._check(x)

    def _resolve(self, ref: str, doc_id: str):
        file, _, frag = ref.partition("#")
        did = urljoin(doc_id, file) if file else doc_id
        s = self.docs.get(did)
        if s is None:
            raise ValueError(f"jsonschema: unknown $ref {ref}")
        for p in [x for x in frag.split("/") if x]:
            s = s[unquote(p)]
        return s, did

    def validate(self, sid: str, value: Any) -> Optional[tuple[list, str]]:
        return self._v(self.docs[sid], value, [], sid)

    def _pattern(self, p: str) -> re.Pattern:
        r = self._re.get(p)
        if r is None:
            # ECMA-262 / RE2 semantics: "$" does not match before a trailing
            # newline (Python's does), so anchor it with \Z.
            q = p[:-1] + r"\Z" if p.endswith("$") and not p.endswith(r"\$") else p
            r = self._re[p] = re.compile(q)
        return r

    def _v(self, s: dict, x: Any, path: list, doc_id: str):
        def err(m):
            return (path, m)
        if "$ref" in s:
            rs, rid = self._resolve(s["$ref"], doc_id)
            e = self._v(rs, x, path, rid)
            if e:
                return e
        if "type" in s:
            ts = s["type"] if isinstance(s["type"], list) else [s["type"]]
            if not any(_is_type(x, t) for t in ts):
                return err("must be " + " or ".join(ts))
        if "const" in s and not _eq(x, s["const"]):
            return err(f"must be {s['const']!r}")
        if "enum" in s and not any(_eq(x, e) for e in s["enum"]):
            return err(f"must be one of {s['enum']!r}")
        if isinstance(x, (int, float)) and not isinstance(x, bool):
            if "minimum" in s and x < s["minimum"]:
                return err(f"must be >= {s['minimum']}")
            if "maximum" in s and x > s["maximum"]:
                return err(f"must be <= {s['maximum']}")
        if isinstance(x, str):
            if "minLength" in s and len(x) < s["minLength"]:
                return err(f"must have at least {s['minLength']} characters")
            if "maxLength" in s and len(x) > s["maxLength"]:
                return err(f"must have at most {s['maxLength']} characters")
            if "pattern" in s and not self._pattern(s["pattern"]).search(x):
                return err(f"must match {s['pattern']}")
        if isinstance(x, list):
            if "minItems" in s and len(x) < s["minItems"]:
                return err(f"must have at least {s['minItems']} items")
            if "maxItems" in s and len(x) > s["maxItems"]:
                return err(f"must have at most {s['maxItems']} items")
            if s.get("uniqueItems") and any(_eq(a, b) for i, a in enumerate(x) for b in x[:i]):
                return err("items must be unique")
            if "items" in s:
                for i, v in enumerate(x):
                    e = self._v(s["items"], v, path + [i], doc_id)
                    if e:
                        return e
        if isinstance(x, dict):
            for k in s.get("required", []):
                if k not in x:
                    return err(f"missing required property {k}")
            props = s.get("properties", {})
            ap = s.get("additionalProperties")
            for k, v in x.items():
                if k in props:
                    e = self._v(props[k], v, path + [k], doc_id)
                    if e:
                        return e
                elif ap is False:
                    return (path + [k], "is not allowed")
                elif isinstance(ap, dict):
                    e = self._v(ap, v, path + [k], doc_id)
                    if e:
                        return e
        for sub in s.get("allOf", []):
            e = self._v(sub, x, path, doc_id)
            if e:
                return e
        if "anyOf" in s and not any(self._v(sub, x, path, doc_id) is None for sub in s["anyOf"]):
            return err("does not match any allowed form")
        if "oneOf" in s and sum(self._v(sub, x, path, doc_id) is None for sub in s["oneOf"]) != 1:
            return err("must match exactly one allowed form")
        if "if" in s:
            branch = s.get("then") if self._v(s["if"], x, path, doc_id) is None else s.get("else")
            if branch:
                e = self._v(branch, x, path, doc_id)
                if e:
                    return e
        return None
