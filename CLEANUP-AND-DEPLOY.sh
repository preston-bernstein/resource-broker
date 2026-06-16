#!/bin/bash
# CLEANUP-AND-DEPLOY.sh
# Cleanup deprecated files and deploy V3 system

echo "=== Ollama Resource Manager - Cleanup and Deploy V3 ==="
echo ""

# Create archive directory for old files
ARCHIVE_DIR="/home/user/Documents/System-Architecture/Ollama-Plex-Gaming/archive"
mkdir -p "$ARCHIVE_DIR"

echo "Step 1: Archiving deprecated documentation files..."
mv -v /home/user/Documents/System-Architecture/Ollama-Plex-Gaming/BLOG-POST-DOCUMENTATION.md "$ARCHIVE_DIR/" 2>/dev/null
mv -v /home/user/Documents/System-Architecture/Ollama-Plex-Gaming/PHASE-3-IMPLEMENTATION-PLAN.md "$ARCHIVE_DIR/" 2>/dev/null
mv -v /home/user/Documents/System-Architecture/Ollama-Plex-Gaming/PHASE-5-PLANNING.md "$ARCHIVE_DIR/" 2>/dev/null
mv -v /home/user/Documents/System-Architecture/Ollama-Plex-Gaming/Resource-Management-Architecture.md "$ARCHIVE_DIR/" 2>/dev/null
mv -v /home/user/Documents/System-Architecture/Ollama-Plex-Gaming/RESOURCE-MANAGER-UPGRADE-V2.md "$ARCHIVE_DIR/" 2>/dev/null

echo ""
echo "Step 2: Archiving old script versions..."
mv -v /home/user/Documents/System-Architecture/Ollama-Plex-Gaming/resource-manager-v2.sh "$ARCHIVE_DIR/" 2>/dev/null
mv -v /home/user/Documents/System-Architecture/Ollama-Plex-Gaming/batch-job-wrapper-v2.sh "$ARCHIVE_DIR/" 2>/dev/null

echo ""
echo "Step 3: Installing dependencies (jq for JSON parsing)..."
if ! command -v jq &> /dev/null; then
    echo "jq not found, installing..."
    sudo apt update && sudo apt install -y jq
else
    echo "jq already installed"
fi

echo ""
echo "Step 4: Creating batch job directories..."
sudo mkdir -p /var/lib/batch-jobs/queue
sudo mkdir -p /var/lib/batch-jobs/metrics
sudo chown -R user:user /var/lib/batch-jobs/

echo ""
echo "Step 5: Deploying V5 scripts (with metadata tracking)..."
sudo cp -v /home/user/Documents/System-Architecture/Ollama-Plex-Gaming/resource-manager-v3.sh /usr/local/bin/resource-manager.sh
sudo chmod +x /usr/local/bin/resource-manager.sh

sudo cp -v /home/user/Documents/System-Architecture/Ollama-Plex-Gaming/batch-job-wrapper-v5.sh /usr/local/bin/batch-job-wrapper.sh
sudo chmod +x /usr/local/bin/batch-job-wrapper.sh

sudo cp -v /home/user/Documents/System-Architecture/Ollama-Plex-Gaming/batch-metrics-export.sh /usr/local/bin/batch-metrics-export
sudo chmod +x /usr/local/bin/batch-metrics-export

echo ""
echo "Step 5b: Installing example calling applications..."
sudo mkdir -p /usr/local/lib/ollama-batch-examples
sudo cp -v /home/user/Documents/System-Architecture/Ollama-Plex-Gaming/examples/*.sh /usr/local/lib/ollama-batch-examples/
sudo chmod +x /usr/local/lib/ollama-batch-examples/*.sh

echo ""
echo "Step 6: Cleaning up old deployed backups..."
sudo rm -v /usr/local/bin/resource-manager.sh.v1-backup 2>/dev/null
sudo rm -v /usr/local/bin/batch-job-wrapper.sh.v1-backup 2>/dev/null

echo ""
echo "Step 7: Restarting services..."
echo "Unloading any loaded models first..."
curl -X POST http://localhost:11434/api/generate -d '{"model": "llama3.2:3b", "keep_alive": 0}' 2>/dev/null

echo "Restarting resource-manager service..."
sudo systemctl restart resource-manager

echo "Restarting ollama service..."
sudo systemctl restart ollama

echo ""
echo "Step 8: Verification..."
sleep 3

echo "Resource manager status:"
sudo systemctl status resource-manager --no-pager | head -10

echo ""
echo "Ollama status:"
sudo systemctl status ollama --no-pager | head -10

echo ""
echo "System state:"
cat /tmp/resource-manager-state 2>/dev/null || echo "State file not yet created"

echo ""
echo "Ollama models loaded:"
ollama ps

echo ""
echo "Queue directory:"
ls -la /var/lib/batch-jobs/queue/ 2>/dev/null || echo "Queue empty"

echo ""
echo "=== Deployment Complete ==="
echo ""
echo "Core System:"
echo "  - /usr/local/bin/resource-manager.sh (process-based, queue-only)"
echo "  - /usr/local/bin/batch-job-wrapper.sh (generic command wrapper)"
echo ""
echo "Example Applications (see /usr/local/lib/ollama-batch-examples/):"
echo "  - home-automation.sh (uses llama3.2:3b - fast)"
echo "  - email-summarizer.sh (uses llama3.1:8b - balanced)"
echo "  - code-reviewer.sh (uses deepseek-coder:33b - specialized)"
echo "  - research-analyzer.sh (uses llama3.1:70b - quality)"
echo ""
echo "Architecture:"
echo "  - Wrapper is GENERIC (just handles queueing)"
echo "  - Calling apps decide model (business logic)"
echo "  - Gaming/Plex: Gets 100% resources"
echo "  - Clean separation of concerns"
echo ""
echo "Archived files in: $ARCHIVE_DIR"
echo ""
echo "Next steps:"
echo "  1. Monitor logs: tail -f /var/log/resource-manager.log"
echo "  2. Test example app:"
echo "     /usr/local/lib/ollama-batch-examples/home-automation.sh 'Turn on lights'"
echo "  3. Launch game while job running, watch queue system work"
echo ""
echo "Example integration (in your apps):"
echo "  batch-job-wrapper.sh --job-id my-job \\"
echo "    --command 'ollama run llama3.2:3b \"Your prompt here\"'"
