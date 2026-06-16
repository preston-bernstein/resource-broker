#!/bin/bash
# batch-job-wrapper-v4.sh
# Generic command wrapper with queue support
# Calling applications decide what to run (model selection is their business logic)

LOG="/var/log/batch-job-wrapper.log"
STATE_FILE="/tmp/resource-manager-state"
QUEUE_DIR="/var/lib/batch-jobs/queue"

# Logging
log() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1" | tee -a "$LOG"
}

# Parse arguments
JOB_ID=""
COMMAND=""

while [[ $# -gt 0 ]]; do
  case $1 in
    --job-id)
      JOB_ID="$2"
      shift 2
      ;;
    --command)
      COMMAND="$2"
      shift 2
      ;;
    *)
      echo "ERROR: Unknown option: $1"
      echo "Usage: batch-job-wrapper.sh --job-id <id> --command <command>"
      exit 1
      ;;
  esac
done

# Generate job ID if not provided
if [ -z "$JOB_ID" ]; then
  JOB_ID="job-$(date +%s)-$$"
fi

# Check if command specified
if [ -z "$COMMAND" ]; then
  log "ERROR: No command specified. Use --command <command>"
  echo "ERROR: No command specified. Use --command <command>"
  exit 1
fi

log "Job starting: $JOB_ID"
log "Command: $COMMAND"

# Check system state
SYSTEM_STATE=$(cat "$STATE_FILE" 2>/dev/null || echo "unknown")

if [[ "$SYSTEM_STATE" == high-resource:* ]]; then
  # Resources are contested - add to queue instead of running
  REASON=$(echo "$SYSTEM_STATE" | cut -d':' -f2)
  log "Resources contested ($REASON), queueing job: $JOB_ID"

  # Create queue directory if it doesn't exist
  mkdir -p "$QUEUE_DIR"

  # Add to queue (priority 2 = new job)
  QUEUE_FILE="${QUEUE_DIR}/2-${JOB_ID}.job"
  cat > "$QUEUE_FILE" <<EOF
{
  "job_id": "$JOB_ID",
  "priority": 2,
  "command": $(echo "$COMMAND" | jq -Rs .),
  "reason": "$REASON",
  "queued_at": "$(date -Iseconds)",
  "interrupted": false
}
EOF

  echo "Job queued. Will run when resources available."
  log "Job queued: $JOB_ID (will run when $REASON ends)"
  exit 0  # Success (queued)
fi

# Resources available - run the command
log "Resources available, running job: $JOB_ID"

# Execute the command
eval "$COMMAND"
EXIT_CODE=$?

if [ $EXIT_CODE -eq 0 ]; then
  log "Job completed successfully: $JOB_ID"
else
  log "Job failed with exit code $EXIT_CODE: $JOB_ID"
fi

exit $EXIT_CODE
