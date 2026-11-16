#!/usr/bin/env python3
"""Run lizard and compare the result with a baseline of known over-limit functions.

    core-lizard.py [--write] [--baseline FILE] [--ccn N] [--length N] [--params N] [--cognitive N] [--nesting N] [-x GLOB]... -- ROOT

Without a baseline file every function over a limit fails. With one, only a NEW over-limit function, or a listed function that got
WORSE (higher ccn, length, params, nesting or cognitive), fails. Fixed functions pass; `--write` rewrites the baseline from the
current code and so drops them. The baseline keeps old debt from blocking work and stops new debt from arriving.

Exit: 0 ok, 1 findings, 2 bad baseline or usage, 127 lizard missing.
"""
from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
from pathlib import Path

WORSE = ("ccn", "length", "param", "nd", "cognitive")
LINE = re.compile(r"^(?P<path>.+?):(?P<line>\d+): warning: (?P<name>.+?) has (?P<rest>.*)$")
PAIR = re.compile(r"(\d+) (NLOC|CCN|token|PARAM|length|ND|cognitive)")
DEFAULT_BASELINE = ".core-baseline.json"


def run_lizard(args, root):
    cmd = ["lizard", "-C", str(args.ccn), "-L", str(args.length), "-a", str(args.params), "-Ecognitive", "-ENS",
           "-T", "cognitive_complexity=%d" % args.cognitive, "-T", "max_nested_structures=%d" % args.nesting, "-w"]
    for glob in args.exclude:
        cmd += ["-x", glob]
    try:
        p = subprocess.run(cmd + [root], capture_output=True, text=True)
    except OSError:
        print("core-lizard: lizard is not installed", file=sys.stderr)
        sys.exit(127)
    found = {}
    for raw in p.stdout.splitlines():
        m = LINE.match(raw)
        if not m:
            continue
        path = m["path"][2:] if m["path"].startswith("./") else m["path"]
        metrics = {k.lower(): int(v) for v, k in PAIR.findall(m["rest"])}
        key = "%s::%s" % (path, m["name"])
        old = found.get(key, {})
        found[key] = {k: max(metrics.get(k, 0), old.get(k, 0)) for k in set(metrics) | set(old)}
    if p.returncode not in (0, 1) and not found:
        print(p.stderr.strip() or "core-lizard: lizard failed", file=sys.stderr)
        sys.exit(p.returncode)
    return found


def load_baseline(path):
    try:
        data = json.loads(Path(path).read_text())
    except (OSError, ValueError) as e:
        print("core-lizard: cannot read %s: %s" % (path, e), file=sys.stderr)
        sys.exit(2)
    if not isinstance(data, dict) or not isinstance(data.get("functions"), dict) or not str(data.get("_reason", "")).strip():
        print("core-lizard: %s needs a non-empty _reason and a functions object (why is this debt accepted?)" % path, file=sys.stderr)
        sys.exit(2)
    return data


def compare(found, baseline):
    problems = []
    for key, m in sorted(found.items()):
        old = baseline.get(key)
        if old is None:
            problems.append("new over-limit function: %s (%s)" % (key, ", ".join("%s %d" % (k, m[k]) for k in WORSE if k in m)))
            continue
        for k in WORSE:
            if m.get(k, 0) > old.get(k, 0):
                problems.append("worse: %s %s %d > baseline %d" % (key, k, m[k], old.get(k, 0)))
    return problems


def main(argv):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--write", action="store_true")
    ap.add_argument("--baseline", default=None)
    for name, default in (("ccn", 10), ("length", 50), ("params", 4), ("cognitive", 15), ("nesting", 4)):
        ap.add_argument("--" + name, type=int, default=default)
    ap.add_argument("-x", dest="exclude", action="append", default=[])
    ap.add_argument("root", nargs="?", default=".")
    args = ap.parse_args([a for a in argv if a != "--"])
    path = args.baseline or DEFAULT_BASELINE
    found = run_lizard(args, args.root)
    if args.write:
        old = {}
        if Path(path).is_file():
            old = load_baseline(path)
        reason = old.get("_reason") or "Existing over-limit functions accepted on %s; new code must stay within the limits. Fix them when touched." % __import__("datetime").date.today()
        Path(path).write_text(json.dumps({"_reason": reason, "functions": dict(sorted(found.items()))}, indent=1, sort_keys=False) + "\n")
        print("core-lizard: wrote %d function(s) to %s" % (len(found), path))
        return 0
    baseline = load_baseline(path)["functions"] if Path(path).is_file() else {}
    if not baseline:
        problems = ["over-limit function: %s" % k for k in sorted(found)]
    else:
        problems = compare(found, baseline)
    for line in problems:
        print("core-lizard: " + line)
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
