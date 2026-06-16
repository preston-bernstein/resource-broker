#!/bin/bash
# email-summarizer.sh
# Email summarization service - uses medium model for quality/speed balance
#
# Usage: ./email-summarizer.sh /path/to/emails.txt

EMAIL_FILE="$1"

if [ -z "$EMAIL_FILE" ] || [ ! -f "$EMAIL_FILE" ]; then
  echo "Usage: $0 <email-file>"
  exit 1
fi

# Read emails
EMAILS=$(cat "$EMAIL_FILE")

# Business logic: Email summarization needs good quality, use medium model
MODEL="llama3.1:8b"

# Build prompt
PROMPT="You are an email assistant. Summarize the following emails and extract any action items:

$EMAILS

Provide:
1. Brief summary of each email (1-2 sentences)
2. List of action items with deadlines
3. Priority classification (high/medium/low)"

# Call generic batch wrapper with metadata
/usr/local/bin/batch-job-wrapper.sh \
  --job-id "email-summary-$(date +%s)" \
  --caller "email-summarizer" \
  --tags "email,daily,batch" \
  --command "ollama run $MODEL '$PROMPT'"
