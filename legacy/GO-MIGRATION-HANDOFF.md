# Go Migration Handoff Document

**Date:** 2026-02-22
**Current State:** Working Bash prototype (process-based detection)
**Next Step:** Migrate to Go with proper testing and hybrid GPU detection

---

## Executive Summary

Built an intelligent resource management system for a Linux machine running:
- **Gaming** (Steam - AAA titles on AMD RX 9070 XT GPU)
- **Plex Media Server** (42TB library, transcoding)
- **Ollama** (Local LLM for automation/analysis)

**Core Requirement:** Gaming takes absolute priority. When GPU-intensive games run, queue Ollama jobs. When games exit, resume queue.

**Current Architecture:** Bash-based, process detection only (simple, works, but conservative)

**Why Migrating to Go:**
1. Need hybrid detection (process + GPU monitoring) for optimal resource usage
2. Bash is untestable for complex state management
3. Race conditions, hysteresis, anti-thrashing require proper testing
4. Want to run Ollama alongside lightweight games (RimWorld) but queue for GPU-heavy games (Cyberpunk 2077)

---

## System Architecture (Current - Bash V5)

### Components

**1. resource-manager-v3.sh** - Main daemon (systemd service)
- Runs every 20 seconds
- Detects high-priority processes (Plex, Steam games)
- Writes state to `/tmp/resource-manager-state`
- Processes job queue when resources available

**2. batch-job-wrapper-v5.sh** - Generic command wrapper
- Accepts any command (not just Ollama)
- Checks resource state before running
- Queues jobs if resources contested
- Tracks comprehensive metadata (caller, timing, exit codes, remote IP)

**3. batch-metrics-export.sh** - Metrics export
- Exports job metrics to JSON, CSV, Prometheus, InfluxDB

### Data Flow

```
User App (email-summarizer.sh)
    ↓
batch-job-wrapper.sh
    ↓
Check: /tmp/resource-manager-state
    ↓
├─ "available" → Run command immediately
└─ "high-resource:gaming" → Queue job to /var/lib/batch-jobs/queue/

resource-manager.sh (background daemon)
    ↓
Detects: Game exits
    ↓
Processes queue → Calls batch-job-wrapper.sh for each queued job
```

### File Locations

```
/usr/local/bin/resource-manager.sh          # Main daemon
/usr/local/bin/batch-job-wrapper.sh         # Job wrapper
/usr/local/bin/batch-metrics-export         # Metrics export
/var/lib/batch-jobs/queue/*.job             # Queued jobs (JSON)
/var/lib/batch-jobs/metrics/*.json          # Job metrics
/tmp/resource-manager-state                 # Current state (available|high-resource:*)
/var/log/resource-manager.log               # Daemon log
/var/log/batch-job-wrapper.log              # Job execution log
```

---

## Key Design Decisions

### 1. Generic Command Wrapper (Not Ollama-Specific)

**Why:** Clean separation of concerns
- Wrapper handles queueing/state management
- Calling applications handle business logic (model selection)

**Example:**
```bash
# home-automation.sh decides to use fast model
batch-job-wrapper.sh \
  --caller "home-automation" \
  --tags "realtime,automation" \
  --command "ollama run llama3.2:3b 'Turn on lights'"

# research-analyzer.sh decides to use quality model
batch-job-wrapper.sh \
  --caller "research-analyzer" \
  --tags "batch,weekly" \
  --command "ollama run llama3.1:70b 'Analyze papers...'"
```

### 2. Process-Based Detection (Not GPU %)

**Evolution:**
- V1: GPU % monitoring → Failed when Ollama uses GPU (circular logic)
- V2: GPU + hysteresis → Still couldn't distinguish Ollama from gaming
- V3: Process detection → Clean separation, Ollama can use 100% GPU

**Why it works:** Detects WHAT is using resources, not THAT resources are high.

**Current detection pattern (tested/validated):**
```bash
# Steam games - only when actually running
if pgrep -f "SteamLaunch AppId=" > /dev/null 2>&1; then
  echo "gaming-steam"
fi

# Plex transcoding
if pgrep -f "Plex Transcoder" > /dev/null 2>&1; then
  echo "plex"
fi
```

**Validated on:** RimWorld, with games in `/mnt/apps-ssd/SteamLibrary/steamapps/common/`

### 3. Queue System with Priorities

**Priority levels:**
- **Priority 1:** Interrupted jobs (game launched mid-execution) - resume first
- **Priority 2:** New jobs submitted while contested - run after interrupted jobs

**Queue format:** `/var/lib/batch-jobs/queue/1-job123.job` (priority prefix)

### 4. Comprehensive Metadata Tracking

**Captured for every job:**
```json
{
  "job_id": "email-summary-1708644503",
  "caller": "email-summarizer",
  "tags": ["email", "daily", "batch"],
  "model": "llama3.1:8b",
  "command": "ollama run...",

  "submitted_by": "user",
  "hostname": "gpu-host",
  "invocation_method": "ssh",
  "remote_ip": "192.168.1.50",
  "parent_pid": 12345,

  "submitted_at": "2026-02-21T18:00:00-05:00",
  "started_at": "2026-02-21T18:00:05-05:00",
  "completed_at": "2026-02-21T18:01:30-05:00",
  "duration_seconds": 85,

  "status": "completed",
  "exit_code": 0,
  "output_size_bytes": 4521
}
```

**Invocation methods detected:**
- `interactive` - Manual terminal
- `ssh` - Remote SSH (captures IP)
- `cron-or-service` - Cron/systemd
- `pipe-or-script` - Called from another script

---

## Testing Findings

### Steam Game Detection

**Tested with:** RimWorld (CPU-based game)

**Process signature when running:**
```
1890646 /home/user/.local/share/Steam/ubuntu12_32/reaper SteamLaunch AppId=294100
1890701 /bin/bash /mnt/apps-ssd/SteamLibrary/steamapps/common/RimWorld/start_RimWorld.sh
1890733 ./RimWorldLinux -disable-compute-shaders
```

**Patterns tested:**

| Pattern | Detects Game? | False Positive (Client Idle)? |
|---------|---------------|-------------------------------|
| `steam.*game` | ✗ No | - |
| `steamapps.*\.exe` | ✗ No (Linux native) | - |
| `SteamLaunch AppId=` | ✓ Yes | ✗ No |
| `gameoverlayrenderer` | ✓ Yes | ✗ No |
| `start_.*\.sh` | ✓ Yes | ✗ No |

**Winner:** `SteamLaunch AppId=` - Most reliable, only present when game actually running

**Validated:** Steam client can run in background without triggering detection

### GPU Detection History

**Hardware:** AMD RX 9070 XT (RDNA 4, gfx1201)

**Issue 1: GPU Not Detected**
- Ollama 0.13.5 (Dec 2024) didn't support RDNA 4 (GPU released Jan 2025)
- Solution: Updated to Ollama 0.16.3

**Issue 2: 100% GPU When Idle**
- `OLLAMA_KEEP_ALIVE=-1` kept models loaded forever
- Caused constant 52W power draw and broke detection
- Solution: Use default 5min timeout, on-demand loading

**Current GPU behavior:**
- Idle: 0-2% GPU usage
- Loading model: ~2 seconds
- Inference: 100% GPU usage (desired)

---

## Problems with Current Bash Implementation

### 1. Conservative Queueing
- Queues for ALL Steam games (even RimWorld which doesn't use GPU)
- Wastes opportunity to run Ollama alongside lightweight games

### 2. No GPU Awareness
- Can't distinguish GPU-intensive games from CPU-only games
- Need hybrid detection: process + GPU monitoring

### 3. Untestable Complex Logic
- Can't unit test race conditions
- Can't mock system calls (pgrep, GPU readings)
- No type safety
- Hysteresis/anti-thrashing requires proper state machine

### 4. Race Conditions (Theoretical)
```
00:00 - Cyberpunk launches (GPU: 5% during load)
00:01 - Check interval passes → Ollama starts
00:05 - Cyberpunk finishes loading (GPU: 95%)
00:06-00:20 - BOTH fighting for GPU (14 sec contention)
00:20 - Next check → Ollama killed
```

### 5. Wasted Work on Interruption
- LLM inference can't resume mid-generation
- If interrupted at 80% complete, restarts from scratch

---

## Go Migration Requirements

### Must Have (Tier 1 - MVP)

**1. Hybrid Detection**
```go
type ResourceState int

const (
    Available ResourceState = iota
    Gaming_GPU_Intensive
    Gaming_Lightweight
    PlexTranscoding
)

func DetectContention(monitor SystemMonitor) ResourceState {
    // Detect game process
    if game := monitor.DetectGame(); game != nil {
        // Check GPU usage
        if monitor.GetGPUUsage() > 50.0 {
            return Gaming_GPU_Intensive  // Queue Ollama
        }
        return Gaming_Lightweight  // Allow Ollama
    }

    if monitor.DetectPlex() {
        return PlexTranscoding  // Queue Ollama
    }

    return Available
}
```

**2. Full Test Coverage**
```go
func TestHybridDetection_GPUIntensiveGame(t *testing.T) {
    mock := &MockMonitor{
        gameRunning: true,
        gameName: "Cyberpunk 2077",
        gpuUsage: 85.0,
    }

    state := DetectContention(mock)
    assert.Equal(t, Gaming_GPU_Intensive, state)
}

func TestHybridDetection_LightweightGame(t *testing.T) {
    mock := &MockMonitor{
        gameRunning: true,
        gameName: "RimWorld",
        gpuUsage: 8.0,
    }

    state := DetectContention(mock)
    assert.Equal(t, Gaming_Lightweight, state)
}
```

**3. Anti-Thrashing Logic**
```go
type StateTracker struct {
    currentState ResourceState
    stateStartTime time.Time
    minStateDuration time.Duration  // 30 seconds
}

// Only transition if new state stable for minStateDuration
func (st *StateTracker) Update(newState ResourceState) bool {
    if newState == st.currentState {
        return false  // No change
    }

    // Hysteresis: require new state to persist
    if time.Since(st.stateStartTime) < st.minStateDuration {
        return false  // Too soon to transition
    }

    st.currentState = newState
    st.stateStartTime = time.Now()
    return true
}
```

**4. Graceful Job Interruption**
```go
type Job struct {
    ID string
    Command string
    Caller string
    StartTime time.Time
    Progress float64  // Future: resume support
}

func (j *Job) Interrupt() error {
    // Kill process
    // Save state for resume
    // Queue with Priority 1
}
```

**5. GPU Monitoring (AMD ROCm)**
```go
// Use rocm-smi or sysfs
func GetGPUUsage() (float64, error) {
    // Option 1: Parse rocm-smi
    cmd := exec.Command("rocm-smi", "--showuse")
    // Parse: "GPU use (%): 85"

    // Option 2: Read sysfs
    // /sys/class/drm/card0/device/gpu_busy_percent

    return usage, nil
}
```

### Should Have (Tier 2 - Nice to Have)

**1. Configurable Thresholds**
```go
type Config struct {
    GPUThreshold float64        // 50.0%
    CheckInterval time.Duration  // 10s
    HysteresisPeriod time.Duration // 30s
    QueueDir string
    MetricsDir string
}
```

**2. Metrics Export Built-In**
```go
// Prometheus endpoint
http.Handle("/metrics", promhttp.Handler())

// Gauges/Counters
jobsQueued := prometheus.NewGauge(...)
jobsCompleted := prometheus.NewCounter(...)
```

**3. Structured Logging**
```go
import "log/slog"

slog.Info("Job queued",
    "job_id", jobID,
    "reason", "gaming-gpu",
    "gpu_usage", gpuUsage)
```

### Could Have (Tier 3 - Future)

1. Web dashboard (real-time queue view)
2. Job resume support (save/restore LLM state)
3. Multi-GPU support
4. Dynamic priority adjustment
5. Webhook notifications

---

## File Structure for Go Project

```
ollama-resource-manager/
├── cmd/
│   ├── resource-manager/
│   │   └── main.go              # Main daemon
│   └── batch-wrapper/
│       └── main.go              # Job wrapper
├── internal/
│   ├── monitor/
│   │   ├── monitor.go           # SystemMonitor interface
│   │   ├── gpu.go               # GPU usage detection
│   │   ├── process.go           # Process detection (pgrep)
│   │   └── monitor_test.go      # Unit tests with mocks
│   ├── state/
│   │   ├── tracker.go           # State machine + hysteresis
│   │   └── tracker_test.go
│   ├── queue/
│   │   ├── queue.go             # Job queue management
│   │   └── queue_test.go
│   └── metrics/
│       ├── collector.go         # Metrics collection
│       └── exporter.go          # Export formats
├── pkg/
│   └── job/
│       └── job.go               # Job struct/methods
├── configs/
│   └── config.yaml              # Default configuration
├── scripts/
│   └── install.sh               # Deployment script
├── go.mod
├── go.sum
└── README.md
```

---

## Migration Checklist

### Phase 1: Core Functionality
- [ ] Set up Go project structure
- [ ] Implement SystemMonitor interface
- [ ] Process detection (Steam games, Plex)
- [ ] GPU usage reading (rocm-smi)
- [ ] State tracker with hysteresis
- [ ] Write unit tests for above
- [ ] Job queue (priority-based)
- [ ] Job wrapper (command execution)

### Phase 2: Feature Parity with Bash
- [ ] Metadata tracking (caller, timing, etc.)
- [ ] Metrics export (JSON, CSV, Prometheus)
- [ ] Systemd service file
- [ ] Deployment script
- [ ] Migration from existing Bash setup

### Phase 3: Enhanced Features
- [ ] Configurable thresholds (YAML config)
- [ ] Structured logging
- [ ] Integration tests (simulate game launch/exit)
- [ ] Performance profiling

---

## Current Example Applications (Keep in Bash)

These are simple and work fine - no need to rewrite:

**home-automation.sh** - Fast responses (llama3.2:3b)
**email-summarizer.sh** - Daily batch (llama3.1:8b)
**code-reviewer.sh** - CI/CD integration (deepseek-coder:33b)
**research-analyzer.sh** - Quality analysis (llama3.1:70b)

They call the batch wrapper, which will be rewritten in Go.

---

## Known Edge Cases to Test

1. **Rapid game launch/exit** (thrashing prevention)
2. **Game crashes** (process disappears suddenly)
3. **Long-running jobs** (>5 min) interrupted multiple times
4. **Queue starvation** (gaming session lasts hours)
5. **GPU spike during cutscenes** (don't kill mid-job for 3 sec spike)
6. **Multiple Ollama jobs** queued simultaneously
7. **System reboot** with jobs in queue (persistence?)
8. **Disk full** during metrics write

---

## Dependencies for Go Implementation

### System Dependencies
```bash
# GPU monitoring
rocm-smi              # AMD GPU usage

# Process detection
pgrep / ps            # Already available on Linux
```

### Go Dependencies
```go
// go.mod
module github.com/yourusername/ollama-resource-manager

go 1.22

require (
    github.com/prometheus/client_golang v1.19.0  // Metrics
    gopkg.in/yaml.v3 v3.0.1                      // Config parsing
    github.com/stretchr/testify v1.9.0           // Testing
)
```

---

## Performance Targets

- **Check interval:** 10 seconds (down from 20 in Bash)
- **State transition latency:** <500ms (detection → queue job)
- **GPU reading overhead:** <50ms per check
- **Memory usage:** <50MB (Go daemon)
- **CPU overhead:** <1% average

---

## References

**System Info:**
- OS: Linux Mint 22.2 (kernel 6.14.0-37)
- GPU: AMD RX 9070 XT (RDNA 4, gfx1201)
- Ollama: 0.16.3
- Steam library: `/mnt/apps-ssd/SteamLibrary/steamapps/common/`
- User: user
- Hostname: gpu-host

**Existing Documentation:**
- `METRICS-REFERENCE.md` - Complete metadata spec
- `APPLICATION-INTEGRATION-GUIDE.md` - How apps call the wrapper
- `ARCHITECTURE-ROADMAP.md` - Original Tier 1/2/3 plan

---

## Next Session Prompt

See `GO-MIGRATION-PROMPT.md` for the complete context handoff prompt.
