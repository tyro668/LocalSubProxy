#!/usr/bin/env python3
"""Run a test suite and fail on ANY failure, skip, todo or empty run, judged
from the runner's machine-readable output, never from filtered text.

  scripts/strict-test.py go      [go test args...]        # go test -json; any Action=skip fails
  scripts/strict-test.py node    [node --test args...]    # node --test, TAP reporter parsed
  scripts/strict-test.py pytest  PYTHON [pytest args...]  # pytest --junitxml parsed
  scripts/strict-test.py unittest PYTHON MODULE...        # unittest result object inspected
  scripts/strict-test.py cargo   [cargo test args...]     # "test result" lines; ignored > 0 fails

The full raw output is echoed so logs stay readable. A Go package without
test files (go test reports it as a package-level skip with "[no test
files]") is the only skip that is tolerated; it is listed, not counted.
"""
import json
import os
import re
import subprocess
import sys
import tempfile
import xml.etree.ElementTree as ET


def die(msg):
    print(f"strict-test: FAIL: {msg}", flush=True)
    sys.exit(1)


def ok(msg):
    print(f"strict-test: ok: {msg}", flush=True)
    sys.exit(0)


def run_go(args):
    p = subprocess.Popen(["go", "test", "-json", *args], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    passed, skipped, failed, notests, out_by = 0, [], [], [], {}
    for line in p.stdout:
        try:
            ev = json.loads(line)
        except ValueError:          # build errors etc. are plain text
            sys.stdout.write(line)
            continue
        key = (ev.get("Package", ""), ev.get("Test", ""))
        if ev.get("Action") == "output":
            sys.stdout.write(ev.get("Output", ""))
            out_by.setdefault(key, []).append(ev.get("Output", ""))
            continue
        act, test = ev.get("Action"), ev.get("Test")
        if act == "skip":
            if not test and any("[no test files]" in o for o in out_by.get(key, [])):
                notests.append(key[0])
            else:
                skipped.append(f"{key[0]} {test or '(package)'}")
        elif act == "fail":
            failed.append(f"{key[0]} {test or '(package)'}")
        elif act == "pass" and test:
            passed += 1
    rc = p.wait()
    if notests:
        print(f"strict-test: packages without test files: {len(notests)}")
    if skipped:
        die(f"go: {len(skipped)} skipped: " + "; ".join(skipped[:20]))
    if failed or rc:
        die(f"go: exit {rc}, failed: " + "; ".join(failed[:20]))
    if passed == 0:
        die("go: no test ran")
    ok(f"go: {passed} tests/subtests passed, 0 skipped")


def run_node(args):
    p = subprocess.run(["node", "--test", "--test-reporter=tap", *args], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    sys.stdout.write(p.stdout)
    directive = [l.strip() for l in p.stdout.splitlines() if re.match(r"^\s*(not )?ok \d+ .*#\s*(SKIP|TODO)\b", l, re.I)]
    summary = {m.group(1): int(m.group(2)) for m in re.finditer(r"^# (tests|suites|pass|fail|cancelled|skipped|todo) (\d+)$", p.stdout, re.M)}
    if directive:
        die(f"node: {len(directive)} skipped/todo: " + "; ".join(directive[:20]))
    for k in ("skipped", "todo", "fail", "cancelled"):
        if summary.get(k, 0):
            die(f"node: {k} {summary[k]}")
    if p.returncode:
        die(f"node: exit {p.returncode}")
    if not summary.get("pass"):
        die("node: no test ran (no '# pass N' summary)")
    ok(f"node: {summary['pass']} passed, 0 skipped/todo")


def run_pytest(py, args):
    fd, xml = tempfile.mkstemp(suffix=".xml")
    os.close(fd)
    try:
        p = subprocess.run([py, "-m", "pytest", "-rsxX", "-p", "no:cacheprovider", f"--junitxml={xml}", *args],
                           stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        sys.stdout.write(p.stdout)
        try:
            root = ET.parse(xml).getroot()
        except (ET.ParseError, OSError):
            die(f"pytest: exit {p.returncode}, no junit report")
        suites = [root] if root.tag == "testsuite" else root.findall("testsuite")
        tot = {k: sum(int(s.get(k, 0)) for s in suites) for k in ("tests", "failures", "errors", "skipped")}
        skips = [f"{c.get('classname')}::{c.get('name')}" for c in root.iter("testcase") if c.find("skipped") is not None]
    finally:
        os.unlink(xml)
    if tot["skipped"] or skips:
        die(f"pytest: {max(tot['skipped'], len(skips))} skipped/xfail: " + "; ".join(skips[:20]))
    if tot["failures"] or tot["errors"] or p.returncode:
        die(f"pytest: exit {p.returncode}, failures {tot['failures']}, errors {tot['errors']}")
    if tot["tests"] == 0:
        die("pytest: no test ran")
    ok(f"pytest: {tot['tests']} passed, 0 skipped")


def run_unittest(py, mods):
    code = ("import sys,unittest\n"
            "s=unittest.defaultTestLoader.loadTestsFromNames(sys.argv[1:])\n"
            "r=unittest.TextTestRunner(verbosity=2,stream=sys.stdout).run(s)\n"
            "bad=[('skipped',len(r.skipped)),('expectedFailures',len(r.expectedFailures)),('unexpectedSuccesses',len(r.unexpectedSuccesses)),"
            "('failures',len(r.failures)),('errors',len(r.errors))]\n"
            "print('UNITTEST-RESULT', ' '.join(f'{k}={v}' for k,v in bad), 'ran=%d' % r.testsRun)\n")
    p = subprocess.run([py, "-c", code, *mods], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    sys.stdout.write(p.stdout)
    m = re.search(r"^UNITTEST-RESULT (.*) ran=(\d+)$", p.stdout, re.M)
    if not m:
        die(f"unittest: exit {p.returncode}, no result")
    counts = dict(kv.split("=") for kv in m.group(1).split())
    bad = {k: v for k, v in counts.items() if v != "0"}
    if bad:
        die(f"unittest: {bad}")
    if p.returncode or m.group(2) == "0":
        die(f"unittest: exit {p.returncode}, ran {m.group(2)}")
    ok(f"unittest: {m.group(2)} passed, 0 skipped")


def run_cargo(args):
    p = subprocess.run(["cargo", "test", *args], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    sys.stdout.write(p.stdout)
    res = re.findall(r"test result: (\w+)\. (\d+) passed; (\d+) failed; (\d+) ignored", p.stdout)
    if not res:
        die(f"cargo: exit {p.returncode}, no 'test result' line")
    passed = sum(int(r[1]) for r in res)
    if any(r[0] != "ok" or r[2] != "0" for r in res) or p.returncode:
        die(f"cargo: exit {p.returncode}, {res}")
    if any(r[3] != "0" for r in res):
        die(f"cargo: ignored tests: {res}")
    if passed == 0:
        die("cargo: no test ran")
    ok(f"cargo: {passed} passed, 0 ignored")


if __name__ == "__main__":
    if len(sys.argv) < 2:
        die(__doc__)
    kind, rest = sys.argv[1], sys.argv[2:]
    {"go": lambda: run_go(rest), "node": lambda: run_node(rest), "cargo": lambda: run_cargo(rest),
     "pytest": lambda: run_pytest(rest[0], rest[1:]), "unittest": lambda: run_unittest(rest[0], rest[1:])}.get(kind, lambda: die("unknown kind " + kind))()
