#!/bin/bash
# home-automation.sh
# Home automation service - uses small, fast model for instant responses
#
# Usage: ./home-automation.sh "Turn on living room lights"

PROMPT="$1"

if [ -z "$PROMPT" ]; then
  echo "Usage: $0 <prompt>"
  exit 1
fi

# Business logic: Home automation needs fast responses, use small model
MODEL="llama3.2:3b"

# Call generic batch wrapper with metadata
/usr/local/bin/batch-job-wrapper.sh \
  --job-id "home-auto-$(date +%s)" \
  --caller "home-automation" \
  --tags "automation,home,realtime" \
  --command "ollama run $MODEL '$PROMPT'"
