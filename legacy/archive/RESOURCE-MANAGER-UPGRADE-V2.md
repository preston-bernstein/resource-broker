# Resource Manager V2: GPU-Based Detection Upgrade

**Date:** 2026-02-21
**Status:** Designed, Ready to Test

---

## Why Upgrade to V2?

### V1 (Current) - Process-Based Detection

**How it works:**
```bash
pgrep -f "\.exe$|steam.*AppId|lutris.*running|proton.*game"
```

**Problems:**
1. ❌ **Misses native Linux games** - Dwarf Fortress, Factorio, Minecraft
2. ❌ **Misses emulators** - RetroArch, Dolphin, PCSX2
3. ❌ **Misses directly-launched games** - GOG games not via Lutris
4. ❌ **Can trigger on non-games** - Wine applications, Steam client itself
5. ❌ **Detects games that don't need GPU** - Text-based games shouldn't trigger throttling

**When it works well:**
- ✅ Steam games (Windows via Proton)
- ✅ Lutris games
- ✅ Most Wine games

---

### V2 (New) - GPU-Based Detection

**How it works:**
```bash
GPU_USAGE=$(cat /sys/class/drm/card1/device/gpu_busy_percent)

# Enter gaming: GPU > 50% for 30 seconds (sustained)
# Exit gaming: GPU < 20% for 5 minutes (sustained)
```

**Advantages:**
1. ✅ **Detects ALL GPU-intensive games** - Native, Wine, emulators, anything
2. ✅ **Ignores non-GPU games** - Text-based games don't trigger (no resource conflict)
3. ✅ **Measures actual resource usage** - GPU is what we're protecting
4. ✅ **Works across all launchers** - Steam, Lutris, GOG, direct launch, emulators
5. ✅ **Prevents state flapping** - Hysteresis handles loading screens, menus, pauses

**How hysteresis works:**
```
Scenario: Playing Cyberpunk 2077

0:00 - Launch game
0:20 - GPU 80% (counter: 20s / 30s needed)
0:40 - GPU 85% (counter: 40s / 30s) → ENTER GAMING STATE ✓

[Playing - GPU varies 60-95%]
2:00 - Pause for menu (GPU drops to 10%)
2:20 - GPU 8% (counter: 20s / 300s needed to exit)
2:40 - Back to game (GPU 90%) → Counter reset, still GAMING ✓

[Keep playing]
10:00 - Quit game
10:20 - GPU 5% (counter: 20s / 300s)
...
15:00 - GPU 3% (counter: 300s / 300s) → EXIT GAMING STATE ✓
```

**Key benefit:** Game "owns" the GPU state even during brief dips (loading, menus, cutscenes)

---

## Side-by-Side Comparison

| Feature | V1 (Process) | V2 (GPU) |
|---------|--------------|----------|
| **Native Linux games** | ❌ Missed | ✅ Detected |
| **Emulators** | ❌ Missed | ✅ Detected |
| **Steam Proton games** | ✅ Detected | ✅ Detected |
| **Lutris games** | ✅ Detected | ✅ Detected |
| **Non-GPU games** | ❌ Triggers unnecessarily | ✅ Ignored (correct) |
| **State flapping** | ❌ Switches on every dip | ✅ Hysteresis prevents |
| **False positives** | ❌ Wine apps, Steam client | ✅ Only real GPU usage |
| **Cross-user detection** | ✅ All users | ✅ All users |
| **Measures real conflict** | ❌ Process != GPU | ✅ GPU = actual resource |

---

## Configuration

### Tunable Parameters in V2

```bash
# GPU thresholds
GAMING_ENTER_THRESHOLD=50      # GPU > 50% to enter gaming
GAMING_EXIT_THRESHOLD=20       # GPU < 20% to exit gaming

# Time durations (hysteresis)
GAMING_ENTER_DURATION=30       # Sustained for 30 seconds
GAMING_EXIT_DURATION=300       # Sustained for 5 minutes
```

**When to adjust:**

**More sensitive (detect games faster):**
```bash
GAMING_ENTER_THRESHOLD=30      # Enter at 30% GPU
GAMING_ENTER_DURATION=10       # Only need 10s sustained
```

**Less sensitive (avoid false positives):**
```bash
GAMING_ENTER_THRESHOLD=70      # Enter at 70% GPU (high load)
GAMING_ENTER_DURATION=60       # Need 60s sustained
```

**Faster state transitions (less "sticky"):**
```bash
GAMING_EXIT_DURATION=60        # Exit after 1 minute low GPU
```

**Slower state transitions (more "sticky"):**
```bash
GAMING_EXIT_DURATION=600       # Exit after 10 minutes low GPU
```

**Recommended defaults (already set):**
- Enter: 50% for 30s - Catches most games quickly, avoids video playback
- Exit: 20% for 5min - Handles long loading screens, menu browsing

---

## Upgrade Path

### Option 1: Test V2 First (Recommended)

**Step 1: Test manually**
```bash
# Make executable
sudo chmod +x /home/user/Documents/System-Architecture/Ollama-Plex-Gaming/resource-manager-v2.sh

# Run in foreground (watch it work)
sudo /home/user/Documents/System-Architecture/Ollama-Plex-Gaming/resource-manager-v2.sh
```

**Step 2: Launch a game**
- Watch the logs in real-time
- Should see: "GPU high: XX% (counter: 20s / 30s)"
- After 30s sustained >50%: "GAMING DETECTED"
- Verify throttling happens

**Step 3: Close game**
- Watch GPU drop
- Should see: "GPU low: XX% (counter: 60s / 300s)" every minute
- After 5 minutes: "GAMING ENDED"
- Verify restoration happens

**Step 4: If tests pass, deploy to production**

### Option 2: Deploy Immediately

**Replace the current service:**

```bash
# Stop current service
sudo systemctl stop resource-manager

# Copy v2 to production location
sudo cp /home/user/Documents/System-Architecture/Ollama-Plex-Gaming/resource-manager-v2.sh \
        /usr/local/bin/resource-manager.sh

# Restart service
sudo systemctl restart resource-manager

# Verify running
systemctl status resource-manager
tail -f /var/log/resource-manager.log
```

### Option 3: Run Both (A/B Test)

Run V2 as a separate service for comparison:

```bash
# Copy v2 with different name
sudo cp /home/user/Documents/System-Architecture/Ollama-Plex-Gaming/resource-manager-v2.sh \
        /usr/local/bin/resource-manager-v2.sh
sudo chmod +x /usr/local/bin/resource-manager-v2.sh

# Create separate service
sudo nano /etc/systemd/system/resource-manager-v2.service
```

But this would cause **conflicts** (both trying to set Ollama limits), so **not recommended**.

---

## Testing Protocol for V2

### Test 1: GPU-Intensive Game (Cyberpunk 2077, Baldur's Gate 3)

**Expected behavior:**
1. Launch game
2. Within 30-60s: "GAMING DETECTED" (after loading)
3. Ollama throttled to 2GB
4. Play for 10+ minutes with various GPU loads
5. State remains "gaming" throughout (even during menu dips)
6. Quit game
7. After 5 minutes idle: "GAMING ENDED"
8. Ollama restored to 30GB

### Test 2: Low-GPU Game (Dwarf Fortress ASCII, Stardew Valley)

**Expected behavior:**
1. Launch game
2. GPU stays <20%
3. State remains "idle"
4. No throttling occurs ✓ (correct - no GPU conflict)

### Test 3: Emulator (RetroArch playing PS2 game)

**Expected behavior:**
1. Launch emulator + game
2. GPU usage ~40-70% (depends on emulation)
3. If >50% sustained 30s: "GAMING DETECTED" ✓
4. If <50%: No detection (might need lower threshold for emulators)

### Test 4: Video Playback (Not a Game)

**Expected behavior:**
1. Play 4K video locally (VLC, mpv)
2. GPU usage ~20-40% (hardware decode)
3. Brief spike might start counter
4. Should not sustain 30s >50%
5. No detection ✓ (correct - not a game)

**If videos trigger gaming state:**
- Increase `GAMING_ENTER_THRESHOLD` to 60-70%
- OR increase `GAMING_ENTER_DURATION` to 60s

### Test 5: Menu Browsing / Loading Screens

**Expected behavior:**
1. In game, enter menu (GPU drops to <10%)
2. Counter starts: "GPU low: 5% (counter: 20s / 300s)"
3. Exit menu, resume game (GPU back to 80%)
4. Counter resets
5. State remains "gaming" ✓ (hysteresis working)

---

## Monitoring V2

### Real-Time GPU Monitoring

**Watch GPU usage live:**
```bash
watch -n 1 cat /sys/class/drm/card1/device/gpu_busy_percent
```

**Watch resource manager decisions:**
```bash
tail -f /var/log/resource-manager.log
```

**Check current state:**
```bash
cat /tmp/resource-manager-state
```

**See GPU usage history:**
```bash
# If you have radeontop installed
radeontop -d /tmp/radeontop.log -l 10
# Logs 10 seconds of history
```

---

## Rollback Plan

If V2 doesn't work as expected:

```bash
# Stop V2
sudo systemctl stop resource-manager

# Restore V1 from backup
sudo cp /usr/local/bin/resource-manager.sh.v1-backup \
        /usr/local/bin/resource-manager.sh

# OR recreate V1 from documentation
sudo nano /usr/local/bin/resource-manager.sh
# Copy V1 code from PHASE-3-IMPLEMENTATION-PLAN.md

# Restart service
sudo systemctl restart resource-manager

# Verify
systemctl status resource-manager
```

**Best practice:** Before upgrading, backup V1:
```bash
sudo cp /usr/local/bin/resource-manager.sh \
        /usr/local/bin/resource-manager.sh.v1-backup
```

---

## Future Enhancements (V3?)

### 1. Multiple GPU Support

If you add more GPUs or have integrated graphics:

```bash
# Check all GPUs
for gpu in /sys/class/drm/card*/device/gpu_busy_percent; do
  usage=$(cat "$gpu")
  # Use max across all GPUs
done
```

### 2. VRAM-Based Detection

Some games use GPU minimally but load VRAM heavily:

```bash
# Check VRAM usage
VRAM_USED=$(cat /sys/class/drm/card1/device/mem_info_vram_used)
VRAM_TOTAL=$(cat /sys/class/drm/card1/device/mem_info_vram_total)
VRAM_PERCENT=$((VRAM_USED * 100 / VRAM_TOTAL))

# Gaming if: GPU >50% OR VRAM >30%
```

### 3. Application Whitelist/Blacklist

Explicitly allow/deny certain applications:

```bash
# Whitelist: Always treat as gaming
if pgrep -f "cyberpunk|baldurs.*gate" > /dev/null; then
  enter_gaming_state
fi

# Blacklist: Never treat as gaming (even if GPU high)
if pgrep -f "blender|davinci.*resolve" > /dev/null; then
  # Skip GPU check - this is work, not gaming
  continue
fi
```

### 4. Adaptive Thresholds

Learn typical GPU usage patterns:

```bash
# If GPU averages 30% during "idle" (video editing workflow)
# Adjust GAMING_ENTER_THRESHOLD dynamically
```

---

## Decision Matrix

**Should you upgrade to V2?**

| Your Situation | Recommendation |
|----------------|----------------|
| Only play Steam Proton games | V1 is fine, V2 is better |
| Play native Linux games | **V2 required** |
| Use emulators (RetroArch, etc.) | **V2 required** |
| Play text-based/low-GPU games | **V2 better** (won't trigger) |
| Want more reliable detection | **V2 better** |
| Want to avoid state flapping | **V2 better** (hysteresis) |
| Don't want to test/change | V1 is stable, keep it |

**Recommendation:** **Upgrade to V2** - It's more accurate and handles edge cases better.

---

## Summary

**V1 (Process-Based):**
- ✅ Works for most Steam/Lutris games
- ❌ Misses native games, emulators
- ❌ Can trigger on non-games
- ❌ State flapping on menu/loading
- Status: **Functional but limited**

**V2 (GPU-Based):**
- ✅ Detects ALL GPU-using games
- ✅ Ignores non-GPU games (correct behavior)
- ✅ Measures actual resource conflict
- ✅ Hysteresis prevents flapping
- ✅ More accurate and reliable
- Status: **Designed, ready to test**

**Next Steps:**
1. Test V2 manually with a game launch
2. Verify hysteresis works (menu browsing)
3. If tests pass, deploy to production
4. Update blog post with V2 approach

---

**Author:** Preston + Claude
**Version:** 2.0
**Date:** 2026-02-21
**Status:** Ready for Testing
