# Go Migration Quick Reference

## What to Take to Your Personal Laptop

### Primary Documents (Essential)

1. **GO-MIGRATION-PROMPT.md** ← **COPY THIS TO CLAUDE**
   - Complete context for continuing development
   - All requirements, examples, test patterns
   - Paste entire file into Claude on your laptop

2. **GO-MIGRATION-HANDOFF.md**
   - Detailed background on all design decisions
   - Testing findings
   - Current Bash architecture

### Reference Implementation (Bash - for patterns)

3. **batch-job-wrapper-v5.sh**
   - Metadata schema (preserve this in Go)
   - Caller detection logic
   - Invocation method detection (ssh, cron, interactive)
   - Job lifecycle tracking

4. **resource-manager-v3.sh**
   - Process detection patterns
   - Queue management logic
   - Main daemon loop structure

5. **METRICS-REFERENCE.md**
   - Complete metadata specification
   - Must preserve all fields in Go implementation

### Supporting Documentation

6. **APPLICATION-INTEGRATION-GUIDE.md**
   - How calling apps use the wrapper
   - Keep apps in Bash, only rewrite wrapper/manager

7. **METRICS-GUIDE.md**
   - Export formats (JSON, CSV, Prometheus)
   - Preserve compatibility

---

## Files You Can Ignore for Migration

These are historical/deprecated:
- `BLOG-POST-*.md` (documentation only)
- `PHASE-*.md` (old planning docs)
- `*-v1.sh`, `*-v2.sh` (old versions)
- `archive/` directory

---

## Quick Start on Personal Laptop

1. **Copy to your laptop:**
   ```bash
   # Just these files:
   GO-MIGRATION-PROMPT.md
   GO-MIGRATION-HANDOFF.md
   batch-job-wrapper-v5.sh (reference)
   resource-manager-v3.sh (reference)
   METRICS-REFERENCE.md
   ```

2. **Open Claude on your laptop**

3. **Paste the entire contents of GO-MIGRATION-PROMPT.md**

4. **Ask Claude to start with Phase 1:**
   ```
   Let's start implementing Phase 1. Create the project structure and
   implement the SystemMonitor interface with full test coverage.
   ```

---

## Development Workflow

### Phase 1: Detection & State (Week 1)
- Set up Go project
- Implement `SystemMonitor` interface
- Implement `StateTracker` with hysteresis
- Write comprehensive unit tests
- Test on real hardware

### Phase 2: Queue System (Week 2)
- Implement job queue (priority-based)
- Main daemon loop
- Batch wrapper (replace Bash script)
- Integration tests

### Phase 3: Polish (Week 3)
- Configuration file (YAML)
- Metrics export
- Systemd service
- Deployment script

---

## Testing on Development Machine

You'll need to test on a machine with:
- AMD GPU (for GPU monitoring)
- Steam installed (for process detection)
- At least one game (RimWorld works, any game is fine)

Can develop most of it with mocks, but final validation needs real hardware.

---

## Key Design Principles

1. **Everything must be unit testable** - Use interfaces, mocks
2. **Fail safely** - If GPU read fails, be conservative (queue jobs)
3. **Hysteresis prevents thrashing** - State must be stable 30+ seconds
4. **Preserve metadata schema** - Go implementation must match Bash JSON format

---

## Success Criteria for MVP

✓ Detects Steam games (process-based)
✓ Reads AMD GPU usage (rocm-smi or sysfs)
✓ Hybrid logic: only queues when GPU >50%
✓ Hysteresis prevents rapid state changes
✓ Job queue works (priority 1 = interrupted, priority 2 = new)
✓ Unit test coverage >70%
✓ Works on real hardware with actual game
✓ Systemd service runs reliably

---

## Contact/Questions

If you get stuck or have questions about design decisions, reference:
- GO-MIGRATION-HANDOFF.md for "why we made this choice"
- The Bash scripts for "how it currently works"
- METRICS-REFERENCE.md for exact data structures

All design decisions are documented with rationale.

---

## After Go Implementation Complete

Come back to this machine (gpu-host) and:
1. Run deployment script
2. Migrate from Bash to Go version
3. Test with real gaming workload
4. Monitor for 24-48 hours to ensure stability

The example apps (home-automation.sh, email-summarizer.sh, etc.) stay in Bash - they just call the new Go wrapper.
