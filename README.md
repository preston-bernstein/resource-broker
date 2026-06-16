# Ollama Resource Manager - Final Implementation

**Project:** Intelligent resource management for Gaming + Plex + Ollama on single PC
**Status:** V3 Ready for Deployment
**Date:** 2026-02-21

---

## Quick Start

### Deploy V3 System

```bash
cd /home/user/Documents/System-Architecture/Ollama-Plex-Gaming/
./CLEANUP-AND-DEPLOY.sh
```

This script will:
1. Archive old deprecated files
2. Install dependencies (jq)
3. Create queue directory
4. Deploy V3 scripts
5. Restart services
6. Verify deployment

### Monitor System

```bash
# Watch resource manager logs
tail -f /var/log/resource-manager.log

# Check system state
cat /tmp/resource-manager-state

# Check job queue
ls -la /var/lib/batch-jobs/queue/

# Check running services
sudo systemctl status resource-manager ollama
```

---

## Architecture Overview

### V3: Process-Based Detection + Queue System

**Detection Method:** Process names (not GPU %)
- Detects: Steam, Lutris, Wine, Proton, Native games, Plex Transcoder
- **Allows:** Ollama to use 100% GPU without false positives

**Queue System:**
- Gaming starts → Interrupt running jobs → Add to front of queue (priority 1)
- New requests during gaming → Add to back of queue (priority 2)
- Gaming ends → Process queue (interrupted first, then new jobs)

**Benefits:**
- ✅ Ollama can fully utilize GPU when available
- ✅ Gaming gets 100% resources (no sharing, no throttling)
- ✅ Fair job ordering (interrupted jobs resume first)
- ✅ Simple, deterministic, no retry logic
- ✅ Clean separation: Either gaming OR Ollama, never both

---

## File Structure

### Active Files (V3)

```
Current Implementation:
├── resource-manager-v3.sh          - Process-based detection + queue
├── batch-job-wrapper-v3.sh         - Queue-aware job wrapper
├── CLEANUP-AND-DEPLOY.sh           - Deployment script
└── README.md                       - This file

Documentation:
├── BLOG-POST-FINAL.md              - Complete technical writeup
├── ARCHITECTURE-ROADMAP.md         - Tier 1/2/3 roadmap
├── TIER-2-IMPLEMENTATION-PLAN.md   - Future: Configurable preemption
├── TIER-3-IMPLEMENTATION-PLAN.md   - Future: Production platform
├── GPU-TROUBLESHOOTING.md          - RX 9070 XT GPU detection fix
├── WORKING-GUIDELINES.md           - Implementation history
└── WORKING-GUIDELINES-APPEND.txt   - Recent discoveries

Archived (Old):
└── archive/
    ├── resource-manager-v2.sh         - GPU-based detection (deprecated)
    ├── batch-job-wrapper-v2.sh        - Retry-based wrapper (deprecated)
    ├── BLOG-POST-DOCUMENTATION.md     - Old blog draft
    ├── PHASE-3-IMPLEMENTATION-PLAN.md - Old planning docs
    ├── PHASE-5-PLANNING.md            - Old planning docs
    ├── Resource-Management-Architecture.md - Old architecture
    └── RESOURCE-MANAGER-UPGRADE-V2.md - Old upgrade docs
```

### Deployed Files

```
/usr/local/bin/
├── resource-manager.sh        → V3 (process-based + queue)
└── batch-job-wrapper.sh       → V3 (queue-aware)

/etc/systemd/system/
├── resource-manager.service   - Systemd service
└── ollama.service             - Ollama with GPU enabled

/var/lib/batch-jobs/
└── queue/                     - Job queue directory
    ├── 1-interrupted-*.job    - Priority 1 (interrupted jobs)
    └── 2-new-*.job            - Priority 2 (new jobs)

/var/log/
├── resource-manager.log       - Resource manager logs
└── batch-job-wrapper.log      - Batch wrapper logs
```

---

## System States

### 1. IDLE State
- No gaming, no Plex transcoding
- Ollama: Full power (30GB RAM, 16 cores, GPU enabled)
- Batch jobs: Run immediately
- Queue: Processed if any jobs queued

### 2. HIGH-RESOURCE State (Gaming or Plex)
- Gaming process detected OR Plex Transcoder running
- **Gaming/Plex gets 100% of resources - NO sharing**
- Running batch jobs: Killed, added to queue (priority 1)
- New batch jobs: Queued immediately (priority 2)
- **Ollama doesn't run at all during gaming** - everything queued

---

## Usage Examples

### Run Batch Job

```bash
# Job runs immediately if resources available
# Gets queued if gaming/Plex active
batch-job-wrapper.sh \
  --job-id "email-summary-$(date +%s)" \
  --model llama3.1:70b \
  "Summarize these 50 emails..."
```

### Check Queue

```bash
# List queued jobs
ls -la /var/lib/batch-jobs/queue/

# Example output:
# 1-interrupted-email-summary-1234.job  <- Will run first
# 2-new-code-review-5678.job            <- Will run second
# 2-new-log-analysis-9012.job           <- Will run third
```

### Manual Queue Processing

```bash
# If you need to manually trigger queue processing
# (normally happens automatically when resources free)
sudo systemctl restart resource-manager
```

---

## Configuration

### Ollama Service

**File:** `/etc/systemd/system/ollama.service`

```ini
[Service]
Environment="HOME=/usr/share/ollama"
Environment="OLLAMA_HOST=0.0.0.0:11434"
Environment="OLLAMA_NUM_GPU=1"          # GPU enabled
# OLLAMA_KEEP_ALIVE not set (uses default 5min)

MemoryMax=30G       # Dynamically changed to 3G during gaming
MemoryHigh=25G      # Dynamically changed to 2560M during gaming
CPUQuota=1600%      # Dynamically changed to 100% during gaming
Nice=10
```

### Resource Manager

**File:** `/usr/local/bin/resource-manager.sh`

**Detection settings:**
```bash
# Processes monitored (edit to add more)
- pgrep -f "Plex Transcoder"
- pgrep -f "steam.*game"
- pgrep -f "lutris.*runner"
- pgrep -f "wine.*\.exe"
- pgrep -f "\.x86_64$"  # Native Linux games
```

**Check interval:** 20 seconds

---

## Testing

### Test 1: Gaming Detection

```bash
# 1. Start a batch job
batch-job-wrapper.sh --job-id "test1" --model llama3.2:3b "Test job" &

# 2. Launch a game (Steam, Lutris, etc)

# 3. Watch logs
tail -f /var/log/resource-manager.log

# Expected:
# - "HIGH RESOURCE STATE DETECTED: gaming-steam"
# - "Killed batch job PID xxx, added to queue: test1"

# 4. Stop the game

# Expected:
# - "RESOURCES FREED"
# - "Resuming interrupted job: test1"
```

### Test 2: Queue System

```bash
# 1. Launch a game first

# 2. Submit 3 batch jobs while gaming
batch-job-wrapper.sh --job-id "job1" --model llama3.2:3b "Job 1" &
batch-job-wrapper.sh --job-id "job2" --model llama3.2:3b "Job 2" &
batch-job-wrapper.sh --job-id "job3" --model llama3.2:3b "Job 3" &

# 3. Check queue
ls /var/lib/batch-jobs/queue/

# Expected:
# 2-job1.job
# 2-job2.job
# 2-job3.job

# 4. Stop game

# 5. Watch logs
tail -f /var/log/resource-manager.log

# Expected:
# - "Processing job queue"
# - "Starting queued job: job1"
# - "Starting queued job: job2"
# - "Starting queued job: job3"
```

### Test 3: GPU Usage

```bash
# 1. Run GPU-heavy Ollama job
ollama run llama3.1:70b "Long analysis task..." &

# 2. Monitor GPU
watch -n 1 rocm-smi

# Expected:
# - GPU: 60-100% (Ollama using GPU)
# - Resource manager: NOT detecting gaming
# - Batch job: Continues running

# 3. Launch actual game

# Expected:
# - Resource manager detects game process
# - Batch job killed and queued
# - Game gets GPU
```

---

## Troubleshooting

### Queue Not Processing

**Symptom:** Jobs stuck in queue even after gaming ends

**Check:**
```bash
# Is resource manager running?
sudo systemctl status resource-manager

# What's the current state?
cat /tmp/resource-manager-state

# Any errors in logs?
tail -20 /var/log/resource-manager.log
```

**Fix:**
```bash
sudo systemctl restart resource-manager
```

### Game Not Detected

**Symptom:** Gaming active but batch jobs not being queued

**Check:**
```bash
# Find the game process
ps aux | grep -i game

# Test detection manually
pgrep -f "steam|lutris|wine|\.x86_64"
```

**Fix:** Add game pattern to `detect_resource_contention()` in resource-manager.sh

### jq Not Found Error

**Symptom:** Logs show "jq not installed"

**Fix:**
```bash
sudo apt update && sudo apt install -y jq
```

---

## Monitoring & Logs

### Key Log Files

```bash
# Resource manager activity
tail -f /var/log/resource-manager.log

# Batch wrapper activity
tail -f /var/log/batch-job-wrapper.log

# Ollama service
sudo journalctl -u ollama -f

# All together
tail -f /var/log/resource-manager.log /var/log/batch-job-wrapper.log
```

### System Health

```bash
# GPU usage
rocm-smi

# GPU busy %
cat /sys/class/drm/card1/device/gpu_busy_percent

# Loaded Ollama models
ollama ps

# System state
cat /tmp/resource-manager-state

# Queue status
ls -la /var/lib/batch-jobs/queue/
```

---

## Evolution History

### V0: Process-Based (Initial)
- Detected specific game processes
- ❌ Missed many games (process name variations)
- ❌ Required constant maintenance

### V1: GPU-Based Detection
- Monitored GPU utilization %
- ✅ Caught all GPU-using games
- ❌ Broke when enabling GPU for Ollama (circular logic)

### V2: GPU + Hysteresis
- Added time-weighted state transitions
- ✅ Prevented state flapping
- ❌ Still had GPU detection conflict

### V3: Process-Based + Queue (Current)
- Back to process detection (smarter patterns)
- Added proper job queue system
- ✅ Ollama can use 100% GPU
- ✅ Simple, deterministic, fair
- ✅ No retry logic needed

---

## Future Enhancements

See:
- `TIER-2-IMPLEMENTATION-PLAN.md` - Configurable preemption (--preempt, --no-preempt flags)
- `TIER-3-IMPLEMENTATION-PLAN.md` - Production platform (web UI, checkpointing, priorities)

---

## Key Lessons Learned

1. **Measure intent, not side-effects** - Detect what process is running, not just that resources are high
2. **Premature optimization breaks things** - Keeping models loaded "for speed" broke detection
3. **Queues > Retry logic** - Proper queueing is simpler than kill-and-retry
4. **Test with actual workloads** - Disk size ≠ RAM usage, assumptions break
5. **Document as you go** - This README exists because we documented everything

---

## Support

For issues, questions, or enhancements:
1. Check logs first: `tail -f /var/log/resource-manager.log`
2. Review troubleshooting section above
3. Read full writeup: `BLOG-POST-FINAL.md`
4. Check architecture docs: `ARCHITECTURE-ROADMAP.md`

**Last Updated:** 2026-02-21
**Version:** V3 (Process-Based + Queue System)
