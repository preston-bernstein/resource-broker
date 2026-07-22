# Spec Challenge Notes — embed-lane-parking

## Agents run
- Requirements Auditor (haiku): 14 found, 12 accepted
- Scope & Dependency Auditor (sonnet): 7 found, 7 accepted (incl. stale-prereq deletion)
- Design Devil's Advocate (sonnet): 6 found, 5 accepted — headline: polling drain replaces YieldEnd()
- Implementation Realist (sonnet): 12 found, 12 accepted — headline: YieldEnd busy-loop proof; ghost-entry lockout
- Steps & Sequencing Critic (sonnet): 11 found, 11 accepted
- State-Model Critic (sonnet): 12 found, 11 accepted
- Security/Resilience Auditor (haiku): 5 found, 4 accepted (1 stale)

## Changes made
- **Design replaced**: yield-end signaling via Controller/interface widening → plain 1s-ticker polling drain (`RunParkDrain(ctx, yielding func() bool)`). Kills the proven busy-loop bug class, preserves yield.Controller as the graded-yield seam, avoids Admission widening + 3-fake churn + gate_cancel_test breakage, and removes yield.go from the Plex-WIP collision set.
- **Ghost-entry cleanup mandated**: every non-release park exit splices its entry out under park.mu (prevents depth drift + permanent park lockout under long yields). Regression test specified.
- **Kill-switch made real**: BROKER_PARK_MAX_QUEUE=0 = parking off (was silently ignored per the old guard shape); documented as first-line rollback.
- **Test hardening**: concurrent-ceiling -race, per-tick pacing, yield-flap bounded-drain, Interactive-class FR-10 test, re-pointed Batch test as fail-closed pin, full-repo -race gate.
- **Operator visibility**: /status gains parked depth; soak runbook uses POST /control mode=yield.
- Stale .gitignore prerequisite deleted (fixed upstream in e565edc); config defaults decided (600s/32/8); ADR-0009 scope pinned (supersedes ADR-0002 for batch-during-yield only, answers its objections).

## Critiques rejected
- Dropping the class label debate beyond removal (done); demanding strict end-to-end FIFO service order (documented as release-order guarantee instead — single-caller reality).
- Security auditor's "blocking .gitignore" finding — stale, already fixed before the audit ran.

## Open questions requiring human input
- None blocking. Deploy timing already constrained by Preston (drain-end window).
