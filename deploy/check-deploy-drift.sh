#!/usr/bin/env bash
# Checks whether the deploy-host git checkout of this repo can still sync
# with origin and how far behind it has fallen, writing Prometheus textfile
# metrics so drift shows up in Grafana instead of silently sitting for weeks.
#
# Why this exists (2026-08-15): the desktop checkout at
# /home/ollama-broker/resource-broker sat 30+ commits behind origin/main
# for weeks with zero observability, because `sudo -u ollama-broker git fetch`
# had been silently failing since the account was created — the ollama-broker
# system user's real home (/var/lib/ollama-broker, per /etc/passwd) doesn't
# contain the .ssh/config that defines the deploy-key SSH alias, which lives
# instead under /home/ollama-broker. Actual binary deploys have always
# happened via a separate build+scp path, so this went unnoticed. Fixed the
# underlying SSH auth (core.sshCommand now references the key by absolute
# path, no alias/HOME dependency) and re-synced the checkout — this script is
# the monitoring half, so the next time something like this breaks, it's a
# dashboard/alert instead of a discovery mid-deploy.
#
# Usage: check-deploy-drift.sh <checkout-path> <textfile-output-path>

set -euo pipefail

CHECKOUT="${1:?usage: check-deploy-drift.sh <checkout-path> <textfile-output-path>}"
OUT="${2:?usage: check-deploy-drift.sh <checkout-path> <textfile-output-path>}"
TMP="${OUT}.tmp.$$"

fetch_ok=1
git -C "$CHECKOUT" fetch origin --quiet || fetch_ok=0

behind=0
if [ "$fetch_ok" -eq 1 ]; then
	behind=$(git -C "$CHECKOUT" rev-list --count HEAD..origin/main 2>/dev/null || echo -1)
fi

# Dirty means uncommitted changes to TRACKED files only — untracked files
# (e.g. broker-control-token.env, a real secret that belongs on this host
# and never in git) are expected and not drift.
dirty=0
if [ -n "$(git -C "$CHECKOUT" diff --name-only HEAD 2>/dev/null)" ]; then
	dirty=1
fi

now=$(date +%s)

{
	echo "# HELP resource_broker_deploy_git_fetch_success Whether the last git fetch against origin succeeded (1) or failed (0)."
	echo "# TYPE resource_broker_deploy_git_fetch_success gauge"
	echo "resource_broker_deploy_git_fetch_success $fetch_ok"

	echo "# HELP resource_broker_deploy_checkout_behind_commits How many commits the deploy checkout is behind origin/main."
	echo "# TYPE resource_broker_deploy_checkout_behind_commits gauge"
	echo "resource_broker_deploy_checkout_behind_commits $behind"

	echo "# HELP resource_broker_deploy_checkout_dirty Whether the deploy checkout has uncommitted changes to tracked files (1) or is clean (0)."
	echo "# TYPE resource_broker_deploy_checkout_dirty gauge"
	echo "resource_broker_deploy_checkout_dirty $dirty"

	echo "# HELP resource_broker_deploy_drift_check_timestamp_seconds Unix timestamp of the last drift check."
	echo "# TYPE resource_broker_deploy_drift_check_timestamp_seconds gauge"
	echo "resource_broker_deploy_drift_check_timestamp_seconds $now"
} >"$TMP"

mv "$TMP" "$OUT"
