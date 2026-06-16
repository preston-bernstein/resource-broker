#!/bin/bash
# batch-job-wrapper.sh v2
# Handles batch jobs with retry and model cleanup
# IMPROVED: Waits for gaming state to end before retrying

SMALL_MODEL="llama3.2:3b"
LARGE_MODEL="${1:-llama3.1:8b}"
PROMPT="$2"

LOG="/var/log/batch-job-wrapper.log"
STATE_FILE="/tmp/resource-manager-state"

log() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1" | tee -a "$LOG"
}

# Wait for system to exit gaming state
wait_for_gaming_end() {
  local wait_count=0
  while [ -f "$STATE_FILE" ] && [ "$(cat $STATE_FILE 2>/dev/null)" = "gaming" ]; do
    if [ $wait_count -eq 0 ]; then
      log "System still gaming, waiting for gaming to end..."
    fi
    sleep 10
    wait_count=$((wait_count + 1))
    # Log every minute
    if [ $((wait_count % 6)) -eq 0 ]; then
      log "Still waiting for gaming to end ($(($wait_count * 10))s elapsed)..."
    fi
  done

  if [ $wait_count -gt 0 ]; then
    log "Gaming ended after $(($wait_count * 10))s, ready to retry"
  fi
}

# Ensure small model is loaded first
log "Ensuring $SMALL_MODEL is loaded for home automation..."
ollama run $SMALL_MODEL "ready" > /dev/null 2>&1

# Run batch job with retry logic
log "Starting batch job with $LARGE_MODEL..."
MAX_RETRIES=3
RETRY_COUNT=0

while [ $RETRY_COUNT -lt $MAX_RETRIES ]; do
  log "Attempt $((RETRY_COUNT + 1))/$MAX_RETRIES"

  # Run the batch job
  ollama run $LARGE_MODEL "$PROMPT"
  EXIT_CODE=$?

  if [ $EXIT_CODE -eq 0 ]; then
    log "Batch job completed successfully"
    break
  elif [ $EXIT_CODE -eq 137 ] || [ $EXIT_CODE -eq 143 ]; then
    # Killed (137=SIGKILL, 143=SIGTERM)
    RETRY_COUNT=$((RETRY_COUNT + 1))
    log "Batch job interrupted (killed by resource manager)"

    if [ $RETRY_COUNT -lt $MAX_RETRIES ]; then
      # Wait for gaming to end before retrying
      wait_for_gaming_end

      # Additional 10 second buffer after gaming ends
      log "Waiting 10 seconds for system to stabilize..."
      sleep 10
    else
      log "Max retries reached, giving up"
    fi
  else
    log "Batch job failed with error code $EXIT_CODE"
    break
  fi
done

# Always reload small model when done
log "Reloading $SMALL_MODEL for home automation..."
ollama run $SMALL_MODEL "ready" > /dev/null 2>&1

log "Small model loaded and ready. Batch job wrapper complete."
