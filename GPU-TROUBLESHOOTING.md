# GPU Detection Troubleshooting Guide

**Issue:** Ollama not detecting AMD GPU (RX 9070 XT / RDNA 4)

**Date:** 2026-02-21
**Status:** Diagnosed - Needs Ollama update

---

## Problem

After enabling GPU support in Ollama service:
```ini
Environment="OLLAMA_NUM_GPU=1"
```

Ollama still shows:
```
msg="entering low vram mode" "total vram"="0 B"
```

GPU is visible to system but not to Ollama.

---

## System Details

```bash
$ rocm-smi
Device 0: AMD Radeon RX 9070 XT
GPU%: 5%
VRAM%: 17%

$ rocminfo | grep "Name.*gfx"
Name: gfx1201  ← RDNA 4 architecture

$ ollama --version
ollama version is 0.13.5  ← Too old!
```

---

## Root Cause

**RX 9070 XT is RDNA 4 (gfx1201) - Brand new architecture!**

- Released: January 2025
- Architecture: gfx1201 (RDNA 4 / Navi 48)
- Ollama 0.13.5: December 2024 (before RX 9070 XT release)
- **Ollama 0.13.5 doesn't recognize RDNA 4 GPUs**

---

## Troubleshooting Steps Attempted

### Attempt 1: Add ollama user to GPU groups
```bash
sudo usermod -aG render ollama
sudo usermod -aG video ollama
sudo systemctl restart ollama
```

**Result:** Still no GPU detection
**Why:** User permissions were fine, issue was architecture support

### Attempt 2: HSA_OVERRIDE_GFX_VERSION
```bash
# Added to service:
Environment="HSA_OVERRIDE_GFX_VERSION=11.0.0"
```

**Theory:** Override gfx1201 to pretend it's gfx1100 (RDNA 3)
**Result:** Still no GPU detection
**Why:** Ollama 0.13.5 too old, override doesn't help

---

## Solution

**Update Ollama to 0.16.3+ (has RDNA 4 support)**

```bash
# Stop service
sudo systemctl stop ollama

# Update to latest
curl -fsSL https://ollama.com/install.sh | sh

# Verify version
ollama --version
# Should show: 0.16.3 or newer

# Restart service
sudo systemctl start ollama

# Check GPU detection
sudo journalctl -u ollama --since "30 seconds ago" | grep -i "vram\|gpu"
```

**Expected after update:**
```
msg="inference compute" ... total="16.0 GiB" available="15.X GiB"
```

---

## Verification Steps

After updating Ollama:

**1. Check GPU detection in logs:**
```bash
sudo journalctl -u ollama --since "1 minute ago" | grep "inference compute"
```

Should show VRAM (not "0 B").

**2. Load model and check GPU usage:**
```bash
ollama run llama3.2:3b "test"
ollama ps
```

Should show:
```
PROCESSOR: 100% CPU/GPU  (or similar GPU indicator)
```

**3. Monitor GPU during inference:**
```bash
watch -n 1 rocm-smi
```

Should show GPU% increase during inference.

---

## Lessons Learned

### 1. New Hardware Needs New Software

**Problem:** Assumed ROCm support = Ollama support
**Reality:** Ollama needs explicit support for new GPU architectures
**Lesson:** Check Ollama release dates vs GPU release dates

### 2. Version Compatibility Matrix

| GPU Architecture | Released | Ollama Version | Status |
|------------------|----------|----------------|--------|
| RDNA 3 (gfx1100) | 2022 | 0.13.5+ | ✅ Supported |
| RDNA 4 (gfx1201) | Jan 2025 | 0.16.0+ | ✅ Supported (newer versions) |

### 3. Don't Assume HSA_OVERRIDE Works

**When HSA_OVERRIDE helps:**
- Minor architecture variants (gfx1103 → gfx1100)
- Known compatible architectures

**When it doesn't help:**
- Ollama version doesn't support base architecture
- Major architecture changes (RDNA 3 → RDNA 4)

### 4. Check Ollama Changelog

Before assuming configuration issues, check:
```bash
# Latest release
curl -s https://api.github.com/repos/ollama/ollama/releases/latest | grep '"tag_name"'

# Your version
ollama --version

# If your GPU is newer than your Ollama version → update first!
```

---

## General GPU Detection Checklist

If Ollama shows "total vram"="0 B":

**[ ] 1. Check GPU is visible to system**
```bash
lspci | grep -i vga
rocm-smi  # Should show your GPU
```

**[ ] 2. Check ROCm installation**
```bash
rocminfo | grep "Name.*gfx"  # Should show gfx version
```

**[ ] 3. Check ollama user permissions**
```bash
groups ollama  # Should include: render, video
```

**[ ] 4. Check Ollama version vs GPU release date**
```bash
ollama --version  # Compare to GPU launch date
```

**[ ] 5. Check Ollama logs for errors**
```bash
sudo journalctl -u ollama --since "5 minutes ago" -n 50
```

**[ ] 6. Try latest Ollama version**
```bash
curl -fsSL https://ollama.com/install.sh | sh
```

---

## For Blog Post / Documentation

**Key Discovery:** Hardware-software version compatibility matters!

**Timeline:**
- Dec 2024: Ollama 0.13.5 released
- Jan 2025: AMD RX 9070 XT released (RDNA 4 / gfx1201)
- **Problem:** Can't use 2-month-old Ollama with 1-month-old GPU!

**Impact on project:**
- Initial architecture assumed GPU would "just work"
- Discovered during deployment that newer hardware needs newer software
- Solution: Update Ollama to 0.16.3+

**Lesson for readers:**
If you have cutting-edge hardware (released in last 6 months), always use latest Ollama version.

---

## Additional Resources

**Ollama GPU Support Documentation:**
- https://github.com/ollama/ollama/blob/main/docs/gpu.md

**ROCm Compatibility:**
- https://rocm.docs.amd.com/

**Checking GPU Architecture:**
```bash
# Quick reference
rocminfo | grep -A 3 "Marketing Name"
```

---

**Status:** ✅ RESOLVED - GPU Detection Working!
**Update Date:** 2026-02-21 13:45

## Solution Verified

After updating to Ollama 0.16.3:

```bash
$ ollama ps
NAME           ID              SIZE      PROCESSOR    UNTIL
llama3.2:3b    a80c4f17acd5    2.8 GB    100% GPU     Forever
```

✅ GPU detected and operational
✅ RX 9070 XT (RDNA 4/gfx1201) fully supported
✅ GPU acceleration active (100% GPU shown in PROCESSOR column)

**Conclusion:** Updating Ollama from 0.13.5 → 0.16.3 successfully resolved the RDNA 4 GPU detection issue.
