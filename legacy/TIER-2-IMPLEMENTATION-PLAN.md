# Tier 2 Implementation Plan: Configurable Preemption

**Date:** 2026-02-21
**Status:** Planning
**Builds on:** Tier 1 (basic resource manager + batch wrapper)

---

## Overview

Tier 2 adds **configurable preemption policies** so different batch jobs can have different priorities during gaming/Plex events.

**Key Features:**
1. Batch jobs can specify preemption behavior via flags
2. Resource manager respects job priority when killing
3. Status command shows running/queued jobs
4. Job metadata tracking for visibility

**Complexity:** +73 lines (~350 total vs 277 Tier 1)

---

## Use Cases

### Preemptible Jobs (Default)
**Example:** Email summarization, code analysis, batch processing

```bash
batch-job-wrapper.sh --preempt \
  --job-id "email-summary-$(date +%s)" \
  --model llama3.1:70b \
  "Summarize the following 50 emails..."
```

**Behavior:**
- Starts immediately if GPU available
- **Killed** when gaming/Plex starts
- Auto-retries when GPU free
- Good for: Long-running, restartable jobs

---

### Non-Preemptible Jobs
**Example:** Quick home automation, time-sensitive alerts

```bash
batch-job-wrapper.sh --no-preempt \
  --job-id "home-lights-$(date +%s)" \
  --model llama3.2:3b \
  "Turn on living room lights"
```

**Behavior:**
- Starts immediately if GPU available
- **NOT killed** during gaming
- Continues on CPU (slower) during gaming
- Good for: Quick jobs (<10s), critical automation

---

### Queue Mode
**Example:** Large batch processing that should wait for ideal conditions

```bash
batch-job-wrapper.sh --queue \
  --job-id "video-transcription-batch" \
  --model whisper:large \
  "Transcribe 100 audio files..."
```

**Behavior:**
- **Waits** if GPU >50% at start time
- Once started, not killed (runs to completion)
- Good for: Jobs that need GPU from start to finish

---

## Implementation

### 1. Batch Wrapper V3 Changes

**File:** `batch-job-wrapper-v3.sh`

**New flags:**
```bash
--preempt          # Default: can be killed, will retry
--no-preempt       # Never killed, continues on CPU
--queue            # Wait for GPU, not killed once started
--job-id <id>      # Unique identifier for this job
--model <model>    # Which model to use
```

**Job metadata file:**
```bash
# /tmp/batch-jobs/<job-id>.meta
{
  "job_id": "email-summary-1234567890",
  "pid": 12345,
  "preemption": "preempt",
  "model": "llama3.1:70b",
  "started": "2026-02-21T18:30:00Z",
  "status": "running"
}
```

**Logic additions:**
```bash
# Parse flags
PREEMPTION_MODE="preempt"  # default
JOB_ID=""
MODEL=""

while [[ $# -gt 0 ]]; do
  case $1 in
    --preempt) PREEMPTION_MODE="preempt"; shift ;;
    --no-preempt) PREEMPTION_MODE="no-preempt"; shift ;;
    --queue) PREEMPTION_MODE="queue"; shift ;;
    --job-id) JOB_ID="$2"; shift 2 ;;
    --model) MODEL="$2"; shift 2 ;;
    *) PROMPT="$1"; shift ;;
  esac
done

# Queue mode: wait for GPU before starting
if [ "$PREEMPTION_MODE" = "queue" ]; then
  while [ $(get_gpu_usage) -gt 50 ]; do
    log "Queue mode: Waiting for GPU <50%..."
    sleep 30
  done
fi

# Write job metadata
mkdir -p /tmp/batch-jobs
cat > /tmp/batch-jobs/${JOB_ID}.meta <<EOF
{
  "job_id": "$JOB_ID",
  "pid": $$,
  "preemption": "$PREEMPTION_MODE",
  "model": "$MODEL",
  "started": "$(date -Iseconds)",
  "status": "running"
}
EOF

# Run the job
ollama run "$MODEL" "$PROMPT"
EXIT_CODE=$?

# Cleanup metadata
rm -f /tmp/batch-jobs/${JOB_ID}.meta

# Retry logic (only for preemptible jobs)
if [ $EXIT_CODE -eq 137 ] || [ $EXIT_CODE -eq 143 ]; then
  if [ "$PREEMPTION_MODE" = "preempt" ]; then
    log "Preemptible job killed, will retry after gaming..."
    wait_for_gaming_end
    sleep 10
    exec "$0" --preempt --job-id "$JOB_ID" --model "$MODEL" "$PROMPT"
  else
    log "Non-preemptible job terminated unexpectedly (exit $EXIT_CODE)"
    exit $EXIT_CODE
  fi
fi
```

**Estimated size:** 150 lines (was 81 lines)

---

### 2. Resource Manager V3 Changes

**File:** `resource-manager-v3.sh`

**Selective killing logic:**
```bash
# When entering gaming state
enter_gaming_state() {
  log "=== GAMING DETECTED (GPU sustained >$GAMING_ENTER_THRESHOLD% for ${GAMING_ENTER_DURATION}s) ==="

  # Kill only preemptible batch jobs
  for metafile in /tmp/batch-jobs/*.meta; do
    [ -e "$metafile" ] || continue

    JOB_PID=$(jq -r '.pid' "$metafile")
    PREEMPTION=$(jq -r '.preemption' "$metafile")
    JOB_ID=$(jq -r '.job_id' "$metafile")

    if [ "$PREEMPTION" = "preempt" ]; then
      log "Killing preemptible job: $JOB_ID (PID $JOB_PID)"
      kill $JOB_PID 2>/dev/null
    elif [ "$PREEMPTION" = "no-preempt" ]; then
      log "Preserving non-preemptible job: $JOB_ID (PID $JOB_PID)"
    elif [ "$PREEMPTION" = "queue" ]; then
      log "Preserving queued job: $JOB_ID (PID $JOB_PID) - already started"
    fi
  done

  # Throttle Ollama to 3GB/1 core
  systemctl set-property ollama.service \
    MemoryMax=3G \
    MemoryHigh=2560M \
    CPUQuota=100%

  log "Ollama throttled: 1 thread, 3GB RAM (small model only)"
  CURRENT_STATE="gaming"
  echo "$CURRENT_STATE" > "$STATE_FILE"
}
```

**Dependencies:**
- Requires `jq` for JSON parsing: `sudo apt install jq`

**Estimated size:** 180 lines (was 196 lines, simplified some logic)

---

### 3. Batch Status Command

**File:** `batch-status.sh`

**Purpose:** Show current batch jobs and system state

```bash
#!/bin/bash
# batch-status.sh - Show running/queued batch jobs

echo "BATCH JOBS:"
echo "ID                    STATUS    PRIORITY      MODEL             STARTED"
echo "--------------------------------------------------------------------------------"

if [ -d /tmp/batch-jobs ]; then
  for metafile in /tmp/batch-jobs/*.meta; do
    [ -e "$metafile" ] || continue

    JOB_ID=$(jq -r '.job_id' "$metafile")
    PID=$(jq -r '.pid' "$metafile")
    PREEMPTION=$(jq -r '.preemption' "$metafile")
    MODEL=$(jq -r '.model' "$metafile")
    STARTED=$(jq -r '.started' "$metafile")

    # Check if process still running
    if kill -0 $PID 2>/dev/null; then
      STATUS="RUNNING"
    else
      STATUS="DEAD"
    fi

    # Format started time (relative)
    STARTED_REL=$(date -d "$STARTED" +'%H:%M:%S' 2>/dev/null || echo "$STARTED")

    printf "%-20s  %-8s  %-12s  %-16s  %s\n" \
      "$JOB_ID" "$STATUS" "$PREEMPTION" "$MODEL" "$STARTED_REL"
  done
else
  echo "(no jobs running)"
fi

echo ""
echo "SYSTEM STATE:"
if [ -f /tmp/resource-manager-state ]; then
  STATE=$(cat /tmp/resource-manager-state)
  GPU=$(cat /sys/class/drm/card1/device/gpu_busy_percent 2>/dev/null || echo "?")
  echo "State: $STATE (GPU: ${GPU}%)"
else
  echo "Resource manager not running"
fi

echo ""
echo "LOADED MODELS:"
ollama ps
```

**Installation:**
```bash
sudo cp batch-status.sh /usr/local/bin/batch-status
sudo chmod +x /usr/local/bin/batch-status
```

**Usage:**
```bash
$ batch-status

BATCH JOBS:
ID                    STATUS    PRIORITY      MODEL             STARTED
--------------------------------------------------------------------------------
email-summary-1234    RUNNING   preempt       llama3.1:70b      18:30:45
code-review-5678      RUNNING   queue         deepseek:33b      18:25:10

SYSTEM STATE:
State: idle (GPU: 15%)

LOADED MODELS:
NAME             ID       SIZE      PROCESSOR    CONTEXT    UNTIL
llama3.1:70b     abc123   40 GB     100% GPU     8192       3 minutes from now
```

**Estimated size:** 20 lines

---

## Migration from Tier 1 to Tier 2

**Backward compatibility:** Tier 2 batch wrapper accepts Tier 1 calling convention:

```bash
# Tier 1 usage (still works)
batch-job-wrapper.sh "llama3.1:70b" "Analyze this code..."

# Tier 2 usage (new)
batch-job-wrapper.sh --preempt --job-id "analysis-1" --model llama3.1:70b "Analyze..."
```

**Migration steps:**
1. Deploy `batch-job-wrapper-v3.sh`
2. Deploy `resource-manager-v3.sh`
3. Install `jq`: `sudo apt install jq`
4. Install `batch-status` command
5. Update cron jobs / automation scripts to use new flags (optional)

---

## Testing Plan

### Test 1: Preemptible Job Gets Killed
```bash
# Start preemptible batch job
batch-job-wrapper.sh --preempt --job-id "test-preempt" --model llama3.1:70b \
  "Write a very long story about space exploration..." &

# Wait for job to start
sleep 10

# Simulate gaming (manually set state)
echo "gaming" > /tmp/resource-manager-state
sudo pkill -f "test-preempt"

# Verify job retries after state changes
echo "idle" > /tmp/resource-manager-state

# Check logs: should see retry
tail -f /var/log/batch-job-wrapper.log
```

**Expected:** Job killed, then retries when state returns to idle

---

### Test 2: Non-Preemptible Job Continues
```bash
# Start non-preemptible job
batch-job-wrapper.sh --no-preempt --job-id "test-no-preempt" --model llama3.2:3b \
  "Quick task..." &

# Simulate gaming
echo "gaming" > /tmp/resource-manager-state

# Check batch-status
batch-status

# Expected: Job still running, not killed
```

**Expected:** Job continues even in gaming state

---

### Test 3: Queue Mode Waits
```bash
# Set GPU to busy state
echo "gaming" > /tmp/resource-manager-state

# Start queue-mode job
batch-job-wrapper.sh --queue --job-id "test-queue" --model llama3.1:70b \
  "Task that should wait..." &

# Check batch-status
batch-status

# Expected: Job waiting (not started)

# Clear gaming state
echo "idle" > /tmp/resource-manager-state

# Wait a bit
sleep 30

# Check batch-status again
batch-status

# Expected: Job now running
```

**Expected:** Job waits until GPU <50%, then starts

---

### Test 4: Batch Status Command
```bash
# Start multiple jobs with different priorities
batch-job-wrapper.sh --preempt --job-id "job1" --model llama3.1:70b "Task 1" &
batch-job-wrapper.sh --no-preempt --job-id "job2" --model llama3.2:3b "Task 2" &
batch-job-wrapper.sh --queue --job-id "job3" --model deepseek:33b "Task 3" &

# Check status
batch-status
```

**Expected:** All jobs visible with correct priority and status

---

## Deployment Checklist

- [ ] Install jq: `sudo apt install jq`
- [ ] Deploy batch-job-wrapper-v3.sh to /usr/local/bin/
- [ ] Deploy resource-manager-v3.sh to /usr/local/bin/
- [ ] Deploy batch-status to /usr/local/bin/
- [ ] Restart resource-manager service
- [ ] Test preemptible job (should be killed during gaming)
- [ ] Test non-preemptible job (should continue during gaming)
- [ ] Test queue mode (should wait for GPU)
- [ ] Update cron jobs to use new flags (optional)

---

## Known Limitations

1. **No job persistence** - If system reboots, job metadata is lost (in /tmp)
2. **No priority ordering** - All preemptible jobs killed equally (no high/low priority)
3. **No CPU/GPU toggle** - Still uses OLLAMA_NUM_GPU=1 during gaming (may conflict)
4. **Manual cleanup** - Dead job metadata files not auto-cleaned

**Solutions in Tier 3.**

---

## Estimated Effort

**Development:** 4-6 hours
**Testing:** 2 hours
**Documentation:** 1 hour
**Total:** 7-9 hours

**Files to create/modify:**
- batch-job-wrapper-v3.sh (new, 150 lines)
- resource-manager-v3.sh (modify, +10 lines)
- batch-status.sh (new, 20 lines)
- TIER-2-IMPLEMENTATION-PLAN.md (this file)

---

## Success Criteria

- [ ] Can run batch job with `--preempt` flag
- [ ] Can run batch job with `--no-preempt` flag
- [ ] Can run batch job with `--queue` flag
- [ ] Resource manager kills only preemptible jobs during gaming
- [ ] Non-preemptible jobs continue during gaming
- [ ] Queue-mode jobs wait for GPU before starting
- [ ] `batch-status` command shows all running jobs
- [ ] Backward compatible with Tier 1 usage
- [ ] All tests pass

---

**Next:** Tier 3 - Checkpointing, Priority Queues, Web UI
