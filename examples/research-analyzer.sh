#!/bin/bash
# research-analyzer.sh
# Research paper analysis - uses largest model for maximum quality
#
# Usage: ./research-analyzer.sh /path/to/papers-directory/

PAPERS_DIR="$1"

if [ -z "$PAPERS_DIR" ] || [ ! -d "$PAPERS_DIR" ]; then
  echo "Usage: $0 <papers-directory>"
  exit 1
fi

# Business logic: Research analysis needs maximum quality, use largest model
MODEL="llama3.1:70b"

# Read all papers (simplified - you'd parse PDFs, etc.)
PAPERS=""
for file in "$PAPERS_DIR"/*.txt; do
  if [ -f "$file" ]; then
    PAPERS="$PAPERS\n\n=== $(basename "$file") ===\n$(cat "$file")"
  fi
done

# Build prompt
PROMPT="You are a research analyst. Analyze the following research papers:

$PAPERS

Provide:
1. Executive summary (2-3 paragraphs)
2. Key findings and methodologies from each paper
3. Common themes and contradictions
4. Gaps in the research
5. Recommendations for future research

Write a comprehensive literature review (2000-3000 words)."

# Call generic batch wrapper with metadata
/usr/local/bin/batch-job-wrapper.sh \
  --job-id "research-$(date +%s)" \
  --caller "research-analyzer" \
  --tags "research,analysis,weekly" \
  --command "ollama run $MODEL '$PROMPT'"
