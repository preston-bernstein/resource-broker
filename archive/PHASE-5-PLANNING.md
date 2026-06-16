# Phase 5 Planning: Job Orchestration & Automation

**Date:** 2026-02-20
**Status:** Planning Only - Not Yet Implemented
**Prerequisites:** Phase 1, 2, 3 Complete

---

## Vision

Fully automated LLM orchestration system that:
- Runs batch jobs automatically without user intervention
- Intelligently decides when to use LLM vs simple scripts
- Provides comprehensive logging and visibility
- Sends email alerts on failures
- Integrates with multiple trigger sources (webhooks, schedules, home automation)

---

## Core Requirements (From User)

### 1. Zero Manual Triggering
- User should never need to manually run batch jobs
- All automation happens in background
- User only needs logs/alerts for visibility

### 2. Three Trigger Sources

**A. Email Summarization (Scheduled)**
- Runs offline (when system idle)
- Daily at optimal time (e.g., 2am)
- Uses systemd timer
- Model: llama3.1:8b or similar

**B. CI/CD Code Analysis (Webhook)**
- Triggered by GitHub/GitLab webhooks
- Runs as soon as possible after webhook received
- Reports results back to CI/CD system
- Model: codellama:7b or similar

**C. Home Automation (On-Demand)**
- Called when user asks complex questions
- Integration with existing home automation system
- Model selection: Small (llama3.2:3b) vs Large (llama3.1:8b)

### 3. Intelligence Layer: LLM vs Simple Scripts
- System should decide if LLM is actually needed
- Don't use LLM when a simple linear script would work
- User needs visibility into these decisions
- Log: "Task X solved with simple script, skipped LLM"

### 4. Comprehensive Logging & Visibility
- Centralized logs for all automation
- Track: What triggered job, which model used, result, duration
- User can review to ensure efficiency
- Identify patterns: "LLM overuse" or "missed opportunities"

### 5. Email Alerts on Failures
- Notify user when jobs fail
- Include: What failed, why, retry status
- Don't spam - intelligent alert throttling

---

## Architecture Components

### Component 1: Job Dispatcher Service
**Purpose:** Central orchestration hub

**Responsibilities:**
- Receives job requests from all sources
- Queues jobs based on priority and system state
- Decides: LLM vs simple script
- Routes to appropriate execution layer
- Tracks job status and results

**Implementation:**
- Language: Python or Bash (TBD based on complexity)
- Runs as: systemd service (user: `ollama-jobs` - dedicated system user)
- API: HTTP API for webhook integration
- Queue: Simple file-based or Redis (TBD)

**CRITICAL: System User, Not Preston**
- Preston is admin account, not always logged in
- Create dedicated system user: `ollama-jobs`
- All automation runs as this user
- No dependency on user login session

**Intelligence Algorithm (Rough):**
```
For each job request:
1. Analyze job type and complexity
2. Check if rule-based script exists
3. If script exists and matches → Use script
4. If no script or ambiguous → Use LLM
5. Log decision reasoning
```

---

### Component 2: Webhook Receiver
**Purpose:** Receive and process CI/CD webhooks

**Responsibilities:**
- Listen on HTTP port (e.g., 8080)
- Validate webhook signatures (GitHub/GitLab)
- Extract payload (commit, PR, branch)
- Send to Job Dispatcher
- Return status to CI/CD

**Implementation:**
- Simple HTTP service (Flask, FastAPI, or lightweight)
- Runs as: systemd service
- Security: Webhook secret validation, IP whitelist

**Example Flow:**
```
GitHub push → Webhook → Receiver validates →
Dispatcher queues → Batch wrapper executes →
Results posted back to GitHub
```

---

### Component 3: Scheduled Task Manager
**Purpose:** Run periodic batch jobs

**Responsibilities:**
- Email summarization (daily at 2am)
- Other scheduled tasks (weekly reports, etc.)
- Only runs when system idle (checks resource-manager state)

**Implementation:**
- systemd timers (not cron - better integration)
- Checks `/tmp/resource-manager-state` before running
- If gaming/Plex active → Skip, reschedule

**Example Timer:**
```ini
[Unit]
Description=Daily Email Summarization
Requires=ollama.service

[Timer]
OnCalendar=daily
OnCalendar=02:00
Persistent=true

[Install]
WantedBy=timers.target
```

**Example Service:**
```bash
#!/bin/bash
# Check if system idle
STATE=$(cat /tmp/resource-manager-state)
if [ "$STATE" != "idle" ]; then
  echo "System not idle, skipping"
  exit 0
fi

# Run email summarization
/usr/local/bin/job-dispatcher.sh submit \
  --type=email-summary \
  --priority=low
```

---

### Component 4: Home Automation Integration
**Purpose:** Handle complex home automation queries

**Responsibilities:**
- Called by home automation system
- Quick decision: Simple response or LLM needed?
- Route appropriately
- Return results fast

**Implementation:**
- HTTP API endpoint: `/api/automation/query`
- Fast path: Simple queries use llama3.2:3b (always loaded)
- Complex path: Dispatch to llama3.1:8b via Job Dispatcher

**Decision Tree:**
```
User query → Automation system → Integration API
  |
  ├─ Simple? (keyword match, < 20 words) → llama3.2:3b (instant)
  └─ Complex? (analysis, multi-step) → Job Dispatcher → llama3.1:8b
```

**Examples:**
- "Turn on lights" → Simple script (no LLM)
- "What lights are on?" → llama3.2:3b (quick)
- "Analyze energy usage this week" → llama3.1:8b (batch job)

---

### Component 5: Monitoring & Alerting System
**Purpose:** Visibility and failure notification

**Responsibilities:**
- Aggregate logs from all components
- Track metrics: Jobs/day, success rate, LLM vs script ratio
- Send email alerts on failures
- Optional: Web dashboard

**Implementation:**

**Logging:**
- Centralized log: `/var/log/ollama-orchestration/`
- Structure: JSON or structured text
- Rotation: logrotate

**Metrics to Track:**
```
- Total jobs submitted
- Jobs by source (webhook, schedule, automation)
- Jobs by type (email, code, automation)
- Execution method (LLM vs script)
- Success/failure rate
- Average duration
- Resource state during execution
```

**Email Alerts:**
- Tool: `sendmail`, `msmtp`, or SMTP library
- Triggers:
  - Job fails 3 times
  - Webhook receiver down
  - Dispatcher queue > 10 jobs
  - Ollama service down
- Throttling: Max 1 email per issue per hour

**Alert Format:**
```
Subject: [Ollama Orchestration] Job Failed: Email Summarization

Job: email-summary-2026-02-20
Status: Failed after 3 retries
Error: Ollama returned 500 Internal Server Error
Last attempt: 2026-02-20 02:15:30

Resource state during failure: gaming
Possible cause: Ollama throttled to 2GB during gaming

Action: Job will retry when system returns to idle state.

View logs: /var/log/ollama-orchestration/dispatcher.log
```

---

## Decision Intelligence: LLM vs Script

**Goal:** Don't waste LLM resources on tasks a simple script can handle

**Strategy:**

1. **Task Classification:**
   - Pattern match: Known simple tasks
   - Complexity analysis: Token count, keywords
   - Historical data: Did LLM add value last time?

2. **Rule Database:**
   - Maintain: `/etc/ollama-orchestration/rules.yaml`
   - Structure:
     ```yaml
     tasks:
       - pattern: "turn (on|off) .*"
         method: script
         script: /usr/local/bin/home-automation/toggle-device.sh

       - pattern: "summarize email.*"
         method: llm
         model: llama3.1:8b

       - pattern: "what is .*"
         method: llm
         model: llama3.2:3b
         max_tokens: 100
     ```

3. **Learning (Future Enhancement):**
   - Track: Did user correct/re-ask?
   - Adjust rules over time
   - Log misclassifications for review

---

## Visibility & Control

### Log Files
```
/var/log/ollama-orchestration/
├── dispatcher.log          # Job routing decisions
├── webhook-receiver.log    # Webhook events
├── scheduled-tasks.log     # Timer executions
├── automation-api.log      # Home automation calls
├── intelligence.log        # LLM vs script decisions
└── alerts.log              # Alert history
```

### Monitoring Commands
```bash
# See recent decisions
tail -f /var/log/ollama-orchestration/intelligence.log

# Job queue status
/usr/local/bin/job-dispatcher.sh status

# Metrics summary
/usr/local/bin/orchestration-stats.sh today

# Test intelligence
/usr/local/bin/orchestration-stats.sh classify "summarize my emails"
```

### Review Dashboard (Optional)
- Simple web UI showing:
  - Jobs today (success/fail)
  - LLM vs Script ratio
  - Current queue
  - Resource state
  - Recent alerts

---

## User Control & Overrides

**Config File:** `/etc/ollama-orchestration/config.yaml`

```yaml
# User preferences
intelligence:
  enabled: true  # Set false to always use LLM
  min_confidence: 0.7  # Only use script if 70%+ confident

alerts:
  email: user@example.com
  throttle_minutes: 60
  notify_on_success: false

scheduling:
  email_summary:
    enabled: true
    time: "02:00"
    model: "llama3.1:8b"

webhooks:
  github_secret: "your-secret-here"
  allowed_ips:
    - "140.82.112.0/20"  # GitHub IPs
```

**Manual Overrides:**
```bash
# Force LLM for next email summary
/usr/local/bin/job-dispatcher.sh submit \
  --type=email-summary \
  --force-llm \
  --model=llama3.1:8b

# Disable intelligence for debugging
sudo systemctl set-environment ORCH_FORCE_LLM=1
sudo systemctl restart job-dispatcher
```

---

## Implementation Phases

### Phase 5a: Job Dispatcher Foundation
- Basic dispatcher service
- Queue management
- Integration with existing batch-job-wrapper

### Phase 5b: Intelligence Layer
- Task classification
- Rule engine
- Decision logging

### Phase 5c: Webhook Integration
- Receiver service
- GitHub/GitLab integration
- CI/CD reporting

### Phase 5d: Scheduled Tasks
- systemd timers
- Email summarization
- Idle detection integration

### Phase 5e: Home Automation API
- HTTP API endpoint
- Fast/slow path routing
- Response optimization

### Phase 5f: Monitoring & Alerts
- Log aggregation
- Email alerting
- Stats/metrics

### Phase 5g: Dashboard (Optional)
- Web UI
- Real-time monitoring
- Historical analytics

---

## Dependencies & Requirements

**Required:**
- Phase 3 complete (resource manager working)
- System user: `ollama-jobs` (will create in Phase 5a or Phase 3)
- Python 3 (for dispatcher/webhook receiver)
- SMTP setup for email alerts (msmtp or similar)

**Note on User Architecture:**
- `user`: Admin account, not used for automation
- `ollama`: Runs Ollama service AND all automation/orchestration
- `root`: Runs resource manager (needs privileges for process control)
- Automation works even when user not logged in

**Decision: No separate ollama-jobs user**
- Initial plan considered `ollama-jobs` for security isolation
- Decided to use existing `ollama` user for simplicity
- Home setup doesn't require strict isolation
- Keeps architecture simpler and easier to maintain

**Optional:**
- Redis (for advanced queue management)
- PostgreSQL (for job history tracking)
- Web framework (Flask/FastAPI for dashboard)

---

## Security Considerations

1. **Webhook Validation:**
   - Verify signatures from GitHub/GitLab
   - IP whitelist
   - Rate limiting

2. **API Authentication:**
   - Home automation API should require token
   - No public exposure

3. **File Permissions:**
   - Config files: 600 (root only)
   - Scripts: 700 (root only)
   - Logs: 640 (readable by monitoring)

4. **Process Isolation:**
   - Dispatcher runs as dedicated user
   - Limited sudo privileges (only what's needed)

---

## Testing Strategy

**Unit Tests:**
- Task classification accuracy
- Queue management
- Alert throttling logic

**Integration Tests:**
- End-to-end webhook → execution → result
- Scheduled task runs at correct time
- Home automation fast/slow path routing

**Stress Tests:**
- 100 webhooks in 1 minute
- Job queue with 50+ items
- Resource state changes during execution

---

## Success Metrics

After Phase 5 implementation, measure:

1. **Automation Rate:** % of jobs running without user intervention (target: 100%)
2. **Intelligence Accuracy:** % of correct LLM vs script decisions (target: 95%+)
3. **Alert Quality:** % of alerts that required action (target: 80%+, avoid noise)
4. **Response Time:** Home automation queries < 5s (simple), < 30s (complex)
5. **System Efficiency:** LLM usage only when needed (track ratio)

---

## Open Questions (To Resolve Before Implementation)

1. **Email Setup:**
   - What email service for alerts? (SMTP server, Gmail, SendGrid?)
   - User's email address?

2. **Home Automation:**
   - What home automation system? (Home Assistant, custom, etc.)
   - Current API/integration method?

3. **CI/CD:**
   - GitHub, GitLab, or both?
   - Self-hosted or cloud?
   - What should code analysis produce? (Comments, test suggestions, etc.)

4. **Email Source:**
   - Where are emails stored? (IMAP, local maildir, etc.)
   - What should summarization include? (threads, attachments, etc.)

5. **Complexity vs Value:**
   - Start with simple bash orchestration or invest in Python framework?
   - Dashboard needed immediately or later?

---

## Next Steps

1. **Complete Phase 3** (resource manager)
2. **Test Phase 3** thoroughly
3. **User feedback:** Validate Phase 5 requirements
4. **Answer open questions** above
5. **Begin Phase 5a:** Job Dispatcher Foundation

---

**Document Version:** 1.0
**Last Updated:** 2026-02-20
**Status:** Planning - Awaiting Phase 3 Completion
