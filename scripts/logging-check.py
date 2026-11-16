#!/usr/bin/env python3
"""Audit a repo's logging against the fleet contract. Pack: logging.

    logging-check.py [ROOT]            audit Compose files and Python service code under ROOT
    logging-check.py --lines FILE      check that every line of FILE is a canonical JSON log line

Standard library only. Exit 0 when clean, 1 with one line per problem.

Audit mode checks:
  * every Compose service sets `logging: options: max-size` (log rotation; Docker keeps logs forever otherwise).
    Waive one service with a comment inside it: `# log-allow: rotation <reason>`. A service that merges a YAML alias
    (`<<: *x`) is skipped, because the alias may carry the logging block.
  * no Compose service sets the label `observability.logs: "true"`. Since 2026-11-14 the collector ships every
    container by default, so the label does nothing; `"false"` opts a service out and is fine.
  * no bare `print(` in service Python code (dot-dirs, vendored dirs, tests, scripts and test_*.py are skipped).
    Waive one with `# log-allow: print <reason>` on the line or the line above, or a path in `.logging-allow`
    (`<glob> <reason>`, the reason is required).

Lines mode checks the canonical line from CONVENTIONS section 18: schema_version, ts (RFC3339 UTC), level (one of
debug, info, warn, error, critical), service, event (dot-namespaced), msg and run_id are required; outcome, when
present, is ok, partial or failed, and anything but ok also needs err_type and err_msg.
"""
from __future__ import annotations

import fnmatch
import json
import re
import sys
from pathlib import Path

SKIP_DIRS = {"node_modules", "vendor", "__pycache__", "dist", "build", "venv", "tests", "test", "scripts"}
LEVELS = {"debug", "info", "warn", "error", "critical"}
OUTCOMES = {"ok", "partial", "failed"}
REQUIRED = ("schema_version", "ts", "level", "service", "event", "msg", "run_id")
TS = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$")
EVENT = re.compile(r"^[a-z0-9_]+(\.[a-z0-9_]+)+$")
COMPOSE = re.compile(r"^(docker-)?compose[^/]*\.ya?ml$")
OPT_IN = re.compile(r"""observability\.logs["']?\s*[:=]\s*["']?true\b""")


def check_lines(path: Path) -> list[str]:
    problems = []
    for n, raw in enumerate(path.read_text().splitlines(), 1):
        if not raw.strip():
            continue
        try:
            obj = json.loads(raw)
        except ValueError:
            obj = None
        if not isinstance(obj, dict):
            problems.append(f"{path.name}:{n}: not a JSON object")
            continue
        for key in REQUIRED:
            if obj.get(key) in (None, ""):
                problems.append(f"{path.name}:{n}: missing required field {key}")
        if "ts" in obj and not (isinstance(obj["ts"], str) and TS.match(obj["ts"])):
            problems.append(f"{path.name}:{n}: ts must be RFC3339 UTC like 2026-11-14T14:32:07Z")
        if obj.get("level") not in LEVELS and obj.get("level") not in (None, ""):
            problems.append(f"{path.name}:{n}: level {obj['level']!r} must be one of {sorted(LEVELS)}")
        if obj.get("event") and not (isinstance(obj["event"], str) and EVENT.match(obj["event"])):
            problems.append(f"{path.name}:{n}: event {obj['event']!r} must be dot-namespaced like batch.completed")
        outcome = obj.get("outcome")
        if outcome is not None and outcome not in OUTCOMES:
            problems.append(f"{path.name}:{n}: outcome {outcome!r} must be one of {sorted(OUTCOMES)}")
        elif outcome in ("partial", "failed"):
            for key in ("err_type", "err_msg"):
                if not obj.get(key):
                    problems.append(f"{path.name}:{n}: outcome {outcome} needs {key}")
    return problems


def visible(root: Path):
    for p in sorted(root.rglob("*")):
        rel = p.relative_to(root).parts
        if p.is_file() and not any(part.startswith(".") or part in SKIP_DIRS for part in rel[:-1]):
            yield p


def services(text: str):
    """Yield (name, first_line_number, block_text) for each service under the top-level `services:` key."""
    lines = text.splitlines()
    in_services, indent, current = False, None, None
    for n, line in enumerate(lines, 1):
        if re.match(r"^services:\s*(#.*)?$", line):
            in_services = True
            continue
        if in_services and re.match(r"^\S", line) and not line.startswith("#"):
            in_services = False
        if not in_services:
            continue
        m = re.match(r"^( +)([A-Za-z0-9_.-]+):\s*(#.*)?$", line)
        if m and (indent is None or len(m.group(1)) == indent):
            indent = len(m.group(1))
            if current:
                yield current[0], current[1], "\n".join(current[2])
            current = (m.group(2), n, [])
        elif current:
            current[2].append(line)
    if current:
        yield current[0], current[1], "\n".join(current[2])


def check_compose(path: Path, root: Path) -> list[str]:
    problems, rel = [], path.relative_to(root).as_posix()
    for name, n, block in services(path.read_text()):
        if OPT_IN.search(block):
            problems.append(f'{rel}:{n}: service {name} sets observability.logs "true"; the collector ships every '
                            'container now, so it does nothing. Remove it, or use "false" to opt out')
        if re.search(r"<<:\s*\*", block) or re.search(r"#\s*log-allow:\s*rotation\s+\S+", block):
            continue
        if "max-size" not in block:
            problems.append(f"{rel}:{n}: service {name} has no logging options max-size (log rotation)")
    return problems


def load_allow(root: Path):
    globs, problems = [], []
    f = root / ".logging-allow"
    if f.is_file():
        for n, line in enumerate(f.read_text().splitlines(), 1):
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            parts = line.split(None, 1)
            if len(parts) < 2:
                problems.append(f".logging-allow:{n}: needs `<glob> <reason>`, the reason is required")
            else:
                globs.append(parts[0])
    return globs, problems


def check_python(path: Path, root: Path) -> list[str]:
    name = path.name
    if name.startswith("test_") or name.endswith("_test.py") or name == "conftest.py":
        return []
    rel, problems = path.relative_to(root).as_posix(), []
    lines = path.read_text().splitlines()
    for n, line in enumerate(lines, 1):
        if not re.match(r"^\s*print\(", line):
            continue
        context = line + "\n" + (lines[n - 2] if n > 1 else "")
        if re.search(r"#\s*log-allow:\s*print\s+\S+", context):
            continue
        problems.append(f"{rel}:{n}: bare print(); use the JSON logger (or `# log-allow: print <reason>`)")
    return problems


def audit(root: Path) -> list[str]:
    allow, problems = load_allow(root)
    for p in visible(root):
        rel = p.relative_to(root).as_posix()
        if any(fnmatch.fnmatchcase(rel, g) for g in allow):
            continue
        if COMPOSE.match(p.name):
            problems += check_compose(p, root)
        elif p.suffix == ".py":
            problems += check_python(p, root)
    return problems


def main(argv: list[str]) -> int:
    if len(argv) >= 2 and argv[0] == "--lines":
        problems = check_lines(Path(argv[1]))
    else:
        problems = audit(Path(argv[0] if argv else "."))
    for line in problems:
        print(line)
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
