package config

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// The deployed unit must raise the batch lane above concurrency 1.
//
// 2026-09-17: LightRAG's embedding worker queued 10 calls at a time against a
// MaxInflight=1 lane. Each call then waited ~300s (the BROKER_BATCH_WAIT cap)
// while a single embedding takes 0.07s of real work, LightRAG's own 2400s
// worker timeout fired, and its pipeline halted and marked 51 documents failed.
// Every nightly retry rebuilt the same jam, so the corpus sat stale for 18 days.
// ADR-0004 is explicit that concurrency-1 is "a conservative default, not a
// law; the only law is yielding to gaming" — the yield controller is untouched
// by this setting, so raising it cannot let inference stutter a game.
func TestDeployedUnitRaisesBatchConcurrency(t *testing.T) {
	unit, err := os.ReadFile("../../deploy/broker.service")
	if err != nil {
		t.Fatalf("reading deploy/broker.service: %v", err)
	}
	match := regexp.MustCompile(`(?m)^Environment=BROKER_MAX_INFLIGHT=(\d+)$`).FindSubmatch(unit)
	if match == nil {
		t.Fatal("deploy/broker.service does not set BROKER_MAX_INFLIGHT, so the batch lane " +
			"falls back to the concurrency-1 default that stalled the corpus ingest")
	}
	got, err := strconv.Atoi(string(match[1]))
	if err != nil {
		t.Fatalf("BROKER_MAX_INFLIGHT is not a number: %q", match[1])
	}
	if got < 2 {
		t.Errorf("BROKER_MAX_INFLIGHT=%d still serializes the batch lane; want >= 2", got)
	}
}
