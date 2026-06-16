#!/bin/bash
# batch-metrics-export.sh
# Export batch job metrics in various formats

METRICS_DIR="/var/lib/batch-jobs/metrics"
FORMAT="${1:-json}"  # json, csv, prometheus, summary

usage() {
  echo "Usage: $0 [format]"
  echo ""
  echo "Formats:"
  echo "  json         - Raw JSON array of all jobs"
  echo "  csv          - CSV format for spreadsheets"
  echo "  prometheus   - Prometheus metrics format"
  echo "  summary      - Human-readable summary"
  echo "  influxdb     - InfluxDB line protocol"
  echo ""
  echo "Examples:"
  echo "  $0 json > jobs.json"
  echo "  $0 csv > jobs.csv"
  echo "  $0 summary"
  echo "  $0 prometheus | curl --data-binary @- http://pushgateway:9091/metrics/job/ollama"
  exit 1
}

if [ "$FORMAT" = "--help" ] || [ "$FORMAT" = "-h" ]; then
  usage
fi

# Check if metrics directory exists
if [ ! -d "$METRICS_DIR" ]; then
  echo "ERROR: Metrics directory not found: $METRICS_DIR"
  exit 1
fi

# Count jobs
TOTAL_JOBS=$(ls -1 "$METRICS_DIR"/*.json 2>/dev/null | wc -l)

if [ $TOTAL_JOBS -eq 0 ]; then
  echo "No job metrics found in $METRICS_DIR"
  exit 0
fi

case "$FORMAT" in
  json)
    # Export as JSON array
    echo "["
    first=true
    for metric_file in "$METRICS_DIR"/*.json; do
      if [ "$first" = true ]; then
        first=false
      else
        echo ","
      fi
      cat "$metric_file"
    done
    echo ""
    echo "]"
    ;;

  csv)
    # Export as CSV
    echo "job_id,caller,model,status,submitted_at,started_at,completed_at,duration_seconds,exit_code,output_size_bytes,tags"

    for metric_file in "$METRICS_DIR"/*.json; do
      jq -r '[
        .job_id,
        .caller,
        .model,
        .status,
        .submitted_at,
        .started_at // "N/A",
        .completed_at // "N/A",
        .duration_seconds // 0,
        .exit_code // "N/A",
        .output_size_bytes // 0,
        (.tags // [] | join(";"))
      ] | @csv' "$metric_file"
    done
    ;;

  prometheus)
    # Export as Prometheus metrics
    echo "# HELP ollama_batch_jobs_total Total number of batch jobs"
    echo "# TYPE ollama_batch_jobs_total counter"

    # Count by status
    for status in completed failed queued running; do
      count=$(jq -r "select(.status == \"$status\") | .job_id" "$METRICS_DIR"/*.json 2>/dev/null | wc -l)
      echo "ollama_batch_jobs_total{status=\"$status\"} $count"
    done

    # Count by caller
    echo ""
    echo "# HELP ollama_batch_jobs_by_caller Jobs by calling application"
    echo "# TYPE ollama_batch_jobs_by_caller counter"
    for caller in $(jq -r '.caller' "$METRICS_DIR"/*.json 2>/dev/null | sort -u); do
      count=$(jq -r "select(.caller == \"$caller\") | .job_id" "$METRICS_DIR"/*.json 2>/dev/null | wc -l)
      echo "ollama_batch_jobs_by_caller{caller=\"$caller\"} $count"
    done

    # Count by model
    echo ""
    echo "# HELP ollama_batch_jobs_by_model Jobs by model"
    echo "# TYPE ollama_batch_jobs_by_model counter"
    for model in $(jq -r '.model' "$METRICS_DIR"/*.json 2>/dev/null | sort -u); do
      count=$(jq -r "select(.model == \"$model\") | .job_id" "$METRICS_DIR"/*.json 2>/dev/null | wc -l)
      echo "ollama_batch_jobs_by_model{model=\"$model\"} $count"
    done

    # Average duration
    echo ""
    echo "# HELP ollama_batch_duration_seconds Job duration in seconds"
    echo "# TYPE ollama_batch_duration_seconds gauge"
    avg_duration=$(jq -s 'map(select(.duration_seconds != null) | .duration_seconds) | add / length' "$METRICS_DIR"/*.json 2>/dev/null)
    echo "ollama_batch_duration_seconds_avg $avg_duration"
    ;;

  summary)
    # Human-readable summary
    echo "=== Batch Job Metrics Summary ==="
    echo ""

    # Total jobs
    echo "Total Jobs: $TOTAL_JOBS"
    echo ""

    # By status
    echo "By Status:"
    for status in completed failed queued running pending; do
      count=$(jq -r "select(.status == \"$status\") | .job_id" "$METRICS_DIR"/*.json 2>/dev/null | wc -l)
      if [ $count -gt 0 ]; then
        echo "  $status: $count"
      fi
    done
    echo ""

    # By caller
    echo "By Caller:"
    for caller in $(jq -r '.caller' "$METRICS_DIR"/*.json 2>/dev/null | sort -u); do
      count=$(jq -r "select(.caller == \"$caller\") | .job_id" "$METRICS_DIR"/*.json 2>/dev/null | wc -l)
      echo "  $caller: $count"
    done
    echo ""

    # By model
    echo "By Model:"
    for model in $(jq -r '.model' "$METRICS_DIR"/*.json 2>/dev/null | sort -u); do
      count=$(jq -r "select(.model == \"$model\") | .job_id" "$METRICS_DIR"/*.json 2>/dev/null | wc -l)
      echo "  $model: $count"
    done
    echo ""

    # Duration stats
    echo "Duration Statistics:"
    avg=$(jq -s 'map(select(.duration_seconds != null) | .duration_seconds) | add / length' "$METRICS_DIR"/*.json 2>/dev/null)
    min=$(jq -s 'map(select(.duration_seconds != null) | .duration_seconds) | min' "$METRICS_DIR"/*.json 2>/dev/null)
    max=$(jq -s 'map(select(.duration_seconds != null) | .duration_seconds) | max' "$METRICS_DIR"/*.json 2>/dev/null)
    echo "  Average: ${avg}s"
    echo "  Min: ${min}s"
    echo "  Max: ${max}s"
    echo ""

    # Success rate
    completed=$(jq -r 'select(.status == "completed") | .job_id' "$METRICS_DIR"/*.json 2>/dev/null | wc -l)
    failed=$(jq -r 'select(.status == "failed") | .job_id' "$METRICS_DIR"/*.json 2>/dev/null | wc -l)
    total_finished=$((completed + failed))
    if [ $total_finished -gt 0 ]; then
      success_rate=$(echo "scale=2; $completed * 100 / $total_finished" | bc)
      echo "Success Rate: ${success_rate}% ($completed/$total_finished)"
    fi
    ;;

  influxdb)
    # Export as InfluxDB line protocol
    for metric_file in "$METRICS_DIR"/*.json; do
      # Parse JSON
      job_id=$(jq -r '.job_id' "$metric_file")
      caller=$(jq -r '.caller' "$metric_file")
      model=$(jq -r '.model' "$metric_file")
      status=$(jq -r '.status' "$metric_file")
      duration=$(jq -r '.duration_seconds // 0' "$metric_file")
      exit_code=$(jq -r '.exit_code // 0' "$metric_file")
      timestamp=$(jq -r '.completed_timestamp // .submitted_timestamp' "$metric_file")

      # InfluxDB line protocol: measurement,tag1=value1 field1=value1 timestamp
      echo "ollama_batch_job,job_id=$job_id,caller=$caller,model=$model,status=$status duration=$duration,exit_code=$exit_code ${timestamp}000000000"
    done
    ;;

  *)
    echo "ERROR: Unknown format: $FORMAT"
    usage
    ;;
esac
