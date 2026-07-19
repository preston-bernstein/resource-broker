#!/bin/bash
# resource-manager-v3.sh
# Process-based detection with job queue system
# Gaming/Plex detection via process names (not GPU %)
# Queue system: Interrupted jobs get priority, new jobs queued in order

LOG="/var/log/resource-manager.log"
STATE_FILE="/tmp/resource-manager-state"
QUEUE_DIR="/var/lib/batch-jobs/queue"
OLLAMA_API="http://localhost:11434/api"

# Create queue directory if it doesn't exist
mkdir -p "$QUEUE_DIR"

# Logging
log() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1" | tee -a "$LOG"
}

# Detect if resources are contested by high-priority processes
detect_resource_contention() {
  # 1. Plex Transcoding (highest priority)
  if pgrep -f "Plex Transcoder" > /dev/null 2>&1; then
    echo "plex"
    return 0
  fi

  # 2. Steam games (most reliable: SteamLaunch only present when game running)
  if pgrep -f "SteamLaunch AppId=" > /dev/null 2>&1; then
    echo "gaming-steam"
    return 0
  fi

  # 3. Lutris games
  if pgrep -f "lutris.*runner" > /dev/null 2>&1; then
    echo "gaming-lutris"
    return 0
  fi

  # 4. Heroic Games Launcher
  if pgrep -f "heroic.*game" > /dev/null 2>&1; then
    echo "gaming-heroic"
    return 0
  fi

  # 5. Wine/Proton games (non-Steam)
  if pgrep -f "wine.*\.exe" | grep -v "protonmail\|protonvpn" > /dev/null 2>&1; then
    echo "gaming-wine"
    return 0
  fi

  # No contention detected
  return 1
}

# Add job to queue
add_to_queue() {
  local JOB_ID="$1"
  local PRIORITY="$2"  # 1=interrupted, 2=new
  local COMMAND="$3"
  local REASON="$4"

  # Generate queue filename (priority prefix for sorting)
  local QUEUE_FILE="${QUEUE_DIR}/${PRIORITY}-${JOB_ID}.job"

  # Create job metadata (command needs to be JSON-escaped)
  local COMMAND_JSON=$(echo "$COMMAND" | jq -Rs .)

  cat > "$QUEUE_FILE" <<EOF
{
  "job_id": "$JOB_ID",
  "priority": $PRIORITY,
  "command": $COMMAND_JSON,
  "reason": "$REASON",
  "queued_at": "$(date -Iseconds)",
  "interrupted": $([ "$PRIORITY" -eq 1 ] && echo "true" || echo "false")
}
EOF

  if [ "$PRIORITY" -eq 1 ]; then
    log "Added interrupted job to queue (priority 1): $JOB_ID"
  else
    log "Added new job to queue (priority 2): $JOB_ID"
  fi
}

# Get job info from running process
get_job_info() {
  local PID=$1

  # Read command line to extract job details
  local CMDLINE=$(cat /proc/$PID/cmdline 2>/dev/null | tr '\0' ' ')

  # Try to extract job ID from command line
  local JOB_ID=$(echo "$CMDLINE" | grep -o '\--job-id [^ ]*' | awk '{print $2}')

  # Try to extract full command (everything after --command)
  local COMMAND=$(echo "$CMDLINE" | sed -n 's/.*--command \(.*\)/\1/p')

  # If we can't extract details, generate generic ones
  if [ -z "$JOB_ID" ]; then
    JOB_ID="interrupted-$(date +%s)-${PID}"
  fi
  if [ -z "$COMMAND" ]; then
    COMMAND="unknown command"
  fi

  echo "$JOB_ID|$COMMAND"
}

# Process the job queue
process_queue() {
  log "=== Processing job queue ==="

  # Count jobs
  local JOB_COUNT=$(ls "$QUEUE_DIR"/*.job 2>/dev/null | wc -l)

  if [ "$JOB_COUNT" -eq 0 ]; then
    log "Queue empty, nothing to process"
    return
  fi

  log "Found $JOB_COUNT queued jobs"

  # Process jobs in priority order (1-* comes before 2-*)
  for job_file in $(ls "$QUEUE_DIR"/*.job 2>/dev/null | sort); do
    # Check if resources still available (could have changed)
    if detect_resource_contention > /dev/null; then
      log "Resources contested again, stopping queue processing"
      return
    fi

    # Extract job details (requires jq)
    if ! command -v jq &> /dev/null; then
      log "ERROR: jq not installed, cannot process queue"
      log "Install with: sudo apt install jq"
      return
    fi

    JOB_ID=$(jq -r '.job_id' "$job_file")
    PRIORITY=$(jq -r '.priority' "$job_file")
    COMMAND=$(jq -r '.command' "$job_file")
    INTERRUPTED=$(jq -r '.interrupted' "$job_file")

    if [ "$INTERRUPTED" = "true" ]; then
      log "Resuming interrupted job: $JOB_ID"
    else
      log "Starting queued job: $JOB_ID"
    fi

    # Start the job via batch wrapper in background
    if [ -f /usr/local/bin/batch-job-wrapper.sh ]; then
      # Remove from queue first
      rm "$job_file"

      # Start job in background
      /usr/local/bin/batch-job-wrapper.sh \
        --job-id "$JOB_ID" \
        --command "$COMMAND" &

      # Small delay between starting jobs
      sleep 2
    else
      log "ERROR: batch-job-wrapper.sh not found, cannot start job"
      return
    fi
  done

  log "Queue processing complete"
}

# Enter high-resource state (gaming or Plex active)
enter_high_resource_state() {
  local REASON=$1

  log "=== HIGH RESOURCE STATE DETECTED: $REASON ==="
  log "Gaming/Plex gets 100% resources - queueing all Ollama requests"

  # Find and interrupt all running batch jobs
  local BATCH_PIDS=$(pgrep -f "batch-job-wrapper")

  if [ -n "$BATCH_PIDS" ]; then
    for pid in $BATCH_PIDS; do
      # Get job info before killing
      JOB_INFO=$(get_job_info $pid)
      JOB_ID=$(echo "$JOB_INFO" | cut -d'|' -f1)
      COMMAND=$(echo "$JOB_INFO" | cut -d'|' -f2-)

      # Add to front of queue (priority 1)
      add_to_queue "$JOB_ID" 1 "$COMMAND" "$REASON"

      # Kill the job
      kill $pid 2>/dev/null
      log "Killed batch job PID $pid, added to queue: $JOB_ID"
    done
  else
    log "No running batch jobs to interrupt"
  fi

  # No throttling - gaming/Plex gets everything
  # All Ollama requests will be queued by batch wrapper
  log "All new Ollama requests will be queued until resources free"

  CURRENT_STATE="high-resource"
  echo "$CURRENT_STATE:$REASON" > "$STATE_FILE"
}

# Exit high-resource state (resources freed)
exit_high_resource_state() {
  local PREVIOUS_REASON=$(cat "$STATE_FILE" 2>/dev/null | cut -d':' -f2)

  log "=== RESOURCES FREED (was: $PREVIOUS_REASON) ==="
  log "Ollama now has full access: 16 cores, 30GB RAM, full GPU"

  CURRENT_STATE="idle"
  echo "$CURRENT_STATE" > "$STATE_FILE"

  # Process any queued jobs
  process_queue
}

# Main monitoring loop
main() {
  log "=== RESOURCE MANAGER V3 STARTING (Process-based detection) ==="
  log "Monitoring: Plex Transcoder, Steam, Lutris, Wine, Native games"
  log "Queue directory: $QUEUE_DIR"
  log "Check interval: 20 seconds"

  # Initialize state
  CURRENT_STATE="unknown"
  echo "idle" > "$STATE_FILE"

  while true; do
    # Detect resource contention
    CONTENTION=$(detect_resource_contention)
    CONTENTION_STATUS=$?

    if [ $CONTENTION_STATUS -eq 0 ]; then
      # Resources are contested
      if [ "$CURRENT_STATE" != "high-resource" ]; then
        # Entering high-resource state
        enter_high_resource_state "$CONTENTION"
        CURRENT_STATE="high-resource"
      fi
      # else: already in high-resource state, stay there

    else
      # Resources are free
      if [ "$CURRENT_STATE" = "high-resource" ]; then
        # Exiting high-resource state
        exit_high_resource_state
        CURRENT_STATE="idle"
      fi
      # else: already idle, stay idle
    fi

    # Wait before next check
    sleep 20
  done
}

# Start the monitoring loop
main
