# Tier 3 Implementation Plan: Production-Grade Batch System

**Date:** 2026-02-21
**Status:** Planning
**Builds on:** Tier 2 (configurable preemption)

---

## Overview

Tier 3 transforms the batch system into a **production-grade job orchestration platform** with checkpointing, priority queues, persistence, and monitoring.

**Key Features:**
1. **Checkpointing** - Long jobs save progress, resume after interruption
2. **Priority queues** - High-priority jobs preempt low-priority jobs
3. **Persistent job storage** - Survives reboots (SQLite database)
4. **Web UI** - Monitor and control jobs via browser
5. **Resource quotas** - Per-user limits on concurrent jobs
6. **Job dependencies** - "Run job B after job A completes"
7. **Scheduled jobs** - Cron-like scheduling integrated

**Complexity:** ~1500 lines total (5x Tier 2)

---

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                     Web UI (Port 8080)                  │
│  - Job submission form                                  │
│  - Real-time status dashboard                           │
│  - GPU/CPU utilization graphs                           │
└─────────────────────┬───────────────────────────────────┘
                      │
┌─────────────────────▼───────────────────────────────────┐
│              Batch Orchestrator Service                 │
│  - Job queue management                                 │
│  - Priority scheduling                                  │
│  - Checkpoint coordination                              │
│  - Dependency resolution                                │
└─────────────────────┬───────────────────────────────────┘
                      │
        ┌─────────────┼─────────────┐
        │             │             │
┌───────▼──────┐ ┌────▼─────┐ ┌────▼──────────┐
│ Job Runner 1 │ │ Runner 2 │ │ Runner N      │
│ (Worker)     │ │ (Worker) │ │ (Worker)      │
└──────────────┘ └──────────┘ └───────────────┘
        │
┌───────▼──────────────────────────────────────┐
│         SQLite Job Database                  │
│  - jobs table (id, status, priority, etc)    │
│  - checkpoints table (job_id, state)         │
│  - dependencies table (job_a, job_b)         │
└──────────────────────────────────────────────┘
```

---

## Feature 1: Checkpointing

### Problem
Long-running jobs (e.g., processing 1000 documents) lose all progress if interrupted during gaming.

### Solution
Jobs periodically save state. When resumed, skip already-processed items.

### Implementation

**Checkpoint file format:**
```json
{
  "job_id": "doc-processing-12345",
  "total_items": 1000,
  "processed_items": 347,
  "last_checkpoint": "2026-02-21T18:45:30Z",
  "partial_results": [
    {"doc_id": 1, "summary": "..."},
    {"doc_id": 2, "summary": "..."}
  ]
}
```

**Batch wrapper checkpoint API:**
```bash
# Job script with checkpointing
batch-job-wrapper.sh --checkpoint --job-id "doc-proc" --model llama3.1:70b << 'SCRIPT'
#!/bin/bash
# Process 1000 documents with checkpointing

CHECKPOINT_FILE="/var/lib/batch-jobs/checkpoints/doc-proc.json"

# Resume from checkpoint if exists
START_INDEX=0
if [ -f "$CHECKPOINT_FILE" ]; then
  START_INDEX=$(jq -r '.processed_items' "$CHECKPOINT_FILE")
  echo "Resuming from item $START_INDEX"
fi

# Process documents
for i in $(seq $START_INDEX 999); do
  # Process document
  RESULT=$(ollama run llama3.1:70b "Summarize document $i")

  # Save checkpoint every 10 items
  if [ $((i % 10)) -eq 0 ]; then
    jq -n \
      --arg job_id "doc-proc" \
      --argjson total 1000 \
      --argjson processed $i \
      --arg timestamp "$(date -Iseconds)" \
      '{job_id: $job_id, total_items: $total, processed_items: $processed, last_checkpoint: $timestamp}' \
      > "$CHECKPOINT_FILE"
  fi
done

# Cleanup checkpoint on completion
rm -f "$CHECKPOINT_FILE"
SCRIPT
```

**Checkpoint manager service:**
- Monitors running jobs
- Forces checkpoint before killing job
- Sends SIGUSR1 to job → job saves state → job killed gracefully

**Storage:**
- `/var/lib/batch-jobs/checkpoints/` (persistent across reboots)
- Automatic cleanup after job completion

**Estimated code:** 200 lines (checkpoint manager service)

---

## Feature 2: Priority Queues

### Problem
All jobs treated equally. High-priority jobs (e.g., user-facing) wait behind low-priority batch jobs.

### Solution
Jobs have numeric priority (0-100). Scheduler runs highest-priority jobs first.

### Implementation

**Job priorities:**
- **100 (Critical):** User-facing requests (home automation, chat)
- **75 (High):** Time-sensitive automation (alerts, notifications)
- **50 (Normal):** Regular batch jobs (email summarization)
- **25 (Low):** Background processing (log analysis)
- **0 (Idle):** Only run when GPU completely idle

**Preemption rules:**
1. Priority 100 job → kills priority <100 jobs
2. Priority 75 job → kills priority <75 jobs
3. Same priority → first-come-first-served

**Usage:**
```bash
# Critical user request (preempts everything)
batch-job-wrapper.sh --priority 100 --model llama3.2:3b "Turn on lights"

# Normal batch job
batch-job-wrapper.sh --priority 50 --checkpoint --model llama3.1:70b "Process emails"

# Low-priority background job
batch-job-wrapper.sh --priority 25 --model deepseek:33b "Analyze logs"
```

**Scheduler logic:**
```python
def schedule_next_job():
    # Get all queued jobs ordered by priority
    jobs = db.query("SELECT * FROM jobs WHERE status='queued' ORDER BY priority DESC, created_at ASC")

    if not jobs:
        return None

    next_job = jobs[0]

    # Check if we should preempt running jobs
    running_jobs = db.query("SELECT * FROM jobs WHERE status='running'")
    for running in running_jobs:
        if next_job.priority > running.priority + 10:  # Hysteresis
            # Preempt lower-priority job
            kill_job(running.id, checkpoint=True)

    # Check resources available
    if gpu_usage() < 50 or next_job.priority >= 100:
        start_job(next_job.id)
        return next_job

    return None
```

**Estimated code:** 300 lines (priority scheduler)

---

## Feature 3: Persistent Job Storage

### Problem
Job metadata in /tmp lost on reboot. No historical job data.

### Solution
SQLite database with full job lifecycle tracking.

**Database schema:**
```sql
CREATE TABLE jobs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id TEXT UNIQUE NOT NULL,
  user TEXT NOT NULL,
  priority INTEGER DEFAULT 50,
  preemption TEXT DEFAULT 'preempt',  -- preempt, no-preempt, queue
  model TEXT NOT NULL,
  prompt TEXT,
  status TEXT DEFAULT 'queued',  -- queued, running, completed, failed, killed
  exit_code INTEGER,
  created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  started_at TIMESTAMP,
  completed_at TIMESTAMP,
  retries INTEGER DEFAULT 0,
  max_retries INTEGER DEFAULT 3,
  checkpoint_path TEXT,
  result_path TEXT,
  pid INTEGER
);

CREATE TABLE checkpoints (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id TEXT NOT NULL,
  checkpoint_data TEXT,  -- JSON blob
  created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY(job_id) REFERENCES jobs(job_id)
);

CREATE TABLE dependencies (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  parent_job_id TEXT NOT NULL,
  child_job_id TEXT NOT NULL,
  dependency_type TEXT DEFAULT 'after_completion',  -- after_completion, after_success
  FOREIGN KEY(parent_job_id) REFERENCES jobs(job_id),
  FOREIGN KEY(child_job_id) REFERENCES jobs(job_id)
);

CREATE TABLE resource_quotas (
  user TEXT PRIMARY KEY,
  max_concurrent_jobs INTEGER DEFAULT 5,
  max_gpu_jobs INTEGER DEFAULT 2,
  priority_bonus INTEGER DEFAULT 0
);
```

**Database operations:**
```bash
# Submit job
batch-submit --priority 75 --model llama3.1:70b "Analyze data"
# Output: Job ID: job-1234567890

# Query job status
batch-query job-1234567890
# Output: Status: running, Started: 2m ago, Progress: 45%

# List all user jobs
batch-list --user user --status running

# Cancel job
batch-cancel job-1234567890

# View job history
batch-history --days 7
```

**Database location:** `/var/lib/batch-jobs/jobs.db`

**Estimated code:** 400 lines (database layer, CLI tools)

---

## Feature 4: Web UI

### Problem
CLI-only interface not user-friendly. No real-time monitoring.

### Solution
Web-based dashboard with real-time job status, GPU monitoring, and job submission.

**Tech stack:**
- **Backend:** FastAPI (Python)
- **Frontend:** HTML + Alpine.js (lightweight, no build step)
- **Real-time:** WebSocket for live updates
- **Deployment:** systemd service on port 8080

**Pages:**

**1. Dashboard (`/`)**
```
┌──────────────────────────────────────────────────────────┐
│  Batch Job Orchestrator                     [user ▼]  │
├──────────────────────────────────────────────────────────┤
│  GPU: [████████░░] 80%  45°C  95W                        │
│  CPU: [████░░░░░░] 40%                                    │
│  Jobs: 3 running, 7 queued, 245 completed                │
├──────────────────────────────────────────────────────────┤
│  RUNNING JOBS                                   [Kill All]│
│  ┌────────────────────────────────────────────────────┐  │
│  │ email-summary-123         Priority: 50            │  │
│  │ Model: llama3.1:70b       Progress: 45% ████░░░   │  │
│  │ Started: 5m ago           [View] [Kill]           │  │
│  ├────────────────────────────────────────────────────┤  │
│  │ code-review-456           Priority: 75            │  │
│  │ Model: deepseek:33b       Progress: 12% ██░░░░░   │  │
│  │ Started: 1m ago           [View] [Kill]           │  │
│  └────────────────────────────────────────────────────┘  │
│                                                           │
│  QUEUED JOBS (7)                              [Clear All]│
│  ┌────────────────────────────────────────────────────┐  │
│  │ doc-processing-789        Priority: 25            │  │
│  │ Model: llama3.1:70b       Waiting for GPU        │  │
│  │                           [Start Now] [Cancel]    │  │
│  └────────────────────────────────────────────────────┘  │
│                                                 [New Job] │
└──────────────────────────────────────────────────────────┘
```

**2. Job Submission (`/submit`)**
```
┌──────────────────────────────────────────────────────────┐
│  Submit New Job                                           │
├──────────────────────────────────────────────────────────┤
│  Job ID: [auto-generated-123456      ]                   │
│  Model:  [llama3.1:70b           ▼]                      │
│  Priority: ●───○────────────────── (50 - Normal)         │
│  Preemption: ⦿ Preemptible  ○ Non-preempt  ○ Queue      │
│  Checkpoint: ☑ Enable checkpointing                      │
│                                                           │
│  Prompt:                                                  │
│  ┌────────────────────────────────────────────────────┐  │
│  │ Summarize the following documents...              │  │
│  │                                                    │  │
│  │                                                    │  │
│  └────────────────────────────────────────────────────┘  │
│                                        [Submit] [Clear]   │
└──────────────────────────────────────────────────────────┘
```

**3. History (`/history`)**
```
┌──────────────────────────────────────────────────────────┐
│  Job History          [Last 7 days ▼] [Export CSV]       │
├──────────────────────────────────────────────────────────┤
│  JOB ID            STATUS     DURATION   COMPLETED        │
│  email-sum-123     ✓ Success  2m 34s    5 hours ago     │
│  code-rev-456      ✓ Success  45s       3 hours ago     │
│  doc-proc-789      ✗ Killed   1m 12s    2 hours ago     │
│  log-analysis-012  ✓ Success  15m 6s    1 hour ago      │
│                                                           │
│  Total: 245 jobs, 89% success rate, Avg duration: 3m 22s │
└──────────────────────────────────────────────────────────┘
```

**Backend API:**
```python
from fastapi import FastAPI, WebSocket
import sqlite3

app = FastAPI()

@app.get("/api/jobs")
def list_jobs(status: str = None):
    """List all jobs, optionally filtered by status"""
    # Query database

@app.post("/api/jobs")
def submit_job(job: JobRequest):
    """Submit new job"""
    # Insert into database, notify scheduler

@app.delete("/api/jobs/{job_id}")
def cancel_job(job_id: str):
    """Cancel running or queued job"""

@app.websocket("/ws/status")
async def websocket_status(websocket: WebSocket):
    """Real-time job status updates"""
    while True:
        status = get_current_status()
        await websocket.send_json(status)
        await asyncio.sleep(1)
```

**Deployment:**
```bash
# Install dependencies
pip install fastapi uvicorn websockets

# systemd service
[Unit]
Description=Batch Job Web UI
After=network.target

[Service]
ExecStart=/usr/bin/uvicorn batch_ui:app --host 0.0.0.0 --port 8080
WorkingDirectory=/usr/local/lib/batch-jobs
Restart=always

[Install]
WantedBy=multi-user.target
```

**Access:** `http://localhost:8080` or `http://gpu-host.local:8080`

**Estimated code:** 600 lines (backend + frontend)

---

## Feature 5: Resource Quotas

### Problem
One user submits 50 jobs, starves other users.

### Solution
Per-user limits on concurrent jobs and GPU usage.

**Configuration:**
```sql
INSERT INTO resource_quotas VALUES
  ('user', 10, 3, 0),     -- max 10 jobs, 3 GPU jobs
  ('family', 5, 1, -10),     -- max 5 jobs, 1 GPU job, -10 priority
  ('automation', 20, 5, 5);  -- max 20 jobs, 5 GPU jobs, +5 priority
```

**Enforcement:**
```python
def can_start_job(user, job):
    quota = db.get_quota(user)
    current_jobs = db.count_jobs(user, status='running')
    current_gpu_jobs = db.count_jobs(user, status='running', uses_gpu=True)

    if current_jobs >= quota.max_concurrent_jobs:
        return False, "Quota exceeded: max concurrent jobs"

    if job.uses_gpu and current_gpu_jobs >= quota.max_gpu_jobs:
        return False, "Quota exceeded: max GPU jobs"

    return True, None
```

**Estimated code:** 100 lines

---

## Feature 6: Job Dependencies

### Problem
Want to run job B only after job A completes successfully.

### Solution
Dependency graph with automatic triggering.

**Usage:**
```bash
# Submit parent job
PARENT_ID=$(batch-submit --model llama3.1:70b "Analyze dataset" --output)

# Submit child job that runs after parent succeeds
batch-submit --model llama3.2:3b "Send summary email" \
  --depends-on $PARENT_ID \
  --depends-type after_success
```

**Dependency types:**
- `after_completion` - Run after parent finishes (success or failure)
- `after_success` - Run only if parent succeeds
- `after_failure` - Run only if parent fails

**Scheduler logic:**
```python
def on_job_completed(job_id, status):
    # Find dependent jobs
    deps = db.query("SELECT * FROM dependencies WHERE parent_job_id = ?", job_id)

    for dep in deps:
        if dep.dependency_type == 'after_completion':
            queue_job(dep.child_job_id)
        elif dep.dependency_type == 'after_success' and status == 'completed':
            queue_job(dep.child_job_id)
        elif dep.dependency_type == 'after_failure' and status == 'failed':
            queue_job(dep.child_job_id)
```

**Estimated code:** 150 lines

---

## Feature 7: Scheduled Jobs

### Problem
Want to run batch job daily at 3am when GPU guaranteed idle.

### Solution
Cron-like scheduling integrated into batch system.

**Usage:**
```bash
# Schedule email summary every morning at 7am
batch-schedule --cron "0 7 * * *" \
  --model llama3.1:70b \
  --priority 50 \
  --job-template "email-summary-daily" \
  "Summarize emails from last 24 hours"

# Schedule log analysis every hour (low priority, only when idle)
batch-schedule --cron "0 * * * *" \
  --model llama3.2:3b \
  --priority 0 \
  --job-template "log-analysis-hourly" \
  "Analyze system logs"
```

**Scheduler daemon:**
- Runs every minute
- Checks cron expressions
- Creates job instances from templates
- Tracks last run time (avoid duplicates)

**Database:**
```sql
CREATE TABLE scheduled_jobs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  template_name TEXT UNIQUE NOT NULL,
  cron_expression TEXT NOT NULL,
  model TEXT NOT NULL,
  prompt TEXT NOT NULL,
  priority INTEGER DEFAULT 50,
  enabled BOOLEAN DEFAULT 1,
  last_run TIMESTAMP,
  next_run TIMESTAMP
);
```

**Estimated code:** 200 lines (cron parser + scheduler)

---

## Component Summary

| Component | Lines of Code | Language | Purpose |
|-----------|--------------|----------|---------|
| Checkpoint Manager | 200 | Bash | Save/restore job progress |
| Priority Scheduler | 300 | Python | Queue management, preemption |
| Database Layer | 400 | Python | SQLite abstraction, CLI tools |
| Web UI Backend | 400 | Python | FastAPI REST API |
| Web UI Frontend | 200 | HTML/JS | Dashboard, job submission |
| Resource Quotas | 100 | Python | Per-user limits |
| Job Dependencies | 150 | Python | Dependency graph |
| Scheduled Jobs | 200 | Python | Cron integration |
| **Total** | **1950** | **Mixed** | **Full orchestration platform** |

---

## Migration from Tier 2 to Tier 3

**Phase 1: Database Migration**
1. Install dependencies: `pip install fastapi uvicorn sqlite3`
2. Create database: `batch-init-db`
3. Import existing jobs from /tmp metadata

**Phase 2: Deploy Scheduler**
1. Deploy orchestrator service
2. Migrate resource-manager to use database
3. Test basic job submission via database

**Phase 3: Deploy Web UI**
1. Deploy FastAPI backend
2. Deploy frontend HTML/JS
3. Configure nginx reverse proxy (optional)

**Phase 4: Enable Advanced Features**
1. Enable checkpointing
2. Configure quotas
3. Set up scheduled jobs
4. Test dependencies

**Backward compatibility:** CLI tools maintain same interface, backend changes transparent.

---

## Deployment Architecture

```
/var/lib/batch-jobs/
├── jobs.db                    # SQLite database
├── checkpoints/               # Checkpoint files
│   ├── job-12345.json
│   └── job-67890.json
├── results/                   # Job output files
│   ├── job-12345.txt
│   └── job-67890.txt
└── logs/                      # Per-job logs
    ├── job-12345.log
    └── job-67890.log

/usr/local/lib/batch-jobs/
├── orchestrator.py            # Main scheduler service
├── checkpoint_manager.py      # Checkpoint coordination
├── web_ui.py                  # FastAPI app
├── static/                    # Web UI assets
│   ├── index.html
│   ├── style.css
│   └── app.js
└── requirements.txt           # Python dependencies

/usr/local/bin/
├── batch-submit               # CLI: Submit job
├── batch-query                # CLI: Query job status
├── batch-cancel               # CLI: Cancel job
├── batch-list                 # CLI: List jobs
├── batch-history              # CLI: Job history
├── batch-schedule             # CLI: Schedule recurring job
└── batch-init-db              # CLI: Initialize database

/etc/systemd/system/
├── batch-orchestrator.service  # Main scheduler
└── batch-web-ui.service        # Web UI
```

---

## Testing Plan

### Test 1: Checkpoint Recovery
```bash
# Start long job
JOB_ID=$(batch-submit --checkpoint --model llama3.1:70b "Process 1000 docs")

# Wait for some progress
sleep 60

# Kill job mid-execution
batch-cancel $JOB_ID

# Restart job
batch-submit --resume $JOB_ID

# Verify: Skips already-processed items
```

### Test 2: Priority Preemption
```bash
# Start low-priority job
LOW=$(batch-submit --priority 25 --model llama3.1:70b "Background task")

# Submit high-priority job
HIGH=$(batch-submit --priority 100 --model llama3.2:3b "Urgent request")

# Verify: Low-priority job killed, high-priority starts immediately
```

### Test 3: Resource Quotas
```bash
# Submit 11 jobs as user with quota of 10
for i in {1..11}; do
  batch-submit --user user --model llama3.2:3b "Task $i"
done

# Verify: Job 11 queued with "Quota exceeded" message
```

### Test 4: Job Dependencies
```bash
# Submit parent
PARENT=$(batch-submit --model llama3.1:70b "Generate report")

# Submit child (depends on parent)
CHILD=$(batch-submit --depends-on $PARENT --model llama3.2:3b "Email report")

# Verify: Child doesn't start until parent completes
```

### Test 5: Scheduled Jobs
```bash
# Schedule job for 2 minutes from now
batch-schedule --cron "$(date -d '+2 minutes' '+%M %H * * *')" \
  --model llama3.2:3b "Test scheduled job"

# Wait 2 minutes
sleep 120

# Verify: Job auto-started
batch-list --status running
```

---

## Security Considerations

1. **Web UI Authentication:**
   - Add basic auth or SSO integration
   - HTTPS via nginx reverse proxy

2. **Input Sanitization:**
   - Sanitize all job prompts (prevent injection)
   - Validate cron expressions

3. **Resource Limits:**
   - Enforce quotas strictly
   - Rate-limit job submissions

4. **File Permissions:**
   - /var/lib/batch-jobs/ restricted to batch-jobs user
   - Checkpoint files not world-readable

---

## Monitoring & Observability

**Metrics to track:**
- Jobs per hour (throughput)
- Average job duration
- Success rate
- GPU utilization over time
- Queue depth

**Integration:**
- Prometheus exporter for metrics
- Grafana dashboard for visualization
- Email/Slack alerts for failures

**Example Grafana dashboard:**
```
┌──────────────────────────────────────────────────────────┐
│  Batch Job System - Last 24 Hours                        │
├──────────────────────────────────────────────────────────┤
│  Jobs Completed: 347 (↑ 15%)      Success Rate: 94.2%   │
│  Avg Duration: 2m 45s             GPU Uptime: 87%       │
│                                                           │
│  [Job Throughput Graph]                                  │
│  [GPU Utilization Graph]                                 │
│  [Queue Depth Graph]                                     │
└──────────────────────────────────────────────────────────┘
```

---

## Known Limitations

1. **Single-node only** - No distributed scheduling (future: Kubernetes)
2. **SQLite scalability** - Works up to ~10k jobs (future: PostgreSQL)
3. **No job sandboxing** - Jobs run with same permissions (future: containers)
4. **Limited LLM integration** - Only Ollama (future: OpenAI, Anthropic APIs)

---

## Estimated Effort

**Development:** 40-60 hours
**Testing:** 10 hours
**Documentation:** 5 hours
**Deployment:** 5 hours
**Total:** 60-80 hours (~2 weeks full-time)

---

## Success Criteria

- [ ] Database stores all jobs persistently
- [ ] Web UI accessible at http://localhost:8080
- [ ] Can submit jobs via CLI and web UI
- [ ] Checkpointing works (job resumes after kill)
- [ ] Priority preemption works (high kills low)
- [ ] Resource quotas enforced
- [ ] Job dependencies work
- [ ] Scheduled jobs trigger automatically
- [ ] All Tier 2 features still work
- [ ] System survives reboot (jobs persist)

---

**Status:** Planning complete, ready for implementation when needed.
