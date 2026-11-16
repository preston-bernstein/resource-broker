#!/usr/bin/env bash
# One command checks the writing in a repo: `bash scripts/writing-validate.sh [paths...]`.
# Developers, the pre-commit hook, CI and agents run this same command. A missing vale fails with exit 127.
#
# With no paths it checks the files that changed against the base branch (git), plus untracked files, so a repo with
# old prose can adopt it without a wall of findings. WRITING_ALL=1 checks every prose and source file.
# Only error rules fail the gate. Warnings print and pass: promote a rule to error when you would reject a PR over it.
# A path pattern in `.writing-allow` (one per line, `# reason` after it) is skipped, and so is anything in vendor or build paths,
# whether the path came from git or from the command line.
set -u
VALE="${WRITING_VALE:-vale}"
if ! command -v "$VALE" >/dev/null 2>&1; then
  echo "writing-validate: vale not found. Install it: https://vale.sh/docs/install" >&2
  exit 127
fi
# The project wins: a repo with its own .vale.ini uses it. Otherwise this script's own styles apply (central mode), so a
# repo needs no copy of the pack. A copy of this script outside the pack has no styles and needs a .vale.ini.
CONFIG=()
if [ ! -f .vale.ini ] && [ ! -f _vale.ini ]; then
  HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  if [ -f "$HERE/vale-central.ini" ] && [ -d "$HERE/styles" ]; then
    CONFIG=(--config "$HERE/vale-central.ini")
  else
    echo "writing-validate: no .vale.ini here. Copy templates/vale.ini from the writing pack." >&2
    exit 2
  fi
fi

EXT='\.(md|py|js|jsx|ts|tsx|go|java|rs|rb|c|h|cc|cpp|css|swift)$'
SKIP='(^|/)(\.vale|node_modules|vendor|dist|build|\.git)/|(^|/)(CHANGELOG|LICENSE)[^/]*$'

all_files() {
  if git rev-parse --git-dir >/dev/null 2>&1; then
    { git ls-files; git ls-files --others --exclude-standard; } | sort -u
  else
    find . -type f -not -path './.git/*' | sed 's|^\./||'
  fi
}

changed_files() {
  local base
  base="$(git merge-base HEAD "origin/${WRITING_BASE_BRANCH:-main}" 2>/dev/null || true)"
  if [ -n "$base" ]; then git diff --name-only --diff-filter=d "$base"; else git diff --name-only --diff-filter=d HEAD 2>/dev/null; fi
  git ls-files --others --exclude-standard
}

if [ "$#" -gt 0 ]; then
  source_list="$(printf '%s\n' "$@")"          # explicit paths still pass the filters below
elif [ "${WRITING_ALL:-0}" = 1 ] || ! git rev-parse --git-dir >/dev/null 2>&1; then
  source_list="$(all_files)"
else
  source_list="$(changed_files | sort -u)"
fi
allow=()
if [ -f .writing-allow ]; then
  while IFS= read -r line; do line="${line%%#*}"; line="${line//[[:space:]]/}"; [ -n "$line" ] && allow+=("$line"); done < .writing-allow
fi
files=()
while IFS= read -r f; do
  [ -f "$f" ] || continue
  printf '%s' "$f" | grep -Eq "$EXT" || continue
  printf '%s' "$f" | grep -Eq "$SKIP" && continue
  skip=0
  for pat in "${allow[@]+"${allow[@]}"}"; do case "$f" in $pat) skip=1 ;; esac; done
  [ "$skip" = 1 ] || files+=("$f")
done <<<"$source_list"

if [ "${#files[@]}" -eq 0 ]; then echo "writing-validate: no prose or source files to check"; exit 0; fi
"$VALE" "${CONFIG[@]+"${CONFIG[@]}"}" --minAlertLevel=warning --no-wrap "${files[@]}"
