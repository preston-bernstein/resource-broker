# Legacy: Bash V3 resource manager

This is the original, deployed resource manager — a polling systemd daemon that
detects gaming/Plex by process name and preempts + queues **CLI batch jobs** run
through its wrapper. It is kept for reference and for the fire-and-forget CLI job
path; it does **not** front Ollama's HTTP API.

It is superseded by the Go HTTP-fronting broker at the repo root (see top-level
`README.md` and `docs/DESIGN.md`). See `docs/adr/0001` for why.

Key files: `resource-manager-v3.sh`, `batch-job-wrapper-v5.sh`,
`CLEANUP-AND-DEPLOY.sh`, plus the original design docs and `archive/`.
