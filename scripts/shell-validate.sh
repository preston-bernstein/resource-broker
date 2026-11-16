#!/usr/bin/env bash
# Validation gate for agent-written shell scripts. Pack: shell, rule sh-one-gate-command.
#
#   shell-validate.sh [ROOT]
#
# Order: bash -n syntax check -> shell-check.py -> shellcheck -x -> shfmt -d -> bats (when *.bats exist).
# Every tool is checked before any step runs. A missing tool fails the gate with exit 127. Nothing is skipped.
# Env:
#   SH_BASH   bash used for the syntax check (default /bin/bash, which is 3.2 on macOS)
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="${1:-.}"
cd "$ROOT"
SH_BASH="${SH_BASH:-/bin/bash}"
[ -x "$SH_BASH" ] || SH_BASH=bash

fail() { echo "shell-validate: $*" >&2; exit "${2:-1}"; }
need() { command -v "$1" >/dev/null 2>&1 || fail "missing tool: $1 (the gate never skips a step)" 127; }

prune=( \( -name node_modules -o -name vendor -o -name '.?*' \) -prune -o )
scripts=()
while IFS= read -r f; do scripts+=("$f"); done < <(
  find . -mindepth 1 "${prune[@]}" -type f \( -name '*.sh' -o -name '*.bash' \) -print | sort)
bats_files=()
while IFS= read -r f; do bats_files+=("$f"); done < <(
  find . -mindepth 1 "${prune[@]}" -type f -name '*.bats' -print | sort)

# .shell-allow: `<glob> <reason>` per line leaves vendored or template paths out of every step (shell-check.py reports a line with no reason).
# A git submodule (.gitmodules) is third-party code and is always left out.
skip_globs=()
if [ -f .gitmodules ]; then
  while IFS= read -r sub; do
    [ -n "$sub" ] && skip_globs+=("$sub/*")
  done < <(sed -n 's/^[[:space:]]*path[[:space:]]*=[[:space:]]*//p' .gitmodules | sed 's#/*$##')
fi
if [ -f .shell-allow ]; then
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in "" | "#"*) continue ;; esac
    read -r glob reason <<<"$line"
    [ -z "${reason:-}" ] || skip_globs+=("$glob")
  done <.shell-allow
fi
if [ "${#skip_globs[@]}" -gt 0 ]; then
  filter() {
    local f rel g keep
    for f in "$@"; do
      rel="${f#./}"
      keep=1
      for g in "${skip_globs[@]+"${skip_globs[@]}"}"; do
        # shellcheck disable=SC2254 # the glob comes from .shell-allow on purpose
        case "$rel" in $g) keep=0 ;; esac
      done
      [ "$keep" = 1 ] && printf '%s\n' "$f"
    done
  }
  filtered=()
  while IFS= read -r f; do filtered+=("$f"); done < <(filter "${scripts[@]+"${scripts[@]}"}")
  scripts=("${filtered[@]+"${filtered[@]}"}")
  filtered=()
  while IFS= read -r f; do filtered+=("$f"); done < <(filter "${bats_files[@]+"${bats_files[@]}"}")
  bats_files=("${filtered[@]+"${filtered[@]}"}")
fi

if [ "${#scripts[@]}" -eq 0 ]; then
  echo "shell-validate: no shell scripts, nothing to check"
  exit 0
fi

need python3
need shellcheck
need shfmt
[ "${#bats_files[@]}" -eq 0 ] || need bats
[ -f "$HERE/shell-check.py" ] || fail "shell-check.py not found next to this script" 2

for s in "${scripts[@]}"; do
  "$SH_BASH" -n "$s" || fail "syntax error in $s"
done
python3 "$HERE/shell-check.py" .
shellcheck -x "${scripts[@]}"
shfmt -d "${scripts[@]}"
if [ "${#bats_files[@]}" -gt 0 ]; then bats "${bats_files[@]}"; fi
echo "shell-validate: all checks passed"
