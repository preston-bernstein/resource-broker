# Resource Management Architecture
## Ollama + Plex + Gaming on Single Machine

**Date Created:** 2026-02-20
**System:** Linux Mint 22.2 (Zara)
**Machine:** gpu-host
**Status:** Architecture Design (Not Yet Implemented)

---

## Table of Contents

1. [System Overview](#system-overview)
2. [Current Issues](#current-issues)
3. [Architecture Design](#architecture-design)
4. [Resource Allocation](#resource-allocation)
5. [Implementation Plan](#implementation-plan)
6. [Testing Scenarios](#testing-scenarios)
7. [Troubleshooting](#troubleshooting)

---

## System Overview

### Hardware Specifications

**CPU:** Intel Core i9-13900K
- 24 cores (8 P-cores + 16 E-cores)
- 32 threads total
- Base: 3.0 GHz, Boost: 5.8 GHz

**Memory:**
- Total: 62GB RAM
- Swap: 2GB (needs upgrade to 8GB)

**GPU:** AMD Radeon RX 9070 XT
- VRAM: ~16GB
- Power: 330W TDP
- Driver: ROCm

**Storage:**
- Primary: Samsung 990 PRO 2TB NVMe (/)
- Apps: Samsung 850 EVO 500GB (/mnt/apps-ssd) - 93% full
- Storage1: Crucial BX500 1TB (/mnt/storage1)
- Storage2: Crucial BX500 1TB (/mnt/storage2)
- NAS: 192.168.1.20:/volume1/Media (/mnt/media) - 100% FULL

### Installed Games

- Baldur's Gate 3 (148GB)
- Kingdom Come Deliverance 2 (90GB)
- Cyberpunk 2077 (85GB)
- Divinity Original Sin 2 (59GB)
- Disco Elysium (9.6GB)
- Dwarf Fortress (1.1GB)
- RimWorld (926MB)

### Services Running

**Plex Media Server:**
- User: plex
- Current RAM: 25.5GB
- **Peak RAM: 54.8GB** (CRITICAL ISSUE)
- CPU time: 102,283 seconds over 22 days uptime
- Tasks: 75 threads
- **NO RESOURCE LIMITS CONFIGURED**

**Ollama:**
- Installed: Yes (/usr/local/bin/ollama v0.13.5)
- Service: Not configured yet
- Models: None installed yet

---

## Current Issues

### Issue 1: Plex Memory Explosion (CRITICAL)
**Problem:** Plex hitting 54.8GB RAM peaks (88% of system memory)
**Symptoms:** System using swap (808MB), potential OOM crashes
**Cause:** No memory limits on Plex service
**Impact:** System instability, swap thrashing
**Priority:** HIGH - Must fix first

### Issue 2: No Resource Management
**Problem:** No coordination between services
**Impact:** Potential for resource conflicts
**Priority:** MEDIUM

### Issue 3: GPU Contention
**Problem:** Only one GPU for gaming + Plex transcoding
**Impact:** Can't do both GPU-accelerated tasks simultaneously
**Priority:** LOW - Acceptable with workarounds

### Issue 4: Storage Nearly Full
**Problem:** /mnt/apps-ssd at 93%, /mnt/media at 100%
**Impact:** Can't install new games or media
**Priority:** MEDIUM

---

## Architecture Design

### Design Philosophy

**Priority Order:**
1. Gaming (highest priority - you're the primary user)
2. Plex Streaming (a household member's use case - 1080p is sufficient)
3. Ollama (batch workloads, can tolerate delays)

**Resource Strategy:** Priority-based with smart degradation
- Gaming gets full resources
- Plex falls back to CPU transcoding when needed
- Ollama freezes during gaming, processes queued work after

### State-Based Resource Allocation

```
┌──────────────────────────────────────────────────────────────┐
│ STATE 1: GAMING (Your Priority)                             │
├──────────────────────────────────────────────────────────────┤
│ Gaming:           100% GPU, 20 threads, 40GB RAM            │
│ Plex (if active):   0% GPU,  6 threads, 20GB RAM (CPU mode) │
│ Ollama:           FROZEN (in RAM, 0 CPU, queues requests)   │
│ Desktop:            0% GPU,  6 threads,  2GB RAM            │
│                                                              │
│ Result: Gaming smooth, a household member's stream works, automation waits│
└──────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────┐
│ STATE 2: IDLE / OLLAMA WORK (Between Gaming Sessions)       │
├──────────────────────────────────────────────────────────────┤
│ Ollama:           50% GPU*, 16 threads, 30GB RAM            │
│ Plex (if active): 50% GPU*,  8 threads, 20GB RAM            │
│ Desktop:            0% GPU,  4 threads,  5GB RAM            │
│                                                              │
│ Result: Ollama processes queued work, Plex uses GPU         │
│ *GPU time-sliced or Ollama yields to Plex if needed         │
└──────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────┐
│ STATE 3: PLEX-ONLY (Wife watching, you not gaming)          │
├──────────────────────────────────────────────────────────────┤
│ Plex:             100% GPU*, 12 threads, 30GB RAM           │
│ Ollama:             0% GPU, 12 threads, 20GB RAM            │
│ Desktop:            0% GPU,  4 threads,  5GB RAM            │
│                                                              │
│ Result: Best quality streams, Ollama still available         │
│ *Or CPU if 1080p, GPU for 4K if you enable remote access    │
└──────────────────────────────────────────────────────────────┘
```

### Key Technologies Used

**SIGSTOP/SIGCONT:**
- Freeze/resume processes without killing them
- Process stays in RAM, uses 0 CPU when frozen
- Instant resume, no model reload
- Battle-tested Linux feature (like Ctrl+Z in terminal)

**systemd Resource Limits:**
- MemoryMax: Hard limit (OOM if exceeded)
- MemoryHigh: Soft limit (kernel throttles before hard limit)
- CPUQuota: Percentage of CPU time (1600% = 16 threads)
- Nice: Process priority (-20 to 19, higher = lower priority)

**Process Detection:**
- pgrep: Find running processes by pattern
- Detect gaming: Look for .exe, steam, lutris, proton patterns
- Detect transcoding: Look for "Plex Transcoder" process

---

## Resource Allocation

### Plex Media Server Limits

**Configuration File:** `/etc/systemd/system/plexmediaserver.service.d/resources.conf`

```ini
[Service]
# Hard memory limit - prevents system crashes
MemoryMax=40G
MemoryHigh=35G

# Soft CPU limit (can burst when needed)
CPUQuota=1200%  # 12 threads

# Medium priority
Nice=5
```

**Rationale:**
- 40GB limit prevents OOM (was hitting 54.8GB)
- Leaves 22GB for gaming + desktop
- 12 threads sufficient for 2-3 concurrent transcodes
- Nice=5 means lower priority than gaming, higher than Ollama

### Ollama Service Configuration

**Configuration File:** `/etc/systemd/system/ollama.service`

```ini
[Unit]
Description=Ollama Service
After=network-online.target
Wants=network-online.target

[Service]
Type=exec
ExecStart=/usr/local/bin/ollama serve
User=ollama
Group=ollama
Restart=always
RestartSec=3

Environment="HOME=/usr/share/ollama"
Environment="OLLAMA_HOST=0.0.0.0:11434"

# Generous limits for idle/batch work
MemoryMax=30G
MemoryHigh=25G
CPUQuota=1600%  # 16 threads when active

# Low priority - yields to gaming/Plex
Nice=10

# CPU-only for now (GPU optional later when idle)
Environment="OLLAMA_NUM_GPU=0"

[Install]
WantedBy=multi-user.target
```

**Rationale:**
- 30GB sufficient for large models (mixtral:8x7b = 26GB)
- 16 threads provides good performance for batch jobs
- Nice=10 ensures yields to gaming and Plex
- CPU-only avoids GPU contention (can enable GPU in STATE 2)

### Resource Manager Script

**Script Location:** `/usr/local/bin/resource-manager.sh`

**Purpose:** Automatically detects system state and adjusts resources

**Logic:**
1. Every 20 seconds, check what's running
2. If gaming detected → Freeze Ollama (SIGSTOP)
3. If no gaming → Resume Ollama (SIGCONT) or start if stopped

**Gaming Detection Patterns:**
- `.exe$` - Windows executables (Proton games)
- `steam.*AppId` - Steam games
- `lutris.*running` - Lutris games
- `proton.*game` - Proton-wrapped games

---

## Implementation Plan

### Phase 1: Fix Plex OOM (CRITICAL - Do First)

**Estimated Time:** 5 minutes
**Risk:** Low
**Impact:** Prevents system crashes

**Steps:**
```bash
# Create override directory
sudo mkdir -p /etc/systemd/system/plexmediaserver.service.d/

# Create resource limits file
sudo tee /etc/systemd/system/plexmediaserver.service.d/resources.conf << 'EOF'
[Service]
MemoryMax=40G
MemoryHigh=35G
CPUQuota=1200%
Nice=5
EOF

# Apply changes
sudo systemctl daemon-reload
sudo systemctl restart plexmediaserver

# Verify
systemctl status plexmediaserver
```

**Verification:**
```bash
# Check that limits are applied
systemctl show plexmediaserver | grep -E "Memory|CPU"

# Monitor memory usage over time
watch -n 5 'systemctl status plexmediaserver | grep Memory'
```

### Phase 2: Set Up Ollama Service

**Estimated Time:** 10 minutes
**Risk:** Low
**Impact:** Enables Ollama with proper resource management

**Steps:**
```bash
# Create ollama user
sudo useradd -r -s /bin/false -U -m -d /usr/share/ollama ollama

# Create service file
sudo tee /etc/systemd/system/ollama.service << 'EOF'
[Unit]
Description=Ollama Service
After=network-online.target
Wants=network-online.target

[Service]
Type=exec
ExecStart=/usr/local/bin/ollama serve
User=ollama
Group=ollama
Restart=always
RestartSec=3
Environment="HOME=/usr/share/ollama"
Environment="OLLAMA_HOST=0.0.0.0:11434"
MemoryMax=30G
MemoryHigh=25G
CPUQuota=1600%
Nice=10
Environment="OLLAMA_NUM_GPU=0"

[Install]
WantedBy=multi-user.target
EOF

# Enable and start
sudo systemctl daemon-reload
sudo systemctl enable ollama
sudo systemctl start ollama

# Verify
systemctl status ollama
```

**Test Ollama:**
```bash
# Pull a small model
ollama pull llama3.2:3b

# Test inference
ollama run llama3.2:3b "Hello, how are you?"

# Check API
curl http://localhost:11434/api/tags
```

### Phase 3: Create Resource Manager

**Estimated Time:** 10 minutes
**Risk:** Medium (test thoroughly)
**Impact:** Automatic resource management

**Steps:**
```bash
# Create script
sudo tee /usr/local/bin/resource-manager.sh << 'EOF'
#!/bin/bash

LOG="/var/log/resource-manager.log"

log() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1" | tee -a "$LOG"
}

while true; do
  # Detect gaming
  if pgrep -f "\.exe$|steam.*AppId|lutris.*running|proton.*game" > /dev/null; then
    # GAMING MODE
    log "Gaming detected - Freezing Ollama"

    if systemctl is-active ollama > /dev/null; then
      systemctl kill -s SIGSTOP ollama.service
    fi

  else
    # IDLE/WORK MODE
    log "No gaming - Resuming Ollama"

    if systemctl is-active ollama > /dev/null; then
      systemctl kill -s SIGCONT ollama.service
    else
      systemctl start ollama
    fi
  fi

  sleep 20
done
EOF

# Make executable
sudo chmod +x /usr/local/bin/resource-manager.sh

# Create systemd service
sudo tee /etc/systemd/system/resource-manager.service << 'EOF'
[Unit]
Description=Resource Manager for Gaming/Plex/Ollama
After=multi-user.target

[Service]
ExecStart=/usr/local/bin/resource-manager.sh
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

# Enable and start
sudo systemctl daemon-reload
sudo systemctl enable resource-manager
sudo systemctl start resource-manager

# Verify
systemctl status resource-manager

# Watch logs
tail -f /var/log/resource-manager.log
```

### Phase 4: Increase Swap Space

**Estimated Time:** 5 minutes (plus time to create 8GB file)
**Risk:** Low
**Impact:** Safety buffer for unexpected memory spikes

**Steps:**
```bash
# Disable current swap
sudo swapoff /swapfile

# Create new 8GB swap file
sudo dd if=/dev/zero of=/swapfile bs=1G count=8 status=progress

# Set permissions
sudo chmod 600 /swapfile

# Make swap
sudo mkswap /swapfile

# Enable swap
sudo swapon /swapfile

# Verify
swapon --show
free -h

# Ensure it persists across reboots (check /etc/fstab)
grep swapfile /etc/fstab
```

**Expected Output:**
```
NAME      TYPE SIZE USED PRIO
/swapfile file   8G   0B   -2
```

---

## Testing Scenarios

### Test 1: Gaming Only

**Setup:**
1. Ensure resource-manager is running
2. Launch Cyberpunk 2077 or BG3

**Expected Behavior:**
- Gaming smooth, 60+ FPS
- Ollama process frozen (SIGSTOP)
- Check: `systemctl status ollama` should show process stopped
- Check log: `tail /var/log/resource-manager.log` should show "Gaming detected"

**Metrics to Monitor:**
```bash
# Gaming FPS (in-game overlay)
# Ollama status
systemctl status ollama

# Memory usage
free -h

# CPU usage per service
systemd-cgtop
```

### Test 2: Gaming + Wife Streaming 1080p

**Setup:**
1. Launch game
2. Have a household member start streaming a show

**Expected Behavior:**
- Gaming: Smooth, unaffected
- Plex: Transcoding on CPU (check with `htop`, should see Plex Transcoder process)
- Stream: Smooth playback (no buffering)
- Ollama: Still frozen

**Verification:**
```bash
# Check if Plex is transcoding
ps aux | grep "Plex Transcoder"

# Check CPU usage
htop  # Look for Plex Transcoder using 4-6 threads

# Check GPU usage (should be for gaming only)
rocm-smi
```

### Test 3: Idle - Ollama Batch Work

**Setup:**
1. Close all games
2. Wait 20 seconds for resource manager to detect

**Expected Behavior:**
- Ollama resumes (SIGCONT)
- Can process requests at full speed
- Home automation works normally

**Test Commands:**
```bash
# Check Ollama resumed
systemctl status ollama

# Test inference
ollama run llama3.2:3b "Summarize this email: ..."

# Check resource usage
systemd-cgtop | grep ollama
```

### Test 4: Plex Memory Limit

**Setup:**
1. Trigger heavy Plex usage (library scan + multiple transcodes)

**Expected Behavior:**
- Memory usage climbs to ~35GB (MemoryHigh)
- Kernel throttles Plex before hitting 40GB (MemoryMax)
- System remains stable
- No swap usage (or minimal)

**Monitoring:**
```bash
# Watch Plex memory
watch -n 2 'systemctl status plexmediaserver | grep Memory'

# Check swap usage
watch -n 2 'free -h'

# If exceeds 35GB, should see throttling in logs
journalctl -u plexmediaserver -f
```

---

## Troubleshooting

### Issue: Ollama Doesn't Freeze During Gaming

**Symptoms:** Gaming starts, but Ollama still running

**Diagnosis:**
```bash
# Check if resource-manager is running
systemctl status resource-manager

# Check detection pattern
tail -f /var/log/resource-manager.log

# Manually check if game detected
pgrep -f "\.exe$|steam.*AppId"
```

**Solutions:**
1. Game not detected by pattern - Add specific pattern to script
2. Resource manager not running - `sudo systemctl start resource-manager`
3. Check game process name: `ps aux | grep -i <game-name>`

### Issue: Plex Still Exceeds 40GB

**Symptoms:** System still swapping, Plex using >40GB

**Diagnosis:**
```bash
# Check if limits are applied
systemctl show plexmediaserver | grep MemoryMax

# Check actual usage
systemctl status plexmediaserver
```

**Solutions:**
1. Limits not applied - Run Phase 1 again
2. Multiple Plex processes - Check for duplicate services
3. Lower limit to 35GB if still problematic

### Issue: Ollama Requests Timing Out

**Symptoms:** Home automation fails, API errors

**Diagnosis:**
```bash
# Check if Ollama is frozen
systemctl status ollama

# Check if gaming detected
tail /var/log/resource-manager.log

# Test API
curl http://localhost:11434/api/tags
```

**Solutions:**
1. Expected behavior during gaming - Requests queue
2. Increase timeout in client applications (30s → 60s)
3. For critical automation, check gaming state first

### Issue: Wife's Stream Buffering

**Symptoms:** Playback stutters during gaming

**Diagnosis:**
```bash
# Check if Plex transcoding
ps aux | grep "Plex Transcoder"

# Check CPU usage
htop

# Check available CPU threads
nproc
```

**Solutions:**
1. Lower stream quality to 720p (less CPU intensive)
2. Reduce game settings slightly (free up CPU threads)
3. Check network bandwidth (separate issue)

### Issue: System Still Swapping

**Symptoms:** swap usage increasing

**Diagnosis:**
```bash
# Check swap usage
swapon --show
free -h

# Find memory hogs
ps aux --sort=-%mem | head -20

# Check all service limits
systemctl show plex* ollama | grep Memory
```

**Solutions:**
1. Lower Plex limit to 35GB
2. Lower Ollama limit to 25GB
3. Close browser tabs (Brave using 1-2GB)
4. Check for memory leaks: `journalctl -b | grep -i oom`

---

## Future Enhancements

### Optional: Enable GPU for Ollama When Idle

**When to consider:** If you want faster inference during idle time

**Configuration:**
```bash
# Modify ollama.service to conditionally enable GPU
# Add environment variable that resource-manager can toggle
# Set OLLAMA_NUM_GPU=1 when in STATE 2 (idle)
```

**Trade-off:** More complex resource management

### Optional: Add Second GPU for Plex

**When to consider:** If you want 4K streaming while gaming

**Hardware:**
- Used RX 580 8GB (~$80-100)
- Intel Arc A380 (~$120, excellent transcoding)

**Configuration:**
```bash
# Configure Plex to use secondary GPU
# Set DRI_PRIME=1 or specific device in Plex settings
```

### Optional: Offload Ollama to Network

**When to consider:** If local resources insufficient

**Options:**
1. NAS (Synology at 192.168.1.20)
2. Dedicated mini PC (Intel N100 ~$150)
3. Cloud instance (Runpod, VastAI)

---

## Monitoring Commands

### Quick Status Check
```bash
# All services status
systemctl status plexmediaserver ollama resource-manager

# Memory usage
free -h
swapon --show

# CPU and memory by service
systemd-cgtop -n 1

# GPU usage
rocm-smi

# Resource manager log
tail -20 /var/log/resource-manager.log
```

### Detailed Monitoring
```bash
# Real-time service resources
watch -n 2 'systemd-cgtop -n 1'

# Plex memory over time
watch -n 5 'systemctl status plexmediaserver | grep Memory'

# Ollama state
watch -n 5 'systemctl status ollama | grep -E "Active|Main PID"'

# Overall system
htop
```

### Log Analysis
```bash
# Resource manager decisions
grep "Gaming detected" /var/log/resource-manager.log
grep "No gaming" /var/log/resource-manager.log

# Plex errors
journalctl -u plexmediaserver --since "1 hour ago" | grep -i error

# Ollama errors
journalctl -u ollama --since "1 hour ago"

# System OOM events
journalctl -b | grep -i "out of memory"
```

---

## Summary

### What This Architecture Provides

✅ **Gaming:** Unaffected, full performance
✅ **Streaming:** Works (1080p on CPU, smooth)
✅ **Ollama:** Stable, handles batch workloads perfectly
✅ **Home Automation:** Works (maybe 30s delay during gaming)
✅ **No crashes:** Plex can't OOM anymore
✅ **No purchases:** $0 cost

### Trade-offs Accepted

❌ Home automation 20-30s slower during gaming (acceptable for batch work)
❌ Email/CI jobs queue up if gaming (fine, they're async anyway)
❌ Wife can't stream 4K while gaming (doesn't need it, 1080p sufficient)

### Success Metrics

- [ ] No swap usage during normal operation
- [ ] Plex memory stays under 40GB
- [ ] Gaming maintains 60+ FPS
- [ ] 1080p streaming smooth during gaming
- [ ] Ollama processes batch jobs within 5 minutes when idle
- [ ] No OOM crashes for 30 days

---

## Change Log

| Date | Change | Reason |
|------|--------|--------|
| 2026-02-20 | Initial architecture design | Document planning before implementation |

---

## References

- Plex service location: `/usr/lib/systemd/system/plexmediaserver.service`
- Ollama binary: `/usr/local/bin/ollama`
- systemd documentation: `man systemd.resource-control`
- Linux signals: `man 7 signal` (SIGSTOP, SIGCONT)
