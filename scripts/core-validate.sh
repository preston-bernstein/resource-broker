#!/usr/bin/env bash
# Validation gate for any codebase. Pack: core, rule core-one-gate.
#
#   core-validate.sh [ROOT]
#
# Order: core-check.py (file size, grab-bag names, short AGENTS.md) -> lizard (complexity, length, params, nesting)
#        -> jscpd (duplication).
# Every tool is checked before any step runs. A missing tool fails the gate with exit 127. Nothing is skipped.
# Tighten a limit with an environment variable; loosening one needs a reason in the PR.
#   CORE_CCN=10 CORE_COGNITIVE=15 CORE_LENGTH=50 CORE_PARAMS=4 CORE_NESTING=4 CORE_DUP_THRESHOLD=3 CORE_MAX_LINES=750
#   CORE_EXCLUDE="space separated globs"  (added to the defaults: vendored, generated, build output, tests data)
set -euo pipefail

ROOT="${1:-.}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

fail() { echo "core-validate: $*" >&2; exit "${2:-1}"; }
need() { command -v "$1" >/dev/null 2>&1 || fail "missing tool: $1 (the gate never skips a step)" 127; }

need python3
need lizard
need jscpd

# .core-config: CORE_* overrides, one per line, each with a reason: CORE_DUP_THRESHOLD=9  # why
if [ -f .core-config ]; then
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in ""|"#"*) continue ;; esac
    if [[ "$line" =~ ^(CORE_[A-Z_]+)=([A-Za-z0-9_.*/-]+)[[:space:]]+#[[:space:]]*[^[:space:]].*$ ]]; then
      cfg_name="${BASH_REMATCH[1]}"; cfg_val="${BASH_REMATCH[2]}"
      [ -n "${!cfg_name+x}" ] || export "$cfg_name=$cfg_val"      # the environment wins over the file
    else
      fail ".core-config: each line must be CORE_NAME=value  # reason (got: $line)" 2
    fi
  done < .core-config
fi

CCN="${CORE_CCN:-10}"
COG="${CORE_COGNITIVE:-15}"
LEN="${CORE_LENGTH:-50}"
PARAMS="${CORE_PARAMS:-4}"
NEST="${CORE_NESTING:-4}"
DUP="${CORE_DUP_THRESHOLD:-3}"
DUP_TOKENS="${CORE_DUP_MIN_TOKENS:-50}"
DUP_LINES="${CORE_DUP_MIN_LINES:-5}"

EXCLUDES=(-x "*/node_modules/*" -x "*/vendor/*" -x "*/dist/*" -x "*/build/*" -x "*/target/*" -x "*/.venv/*" -x "*/venv/*"
          -x "*.min.js" -x "*/generated/*" -x "*/__generated__/*" -x "*.lock")
JSCPD_IGNORE="**/node_modules/**,**/vendor/**,**/dist/**,**/build/**,**/target/**,**/.venv/**,**/venv/**,**/*.min.js,**/generated/**,**/*.lock"
for glob in ${CORE_EXCLUDE:-}; do
  EXCLUDES+=(-x "$glob")
  JSCPD_IGNORE="$JSCPD_IGNORE,$glob"
done

# A git submodule is third-party code the repo pins: lizard and jscpd skip it, as core-check.py does.
if [ -f .gitmodules ]; then
  while IFS= read -r sub; do
    [ -n "$sub" ] || continue
    EXCLUDES+=(-x "*/$sub/*")
    JSCPD_IGNORE="$JSCPD_IGNORE,**/$sub/**"
  done < <(sed -n 's/^[[:space:]]*path[[:space:]]*=[[:space:]]*//p' .gitmodules | sed 's#/*$##')
fi

# CORE_WRITE_BASELINE=1 records today's over-limit functions in .core-baseline.json (same limits and excludes as the gate) and stops.
if [ "${CORE_WRITE_BASELINE:-}" = "1" ]; then
  python3 "$HERE/core-lizard.py" --write --baseline .core-baseline.json --ccn "$CCN" --length "$LEN" --params "$PARAMS" \
    --cognitive "$COG" --nesting "$NEST" "${EXCLUDES[@]}" -- .
  exit $?
fi

python3 "$HERE/core-check.py" . || fail "core-check failed"

# -w prints only functions over a limit. Exit 1 when any warning fires.
# A repo with a .core-baseline.json fails only on new or worse functions (core-lizard.py); write one with
# `python3 core-lizard.py --write --baseline .core-baseline.json -- .` and a _reason.
if [ -f .core-baseline.json ]; then
  python3 "$HERE/core-lizard.py" --baseline .core-baseline.json --ccn "$CCN" --length "$LEN" --params "$PARAMS" \
    --cognitive "$COG" --nesting "$NEST" "${EXCLUDES[@]}" -- . \
    || fail "lizard: a function is new or worse than .core-baseline.json"
else
  lizard -C "$CCN" -L "$LEN" -a "$PARAMS" -Ecognitive -ENS \
    -T "cognitive_complexity=$COG" -T "max_nested_structures=$NEST" -w "${EXCLUDES[@]}" . \
    || fail "lizard: a function is too complex, long, nested or has too many parameters"
fi

jscpd --min-tokens "$DUP_TOKENS" --min-lines "$DUP_LINES" --threshold "$DUP" --silent --ignore "$JSCPD_IGNORE" . \
  || fail "jscpd: duplicated code is over ${DUP}%"
echo "core-validate: all checks passed"
