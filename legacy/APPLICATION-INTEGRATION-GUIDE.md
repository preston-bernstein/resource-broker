# Application Integration Guide

**How to integrate your applications with the Ollama batch system**

---

## Architecture Overview

```
Your Application
├─ Business Logic (decides which model to use)
├─ Calls: batch-job-wrapper.sh --command "ollama run <model> <prompt>"
└─ Wrapper handles queueing (if gaming/Plex active)
```

**Key principle:** The wrapper is GENERIC. Your application decides the model.

---

## Quick Start

### 1. Basic Integration

```bash
#!/bin/bash
# my-app.sh

# Your app decides the model based on task
MODEL="llama3.2:3b"  # Fast for simple tasks

# Call the generic wrapper
batch-job-wrapper.sh \
  --job-id "my-app-$(date +%s)" \
  --command "ollama run $MODEL 'Your prompt here'"
```

### 2. Model Selection Logic

```bash
#!/bin/bash
# smart-app.sh

TASK_COMPLEXITY="$1"  # simple, medium, complex
PROMPT="$2"

# Business logic: Choose model based on complexity
case "$TASK_COMPLEXITY" in
  simple)
    MODEL="llama3.2:3b"     # 2-3 seconds
    ;;
  medium)
    MODEL="llama3.1:8b"     # 30 seconds
    ;;
  complex)
    MODEL="llama3.1:70b"    # 5-10 minutes
    ;;
esac

# Call wrapper with chosen model
batch-job-wrapper.sh \
  --job-id "smart-$(date +%s)" \
  --command "ollama run $MODEL '$PROMPT'"
```

---

## Example Applications

All examples are in `/usr/local/lib/ollama-batch-examples/`

### Home Automation (Fast Response)

**File:** `home-automation.sh`

```bash
#!/bin/bash
# Needs instant responses - use smallest model

MODEL="llama3.2:3b"
PROMPT="$1"

batch-job-wrapper.sh \
  --job-id "home-auto-$(date +%s)" \
  --command "ollama run $MODEL '$PROMPT'"
```

**Usage:**
```bash
./home-automation.sh "Turn on living room lights"
./home-automation.sh "Set temperature to 72°F"
```

**Why llama3.2:3b:** 2-3 second responses, good enough for commands

---

### Email Summarization (Quality/Speed Balance)

**File:** `email-summarizer.sh`

```bash
#!/bin/bash
# Needs good quality but reasonable speed - use medium model

MODEL="llama3.1:8b"
EMAIL_FILE="$1"
EMAILS=$(cat "$EMAIL_FILE")

PROMPT="Summarize these emails:
$EMAILS"

batch-job-wrapper.sh \
  --job-id "email-$(date +%s)" \
  --command "ollama run $MODEL '$PROMPT'"
```

**Usage:**
```bash
./email-summarizer.sh /path/to/emails.txt
```

**Why llama3.1:8b:** 30 second responses, excellent quality

---

### Code Review (Specialized Task)

**File:** `code-reviewer.sh`

```bash
#!/bin/bash
# Code analysis needs specialized model

MODEL="deepseek-coder:33b"
PR_NUMBER="$1"
# Fetch PR diff from GitHub API
PR_DIFF="<your code to fetch diff>"

PROMPT="Review this PR for security and quality:
$PR_DIFF"

batch-job-wrapper.sh \
  --job-id "pr-review-$PR_NUMBER" \
  --command "ollama run $MODEL '$PROMPT'"
```

**Usage:**
```bash
./code-reviewer.sh 123  # Review PR #123
```

**Why deepseek-coder:33b:** Trained on code, best for code tasks

---

### Research Analysis (Maximum Quality)

**File:** `research-analyzer.sh`

```bash
#!/bin/bash
# Complex analysis needs best model - worth the wait

MODEL="llama3.1:70b"
PAPERS_DIR="$1"
# Collect papers
PAPERS="<your code to read papers>"

PROMPT="Analyze these research papers:
$PAPERS"

batch-job-wrapper.sh \
  --job-id "research-$(date +%s)" \
  --command "ollama run $MODEL '$PROMPT'"
```

**Usage:**
```bash
./research-analyzer.sh /path/to/papers/
```

**Why llama3.1:70b:** 5-10 minutes, but maximum quality

---

## Advanced Integration Patterns

### 1. Daemon Service (Continuous Processing)

```bash
#!/bin/bash
# email-daemon.sh - Runs continuously, processes email queue

while true; do
  # Check if new emails exist
  if [ -f /var/spool/emails/new/*.eml ]; then
    for email in /var/spool/emails/new/*.eml; do
      # Process each email
      /usr/local/lib/ollama-batch-examples/email-summarizer.sh "$email"

      # Move to processed
      mv "$email" /var/spool/emails/processed/
    done
  fi

  # Wait before checking again
  sleep 60
done
```

### 2. Webhook Handler (CI/CD Integration)

```bash
#!/bin/bash
# github-webhook-handler.sh - Called by GitHub webhook

# Parse webhook payload
PR_NUMBER=$(echo "$WEBHOOK_PAYLOAD" | jq -r '.pull_request.number')

# Trigger code review
/usr/local/lib/ollama-batch-examples/code-reviewer.sh "$PR_NUMBER"

# Return immediately (review happens in background)
echo "Code review queued for PR #$PR_NUMBER"
```

### 3. Scheduled Batch Jobs (Cron)

```bash
# crontab -e

# Daily email summary at 7am
0 7 * * * /usr/local/lib/ollama-batch-examples/email-summarizer.sh /var/mail/inbox.txt > /var/log/email-summary.log 2>&1

# Weekly research digest on Sundays
0 9 * * 0 /usr/local/lib/ollama-batch-examples/research-analyzer.sh /data/papers/this-week/ > /var/log/research-digest.log 2>&1
```

---

## Model Selection Guidelines

| Task Type | Model | Speed | Quality | Use When |
|-----------|-------|-------|---------|----------|
| Commands | llama3.2:3b | ⚡⚡⚡ | ⭐⭐ | Speed critical |
| General | llama3.1:8b | ⚡⚡ | ⭐⭐⭐ | Most tasks |
| Code | deepseek-coder:33b | ⚡ | ⭐⭐⭐⭐ | Code-specific |
| Research | llama3.1:70b | 🐌 | ⭐⭐⭐⭐⭐ | Quality critical |

---

## Handling Responses

### Synchronous (Wait for Response)

```bash
#!/bin/bash
# Wait for response and use it

RESPONSE=$(batch-job-wrapper.sh \
  --job-id "sync-$(date +%s)" \
  --command "ollama run llama3.2:3b 'What is 2+2?'")

echo "AI says: $RESPONSE"
```

**Note:** If gaming starts, job will queue and this will block until gaming ends!

### Asynchronous (Background Processing)

```bash
#!/bin/bash
# Fire and forget - process in background

batch-job-wrapper.sh \
  --job-id "async-$(date +%s)" \
  --command "ollama run llama3.1:70b 'Long task...' > /tmp/result.txt" &

echo "Job started in background. Check /tmp/result.txt later."
```

---

## Error Handling

```bash
#!/bin/bash
# Robust error handling

JOB_ID="my-job-$(date +%s)"

batch-job-wrapper.sh \
  --job-id "$JOB_ID" \
  --command "ollama run llama3.2:3b 'Your prompt'"

EXIT_CODE=$?

if [ $EXIT_CODE -eq 0 ]; then
  echo "✓ Job completed successfully"
else
  echo "✗ Job failed with exit code $EXIT_CODE"
  echo "Check logs: tail /var/log/batch-job-wrapper.log | grep $JOB_ID"
fi
```

---

## Monitoring & Debugging

### Check Queue Status

```bash
# See what's queued
ls -la /var/lib/batch-jobs/queue/

# Example output:
# 1-interrupted-email-summary-123.job  <- Will run first (interrupted)
# 2-new-home-auto-456.job              <- Will run second (new)
```

### Monitor Logs

```bash
# Watch wrapper activity
tail -f /var/log/batch-job-wrapper.log

# Watch resource manager
tail -f /var/log/resource-manager.log

# Watch both
tail -f /var/log/{batch-job-wrapper,resource-manager}.log
```

### Check System State

```bash
# What state are we in?
cat /tmp/resource-manager-state

# Output examples:
# idle                    <- Ollama can run
# high-resource:gaming    <- Gaming active, jobs queued
# high-resource:plex      <- Plex active, jobs queued
```

---

## Integration Checklist

When building your application:

- [ ] **Choose appropriate model** for task complexity
- [ ] **Generate unique job-id** (include timestamp, task type)
- [ ] **Handle queueing gracefully** (job may not run immediately)
- [ ] **Log job submission** for debugging
- [ ] **Handle failures** (check exit codes)
- [ ] **Test during gaming** (ensure queueing works)
- [ ] **Monitor resource usage** (check GPU/RAM)

---

## Common Patterns

### Pattern 1: Simple Task
```bash
batch-job-wrapper.sh --job-id "quick" \
  --command "ollama run llama3.2:3b 'Simple task'"
```

### Pattern 2: Complex Task
```bash
batch-job-wrapper.sh --job-id "complex" \
  --command "ollama run llama3.1:70b 'Complex analysis...'"
```

### Pattern 3: Code Task
```bash
batch-job-wrapper.sh --job-id "code" \
  --command "ollama run deepseek-coder:33b 'Review code...'"
```

### Pattern 4: With Output Redirection
```bash
batch-job-wrapper.sh --job-id "save" \
  --command "ollama run llama3.1:8b 'Analyze...' > /tmp/output.txt"
```

### Pattern 5: Background Job
```bash
batch-job-wrapper.sh --job-id "background" \
  --command "ollama run llama3.1:70b 'Long task...'" &
```

---

## Next Steps

1. **Review examples:** Check `/usr/local/lib/ollama-batch-examples/`
2. **Test an example:** Run `home-automation.sh "test"`
3. **Build your app:** Copy an example and modify for your use case
4. **Test queueing:** Run job, launch game, verify queueing works
5. **Deploy:** Integrate into your production services

---

**Questions?** Check `README.md` or logs in `/var/log/`
