#!/bin/bash
# batch-job-wrapper-v5.sh
# Generic command wrapper with queue support + metadata tracking

LOG="/var/log/batch-job-wrapper.log"
STATE_FILE="/tmp/resource-manager-state"
QUEUE_DIR="/var/lib/batch-jobs/queue"
METRICS_DIR="/var/lib/batch-jobs/metrics"

# Create directories if they don't exist
mkdir -p "$QUEUE_DIR" "$METRICS_DIR"

# Logging
log() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1" | tee -a "$LOG"
}

# Parse arguments
JOB_ID=""
COMMAND=""
CALLER=""
TAGS=""

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
    --caller)
      CALLER="$2"
      shift 2
      ;;
    --tags)
      TAGS="$2"
      shift 2
      ;;
    *)
      echo "ERROR: Unknown option: $1"
      echo "Usage: batch-job-wrapper.sh --job-id <id> --command <command> [--caller <name>] [--tags <tag1,tag2>]"
      exit 1
      ;;
  esac
done

# Generate job ID if not provided
if [ -z "$JOB_ID" ]; then
  JOB_ID="job-$(date +%s)-$$"
fi

# Detect caller if not provided
if [ -z "$CALLER" ]; then
  # Try to get parent process name (what actually invoked this)
  PARENT_PID=$PPID
  CALLER=$(ps -p $PARENT_PID -o comm= 2>/dev/null || echo "unknown")

  # If parent is bash/sh, try to get the script name from command line
  if [[ "$CALLER" =~ ^(bash|sh)$ ]]; then
    PARENT_CMD=$(ps -p $PARENT_PID -o args= 2>/dev/null)
    # Extract script name if present
    if [[ "$PARENT_CMD" =~ ([^/[:space:]]+\.sh) ]]; then
      CALLER="${BASH_REMATCH[1]%.sh}"
    else
      CALLER="manual-shell"
    fi
  fi
fi

# Detect invocation context
INVOCATION_METHOD="unknown"
if [ -n "$SSH_CLIENT" ]; then
  INVOCATION_METHOD="ssh"
  REMOTE_IP=$(echo "$SSH_CLIENT" | awk '{print $1}')
elif [ "$TERM" = "dumb" ] || [ -z "$TERM" ]; then
  INVOCATION_METHOD="cron-or-service"
elif [ -n "$SYSTEMD_EXEC_PID" ]; then
  INVOCATION_METHOD="systemd"
elif [ -t 0 ]; then
  INVOCATION_METHOD="interactive"
else
  INVOCATION_METHOD="pipe-or-script"
fi

# Check if command specified
if [ -z "$COMMAND" ]; then
  log "ERROR: No command specified. Use --command <command>"
  echo "ERROR: No command specified. Use --command <command>"
  exit 1
fi

# Extract model from command (if it's an ollama command)
MODEL="unknown"
if [[ "$COMMAND" =~ ollama\ run\ ([^\ ]+) ]]; then
  MODEL="${BASH_REMATCH[1]}"
fi

# Record job metadata (submission)
SUBMIT_TIME=$(date -Iseconds)
SUBMIT_TIMESTAMP=$(date +%s)

cat > "$METRICS_DIR/${JOB_ID}.json" <<EOF
{
  "job_id": "$JOB_ID",
  "caller": "$CALLER",
  "tags": $(echo "$TAGS" | jq -Rs 'split(",") | map(select(length > 0))'),
  "model": "$MODEL",
  "command": $(echo "$COMMAND" | jq -Rs .),
  "submitted_at": "$SUBMIT_TIME",
  "submitted_timestamp": $SUBMIT_TIMESTAMP,
  "submitted_by": "$USER",
  "hostname": "$(hostname)",
  "invocation_method": "$INVOCATION_METHOD",
  "remote_ip": "${REMOTE_IP:-null}",
  "parent_pid": $PARENT_PID,
  "wrapper_pid": $$,
  "status": "pending"
}
EOF

log "Job submitted: $JOB_ID (caller: $CALLER, model: $MODEL)"

# Check system state
SYSTEM_STATE=$(cat "$STATE_FILE" 2>/dev/null || echo "unknown")

if [[ "$SYSTEM_STATE" == high-resource:* ]]; then
  # Resources are contested - add to queue instead of running
  REASON=$(echo "$SYSTEM_STATE" | cut -d':' -f2)
  log "Resources contested ($REASON), queueing job: $JOB_ID"

  # Update metadata
  jq --arg reason "$REASON" \
     --arg queued_at "$(date -Iseconds)" \
     '. + {status: "queued", queue_reason: $reason, queued_at: $queued_at}' \
     "$METRICS_DIR/${JOB_ID}.json" > "$METRICS_DIR/${JOB_ID}.json.tmp" && \
     mv "$METRICS_DIR/${JOB_ID}.json.tmp" "$METRICS_DIR/${JOB_ID}.json"

  # Add to queue (priority 2 = new job)
  QUEUE_FILE="${QUEUE_DIR}/2-${JOB_ID}.job"
  cat > "$QUEUE_FILE" <<EOF
{
  "job_id": "$JOB_ID",
  "priority": 2,
  "command": $(echo "$COMMAND" | jq -Rs .),
  "reason": "$REASON",
  "queued_at": "$(date -Iseconds)",
  "interrupted": false,
  "caller": "$CALLER",
  "model": "$MODEL"
}
EOF

  echo "Job queued. Will run when resources available."
  log "Job queued: $JOB_ID (will run when $REASON ends)"
  exit 0  # Success (queued)
fi

# Resources available - run the command
log "Resources available, running job: $JOB_ID"

# Update metadata (running)
START_TIME=$(date -Iseconds)
START_TIMESTAMP=$(date +%s)

jq --arg start "$START_TIME" \
   --argjson start_ts $START_TIMESTAMP \
   '. + {status: "running", started_at: $start, started_timestamp: $start_ts}' \
   "$METRICS_DIR/${JOB_ID}.json" > "$METRICS_DIR/${JOB_ID}.json.tmp" && \
   mv "$METRICS_DIR/${JOB_ID}.json.tmp" "$METRICS_DIR/${JOB_ID}.json"

# Execute the command and capture output
OUTPUT_FILE="$METRICS_DIR/${JOB_ID}.output"
eval "$COMMAND" > "$OUTPUT_FILE" 2>&1
EXIT_CODE=$?

# Record completion
END_TIME=$(date -Iseconds)
END_TIMESTAMP=$(date +%s)
DURATION=$((END_TIMESTAMP - START_TIMESTAMP))
OUTPUT_SIZE=$(wc -c < "$OUTPUT_FILE" 2>/dev/null || echo 0)

# Determine final status
if [ $EXIT_CODE -eq 0 ]; then
  FINAL_STATUS="completed"
  log "Job completed successfully: $JOB_ID (${DURATION}s)"
else
  FINAL_STATUS="failed"
  log "Job failed with exit code $EXIT_CODE: $JOB_ID (${DURATION}s)"
fi

# Update metadata (final)
jq --arg end "$END_TIME" \
   --argjson end_ts $END_TIMESTAMP \
   --argjson duration $DURATION \
   --argjson exit_code $EXIT_CODE \
   --argjson output_size $OUTPUT_SIZE \
   --arg status "$FINAL_STATUS" \
   '. + {
     status: $status,
     completed_at: $end,
     completed_timestamp: $end_ts,
     duration_seconds: $duration,
     exit_code: $exit_code,
     output_size_bytes: $output_size
   }' \
   "$METRICS_DIR/${JOB_ID}.json" > "$METRICS_DIR/${JOB_ID}.json.tmp" && \
   mv "$METRICS_DIR/${JOB_ID}.json.tmp" "$METRICS_DIR/${JOB_ID}.json"

exit $EXIT_CODE
