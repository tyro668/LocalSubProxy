#!/usr/bin/env python3
"""Route naming lint (spec §4.5): every HTTP route path segment is ONE
lowercase word: no '-' or '_' compounds (/app/ask/claude, not /app/ask-claude).

Checked: every OpenAPI path key, every AsyncAPI channel address, and every
route-like string in source/docs (paths under /lmbp, /app, /.well-known,
/v1, /api). Allowed segments: a word ([a-z0-9]+), a single-word path
parameter ({task}, :id), a single-word file name (jwks.json) or a listed
exception. Concrete ids in examples (task_x, dev_…) are values, not route
segments, and are skipped. Exit 1 on any violation.
"""
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]
# Externally mandated segments (reason in spec §4.5).
EXCEPTIONS = {
    ".well-known": "RFC 8615 well-known URI prefix",
}
WORD = re.compile(r"^[a-z0-9]+$")
PARAM = re.compile(r"^(\{[a-z]+\}|:[a-z]+)$")
# code that builds a path from a variable (${encodeURIComponent(id)}, {r["id"]}) is not a literal segment
EXPR = re.compile(r"^(\$\{|\{[^}]*$|\{[^}]*[^a-z_}][^}]*\})")
FILE = re.compile(r"^[a-z0-9]+\.(json|js|css|html)$")
VALUE = re.compile(r"^(task|dev|rid|msg|call|blob)_[A-Za-z0-9_]*$")  # concrete ids
ROUTE = re.compile(r"""(?<![A-Za-z0-9.])(/(?:lmbp|app|\.well-known|v1|api)(?:/[A-Za-z0-9_.{}:$-]+)+)""")
SKIP_DIRS = {"node_modules", ".venv", "dist", "target", ".git", "build", ".dart_tool", "__pycache__"}
EXTS = {".go", ".ts", ".js", ".mjs", ".py", ".rs", ".java", ".dart", ".md", ".yaml", ".yml", ".json", ".html", ".sh"}
# not routes: schema $id paths (…/spec/lmbp/x.json) and Go import paths
NOT_ROUTE = re.compile(r"(spec/lmbp/|github\.com/|/lmbp/(gateway|client|store|conformance)\b|internal/lmbp|learn\.microsoft)")


def bad_segments(path):
    out = []
    for seg in path.strip("/").split("/"):
        seg = seg.split("?")[0].split("#")[0].rstrip(".,:;)$")
        if not seg or seg in EXCEPTIONS or WORD.match(seg) or PARAM.match(seg) or FILE.match(seg) or VALUE.match(seg) or EXPR.match(seg):
            continue
        out.append(seg)
    return out


def main():
    bad = []
    # 1. spec: every OpenAPI path key and AsyncAPI address
    for f, rx in (("spec/openapi.yaml", r"^  (/[^:\s]*):\s*$"), ("spec/asyncapi.yaml", r"^\s*address:\s*(/\S+)")):
        for n, line in enumerate((ROOT / f).read_text().splitlines(), 1):
            m = re.match(rx, line)
            if m:
                for s in bad_segments(m.group(1)):
                    bad.append(f"{f}:{n}: {m.group(1)}: segment {s!r}")
    # 2. route-like literals everywhere else
    for p in ROOT.rglob("*"):
        if not p.is_file() or p.suffix not in EXTS or SKIP_DIRS & set(p.relative_to(ROOT).parts):
            continue
        if p.name == "route-lint.py" or p.name.endswith("route_lint_test.go"):
            continue
        try:
            text = p.read_text()
        except (UnicodeDecodeError, OSError):
            continue
        for n, line in enumerate(text.splitlines(), 1):
            for m in ROUTE.finditer(line):
                ctx = line[max(0, m.start() - 40):m.end()]
                if NOT_ROUTE.search(ctx):
                    continue
                for s in bad_segments(m.group(1)):
                    bad.append(f"{p.relative_to(ROOT)}:{n}: {m.group(1)}: segment {s!r}")
    if bad:
        print("route-lint: compound route segments (spec §4.5 requires one word per segment):")
        print("\n".join(sorted(set(bad))))
        return 1
    print("route-lint: ok (every route segment is a single word)")
    return 0


def selftest():
    cases = {"/app/ask-claude": ["ask-claude"], "/lmbp/v1/tenant/device-approvals": ["device-approvals"],
             "/.well-known/lmbp-gateway": ["lmbp-gateway"], "/x/{device_id}/revoke": ["{device_id}"],
             "/app/ask/claude": [], "/.well-known/jwks.json": [], "/lmbp/v1/tenant/tasks/{task}/events": [],
             "/v1/chat/completions": [], "/app/devices/:id/revoke": [], "/lmbp/v1/tenant/tasks/task_x/events": [],
             "/x/my_route": ["my_route"]}
    for path, want in cases.items():
        got = bad_segments(path)
        assert got == want, (path, got, want)
    print("route-lint: selftest ok")


if __name__ == "__main__":
    if "--selftest" in sys.argv:
        selftest()
    sys.exit(main())
