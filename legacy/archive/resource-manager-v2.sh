#!/bin/bash
# resource-manager-v2.sh
# GPU-based detection with hysteresis to prevent state flapping
# Choice B: Throttle-Only (Always Available)
# INCLUDES: Explicit model loading/unloading to prevent OOM

LOG="/var/log/resource-manager.log"
STATE_FILE="/tmp/resource-manager-state"
OLLAMA_API="http://localhost:11434/api"
SMALL_MODEL="llama3.2:3b"

# GPU detection settings
GPU_DEVICE="/sys/class/drm/card1/device/gpu_busy_percent"
GAMING_ENTER_THRESHOLD=50      # GPU > 50% to enter gaming
GAMING_EXIT_THRESHOLD=20       # GPU < 20% to exit gaming
GAMING_ENTER_DURATION=30       # Sustained for 30 seconds
GAMING_EXIT_DURATION=300       # Sustained for 5 minutes (300s)

# State counters
high_gpu_counter=0
low_gpu_counter=0

log() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1" | tee -a "$LOG"
}

# Get GPU utilization percentage (0-100)
get_gpu_usage() {
  if [ -f "$GPU_DEVICE" ]; then
    cat "$GPU_DEVICE" 2>/dev/null || echo 0
  else
    log "WARNING: GPU device not found at $GPU_DEVICE"
    echo 0
  fi
}

# Force load small model (evicts large models from memory)
load_small_model() {
  log "Loading small model: $SMALL_MODEL (evicting any large models)"
  curl -s -X POST "$OLLAMA_API/generate" \
    -d "{\"model\":\"$SMALL_MODEL\",\"prompt\":\"ready\"}" \
    > /dev/null 2>&1

  if [ $? -eq 0 ]; then
    log "Small model loaded successfully"
  else
    log "WARNING: Failed to load small model"
  fi
}

# Check what models are currently loaded in Ollama
check_loaded_models() {
  LOADED=$(curl -s "$OLLAMA_API/ps" 2>/dev/null | grep -o '"name":"[^"]*"' | cut -d'"' -f4)
  if [ -n "$LOADED" ]; then
    echo "$LOADED"
  else
    echo "none"
  fi
}

# Enter gaming state
enter_gaming_state() {
  log "=== GAMING DETECTED (GPU sustained >$GAMING_ENTER_THRESHOLD% for ${GAMING_ENTER_DURATION}s) ==="

  # Step 1: Kill batch job processes
  if pgrep -f "ollama run llama3.1" > /dev/null || \
     pgrep -f "ollama run codellama" > /dev/null || \
     pgrep -f "ollama run mistral" > /dev/null; then
    log "Killing batch jobs..."
    pkill -f "ollama run llama3.1"
    pkill -f "ollama run codellama"
    pkill -f "ollama run mistral"
    log "Batch job processes terminated"
  fi

  # Step 2: Unload large models by loading small model
  # This is CRITICAL - large models may still be in RAM after killing process
  load_small_model
  sleep 2

  # Step 3: Verify what's loaded
  LOADED=$(check_loaded_models)
  log "Models after cleanup: $LOADED"

  # Step 4: Throttle Ollama to 3GB (small model is 2.5GB)
  systemctl set-property ollama.service \
    MemoryMax=3G \
    MemoryHigh=2560M \
    CPUQuota=100%

  log "Ollama throttled: 1 thread, 3GB RAM (small model only)"
  CURRENT_STATE="gaming"
  echo "$CURRENT_STATE" > "$STATE_FILE"
}

# Exit gaming state
exit_gaming_state() {
  log "=== GAMING ENDED (GPU sustained <$GAMING_EXIT_THRESHOLD% for ${GAMING_EXIT_DURATION}s) ==="

  # Restore full power (models will load on-demand)
  systemctl set-property ollama.service \
    MemoryMax=30G \
    MemoryHigh=25G \
    CPUQuota=1600%

  log "Ollama full power restored: 16 threads, 30GB RAM (on-demand loading)"
  CURRENT_STATE="idle"
  echo "$CURRENT_STATE" > "$STATE_FILE"
}

# Initialize state
CURRENT_STATE="unknown"

# Start with no models loaded (on-demand loading)
log "=== RESOURCE MANAGER V2 STARTING (GPU-based detection) ==="
log "GPU Device: $GPU_DEVICE"
log "Gaming enter: >$GAMING_ENTER_THRESHOLD% for ${GAMING_ENTER_DURATION}s"
log "Gaming exit: <$GAMING_EXIT_THRESHOLD% for ${GAMING_EXIT_DURATION}s"
log "Starting with on-demand model loading (GPU idle)"

while true; do
  # Get current GPU usage
  GPU_USAGE=$(get_gpu_usage)

  # Detect Plex transcoding (takes priority over GPU gaming detection)
  if pgrep -f "Plex Transcoder" > /dev/null; then

    if [ "$CURRENT_STATE" != "plex" ]; then
      log "=== PLEX TRANSCODING DETECTED ==="

      # Reset GPU counters
      high_gpu_counter=0
      low_gpu_counter=0

      # Throttle Ollama (moderate - 8GB allows large models if needed)
      systemctl set-property ollama.service \
        MemoryMax=8G \
        MemoryHigh=7G \
        CPUQuota=400%

      # Ensure small model loaded for responsive home automation
      load_small_model

      LOADED=$(check_loaded_models)
      log "Ollama throttled: 4 threads, 8GB RAM. Models: $LOADED"
      CURRENT_STATE="plex"
      echo "$CURRENT_STATE" > "$STATE_FILE"
    fi

  # GPU-based gaming detection with hysteresis
  elif [ "$CURRENT_STATE" = "idle" ] || [ "$CURRENT_STATE" = "unknown" ]; then
    # Trying to enter gaming state
    if [ "$GPU_USAGE" -gt "$GAMING_ENTER_THRESHOLD" ]; then
      high_gpu_counter=$((high_gpu_counter + 20))  # +20 seconds per check
      log "GPU high: ${GPU_USAGE}% (counter: ${high_gpu_counter}s / ${GAMING_ENTER_DURATION}s)"

      if [ "$high_gpu_counter" -ge "$GAMING_ENTER_DURATION" ]; then
        # GPU has been high for required duration
        enter_gaming_state
        high_gpu_counter=0
        low_gpu_counter=0
      fi
    else
      if [ "$high_gpu_counter" -gt 0 ]; then
        log "GPU dropped to ${GPU_USAGE}%, resetting enter counter"
      fi
      high_gpu_counter=0  # Reset counter on dip below threshold
    fi

  elif [ "$CURRENT_STATE" = "gaming" ]; then
    # Trying to exit gaming state
    if [ "$GPU_USAGE" -lt "$GAMING_EXIT_THRESHOLD" ]; then
      low_gpu_counter=$((low_gpu_counter + 20))  # +20 seconds per check

      # Log every minute to avoid spam
      if [ $((low_gpu_counter % 60)) -eq 0 ]; then
        log "GPU low: ${GPU_USAGE}% (counter: ${low_gpu_counter}s / ${GAMING_EXIT_DURATION}s)"
      fi

      if [ "$low_gpu_counter" -ge "$GAMING_EXIT_DURATION" ]; then
        # GPU has been low for required duration
        exit_gaming_state
        high_gpu_counter=0
        low_gpu_counter=0
      fi
    else
      if [ "$low_gpu_counter" -gt 0 ]; then
        log "GPU active: ${GPU_USAGE}%, resetting exit counter (game still running)"
      fi
      low_gpu_counter=0  # Reset counter - game still active
    fi
  fi

  # Check every 20 seconds
  sleep 20
done
