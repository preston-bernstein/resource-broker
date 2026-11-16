#!/usr/bin/env bash
# Installs the tools the gates in scripts/ need, at pinned versions, into a tool directory.
# Python tools install from a hash lock. Node tools install from package-lock.json. Binaries are
# checked against a SHA-256 before they are unpacked. Usage: ci/install-gate-tools.sh <tool-dir>
# Then add <tool-dir>/bin, <tool-dir>/venv/bin and ci/node_modules/.bin to PATH.
set -euo pipefail

GOLANGCI_LINT_VERSION=2.12.2
GOLANGCI_LINT_SHA256=8df580d2670fed8fa984aac0507099af8df275e665215f5c7a2ae3943893a553
ACTIONLINT_VERSION=1.7.12
ACTIONLINT_SHA256=8aca8db96f1b94770f1b0d72b6dddcb1ebb8123cb3712530b08cc387b349a3d8
VALE_VERSION=3.27.0
VALE_SHA256=c80d62f6bce87b81843ac5b512a26e63213927b957adba0a64fc594570da2000
SHFMT_VERSION=3.13.1
SHFMT_SHA256=fb096c5d1ac6beabbdbaa2874d025badb03ee07929f0c9ff67563ce8c75398b1

scratch=""

log() { echo "install-gate-tools: $*" >&2; }
die() {
  log "$*"
  exit "${2:-1}"
}

# Downloads <url> to <file> and fails unless it matches <sha256>.
fetch_verified() {
  local url="$1" file="$2" sha256="$3"
  curl --fail --silent --show-error --location --output "$file" "$url"
  echo "$sha256  $file" | sha256sum --check --status || die "checksum mismatch for $url"
}

main() {
  local tools="${1:-}"
  [ -n "$tools" ] || die "usage: install-gate-tools.sh <tool-dir>" 2
  local root
  root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  scratch="$(mktemp -d)"
  trap 'rm -rf "$scratch"' EXIT
  mkdir -p "$tools/bin"
  tools="$(cd "$tools" && pwd)"

  python3 -m venv "$tools/venv"
  "$tools/venv/bin/python" -m pip install --quiet --require-hashes -r "$root/ci/gate-tools.lock"
  npm ci --prefix "$root/ci" --ignore-scripts --no-audit --no-fund --silent

  fetch_verified "https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-amd64.tar.gz" \
    "$scratch/golangci-lint.tar.gz" "$GOLANGCI_LINT_SHA256"
  tar -xzf "$scratch/golangci-lint.tar.gz" -C "$scratch"
  install -m 0755 "$scratch/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-amd64/golangci-lint" "$tools/bin/golangci-lint"

  fetch_verified "https://github.com/rhysd/actionlint/releases/download/v${ACTIONLINT_VERSION}/actionlint_${ACTIONLINT_VERSION}_linux_amd64.tar.gz" \
    "$scratch/actionlint.tar.gz" "$ACTIONLINT_SHA256"
  tar -xzf "$scratch/actionlint.tar.gz" -C "$tools/bin" actionlint

  fetch_verified "https://github.com/vale-cli/vale/releases/download/v${VALE_VERSION}/vale_${VALE_VERSION}_Linux_64-bit.tar.gz" \
    "$scratch/vale.tar.gz" "$VALE_SHA256"
  tar -xzf "$scratch/vale.tar.gz" -C "$tools/bin" vale

  fetch_verified "https://github.com/mvdan/sh/releases/download/v${SHFMT_VERSION}/shfmt_v${SHFMT_VERSION}_linux_amd64" \
    "$tools/bin/shfmt" "$SHFMT_SHA256"
  chmod 0755 "$tools/bin/shfmt"
  log "installed into $tools"
}

main "$@"
