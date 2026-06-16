#!/bin/bash
# batch-job-wrapper-v3.sh
# Queue-aware batch job wrapper
# Checks system state before running, queues if resources contested

LOG="/var/log/batch-job-wrapper.log"
STATE_FILE="/tmp/resource-manager-state"
QUEUE_DIR="/var/lib/batch-jobs/queue"

# Logging
log() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1" | tee -a "$LOG"
}

# Parse arguments
JOB_ID=""
MODEL=""
PROMPT=""

while [[ $# -gt 0 ]]; do
  case $1 in
    --job-id)
      JOB_ID="$2"
      shift 2
      ;;
    --model)
      MODEL="$2"
      shift 2
      ;;
    *)
      PROMPT="$1"
      shift
      ;;
  esac
done

# Generate job ID if not provided
if [ -z "$JOB_ID" ]; then
  JOB_ID="job-$(date +%s)-$$"
fi

# Check if model specified
if [ -z "$MODEL" ]; then
  log "ERROR: No model specified. Use --model <model_name>"
  exit 1
fi

# Check if prompt specified
if [ -z "$PROMPT" ]; then
  log "ERROR: No prompt specified"
  exit 1
fi

log "Job starting: $JOB_ID (model: $MODEL)"

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
  "model": "$MODEL",
  "prompt": "$PROMPT",
  "reason": "$REASON",
  "queued_at": "$(date -Iseconds)",
  "interrupted": false
}
EOF

  echo "Job queued. Will run when resources available."
  log "Job queued: $JOB_ID"
  exit 0  # Success (queued)
fi

# Resources available - run the job
log "Resources available, running job: $JOB_ID"

# Run Ollama
ollama run "$MODEL" "$PROMPT"
EXIT_CODE=$?

if [ $EXIT_CODE -eq 0 ]; then
  log "Job completed successfully: $JOB_ID"
else
  log "Job failed with exit code $EXIT_CODE: $JOB_ID"
fi

exit $EXIT_CODE
