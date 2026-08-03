# Tasks: Embed-Lane Request Parking

Generated from: docs/embed-lane-parking/ on 2026-07-22 (steps consolidated into compile-atomic tasks)

## Status legend
- [ ] pending / [>] in progress / [x] done / [!] blocked

## Tasks

### Task A: park.go + scheduler.go (steps 1+2)
**Status**: [ ] pending
**Files**: internal/queue/park.go, internal/queue/scheduler.go
**Test**: go build ./internal/queue/ + existing scheduler tests pass
**Depends on**: none | **Parallelizable**: yes | **Model**: sonnet
**Notes**:

### Task B: metrics.go RecordPark + gauges (step 4)
**Status**: [ ] pending
**Files**: internal/metrics/metrics.go
**Test**: go build; existing metrics tests pass
**Depends on**: none | **Parallelizable**: yes | **Model**: haiku
**Notes**:

### Task C: config.go + config_test.go (steps 5+6)
**Status**: [ ] pending
**Files**: internal/config/config.go, internal/config/config_test.go
**Test**: go test ./internal/config/ (defaults, overrides, kill-switch=0 valid, <=0 warnings, 900s budget assertion)
**Depends on**: none | **Parallelizable**: yes | **Model**: sonnet
**Notes**:

### Task D: broker.service env lines (step 13)
**Status**: [ ] pending
**Files**: deploy/broker.service
**Test**: grep 3 BROKER_PARK_ lines; Restart=always still present
**Depends on**: none | **Parallelizable**: yes | **Model**: haiku
**Notes**:

### Task E: soak runbook (step 16)
**Status**: [ ] pending
**Files**: docs/runbooks/embed-parking-soak.md
**Test**: file exists, uses POST /control mode=yield/auto with token header
**Depends on**: none | **Parallelizable**: yes | **Model**: haiku
**Notes**:

### Task F: skills docs (steps 14+15)
**Status**: [ ] pending
**Files**: .claude/skills/broker-config-and-flags/SKILL.md, .claude/skills/broker-architecture-contract/SKILL.md
**Test**: grep BROKER_PARK_ rows + parking invariant line
**Depends on**: none | **Parallelizable**: yes | **Model**: haiku
**Notes**:

### Task G: gate.go rewrite + fakes + re-point (steps 3+8+9)
**Status**: [ ] pending
**Files**: internal/queue/gate.go, internal/queue/gate_test.go, internal/queue/gate_wait_test.go, internal/queue/gate_yield_test.go (comment only), internal/queue/gate_cancel_test.go (RecordPark fake if Recorder widened)
**Test**: go test ./internal/queue/ all pass
**Depends on**: A, B | **Parallelizable**: no | **Model**: sonnet
**Notes**:

### Task H: AC test suite (step 7)
**Status**: [ ] pending
**Files**: internal/queue/gate_yield_test.go, internal/queue/scheduler_test.go
**Test**: go test -race ./internal/queue/ all pass incl. ghost-cleanup, concurrent ceiling, per-tick pacing, flap-bounded, Interactive FR-10
**Depends on**: G | **Parallelizable**: no | **Model**: sonnet
**Notes**:

### Task I: main.go wiring (step 10)
**Status**: [ ] pending
**Files**: cmd/broker/main.go
**Test**: go build ./...; smoke run with BROKER_PARK_MAX_QUEUE=5 starts+stops clean
**Depends on**: A, C, G | **Parallelizable**: yes | **Model**: sonnet
**Notes**:

### Task J: admin /status parked (step 11)
**Status**: [ ] pending
**Files**: internal/admin/admin.go
**Test**: go build; /status JSON includes parked
**Depends on**: A | **Parallelizable**: yes | **Model**: haiku
**Notes**:

### Task K: ADR-0009 + ADR-0002 status + CONTEXT.md + README (step 12)
**Status**: [ ] pending
**Files**: docs/adr/0009-embed-lane-parking.md, docs/adr/0002-stateless-http-bounded-wait.md, CONTEXT.md, README.md
**Test**: grep supersession line, Park/Drain burst glossary entries, 3 env rows + alert expression
**Depends on**: none (content decided in plan) | **Parallelizable**: yes | **Model**: haiku
**Notes**:

### Task L: final gate (step 17)
**Status**: [ ] pending
**Files**: (none — verification)
**Test**: go vet ./... clean; go test -race ./... clean
**Depends on**: all | **Parallelizable**: no | **Model**: orchestrator
**Notes**:

## Blocked / open
(populated during implementation)
