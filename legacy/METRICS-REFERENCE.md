# Metrics Metadata Reference

## Complete Metadata Captured

### Who (Identity & Source)

```json
{
  "submitted_by": "user",           // Linux user who ran the job
  "caller": "email-summarizer",        // Application/script that called wrapper
  "hostname": "gpu-host",            // Which machine
  "invocation_method": "cron",         // How it was invoked
  "remote_ip": "lan.example.internal",       // If SSH (null otherwise)
  "parent_pid": 12345,                // PID of calling script
  "wrapper_pid": 12346                // PID of batch wrapper
}
```

**Invocation methods detected:**
- `interactive` - Manually run in terminal
- `ssh` - Run via SSH (includes remote_ip)
- `cron-or-service` - Cron job or systemd service
- `systemd` - Directly from systemd unit
- `pipe-or-script` - Called from another script
- `unknown` - Couldn't determine

---

### What (Job Details)

```json
{
  "job_id": "email-summary-1234567890",   // Unique job identifier
  "caller": "email-summarizer",           // Calling application
  "tags": ["email", "daily", "batch"],    // Custom categorization tags
  "model": "llama3.1:8b",                 // Ollama model used
  "command": "ollama run llama3.1:8b..." // Full command executed
}
```

---

### When (Timing)

```json
{
  "submitted_at": "2026-02-21T18:00:00-05:00",    // ISO 8601 timestamp
  "submitted_timestamp": 1708556400,              // Unix timestamp
  "started_at": "2026-02-21T18:00:05-05:00",
  "started_timestamp": 1708556405,
  "completed_at": "2026-02-21T18:01:30-05:00",
  "completed_timestamp": 1708556490,
  "duration_seconds": 85                          // Total execution time
}
```

---

### How (Execution & Results)

```json
{
  "status": "completed",           // pending, queued, running, completed, failed
  "queue_reason": "gaming",        // If queued: gaming, plex (null if run immediately)
  "queued_at": "...",             // When it was queued (if applicable)
  "exit_code": 0,                 // Command exit code (0 = success)
  "output_size_bytes": 4521       // Size of command output
}
```

---

## Example Scenarios

### Scenario 1: Manual SSH Execution

```bash
# User user SSHs from laptop and runs:
ssh user@gpu-host
./home-automation.sh "Turn on lights"
```

**Metadata captured:**
```json
{
  "caller": "home-automation",
  "submitted_by": "user",
  "hostname": "gpu-host",
  "invocation_method": "ssh",
  "remote_ip": "lan.example.internal",
  "tags": ["automation", "home", "realtime"]
}
```

---

### Scenario 2: Cron Job

```bash
# Crontab entry:
# 0 7 * * * /usr/local/lib/ollama-batch-examples/email-summarizer.sh /var/mail/inbox
```

**Metadata captured:**
```json
{
  "caller": "email-summarizer",
  "submitted_by": "user",
  "hostname": "gpu-host",
  "invocation_method": "cron-or-service",
  "remote_ip": null,
  "tags": ["email", "daily", "batch"]
}
```

---

### Scenario 3: Webhook (HTTP → Script)

```bash
# GitHub webhook calls:
# curl -X POST http://gpu-host:8000/webhook -d '{"pr": 123}'
# Server script runs:
./code-reviewer.sh 123
```

**Metadata captured:**
```json
{
  "caller": "code-reviewer",
  "submitted_by": "www-data",        // Web server user
  "hostname": "gpu-host",
  "invocation_method": "pipe-or-script",
  "remote_ip": null,
  "tags": ["cicd", "code-review", "pr-123"]
}
```

**Note:** For webhooks, consider adding custom metadata in your webhook handler:
```bash
#!/bin/bash
# webhook-handler.sh

# Extract requester info from webhook
WEBHOOK_IP=$(echo "$HTTP_X_FORWARDED_FOR" | cut -d',' -f1)

batch-job-wrapper.sh \
  --caller "code-reviewer" \
  --tags "webhook,github,pr-$PR_NUMBER,ip-$WEBHOOK_IP" \
  --command "..."
```

---

### Scenario 4: Interactive Terminal

```bash
# User runs directly in terminal:
user@gpu-host:~$ ./email-summarizer.sh /tmp/emails.txt
```

**Metadata captured:**
```json
{
  "caller": "email-summarizer",
  "submitted_by": "user",
  "hostname": "gpu-host",
  "invocation_method": "interactive",
  "remote_ip": null,
  "tags": ["email", "daily", "batch"]
}
```

---

## Querying Specific Metadata

### Find all SSH jobs

```bash
jq -r 'select(.invocation_method == "ssh") | "\(.submitted_at) \(.caller) from \(.remote_ip)"' \
  /var/lib/batch-jobs/metrics/*.json
```

### Find all cron jobs

```bash
jq -r 'select(.invocation_method == "cron-or-service") | .job_id' \
  /var/lib/batch-jobs/metrics/*.json
```

### Group by remote IP

```bash
jq -r 'select(.remote_ip != null) | .remote_ip' \
  /var/lib/batch-jobs/metrics/*.json | sort | uniq -c | sort -rn
```

### Find jobs by specific user

```bash
jq -r 'select(.submitted_by == "user") | "\(.job_id) \(.caller)"' \
  /var/lib/batch-jobs/metrics/*.json
```

### Track per-application success rates

```bash
for caller in $(jq -r '.caller' /var/lib/batch-jobs/metrics/*.json | sort -u); do
  total=$(jq -r "select(.caller == \"$caller\") | .job_id" \
    /var/lib/batch-jobs/metrics/*.json | wc -l)
  success=$(jq -r "select(.caller == \"$caller\" and .status == \"completed\") | .job_id" \
    /var/lib/batch-jobs/metrics/*.json | wc -l)
  rate=$(echo "scale=2; $success * 100 / $total" | bc)
  echo "$caller: $rate% ($success/$total)"
done
```

---

## Custom Tagging Strategy

**Recommended tags for tracking:**

### By Source
- `manual` - User manually invoked
- `cron` - Scheduled via cron
- `webhook` - External HTTP trigger
- `systemd` - System service

### By Priority
- `priority-critical` - Must complete ASAP
- `priority-high` - Important
- `priority-normal` - Standard
- `priority-low` - Background processing

### By Data Source
- `user-user` - Specific user's data
- `team-engineering` - Team-specific
- `external-api` - Data from external API

### By Purpose
- `realtime` - User waiting for response
- `batch` - Background processing
- `scheduled` - Regular scheduled task

### By Environment
- `prod` - Production
- `dev` - Development
- `test` - Testing

---

## Audit Trail Example

**Question:** "Who processed emails on Feb 21st?"

```bash
jq -r 'select(.submitted_at | startswith("2026-02-21")) |
       select(.caller == "email-summarizer") |
       "\(.submitted_at) - \(.submitted_by) via \(.invocation_method) - \(.status)"' \
  /var/lib/batch-jobs/metrics/*.json

# Output:
# 2026-02-21T07:00:00-05:00 - user via cron-or-service - completed
# 2026-02-21T14:30:00-05:00 - user via ssh (lan.example.internal) - completed
```

---

## Security Considerations

**What gets logged:**
- ✅ User identity (username)
- ✅ Source IP (if SSH)
- ✅ Timestamp (audit trail)
- ✅ Command executed (full command with args)

**Privacy notes:**
- Prompts may contain sensitive data (emails, code, etc.)
- Output is saved to `/var/lib/batch-jobs/metrics/<job-id>.output`
- Ensure proper file permissions: `chmod 600` on sensitive metrics
- Consider encrypting metrics directory if needed
- Rotate/archive old metrics regularly

**Recommended:**
```bash
# Restrict metrics access
sudo chown -R user:user /var/lib/batch-jobs/metrics
sudo chmod 700 /var/lib/batch-jobs/metrics
```

---

## Integration with External Systems

### Webhook Receiver Example

```python
from flask import Flask, request
import subprocess

app = Flask(__name__)

@app.route('/webhook/email', methods=['POST'])
def email_webhook():
    # Extract metadata from request
    source_ip = request.headers.get('X-Forwarded-For', request.remote_addr)
    user_agent = request.headers.get('User-Agent', 'unknown')

    # Run batch job with rich metadata
    subprocess.run([
        'batch-job-wrapper.sh',
        '--caller', 'email-summarizer',
        '--tags', f'webhook,external,ip-{source_ip}',
        '--command', 'ollama run llama3.1:8b "Process emails..."'
    ])

    return {'status': 'queued'}
```

---

**Complete metadata fields:**
- job_id, caller, tags, model, command
- submitted_at, submitted_timestamp
- started_at, started_timestamp
- completed_at, completed_timestamp
- duration_seconds
- submitted_by, hostname
- invocation_method, remote_ip
- parent_pid, wrapper_pid
- status, queue_reason, queued_at
- exit_code, output_size_bytes
