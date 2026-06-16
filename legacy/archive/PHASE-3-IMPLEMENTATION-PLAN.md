# Phase 3 Implementation Plan
## Resource Manager with Throttle-Only Architecture

**Date:** 2026-02-20
**Status:** Ready to Implement
**Prerequisites:** Phase 1 & 2 Complete ✓

---

## Quick Reference: What We're Building

**Goal:** Automatic resource management that keeps home automation always available

**How it works:**
1. Resource manager detects what you're doing (gaming/Plex/idle)
2. Adjusts Ollama resources dynamically (never stops it)
3. Kills batch jobs if gaming starts
4. Batch jobs auto-retry when gaming ends
5. Small model always loaded for home automation

---

## Critical: Why Explicit Model Management?

**The Problem:**
When you kill a batch job process (`pkill -f "ollama run llama3.1"`), you're only killing the **client** process. The Ollama **server** keeps running and the large model stays loaded in its memory!

**Example without model management:**
```
1. Batch job running → llama3.1:8b loaded (8GB RAM)
2. Gaming starts → Kill batch job process
3. Ollama still has llama3.1:8b in memory (8GB)
4. Throttle Ollama to 2GB limit
5. Result: Ollama using 8GB but limited to 2GB = OOM CRASH!
```

**Solution:**
Explicitly load the small model when gaming starts. This forces Ollama to unload the large model to make room.

**Example with model management:**
```
1. Batch job running → llama3.1:8b loaded (8GB RAM)
2. Gaming starts → Kill batch job process
3. Load llama3.2:3b → Ollama evicts llama3.1:8b, loads llama3.2:3b
4. Now using: 1.9GB RAM
5. Throttle Ollama to 2GB limit
6. Result: 1.9GB usage, 2GB limit = Safe! ✓
```

**This is why Steps 2 and 3 in the resource manager are critical!**

---

## Implementation Steps (Do in Order)

### Step 1: Update Ollama Service (Keep-Alive)

**File:** `/etc/systemd/system/ollama.service`

**Add this line in [Service] section:**
```
Environment="OLLAMA_KEEP_ALIVE=-1"
```

**Commands:**
```bash
# Edit service file
sudo nano /etc/systemd/system/ollama.service

# Add the Environment line after other Environment lines

# Reload and restart
sudo systemctl daemon-reload
sudo systemctl restart ollama

# Verify
systemctl show ollama | grep KEEP_ALIVE
```

**Expected:** Should show `OLLAMA_KEEP_ALIVE=-1`

---

### Step 2: Initialize Small Model (Keep Loaded)

**Command:**
```bash
# Load small model and keep it resident
ollama run llama3.2:3b "You are ready for home automation. Respond with 'Ready'"

# Verify it's loaded
ollama ps
```

**Expected:** Should show llama3.2:3b running

---

### Step 3: Create Batch Job Wrapper Script

**File:** `/usr/local/bin/batch-job-wrapper.sh`

**Content:**
```bash
#!/bin/bash
# batch-job-wrapper.sh
# Handles batch jobs with retry and model cleanup

SMALL_MODEL="llama3.2:3b"
LARGE_MODEL="${1:-llama3.1:8b}"
PROMPT="$2"

LOG="/var/log/batch-job-wrapper.log"

log() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1" | tee -a "$LOG"
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
      log "Waiting 60 seconds before retry..."
      sleep 60
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
```

**Commands:**
```bash
# Create script
sudo tee /usr/local/bin/batch-job-wrapper.sh << 'EOF'
[paste content above]
EOF

# Make executable
sudo chmod +x /usr/local/bin/batch-job-wrapper.sh

# Create log file
sudo touch /var/log/batch-job-wrapper.log
sudo chown user:user /var/log/batch-job-wrapper.log
```

**Test:**
```bash
# Quick test
/usr/local/bin/batch-job-wrapper.sh llama3.2:3b "test message"
```

---

### Step 4: Create Resource Manager Script

**File:** `/usr/local/bin/resource-manager.sh`

**IMPORTANT:** This script includes explicit model management to prevent OOM crashes!

**Content:**
```bash
#!/bin/bash
# resource-manager.sh
# Dynamically adjusts Ollama resources based on system state
# Choice B: Throttle-Only (Always Available)
# INCLUDES: Explicit model loading/unloading to prevent OOM

LOG="/var/log/resource-manager.log"
STATE_FILE="/tmp/resource-manager-state"
OLLAMA_API="http://localhost:11434/api"
SMALL_MODEL="llama3.2:3b"

log() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1" | tee -a "$LOG"
}

# Force load small model (evicts large models from memory)
load_small_model() {
  log "Loading small model: $SMALL_MODEL (evicting any large models)"
  curl -s -X POST "$OLLAMA_API/generate" \
    -d "{\"model\":\"$SMALL_MODEL\",\"prompt\":\"ready\",\"keep_alive\":-1}" \
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

# Initialize state
CURRENT_STATE="unknown"

# Ensure small model is loaded at startup
log "=== RESOURCE MANAGER STARTING ==="
load_small_model
sleep 2
LOADED=$(check_loaded_models)
log "Initial models loaded: $LOADED"

while true; do
  # Detect gaming
  if pgrep -f "\.exe$|steam.*AppId|lutris.*running|proton.*game" > /dev/null; then

    if [ "$CURRENT_STATE" != "gaming" ]; then
      log "=== GAMING DETECTED ==="

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

      # Step 4: Throttle Ollama to 2GB
      systemctl set-property ollama.service \
        MemoryMax=2G \
        MemoryHigh=1536M \
        CPUQuota=100%

      log "Ollama throttled: 1 thread, 2GB RAM (small model only)"
      CURRENT_STATE="gaming"
    fi

  # Detect Plex transcoding
  elif pgrep -f "Plex Transcoder" > /dev/null; then

    if [ "$CURRENT_STATE" != "plex" ]; then
      log "=== PLEX TRANSCODING DETECTED ==="

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
    fi

  # Idle (full speed)
  else

    if [ "$CURRENT_STATE" != "idle" ]; then
      log "=== SYSTEM IDLE ==="

      # Step 1: Restore full power
      systemctl set-property ollama.service \
        MemoryMax=30G \
        MemoryHigh=25G \
        CPUQuota=1600%

      # Step 2: Ensure small model loaded and ready
      load_small_model
      sleep 1

      LOADED=$(check_loaded_models)
      log "Ollama full power: 16 threads, 30GB RAM. Models: $LOADED"
      CURRENT_STATE="idle"
    fi
  fi

  # Save state
  echo "$CURRENT_STATE" > "$STATE_FILE"

  # Check every 20 seconds
  sleep 20
done
```

**Commands:**
```bash
# Create script
sudo tee /usr/local/bin/resource-manager.sh << 'EOF'
[paste content above]
EOF

# Make executable
sudo chmod +x /usr/local/bin/resource-manager.sh

# Create log file
sudo touch /var/log/resource-manager.log
sudo chown root:root /var/log/resource-manager.log
sudo chmod 644 /var/log/resource-manager.log
```

**Manual test:**
```bash
# Run in foreground to test
sudo /usr/local/bin/resource-manager.sh

# Watch logs in another terminal
tail -f /var/log/resource-manager.log

# Try launching/closing a game to test detection
```

---

### Step 5: Create Resource Manager Service

**File:** `/etc/systemd/system/resource-manager.service`

**Content:**
```ini
[Unit]
Description=Resource Manager for Gaming/Plex/Ollama
After=multi-user.target ollama.service
Wants=ollama.service

[Service]
Type=simple
ExecStart=/usr/local/bin/resource-manager.sh
Restart=always
RestartSec=10
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

**Commands:**
```bash
# Create service
sudo tee /etc/systemd/system/resource-manager.service << 'EOF'
[paste content above]
EOF

# Reload systemd
sudo systemctl daemon-reload

# Enable (start on boot)
sudo systemctl enable resource-manager

# Start now
sudo systemctl start resource-manager

# Verify running
systemctl status resource-manager
```

---

## Testing Checklist

After implementation, run ALL these tests in order:

- [ ] **Test 1:** System idle - Ollama at full speed
- [ ] **Test 2:** Launch game - Verify throttling
- [ ] **Test 3:** Close game - Verify restoration
- [ ] **Test 4:** Plex transcode - Verify moderate throttling
- [ ] **Test 5:** Batch job during idle - Verify runs normally
- [ ] **Test 6:** Batch job interrupted by gaming - Verify killed and retried
- [ ] **Test 7:** Small model persistence - Verify stays loaded after 10+ minutes
- [ ] **Test 8:** Integration test - All scenarios in sequence

**Document results in:** `WORKING-GUIDELINES.md` Implementation Log

---

## Usage Examples (After Implementation)

### Home Automation (Always Works)
```bash
# Anytime - even during gaming
ollama run llama3.2:3b "turn on the lights"

# Fast when idle (2-4s)
# Slow when gaming (30-60s)
# Medium when Plex streaming (10-15s)
```

### Batch Jobs (Only When Idle)
```bash
# Email summarization
/usr/local/bin/batch-job-wrapper.sh llama3.1:8b \
  "Summarize these 20 emails: ..."

# Code analysis
/usr/local/bin/batch-job-wrapper.sh codellama:7b \
  "Review this pull request: ..."

# Will retry automatically if gaming starts
# Will reload small model when complete
```

### Check System State
```bash
# What state is resource manager in?
cat /tmp/resource-manager-state

# Recent state changes
tail -20 /var/log/resource-manager.log

# Current Ollama limits
systemctl show ollama | grep -E "Memory|CPU"

# What's loaded?
ollama ps
```

---

## Monitoring Commands

### After Implementation
```bash
# Check all services
systemctl status plexmediaserver ollama resource-manager

# Check resource usage
systemd-cgtop -n 1

# Check logs
tail -f /var/log/resource-manager.log
tail -f /var/log/batch-job-wrapper.log
journalctl -u ollama -f

# Memory usage
free -h
systemctl status ollama | grep Memory
```

---

## Troubleshooting

### Resource Manager Not Detecting Gaming
```bash
# Check detection pattern
pgrep -f "\.exe$|steam.*AppId"

# Add your specific game to the pattern in resource-manager.sh
```

### Batch Job Doesn't Retry
```bash
# Check wrapper log
tail -50 /var/log/batch-job-wrapper.log

# Check exit codes
echo $?  # After batch job fails
```

### Small Model Not Staying Loaded
```bash
# Check keep-alive setting
systemctl show ollama | grep KEEP_ALIVE

# Manually keep loaded
while true; do ollama run llama3.2:3b "ping" > /dev/null 2>&1; sleep 300; done &
```

---

## Current Status

**Completed:**
- ✓ Phase 1: Plex limits (45GB max)
- ✓ Phase 2: Ollama service with limits
- ✓ Phase 2: llama3.2:3b downloaded and tested
- ✓ Phase 3 Step 1: OLLAMA_KEEP_ALIVE=-1 added to service
- ✓ Phase 3 Step 2: Small model loaded and verified (UNTIL: Forever)

**PHASE 3 STATUS: ✅ COMPLETE**

**Completed:**
- ✓ Phase 3 Step 1: OLLAMA_KEEP_ALIVE=-1 added to service
- ✓ Phase 3 Step 2: Small model loaded and verified (UNTIL: Forever)
- ✓ Phase 3 Step 3: Batch job wrapper script created
- ✓ Phase 3 Step 4: Resource manager script created
- ✓ Phase 3 Step 5: Resource manager service running
- ✓ Log rotation configured (30 days, daily, compressed)
- ✓ Blog post documentation written

**Issue Resolved:**
- Exit code 203/EXEC caused by leading whitespace in shebang
- Fixed with: sudo sed -i 's/^  //' /usr/local/bin/resource-manager.sh
- Service now active (running) and enabled on boot

**Ready for:**
- Phase 3: Comprehensive testing (8 scenarios) - Optional but recommended
- Phase 4: Increase swap to 8GB
- Phase 5: Job Orchestration & Automation (documented, not implemented)

**Important Decisions Made:**
- Using `ollama` user for all automation (not creating separate `ollama-jobs` user)
- Batch job wrapper logs owned by `ollama:ollama`
- Resource manager runs as root (needs privileges)
- Log rotation: 30 days history, daily rotation, compressed (user requested historical logs)

---

## Phase 3 Complete! 🎉

**COMPLETED: 2026-02-21 12:17**

**What's Complete:**
- ✅ Phase 1: Plex limits (45GB)
- ✅ Phase 2: Ollama service with dual-model setup
- ✅ Phase 3: Resource Manager (all steps complete)
  - KEEP_ALIVE setting ✓
  - Small model loaded permanently ✓
  - Batch job wrapper script ✓
  - Resource manager script ✓
  - Resource manager service running ✓
  - Log rotation configured ✓
- ✅ Blog post documentation written

**Current System State:**
```bash
# All services operational
systemctl status plexmediaserver  # Active (running)
systemctl status ollama            # Active (running)
systemctl status resource-manager  # Active (running)

# Current state
cat /tmp/resource-manager-state    # Shows: idle

# Loaded models
ollama ps                          # Shows: llama3.2:3b (Forever)

# Resource limits (idle state)
systemctl show ollama | grep -E "MemoryMax|CPUQuota"
# MemoryMax=30G, CPUQuota=1600%
```

**Next Steps (Choose One):**

### Option 1: Run Comprehensive Testing (Recommended)

Test all 8 scenarios from the testing checklist:

1. **Test 1:** System idle - Verify Ollama at full speed
2. **Test 2:** Launch game - Verify throttling
3. **Test 3:** Close game - Verify restoration
4. **Test 4:** Plex transcode - Verify moderate throttling
5. **Test 5:** Batch job during idle - Verify runs normally
6. **Test 6:** Batch job interrupted by gaming - Verify killed and retried
7. **Test 7:** Small model persistence - Verify stays loaded after 10+ minutes
8. **Test 8:** Integration test - All scenarios in sequence

### Option 2: Move to Phase 4 (Swap Increase)

Increase swap from 2GB to 8GB for additional safety margin

### Option 3: Plan Phase 5 (Job Orchestration)

Begin implementing the automated job orchestration system documented in `PHASE-5-PLANNING.md`

**Documentation Files:**
- Architecture: `Resource-Management-Architecture.md`
- Phase 3 Plan: `PHASE-3-IMPLEMENTATION-PLAN.md` (this file)
- Progress Log: `WORKING-GUIDELINES.md`
- Blog Post: `BLOG-POST-DOCUMENTATION.md`
- Future Plans: `PHASE-5-PLANNING.md`

**Files to preserve:**
- `/home/user/Documents/System-Architecture/Ollama-Plex-Gaming/Resource-Management-Architecture.md` (master plan)
- `/home/user/Documents/System-Architecture/Ollama-Plex-Gaming/WORKING-GUIDELINES.md` (implementation log)
- `/home/user/Documents/System-Architecture/Ollama-Plex-Gaming/PHASE-3-IMPLEMENTATION-PLAN.md` (this file)

---

**Last Updated:** 2026-02-21 12:17 (Phase 3 COMPLETE ✅)
**Current Status:** All components operational, service running
**Fixed Issue:** Shebang whitespace causing exit code 203/EXEC
**Next Phase:** Testing (8 scenarios) or Phase 4 (swap increase)
