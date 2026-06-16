## Batch Job Metrics Guide

**Automatic telemetry tracking for all batch jobs**

---

## What Gets Tracked

Every job automatically records:

```json
{
  "job_id": "email-summary-1234567890",
  "caller": "email-summarizer",
  "tags": ["email", "daily", "batch"],
  "model": "llama3.1:8b",
  "command": "ollama run llama3.1:8b 'Summarize...'",

  "submitted_at": "2026-02-21T18:00:00-05:00",
  "submitted_timestamp": 1708556400,
  "submitted_by": "user",
  "hostname": "gpu-host",

  "started_at": "2026-02-21T18:00:05-05:00",
  "started_timestamp": 1708556405,

  "completed_at": "2026-02-21T18:01:30-05:00",
  "completed_timestamp": 1708556490,

  "duration_seconds": 85,
  "exit_code": 0,
  "output_size_bytes": 4521,
  "status": "completed"
}
```

**Metrics stored:** `/var/lib/batch-jobs/metrics/<job-id>.json`

---

## Usage in Your Applications

### Add Metadata to Jobs

```bash
#!/bin/bash
# my-app.sh

batch-job-wrapper.sh \
  --job-id "my-app-$(date +%s)" \
  --caller "my-app"  \
  --tags "tag1,tag2,tag3" \
  --command "ollama run llama3.2:3b 'prompt'"
```

**Parameters:**
- `--caller` (optional): Name of calling application (auto-detected from script name)
- `--tags` (optional): Comma-separated tags for categorization

---

## Exporting Metrics

### 1. View Summary

```bash
batch-metrics-export summary
```

**Output:**
```
=== Batch Job Metrics Summary ===

Total Jobs: 47

By Status:
  completed: 42
  failed: 3
  queued: 2

By Caller:
  email-summarizer: 20
  home-automation: 15
  code-reviewer: 10
  research-analyzer: 2

By Model:
  llama3.2:3b: 15
  llama3.1:8b: 20
  deepseek-coder:33b: 10
  llama3.1:70b: 2

Duration Statistics:
  Average: 45.3s
  Min: 2.1s
  Max: 342.7s

Success Rate: 93.33% (42/45)
```

---

### 2. Export to JSON

```bash
batch-metrics-export json > /data/metrics/jobs-$(date +%Y%m%d).json
```

**Use for:** Custom analysis, visualization tools

---

### 3. Export to CSV

```bash
batch-metrics-export csv > /data/metrics/jobs.csv
```

**Use for:** Spreadsheets (Excel, Google Sheets), data analysis

**Example CSV:**
```csv
job_id,caller,model,status,submitted_at,duration_seconds
email-summary-123,email-summarizer,llama3.1:8b,completed,2026-02-21T18:00:00,85
```

---

### 4. Export to Prometheus

```bash
# Manual export
batch-metrics-export prometheus

# Push to Prometheus Pushgateway
batch-metrics-export prometheus | \
  curl --data-binary @- http://pushgateway:9091/metrics/job/ollama-batch

# Or save to file for node_exporter textfile collector
batch-metrics-export prometheus > /var/lib/node_exporter/ollama_batch.prom
```

**Metrics exported:**
- `ollama_batch_jobs_total{status="completed|failed|queued"}` - Job counts by status
- `ollama_batch_jobs_by_caller{caller="email-summarizer"}` - Jobs by application
- `ollama_batch_jobs_by_model{model="llama3.1:8b"}` - Jobs by model
- `ollama_batch_duration_seconds_avg` - Average job duration

---

### 5. Export to InfluxDB

```bash
# Generate line protocol
batch-metrics-export influxdb > /tmp/metrics.influx

# Send to InfluxDB
batch-metrics-export influxdb | \
  curl -XPOST 'http://localhost:8086/write?db=ollama' --data-binary @-
```

---

## Automated Metrics Collection

### Cron Job (Daily Export)

```bash
# crontab -e

# Export daily summary
0 0 * * * batch-metrics-export summary > /var/log/batch-metrics-daily-$(date +\%Y\%m\%d).txt

# Export JSON for analysis
0 0 * * * batch-metrics-export json > /data/metrics/jobs-$(date +\%Y\%m\%d).json

# Push to Prometheus
*/5 * * * * batch-metrics-export prometheus | curl --data-binary @- http://pushgateway:9091/metrics/job/ollama-batch
```

---

## Integration Examples

### Grafana Dashboard

**InfluxDB Query:**
```sql
SELECT mean("duration")
FROM "ollama_batch_job"
WHERE time > now() - 24h
GROUP BY time(1h), "model"
```

**Prometheus Query:**
```promql
# Job completion rate
rate(ollama_batch_jobs_total{status="completed"}[5m])

# Average duration by model
ollama_batch_duration_seconds_avg{model="llama3.1:8b"}

# Jobs per caller
sum by (caller) (ollama_batch_jobs_by_caller)
```

---

### Custom Analysis Script

```python
#!/usr/bin/env python3
# analyze-jobs.py

import json
import glob

metrics_dir = "/var/lib/batch-jobs/metrics"

# Load all metrics
jobs = []
for file in glob.glob(f"{metrics_dir}/*.json"):
    with open(file) as f:
        jobs.append(json.load(f))

# Analysis
print(f"Total jobs: {len(jobs)}")

# Average duration by model
from collections import defaultdict
durations = defaultdict(list)

for job in jobs:
    if job.get("duration_seconds"):
        durations[job["model"]].append(job["duration_seconds"])

for model, times in durations.items():
    avg = sum(times) / len(times)
    print(f"{model}: avg {avg:.1f}s ({len(times)} jobs)")
```

---

## Metrics Cleanup

Metrics files accumulate over time. Clean old metrics periodically:

```bash
# Delete metrics older than 30 days
find /var/lib/batch-jobs/metrics -name "*.json" -mtime +30 -delete

# Archive before deleting
tar -czf /data/archives/metrics-$(date +%Y%m).tar.gz \
  /var/lib/batch-jobs/metrics/*.json
find /var/lib/batch-jobs/metrics -name "*.json" -mtime +30 -delete
```

**Add to cron:**
```bash
# Monthly cleanup
0 0 1 * * find /var/lib/batch-jobs/metrics -name "*.json" -mtime +30 -delete
```

---

## Useful Queries

### Jobs failing most often
```bash
jq -r 'select(.status == "failed") | "\(.caller): \(.exit_code)"' \
  /var/lib/batch-jobs/metrics/*.json | sort | uniq -c | sort -rn
```

### Slowest jobs
```bash
jq -r '"\(.duration_seconds // 0)\t\(.job_id)\t\(.model)"' \
  /var/lib/batch-jobs/metrics/*.json | sort -rn | head -10
```

### Jobs by time of day
```bash
jq -r '.submitted_at | split("T")[1] | split(":")[0]' \
  /var/lib/batch-jobs/metrics/*.json | sort | uniq -c
```

### Success rate by caller
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

## Dashboard Example

**Simple web dashboard (Python + Flask):**

```python
from flask import Flask, jsonify
import json, glob

app = Flask(__name__)

@app.route('/metrics')
def metrics():
    jobs = []
    for file in glob.glob('/var/lib/batch-jobs/metrics/*.json'):
        with open(file) as f:
            jobs.append(json.load(f))

    return jsonify({
        'total': len(jobs),
        'completed': len([j for j in jobs if j['status'] == 'completed']),
        'failed': len([j for j in jobs if j['status'] == 'failed']),
        'recent': jobs[-10:]
    })

if __name__ == '__main__':
    app.run(port=8080)
```

---

## What to Track

**Recommended tags:**

| Application | Tags | Purpose |
|-------------|------|---------|
| Home Automation | `automation,home,realtime` | Track quick requests |
| Email Summarizer | `email,daily,batch` | Track batch processing |
| Code Reviewer | `cicd,code-review,pr-123` | Track CI/CD jobs |
| Research | `research,weekly,analysis` | Track long-running analysis |

**Custom tags examples:**
- Priority: `priority-high`, `priority-low`
- Environment: `prod`, `dev`, `test`
- User: `user-user`, `user-family`
- Source: `webhook`, `cron`, `manual`

---

## Next Steps

1. **Run a job** with metrics
2. **Check metrics:** `ls -la /var/lib/batch-jobs/metrics/`
3. **View summary:** `batch-metrics-export summary`
4. **Set up daily export** (cron)
5. **Integrate with monitoring** (Prometheus/Grafana)

**Full metrics stored in:** `/var/lib/batch-jobs/metrics/<job-id>.json`
