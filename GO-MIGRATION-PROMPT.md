# Go Migration Context Prompt

**Copy this entire document and paste it into Claude on your personal laptop to continue development.**

---

## Project Overview

I'm migrating a Bash-based resource management system to Go. The system manages competing resources on a single Linux machine running:

1. **Gaming** (Steam, GPU-intensive AAA titles) - Priority #1
2. **Plex Media Server** (transcoding, CPU/GPU heavy)
3. **Ollama** (Local LLM for automation, analysis, summarization)

**Core requirement:** When GPU-intensive games launch, queue all Ollama jobs. When games exit, resume queued jobs. Lightweight games (RimWorld, Dwarf Fortress) can run alongside Ollama.

---

## Why Migrating to Go

**Current Bash implementation problems:**
1. **Untestable** - No unit tests, can't mock system calls, no way to test race conditions
2. **Conservative** - Queues for ALL games (even CPU-only ones that don't need GPU)
3. **No hybrid detection** - Can't check GPU usage when game detected
4. **Complex state management** - Hysteresis, anti-thrashing require proper state machine
5. **Type safety** - Bash makes it easy to introduce bugs

**Go benefits:**
- Full unit test coverage with mocked system interfaces
- Type safety prevents entire classes of bugs
- Proper state machine for complex logic
- Easy to add features (config files, webhooks, metrics)
- Standard library excellent for system monitoring

---

## Current Architecture (Bash V5)

### Components

**1. resource-manager.sh** (Main daemon)
- Runs as systemd service, checks every 20 seconds
- Detects: Plex transcoding, Steam games (process-based, not GPU %)
- Writes state to `/tmp/resource-manager-state`
- Processes job queue when resources free

**2. batch-job-wrapper.sh** (Generic command wrapper)
- Accepts any command (not just Ollama)
- Checks resource state before running
- Queues jobs if contested, runs immediately if available
- Tracks metadata: caller, user, timing, exit codes, remote IP

**3. Queue system**
- Priority 1: Interrupted jobs (game launched mid-execution)
- Priority 2: New jobs submitted during contention
- Files: `/var/lib/batch-jobs/queue/1-jobid.job`

### Detection Logic (Current)

```bash
# Steam games - validated pattern
if pgrep -f "SteamLaunch AppId=" > /dev/null 2>&1; then
  echo "gaming-steam"
  # Queues ALL Ollama jobs
fi

# Plex transcoding
if pgrep -f "Plex Transcoder" > /dev/null 2>&1; then
  echo "plex"
fi
```

**Problem:** This queues even for lightweight games like RimWorld (CPU-only, doesn't need GPU).

**Solution needed:** Hybrid detection - detect game process, THEN check GPU usage to decide if queueing needed.

---

## Go Migration Requirements

### Must Have (Tier 1 - MVP)

#### 1. Hybrid Detection (Process + GPU)

```go
type SystemMonitor interface {
    DetectGame() (*GameProcess, error)
    GetGPUUsage() (float64, error)
    DetectPlex() (bool, error)
}

type GameProcess struct {
    Name string
    PID  int
}

type ResourceState int
const (
    Available ResourceState = iota
    Gaming_GPU_Intensive
    Gaming_Lightweight
    PlexTranscoding
)

func DetectContention(monitor SystemMonitor) (ResourceState, error) {
    // Check for game
    if game, err := monitor.DetectGame(); err == nil && game != nil {
        // Game running - check GPU usage
        gpuUsage, err := monitor.GetGPUUsage()
        if err != nil {
            // Can't read GPU, be conservative
            return Gaming_GPU_Intensive, nil
        }

        if gpuUsage > 50.0 {
            return Gaming_GPU_Intensive, nil  // Queue Ollama
        }
        return Gaming_Lightweight, nil  // Allow Ollama alongside
    }

    // Check for Plex
    if plex, _ := monitor.DetectPlex(); plex {
        return PlexTranscoding, nil
    }

    return Available, nil
}
```

#### 2. Anti-Thrashing with Hysteresis

**Problem:** GPU usage can spike briefly (cutscenes, menus), don't want to kill/restart jobs constantly.

**Solution:** State must be stable for N seconds before transitioning.

```go
type StateTracker struct {
    currentState     ResourceState
    candidateState   ResourceState
    candidateStarted time.Time
    minStateDuration time.Duration  // e.g., 30 seconds
}

func (st *StateTracker) Update(newState ResourceState) (changed bool, finalState ResourceState) {
    // Already in this state
    if newState == st.currentState {
        st.candidateState = newState
        return false, st.currentState
    }

    // New candidate state
    if newState != st.candidateState {
        st.candidateState = newState
        st.candidateStarted = time.Now()
        return false, st.currentState  // Don't transition yet
    }

    // Candidate state persisting - check if stable long enough
    if time.Since(st.candidateStarted) >= st.minStateDuration {
        // Transition!
        st.currentState = newState
        return true, newState
    }

    // Not stable long enough yet
    return false, st.currentState
}
```

**Example:**
```
00:00 - Cyberpunk running, GPU 85% → Gaming_GPU_Intensive (stable)
00:30 - Player enters menu, GPU drops to 10% → Candidate: Gaming_Lightweight
00:35 - Still in menu (GPU 8%) → Candidate still Gaming_Lightweight
00:50 - Still in menu → 20 seconds stable, TRANSITION to Gaming_Lightweight
00:51 - Ollama jobs start running
01:00 - Player exits menu, GPU 90% → Candidate: Gaming_GPU_Intensive
01:05 - Still gaming → Candidate persists
01:30 - Still gaming → 30 seconds stable, TRANSITION to Gaming_GPU_Intensive
01:31 - Kill Ollama jobs, add to queue (Priority 1)
```

This prevents thrashing on brief GPU spikes.

#### 3. Full Test Coverage

```go
// Example test structure
func TestHybridDetection_GPUIntensiveGame(t *testing.T) {
    mock := &MockMonitor{
        gameRunning: true,
        gameName:    "Cyberpunk 2077",
        gpuUsage:    85.0,
    }

    state, err := DetectContention(mock)

    assert.NoError(t, err)
    assert.Equal(t, Gaming_GPU_Intensive, state)
}

func TestHybridDetection_LightweightGame(t *testing.T) {
    mock := &MockMonitor{
        gameRunning: true,
        gameName:    "RimWorld",
        gpuUsage:    8.0,
    }

    state, err := DetectContention(mock)

    assert.NoError(t, err)
    assert.Equal(t, Gaming_Lightweight, state)
}

func TestStateTracker_Hysteresis(t *testing.T) {
    tracker := NewStateTracker(30 * time.Second)

    // Initial state: Available
    tracker.currentState = Available

    // GPU spike for 5 seconds
    changed, state := tracker.Update(Gaming_GPU_Intensive)
    assert.False(t, changed)  // Too soon to transition
    assert.Equal(t, Available, state)

    // Simulate 35 seconds passing with same state
    tracker.candidateStarted = time.Now().Add(-35 * time.Second)
    changed, state = tracker.Update(Gaming_GPU_Intensive)
    assert.True(t, changed)  // Should transition now
    assert.Equal(t, Gaming_GPU_Intensive, state)
}
```

#### 4. GPU Monitoring (AMD ROCm)

**Hardware:** AMD RX 9070 XT (RDNA 4, gfx1201)

**Option 1: rocm-smi (Recommended)**
```go
func GetGPUUsage() (float64, error) {
    cmd := exec.Command("rocm-smi", "--showuse")
    output, err := cmd.Output()
    if err != nil {
        return 0, err
    }

    // Parse output like:
    // GPU use (%): 85

    // Regex or string parsing
    re := regexp.MustCompile(`GPU use \(%\):\s+(\d+)`)
    matches := re.FindStringSubmatch(string(output))
    if len(matches) < 2 {
        return 0, fmt.Errorf("couldn't parse GPU usage")
    }

    usage, _ := strconv.ParseFloat(matches[1], 64)
    return usage, nil
}
```

**Option 2: sysfs (Faster, no subprocess)**
```go
func GetGPUUsage() (float64, error) {
    // Read: /sys/class/drm/card0/device/gpu_busy_percent
    data, err := os.ReadFile("/sys/class/drm/card0/device/gpu_busy_percent")
    if err != nil {
        return 0, err
    }

    usage, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
    return usage, err
}
```

Test which file exists on your system and use that.

#### 5. Process Detection (Steam Games)

**Validated pattern:** `SteamLaunch AppId=` only present when game running

```go
func DetectGame() (*GameProcess, error) {
    // Option 1: Use pgrep
    cmd := exec.Command("pgrep", "-f", "SteamLaunch AppId=")
    output, err := cmd.Output()
    if err != nil {
        // Exit code 1 = no match (not an error)
        if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
            return nil, nil  // No game running
        }
        return nil, err
    }

    // Parse PID
    pidStr := strings.TrimSpace(string(output))
    pid, _ := strconv.Atoi(pidStr)

    // Get process name
    cmd = exec.Command("ps", "-p", pidStr, "-o", "comm=")
    nameOutput, _ := cmd.Output()
    name := strings.TrimSpace(string(nameOutput))

    return &GameProcess{
        Name: name,
        PID:  pid,
    }, nil
}

func DetectPlex() (bool, error) {
    cmd := exec.Command("pgrep", "-f", "Plex Transcoder")
    err := cmd.Run()
    if err != nil {
        if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
            return false, nil  // Not running
        }
        return false, err
    }
    return true, nil  // Running
}
```

#### 6. Job Queue Management

```go
type Job struct {
    ID       string    `json:"job_id"`
    Command  string    `json:"command"`
    Caller   string    `json:"caller"`
    Priority int       `json:"priority"`  // 1=interrupted, 2=new
    QueuedAt time.Time `json:"queued_at"`
}

type Queue struct {
    dir string  // /var/lib/batch-jobs/queue
}

func (q *Queue) Enqueue(job Job) error {
    filename := fmt.Sprintf("%d-%s.job", job.Priority, job.ID)
    path := filepath.Join(q.dir, filename)

    data, err := json.Marshal(job)
    if err != nil {
        return err
    }

    return os.WriteFile(path, data, 0644)
}

func (q *Queue) Dequeue() ([]Job, error) {
    files, err := filepath.Glob(filepath.Join(q.dir, "*.job"))
    if err != nil {
        return nil, err
    }

    // Sort by priority (filename prefix)
    sort.Strings(files)

    jobs := []Job{}
    for _, file := range files {
        data, _ := os.ReadFile(file)
        var job Job
        json.Unmarshal(data, &job)
        jobs = append(jobs, job)
        os.Remove(file)  // Remove from queue
    }

    return jobs, nil
}
```

### Should Have (Tier 2)

1. **Configuration file (YAML)**
   ```go
   type Config struct {
       GPUThreshold      float64       `yaml:"gpu_threshold"`       // 50.0
       CheckInterval     time.Duration `yaml:"check_interval"`      // 10s
       HysteresisPeriod  time.Duration `yaml:"hysteresis_period"`   // 30s
       QueueDir          string        `yaml:"queue_dir"`
       MetricsDir        string        `yaml:"metrics_dir"`
   }
   ```

2. **Structured logging**
   ```go
   import "log/slog"

   slog.Info("Resource state changed",
       "old_state", oldState,
       "new_state", newState,
       "gpu_usage", gpuUsage)
   ```

3. **Metrics endpoint (Prometheus)**
   ```go
   var (
       jobsQueued = prometheus.NewGauge(prometheus.GaugeOpts{
           Name: "ollama_jobs_queued",
           Help: "Number of jobs currently queued",
       })

       stateGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
           Name: "resource_state",
           Help: "Current resource state (0=available, 1=gaming, 2=plex)",
       }, []string{"state"})
   )
   ```

---

## Project Structure

```
ollama-resource-manager/
├── cmd/
│   ├── resource-manager/
│   │   └── main.go              # Main daemon
│   └── batch-wrapper/
│       └── main.go              # Job wrapper (replaces Bash script)
├── internal/
│   ├── monitor/
│   │   ├── monitor.go           # SystemMonitor interface + implementations
│   │   ├── gpu.go               # GPU usage (rocm-smi or sysfs)
│   │   ├── process.go           # Process detection (pgrep)
│   │   └── monitor_test.go      # Unit tests with mocks
│   ├── state/
│   │   ├── tracker.go           # StateTracker with hysteresis
│   │   └── tracker_test.go
│   ├── queue/
│   │   ├── queue.go             # Job queue management
│   │   └── queue_test.go
│   ├── metrics/
│   │   ├── collector.go         # Metrics collection
│   │   └── exporter.go          # Export to JSON/CSV/Prometheus
│   └── config/
│       └── config.go            # YAML config loading
├── pkg/
│   └── job/
│       └── job.go               # Job struct and methods
├── configs/
│   └── config.yaml              # Default configuration
├── deployments/
│   └── systemd/
│       └── resource-manager.service
├── scripts/
│   └── install.sh               # Deployment script
├── go.mod
├── go.sum
├── README.md
└── Makefile
```

---

## Mock Interface Pattern for Testing

```go
// monitor/monitor.go
type SystemMonitor interface {
    DetectGame() (*GameProcess, error)
    GetGPUUsage() (float64, error)
    DetectPlex() (bool, error)
}

// Real implementation
type RealMonitor struct{}

func (m *RealMonitor) DetectGame() (*GameProcess, error) {
    // Real pgrep logic
}

func (m *RealMonitor) GetGPUUsage() (float64, error) {
    // Real rocm-smi or sysfs read
}

// monitor/monitor_test.go
type MockMonitor struct {
    gameRunning bool
    gameName    string
    gpuUsage    float64
    plexRunning bool
}

func (m *MockMonitor) DetectGame() (*GameProcess, error) {
    if m.gameRunning {
        return &GameProcess{Name: m.gameName, PID: 1234}, nil
    }
    return nil, nil
}

func (m *MockMonitor) GetGPUUsage() (float64, error) {
    return m.gpuUsage, nil
}

func (m *MockMonitor) DetectPlex() (bool, error) {
    return m.plexRunning, nil
}
```

This allows you to test ALL logic without needing actual games, GPU, or Plex running.

---

## Implementation Steps

### Phase 1: Core Detection (Week 1)

1. **Set up project**
   ```bash
   mkdir ollama-resource-manager && cd ollama-resource-manager
   go mod init github.com/yourusername/ollama-resource-manager
   ```

2. **Implement SystemMonitor interface**
   - Process detection (Steam, Plex)
   - GPU usage (rocm-smi or sysfs - test which works)
   - Write unit tests with mocks

3. **Implement StateTracker with hysteresis**
   - State machine logic
   - Transition only when stable
   - Unit tests for edge cases

4. **Test on real system**
   - Launch RimWorld, verify GPU reading ~5-10%
   - Launch GPU-heavy game (if available), verify >50%
   - Test state transitions with hysteresis

### Phase 2: Queue System (Week 2)

5. **Implement Job struct and Queue**
   - Priority queue (1=interrupted, 2=new)
   - Persist to `/var/lib/batch-jobs/queue/*.job`
   - Unit tests

6. **Main daemon loop**
   - Check resources every 10 seconds
   - State transitions → kill jobs / resume queue
   - Integration tests

7. **Batch wrapper (replaces Bash script)**
   - Check state file
   - Run immediately or enqueue
   - Metadata tracking (caller, timing, etc.)

### Phase 3: Polish (Week 3)

8. **Configuration file**
   - YAML config for thresholds
   - Validate and load on startup

9. **Metrics export**
   - JSON/CSV export (match Bash version)
   - Prometheus endpoint (bonus)

10. **Deployment**
    - Systemd service file
    - Install script
    - Migration guide from Bash version

---

## Testing Strategy

### Unit Tests (70% coverage minimum)
- All detection logic with mocked system calls
- State tracker edge cases
- Queue operations (enqueue, dequeue, priority)

### Integration Tests
- Simulate game launch/exit sequence
- Verify job queueing/resuming
- Verify hysteresis prevents thrashing

### Manual Tests on Real Hardware
- Launch RimWorld → Verify state stays "Available" (GPU low)
- Launch Cyberpunk → Verify queues (GPU high)
- Test rapid game launch/exit (thrashing scenario)
- Long gaming session (queue persistence)

---

## Edge Cases to Handle

1. **GPU read fails** → Be conservative, assume contested
2. **Game crashes** → Process disappears suddenly, should resume queue
3. **Multiple Ollama jobs running** → Kill all when contested
4. **Disk full** during queue write → Log error, reject job
5. **Invalid JSON in queue file** → Skip file, log error
6. **Process check fails** (pgrep error) → Retry once, then assume available
7. **State file corrupted** → Recreate, assume available

---

## Dependencies

```go
// go.mod
module github.com/yourusername/ollama-resource-manager

go 1.22

require (
    github.com/prometheus/client_golang v1.19.0  // Metrics (optional)
    gopkg.in/yaml.v3 v3.0.1                      // Config
    github.com/stretchr/testify v1.9.0           // Testing
)
```

**System requirements:**
- Linux (process detection, sysfs)
- AMD GPU with ROCm (rocm-smi or sysfs gpu_busy_percent)
- systemd (for service management)

---

## Validation Criteria (MVP Complete)

MVP is complete when:

- [ ] Can detect Steam games (process-based)
- [ ] Can read GPU usage (AMD ROCm)
- [ ] Hybrid logic: queues only when GPU >50%
- [ ] State transitions use hysteresis (30 sec minimum)
- [ ] Jobs queue/resume correctly
- [ ] Unit test coverage >70%
- [ ] Works on real hardware (tested with actual game)
- [ ] Systemd service runs reliably

---

## Your Task

Build the Go-based resource manager with:

1. **Hybrid detection** - Process + GPU monitoring
2. **Full test coverage** - Mocked interfaces for all system calls
3. **Anti-thrashing** - Hysteresis prevents rapid state changes
4. **Production ready** - Proper error handling, logging, configuration

Focus on **rocksolid reliability** with **minimal user tinkering**. Make it testable, type-safe, and maintainable.

Start with Phase 1 (core detection + state tracker), get that working with tests, then move to Phase 2 (queue system).

---

## Questions to Address

1. Should we use rocm-smi or sysfs for GPU reading? (Test which file exists: `/sys/class/drm/card0/device/gpu_busy_percent`)
2. What GPU threshold makes sense? (50% seems reasonable)
3. How long should hysteresis period be? (30 seconds to prevent menu/cutscene thrashing)
4. Should interrupted jobs always go to front of queue? (Yes, Priority 1)

---

## Reference Files

The following files from the Bash implementation contain useful patterns/data structures:

- `batch-job-wrapper-v5.sh` - Metadata schema, caller detection, invocation methods
- `resource-manager-v3.sh` - Detection patterns, queue processing logic
- `METRICS-REFERENCE.md` - Complete metadata spec (preserve in Go)

Keep the example applications (home-automation.sh, etc.) in Bash - they're simple and just call the wrapper.

---

Ready to start? Begin with:

```bash
mkdir ollama-resource-manager
cd ollama-resource-manager
go mod init github.com/yourusername/ollama-resource-manager

# Create structure
mkdir -p cmd/resource-manager
mkdir -p internal/{monitor,state,queue}

# Start with monitor interface and tests
touch internal/monitor/monitor.go
touch internal/monitor/monitor_test.go
```

Good luck! Focus on getting the detection + state tracker working with full test coverage first.
