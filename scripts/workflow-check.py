#!/usr/bin/env python3
"""Audit GitHub Actions workflows for the mistakes agents make most. Pack: github-actions.

    workflow-check.py [ROOT]

Reads ROOT/.github/workflows/*.yml and *.yaml as text (standard library only). Exit 0 when clean, 1 with one line
per problem. It does not replace actionlint (syntax) or zizmor (the full audit catalog); it makes the five rules
below a hard gate that needs no network:

  - every `uses:` is pinned to a full 40-character commit SHA (local `./` and `docker://` are exempt)
  - the workflow sets `permissions:` at the top level, or every job sets its own
  - no `pull_request_target` trigger, unless its line says `# gha-allow: pull_request_target <reason>`
  - no untrusted context (PR title, body, branch name, issue text, inputs) inside a `run:` or `script:` body
  - actions/checkout sets `persist-credentials: false`; every job has `timeout-minutes`; no `secrets: inherit`
  - .github/dependabot.yml keeps the `github-actions` ecosystem current
"""
from __future__ import annotations

import re
import sys
from pathlib import Path

SHA_RE = re.compile(r"[0-9a-f]{40}")
USES_RE = re.compile(r"^(\s*)(-\s+)?uses:\s*['\"]?([^\s'\"#]+)")
RUN_RE = re.compile(r"^(\s*)(-\s+)?(run|script):\s*(.*)$")
BLOCK_MARKERS = {"|", "|-", "|+", ">", ">-", ">+"}
UNTRUSTED = re.compile(
    r"\$\{\{[^}]*?(github\.head_ref|github\.event\.inputs\.|\binputs\.\w+"
    r"|github\.event\.[\w.\[\]*'-]*?\.(title|body|name|label|ref|message|email|login|page_name|default_branch))[^}]*\}\}")


def indent(line: str) -> int:
    return len(line) - len(line.lstrip(" "))


def code_part(line: str) -> str:
    return re.sub(r"\s+#.*$", "", line)


def job_blocks(lines: list[str]) -> list[tuple[str, int, int]]:
    """(name, start, end) for each job under `jobs:`, by indentation."""
    jobs: list[tuple[str, int, int]] = []
    in_jobs = False
    for i, line in enumerate(lines):
        if re.match(r"^jobs\s*:", line):
            in_jobs = True
            continue
        if in_jobs and line.strip() and indent(line) == 0 and not line.lstrip().startswith("#"):
            in_jobs = False
        m = re.match(r"^  ([\w.-]+)\s*:\s*$", line) if in_jobs else None
        if m:
            if jobs:
                jobs[-1] = (jobs[-1][0], jobs[-1][1], i)
            jobs.append((m.group(1), i, len(lines)))
    return jobs


def check_workflow(path: Path) -> list[str]:
    lines = path.read_text().splitlines()
    out: list[str] = []
    name = path.name

    def add(i: int, msg: str) -> None:
        out.append(f"{name}:{i + 1}: {msg}")

    # permissions
    jobs = job_blocks(lines)
    top_perm = any(re.match(r"^permissions\s*:", l) for l in lines)
    if not top_perm:
        for jname, start, end in jobs:
            if not any(re.match(r"^ {4}permissions\s*:", l) for l in lines[start:end]):
                add(start, f"job '{jname}' has no permissions (set `permissions:` at the top level, read-only by default)")
        if not jobs:
            add(0, "no permissions set (add a top-level `permissions: contents: read`)")

    # timeout-minutes (reusable-workflow calls are exempt)
    for jname, start, end in jobs:
        block = lines[start:end]
        if any(re.match(r"^ {4}uses\s*:", l) for l in block):
            continue
        if not any(re.match(r"^ {4}timeout-minutes\s*:", l) for l in block):
            add(start, f"job '{jname}' has no timeout-minutes")

    for i, line in enumerate(lines):
        stripped = code_part(line)
        # pull_request_target
        if re.search(r"\bpull_request_target\b", stripped) and "gha-allow: pull_request_target" not in line:
            add(i, "pull_request_target trigger (runs with secrets on fork code); use pull_request, or justify with "
                   "`# gha-allow: pull_request_target <reason>`")
        # secrets: inherit
        if re.match(r"^\s*secrets\s*:\s*inherit\b", stripped):
            add(i, "secrets: inherit passes every secret to the called workflow; pass only the ones it needs")
        # unpinned uses
        m = USES_RE.match(line)
        if m:
            ref = m.group(3)
            if not (ref.startswith("./") or ref.startswith("docker://")):
                version = ref.split("@", 1)[1] if "@" in ref else ""
                if not SHA_RE.fullmatch(version):
                    add(i, f"action '{ref}' is not pinned to a full commit SHA (pin it and keep the version in a comment)")
            if ref.startswith("actions/checkout@"):
                marker = indent(line) if m.group(2) else None
                if marker is None:
                    j = i - 1
                    while j >= 0 and not (re.match(r"^\s*-\s", lines[j]) and indent(lines[j]) < indent(line)):
                        j -= 1
                    marker = indent(lines[j]) if j >= 0 else indent(line)
                    start = max(j, 0)
                else:
                    start = i
                end = start + 1
                while end < len(lines) and not (lines[end].strip() and indent(lines[end]) <= marker
                                                and not lines[end].lstrip().startswith("#")):
                    end += 1
                if not any(re.search(r"persist-credentials\s*:\s*false", l) for l in lines[start:end]):
                    add(i, "actions/checkout without `persist-credentials: false` leaves the token on disk for later steps")
        # script injection in run: / script:
        r = RUN_RE.match(line)
        if r:
            key_indent = len(r.group(1)) + (len(r.group(2)) if r.group(2) else 0)
            rest = code_part(r.group(4)).strip()
            if rest in BLOCK_MARKERS:
                j = i + 1
                while j < len(lines) and (not lines[j].strip() or indent(lines[j]) > key_indent):
                    if UNTRUSTED.search(lines[j]):
                        add(j, "script injection: untrusted context inside a run/script body; pass it through `env:` and read $VAR")
                    j += 1
            elif UNTRUSTED.search(rest):
                add(i, "script injection: untrusted context inside a run/script body; pass it through `env:` and read $VAR")
    return out


def check_dependabot(root: Path) -> list[str]:
    for name in ("dependabot.yml", "dependabot.yaml"):
        f = root / ".github" / name
        if f.is_file():
            if re.search(r"package-ecosystem\s*:\s*['\"]?github-actions", f.read_text()):
                return []
            return [f"{name}: no `package-ecosystem: github-actions` entry, so pinned SHAs will go stale"]
    return ["dependabot: .github/dependabot.yml is missing; add the github-actions ecosystem to keep SHA pins current"]


def main(argv: list[str]) -> int:
    root = Path(argv[1] if len(argv) > 1 else ".")
    wf_dir = root / ".github" / "workflows"
    files = sorted(list(wf_dir.glob("*.yml")) + list(wf_dir.glob("*.yaml"))) if wf_dir.is_dir() else []
    if not files:
        print("workflow-check: no workflows, nothing to check")
        return 0
    problems: list[str] = []
    for f in files:
        problems += check_workflow(f)
    problems += check_dependabot(root)
    for p in problems:
        print(p)
    if not problems:
        print("workflow-check: clean")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
