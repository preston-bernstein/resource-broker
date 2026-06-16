#!/bin/bash
# code-reviewer.sh
# CI/CD code review service - uses code specialist model
#
# Usage: ./code-reviewer.sh <PR-number>

PR_NUMBER="$1"

if [ -z "$PR_NUMBER" ]; then
  echo "Usage: $0 <PR-number>"
  exit 1
fi

# Business logic: Code review needs specialized model
MODEL="deepseek-coder:33b"

# Fetch PR diff (example - you'd integrate with your actual git/GitHub setup)
# For now, just a placeholder
PR_DIFF="<fetch from git or GitHub API>"

# Build prompt
PROMPT="You are a senior code reviewer. Review the following pull request:

PR #$PR_NUMBER

$PR_DIFF

Analyze for:
1. Security vulnerabilities (SQL injection, XSS, etc.)
2. Code quality issues (complexity, duplication, naming)
3. Performance concerns
4. Testing coverage
5. Architecture/design issues

Provide actionable feedback with severity levels (critical/high/medium/low)."

# Call generic batch wrapper with metadata
/usr/local/bin/batch-job-wrapper.sh \
  --job-id "pr-review-$PR_NUMBER" \
  --caller "code-reviewer" \
  --tags "cicd,code-review,pr-$PR_NUMBER" \
  --command "ollama run $MODEL '$PROMPT'"
