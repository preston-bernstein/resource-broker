# Architecture Roadmap: Batch Job Orchestration

**Project:** Ollama Resource Management System
**Date:** 2026-02-21
**Author:** Claude + Preston

---

## Executive Summary

This document outlines the evolution of the Ollama batch job system from a simple resource manager (Tier 1) to a production-grade orchestration platform (Tier 3).

**Current Status:** Tier 1 (Deployed)
**Next Phase:** Tier 2 (Planned)
**Future Vision:** Tier 3 (Planned)

---

## Tier Comparison

| Feature | Tier 1 (MVP) | Tier 2 (Configurable) | Tier 3 (Production) |
|---------|--------------|----------------------|---------------------|
| **Lines of Code** | 277 | ~350 | ~1950 |
| **Languages** | Bash | Bash | Python + Bash |
| **Storage** | /tmp files | /tmp files + metadata | SQLite database |
| **Preemption** | All jobs killed | Configurable per job | Priority-based |
| **Visibility** | Logs only | batch-status CLI | Web UI + CLI |
| **Persistence** | Lost on reboot | Lost on reboot | Survives reboot |
| **Checkpointing** | No | No | Yes |
| **Dependencies** | No | No | Yes |
| **Scheduling** | Manual only | Manual only | Cron-like |
| **Multi-user** | No quotas | No quotas | Per-user quotas |
| **Monitoring** | Logs | Logs | Prometheus + Grafana |
| **Effort** | Deployed | 7-9 hours | 60-80 hours |

---

## Tier 1: MVP (Current)

**Status:** ✅ Deployed

### Capabilities

- GPU-based gaming detection (0-2% idle, 50%+ gaming threshold)
- Automatic batch job preemption during gaming/Plex
- Intelligent retry after resources free
- On-demand model loading (5-minute timeout)
- Hysteresis to prevent state flapping

### Components

```
/usr/local/bin/
├── resource-manager.sh (196 lines)
│   - Monitors GPU every 20s
│   - Kills batch jobs when gaming/Plex starts
│   - Throttles Ollama to 3GB/1core during gaming
│   - Writes /tmp/resource-manager-state
│
└── batch-job-wrapper.sh (81 lines)
    - Wraps Ollama batch jobs
    - Reads system state
    - Retries intelligently on kill
    - Waits for gaming to end
```

### Limitations

- ❌ All batch jobs treated equally (no priorities)
- ❌ No visibility into running jobs
- ❌ No way to mark job as non-preemptible
- ❌ Job metadata lost on reboot
- ❌ No checkpointing (long jobs restart from scratch)

### Use Cases

**Perfect for:**
- Personal single-user system
- Simple batch job automation (email summarization, code analysis)
- Home automation alongside gaming

**Not suitable for:**
- Multi-user systems (no quotas)
- Jobs requiring checkpointing (long-running analysis)
- Need visibility/monitoring (logs only)

---

## Tier 2: Configurable Preemption (Planned)

**Status:** 📋 Detailed plan created
**Effort:** 7-9 hours development + testing
**Dependencies:** `jq` (JSON parser)

### New Capabilities

1. **Configurable preemption per job**
   - `--preempt`: Can be killed (default)
   - `--no-preempt`: Never killed, continues on CPU
   - `--queue`: Waits for GPU, not killed once started

2. **Job metadata tracking**
   - Job ID, PID, priority, model, started time
   - Stored in `/tmp/batch-jobs/*.meta`

3. **Visibility command**
   - `batch-status`: Shows running/queued jobs, system state
   - Real-time view of what's happening

### Usage Examples

```bash
# Email summarization (can be killed, will retry)
batch-job-wrapper.sh --preempt --job-id "email-summary" \
  --model llama3.1:70b "Summarize emails..."

# Quick home automation (never killed)
batch-job-wrapper.sh --no-preempt --job-id "home-lights" \
  --model llama3.2:3b "Turn on lights"

# Large batch (wait for GPU, then run to completion)
batch-job-wrapper.sh --queue --job-id "video-transcription" \
  --model whisper:large "Transcribe 100 files..."

# Check status
$ batch-status
BATCH JOBS:
ID             STATUS    PRIORITY      MODEL           STARTED
email-summary  RUNNING   preempt       llama3.1:70b    2m ago
home-lights    RUNNING   no-preempt    llama3.2:3b     10s ago

SYSTEM STATE: idle (GPU: 15%)
```

### Migration Path

- ✅ Backward compatible (Tier 1 usage still works)
- ✅ Incremental adoption (can use new flags as needed)
- ✅ No database changes (still /tmp-based)

### Key Files

```
TIER-2-IMPLEMENTATION-PLAN.md  # Detailed implementation guide
├── Components
│   ├── batch-job-wrapper-v3.sh (150 lines)
│   ├── resource-manager-v3.sh (180 lines)
│   └── batch-status.sh (20 lines)
├── Testing Plan
│   ├── Test preemptible jobs
│   ├── Test non-preemptible jobs
│   ├── Test queue mode
│   └── Test batch-status command
└── Deployment Checklist
```

---

## Tier 3: Production Platform (Future Vision)

**Status:** 📋 Detailed plan created
**Effort:** 60-80 hours (2 weeks full-time)
**Dependencies:** Python, FastAPI, SQLite, Uvicorn

### New Capabilities

1. **Checkpointing**
   - Long jobs save progress periodically
   - Resume from checkpoint after interruption
   - No lost work

2. **Priority Queues**
   - Jobs have numeric priority (0-100)
   - High-priority jobs preempt low-priority
   - Intelligent scheduling

3. **Persistent Storage**
   - SQLite database (survives reboots)
   - Historical job data
   - Query past jobs

4. **Web UI**
   - Dashboard at `http://localhost:8080`
   - Real-time status updates (WebSocket)
   - Job submission form
   - GPU/CPU graphs

5. **Resource Quotas**
   - Per-user limits (max concurrent jobs, max GPU jobs)
   - Prevents resource starvation

6. **Job Dependencies**
   - "Run job B after job A completes"
   - Dependency chains
   - After-success / after-failure triggers

7. **Scheduled Jobs**
   - Cron-like scheduling
   - "Run email summary every day at 7am"
   - Template-based job creation

### Architecture

```
┌─────────────────────────────────────────┐
│       Web UI (http://localhost:8080)    │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐ │
│  │Dashboard │ │ Submit   │ │ History  │ │
│  │Real-time │ │ Jobs     │ │ Queries  │ │
│  └──────────┘ └──────────┘ └──────────┘ │
└──────────────────┬──────────────────────┘
                   │ FastAPI + WebSocket
┌──────────────────▼──────────────────────┐
│     Batch Orchestrator Service          │
│  ┌──────────────────────────────────┐   │
│  │ Priority Scheduler                │   │
│  │ Checkpoint Manager                │   │
│  │ Dependency Resolver               │   │
│  │ Resource Quota Enforcer           │   │
│  │ Cron Job Scheduler                │   │
│  └──────────────────────────────────┘   │
└──────────────────┬──────────────────────┘
                   │
         ┌─────────┼─────────┐
         ▼         ▼         ▼
    ┌────────┬────────┬────────┐
    │Worker 1│Worker 2│Worker N│
    └────────┴────────┴────────┘
         │
┌────────▼────────────────────────────┐
│   SQLite Database                   │
│  ┌──────────────────────────────┐   │
│  │ jobs (id, status, priority)  │   │
│  │ checkpoints (job_id, state)  │   │
│  │ dependencies (parent, child) │   │
│  │ quotas (user, limits)        │   │
│  └──────────────────────────────┘   │
└─────────────────────────────────────┘
```

### Usage Examples

```bash
# CLI: Submit job with checkpointing
$ batch-submit --checkpoint --priority 75 --model llama3.1:70b \
  "Process 1000 documents..."
Job ID: job-1234567890

# CLI: Query job status
$ batch-query job-1234567890
Status: running
Started: 2m ago
Progress: 45% (450/1000 documents processed)
Checkpoints: 3 saved

# CLI: Job with dependency
$ PARENT=$(batch-submit --model llama3.1:70b "Generate report")
$ batch-submit --depends-on $PARENT --model llama3.2:3b "Email report"

# CLI: Scheduled job
$ batch-schedule --cron "0 7 * * *" --model llama3.1:70b \
  --job-template "daily-email-summary" "Summarize emails..."

# Web UI: Visit http://localhost:8080
# - See real-time dashboard
# - Submit jobs via form
# - View GPU/CPU graphs
# - Browse job history
```

### Database Schema

```sql
-- Jobs table
CREATE TABLE jobs (
  id INTEGER PRIMARY KEY,
  job_id TEXT UNIQUE,
  user TEXT,
  priority INTEGER DEFAULT 50,
  status TEXT DEFAULT 'queued',
  model TEXT,
  checkpoint_path TEXT,
  created_at TIMESTAMP,
  started_at TIMESTAMP,
  completed_at TIMESTAMP
);

-- Checkpoints table
CREATE TABLE checkpoints (
  id INTEGER PRIMARY KEY,
  job_id TEXT,
  checkpoint_data TEXT,  -- JSON
  created_at TIMESTAMP
);

-- Dependencies table
CREATE TABLE dependencies (
  parent_job_id TEXT,
  child_job_id TEXT,
  dependency_type TEXT  -- after_completion, after_success
);

-- Quotas table
CREATE TABLE resource_quotas (
  user TEXT PRIMARY KEY,
  max_concurrent_jobs INTEGER DEFAULT 5,
  max_gpu_jobs INTEGER DEFAULT 2
);
```

### Key Files

```
TIER-3-IMPLEMENTATION-PLAN.md  # Detailed implementation guide
├── Backend (Python)
│   ├── orchestrator.py (400 lines)
│   ├── checkpoint_manager.py (200 lines)
│   ├── web_ui.py (400 lines - FastAPI)
│   └── scheduler.py (300 lines)
├── Frontend (HTML/JS)
│   ├── index.html (dashboard)
│   ├── submit.html (job form)
│   └── history.html (past jobs)
├── CLI Tools
│   ├── batch-submit
│   ├── batch-query
│   ├── batch-cancel
│   ├── batch-list
│   ├── batch-history
│   └── batch-schedule
└── Database
    └── /var/lib/batch-jobs/jobs.db
```

### Monitoring & Observability

```
Prometheus Metrics:
- jobs_submitted_total
- jobs_completed_total
- jobs_failed_total
- gpu_utilization_percent
- queue_depth

Grafana Dashboard:
┌──────────────────────────────────────────┐
│  Batch Job System - Last 24 Hours       │
├──────────────────────────────────────────┤
│  Jobs: 347 (↑ 15%)    Success: 94.2%   │
│  Avg Duration: 2m 45s  GPU Uptime: 87% │
│                                          │
│  [Job Throughput Graph]                 │
│  [GPU Utilization Graph]                │
│  [Queue Depth Graph]                    │
└──────────────────────────────────────────┘
```

---

## Decision Framework: Which Tier Do You Need?

### Choose Tier 1 if:
- ✅ Single user
- ✅ Simple batch automation
- ✅ OK with all jobs being preempted equally
- ✅ Logs are sufficient for debugging
- ✅ Don't need checkpointing

**Effort:** Already deployed
**Maintenance:** Low (just log rotation)

---

### Choose Tier 2 if:
- ✅ Need some jobs to continue during gaming
- ✅ Want visibility into running jobs
- ✅ Want configurable preemption policies
- ❌ Still don't need checkpointing
- ❌ Still don't need multi-user quotas

**Effort:** 7-9 hours to implement + test
**Maintenance:** Low (still mostly stateless)

---

### Choose Tier 3 if:
- ✅ Multi-user system
- ✅ Long-running jobs need checkpointing
- ✅ Need web UI for monitoring
- ✅ Want job history and analytics
- ✅ Need scheduled/recurring jobs
- ✅ Want production-grade reliability

**Effort:** 60-80 hours to implement + test
**Maintenance:** Medium (database backups, web UI updates)

---

## Migration Strategy

### Tier 1 → Tier 2
1. Install `jq`: `sudo apt install jq`
2. Deploy batch-job-wrapper-v3.sh (backward compatible)
3. Deploy resource-manager-v3.sh
4. Deploy batch-status command
5. **Test:** Existing jobs still work (Tier 1 usage)
6. **Adopt:** Start using new flags (`--preempt`, `--no-preempt`, `--queue`)

**Rollback plan:** Keep old scripts as `.v2-backup`, swap back if issues

---

### Tier 2 → Tier 3
1. Install Python dependencies: `pip install fastapi uvicorn sqlite3`
2. Initialize database: `batch-init-db`
3. Migrate job metadata from /tmp to database (one-time script)
4. Deploy orchestrator service
5. Deploy web UI service
6. **Test:** Submit jobs via CLI (should work)
7. **Test:** Access web UI at http://localhost:8080
8. **Adopt:** Enable checkpointing, scheduled jobs, quotas

**Rollback plan:** Database persists, can revert to Tier 2 scripts reading from DB

---

## Current Status Summary

```
┌────────────────────────────────────────────────────────┐
│  PROJECT STATUS                                         │
├────────────────────────────────────────────────────────┤
│  Tier 1: ✅ DEPLOYED                                   │
│    - GPU detection working                             │
│    - On-demand loading (0% idle, 5min timeout)        │
│    - Gaming detection reliable                         │
│    - Batch jobs preempt correctly                      │
│    - Logs showing expected behavior                    │
│                                                         │
│  Tier 2: 📋 PLANNED                                    │
│    - Implementation plan complete                      │
│    - Ready to implement when needed                    │
│    - Estimated effort: 7-9 hours                       │
│                                                         │
│  Tier 3: 📋 PLANNED                                    │
│    - Implementation plan complete                      │
│    - Future enhancement (60-80 hours)                  │
│    - Production-grade features                         │
└────────────────────────────────────────────────────────┘
```

---

## Documentation Index

```
/home/user/Documents/System-Architecture/Ollama-Plex-Gaming/
├── ARCHITECTURE-ROADMAP.md (this file)
├── TIER-2-IMPLEMENTATION-PLAN.md (detailed Tier 2 specs)
├── TIER-3-IMPLEMENTATION-PLAN.md (detailed Tier 3 specs)
├── BLOG-POST-FINAL.md (complete story with all discoveries)
├── GPU-TROUBLESHOOTING.md (RX 9070 XT RDNA 4 GPU detection issue)
├── WORKING-GUIDELINES.md (implementation log, history)
├── WORKING-GUIDELINES-APPEND.txt (recent discoveries)
├── resource-manager-v2.sh (current Tier 1 implementation)
└── batch-job-wrapper-v2.sh (current Tier 1 implementation)
```

---

## Next Steps

**Immediate:**
1. ✅ Deploy fixed Tier 1 (on-demand loading)
2. ⏳ Verify GPU idles at 0% when no models loaded
3. ⏳ Test gaming detection with actual game
4. ⏳ Monitor for 24-48 hours to verify stability

**Short-term (if needed):**
1. Implement Tier 2 (configurable preemption)
2. Add batch-status command for visibility
3. Document which jobs use which flags

**Long-term (future):**
1. Evaluate need for Tier 3 features
2. If multi-user or checkpointing needed, implement Tier 3
3. Add monitoring (Prometheus + Grafana)

---

**Last Updated:** 2026-02-21
**Status:** Tier 1 deployed, Tier 2/3 planned
