#!/usr/bin/env bash
# Validation gate for agent-written GitHub Actions workflows. Pack: github-actions, rule gha-one-gate-command.
#
#   gha-validate.sh [ROOT]
#
# Order: workflow-check.py (pins, permissions, injection, pull_request_target, checkout, timeouts, dependabot)
#        -> actionlint (syntax and expressions) -> zizmor (security audit).
# Every tool is checked before any step runs. A missing tool fails the gate with exit 127. Nothing is skipped.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="${1:-.}"
cd "$ROOT"

fail() { echo "gha-validate: $*" >&2; exit "${2:-1}"; }
need() { command -v "$1" >/dev/null 2>&1 || fail "missing tool: $1 (the gate never skips a step)" 127; }

if [ ! -d .github/workflows ] || [ -z "$(find .github/workflows -maxdepth 1 -type f \( -name '*.yml' -o -name '*.yaml' \))" ]; then
  echo "gha-validate: no workflows, nothing to check"
  exit 0
fi

need python3
need actionlint
need zizmor
[ -f "$HERE/workflow-check.py" ] || fail "workflow-check.py not found next to this script" 2

python3 "$HERE/workflow-check.py" .
actionlint
zizmor .github/workflows
echo "gha-validate: all checks passed"
