# Start Here - Go Migration Package

**Created:** 2026-02-22
**Purpose:** Continue Ollama resource manager development on personal laptop

---

## What This Is

We've completed the Bash prototype of an intelligent resource manager for Gaming/Plex/Ollama on a single machine. It works, but is:
- Untestable (no unit tests possible in Bash)
- Conservative (queues for ALL games, even lightweight ones)
- Missing hybrid detection (process + GPU monitoring)

**Next step:** Migrate to Go for proper testing, hybrid detection, and anti-thrashing logic.

---

## Files to Copy to Your Laptop

### Essential (Copy These)

1. **GO-MIGRATION-PROMPT.md** (19K) ← **PASTE THIS INTO CLAUDE**
   - Complete self-contained context
   - All requirements, code examples, test patterns
   - Everything needed to continue development

2. **GO-MIGRATION-HANDOFF.md** (15K)
   - Deep dive on all design decisions
   - Testing findings from this session
   - Why we made each architectural choice

3. **MIGRATION-QUICK-REFERENCE.md** (4.0K)
   - Quick navigation guide
   - Development workflow
   - Success criteria

### Reference Implementation (For Patterns)

4. **batch-job-wrapper-v5.sh** (5.7K)
   - Current Bash implementation
   - Metadata schema to preserve in Go
   - Caller detection logic

5. **resource-manager-v3.sh** (6.9K)
   - Current daemon implementation
   - Process detection patterns (validated)
   - Queue management logic

6. **METRICS-REFERENCE.md** (7.9K)
   - Complete metadata specification
   - Must preserve all fields in Go

7. **APPLICATION-INTEGRATION-GUIDE.md** (8.4K)
   - How apps call the wrapper
   - Keep apps in Bash, only rewrite core system

---

## How to Use This Package

### On Your Personal Laptop

1. **Copy the 7 files above to your laptop**
   ```bash
   # From this machine (gpu-host):
   cd /home/user/Documents/System-Architecture/Ollama-Plex-Gaming

   # Copy to USB/cloud/whatever:
   cp GO-MIGRATION-PROMPT.md \
      GO-MIGRATION-HANDOFF.md \
      MIGRATION-QUICK-REFERENCE.md \
      batch-job-wrapper-v5.sh \
      resource-manager-v3.sh \
      METRICS-REFERENCE.md \
      APPLICATION-INTEGRATION-GUIDE.md \
      /path/to/transfer/location/
   ```

2. **Open Claude on your laptop**

3. **Paste the ENTIRE contents of GO-MIGRATION-PROMPT.md** into Claude
   - This gives Claude all the context
   - It's self-contained, no other files needed initially

4. **Start development**
   ```
   You: "Let's start implementing Phase 1. Create the project
   structure and implement the SystemMonitor interface with
   full unit tests."
   ```

5. **Reference the other files as needed**
   - Use Bash scripts to understand current behavior
   - Use HANDOFF.md to understand "why" decisions were made
   - Use METRICS-REFERENCE.md for exact data structures

---

## Development Phases

### Phase 1: Core Detection (Week 1)
**Goal:** Hybrid detection with full test coverage

- [ ] Set up Go project structure
- [ ] Implement `SystemMonitor` interface
- [ ] GPU monitoring (rocm-smi or sysfs)
- [ ] Process detection (Steam, Plex)
- [ ] Unit tests with mocks (>70% coverage)
- [ ] `StateTracker` with hysteresis (30 sec minimum)
- [ ] Test on real hardware

### Phase 2: Queue System (Week 2)
**Goal:** Job queueing and execution

- [ ] Job struct and queue implementation
- [ ] Main daemon loop
- [ ] Batch wrapper (replace Bash script)
- [ ] Metadata tracking (preserve schema)
- [ ] Integration tests

### Phase 3: Polish (Week 3)
**Goal:** Production ready

- [ ] YAML configuration file
- [ ] Metrics export (JSON, CSV, Prometheus)
- [ ] Systemd service file
- [ ] Deployment script
- [ ] Migration guide

---

## Testing Strategy

**Unit Tests (Mock Everything)**
```go
func TestHybridDetection_GPUIntensiveGame(t *testing.T) {
    mock := &MockMonitor{
        gameRunning: true,
        gpuUsage: 85.0,
    }
    state, _ := DetectContention(mock)
    assert.Equal(t, Gaming_GPU_Intensive, state)
}
```

**Integration Tests (Simulated Scenarios)**
- Game launch → job queued
- Game exit → queue processed
- Rapid state changes → hysteresis prevents thrashing

**Manual Tests (Real Hardware)**
- Launch lightweight game (RimWorld) → verify GPU <50%, jobs run
- Launch GPU game (Cyberpunk) → verify GPU >50%, jobs queued
- Long gaming session → queue persistence

---

## Key Requirements (Don't Forget These)

### Hybrid Detection Logic
```
Game detected → Check GPU usage
  ├─ GPU >50% → Queue all Ollama jobs (gaming-gpu-intensive)
  └─ GPU <50% → Allow Ollama to run (gaming-lightweight)

No game → Check Plex
  ├─ Plex transcoding → Queue Ollama jobs
  └─ No Plex → Available (run everything)
```

### Anti-Thrashing (Hysteresis)
- State must be stable for 30+ seconds before transitioning
- Prevents constant kill/restart on menu screens, cutscenes
- Use `StateTracker` with candidate state pattern

### Job Priorities
- Priority 1: Interrupted jobs (game launched mid-execution)
- Priority 2: New jobs submitted during contention

### Metadata Preservation
Go implementation must output same JSON schema as Bash version (see METRICS-REFERENCE.md)

---

## Success Criteria

MVP is complete when:

✅ Detects Steam games via `SteamLaunch AppId=` pattern
✅ Reads AMD GPU usage (rocm-smi or sysfs)
✅ Hybrid logic: queues only when GPU >50%
✅ Hysteresis prevents state thrashing
✅ Job queue works with priorities
✅ Unit test coverage >70%
✅ Integration tests pass
✅ Works on real hardware with actual game
✅ Systemd service runs reliably

---

## When Development Complete

### Bring Go Implementation Back to This Machine

1. **Copy built binaries**
   ```bash
   scp resource-manager batch-wrapper user@gpu-host:/tmp/
   ```

2. **Run deployment script**
   ```bash
   ./install-go-version.sh  # You'll create this
   ```

3. **Test with real workload**
   - Launch games, run Ollama jobs
   - Monitor for 24-48 hours
   - Check metrics, logs

4. **Keep example apps in Bash**
   - home-automation.sh, email-summarizer.sh, etc.
   - They just call the new Go wrapper
   - No changes needed

---

## Questions? Issues?

All design decisions are documented in GO-MIGRATION-HANDOFF.md with rationale:
- Why process-based detection?
- Why hybrid (process + GPU)?
- Why Go instead of Bash?
- What patterns were tested and validated?

The Bash implementation is working and deployed on this machine. You can test it anytime to see current behavior.

---

## Final Notes

**What works now (Bash):**
- Process-based detection (all games queued)
- Job queueing with priorities
- Comprehensive metadata tracking
- Metrics export (JSON, CSV, Prometheus)

**What we're adding (Go):**
- Hybrid detection (GPU awareness)
- Anti-thrashing (hysteresis)
- Full unit test coverage
- Type safety and maintainability

**Example applications stay the same:**
- home-automation.sh
- email-summarizer.sh
- code-reviewer.sh
- research-analyzer.sh

They're simple, work fine, just call the wrapper.

---

## Ready to Start?

1. Copy the 7 files to your laptop
2. Open Claude
3. Paste GO-MIGRATION-PROMPT.md
4. Start with Phase 1

Good luck! 🚀
