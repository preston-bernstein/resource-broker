# Tasks: resource-broker-canonical-logging

steps_hash: 92ec2ca770c2b194d930bf1c8769382e99624e1022fc0b2385f2c181e4b3506a
origin_behind: 0

- [!] Task 1: Confirm the logging seam and the outcome rule | Files: docs/resource-broker-canonical-logging/design-note.md | Deps:  | Parallelizable: no
- [!] Task 2: Capture the pre-change metrics baseline | Files: internal/metrics/testdata/metrics.golden, internal/metrics/metrics_golden_test.go | Deps:  | Parallelizable: yes
- [!] Task 3: Rework the logger core | Files: cmd/broker/main.go, cmd/broker/events.go, cmd/broker/logging_test.go | Deps: 1 | Parallelizable: no
- [!] Task 4: Give request handler lines events and outcomes | Files: internal/proxy/proxy.go, internal/openaicompat/handler.go, internal/proxy/proxy_test.go, cmd/broker/tests/preemption_log_test.go | Deps: 3 | Parallelizable: no
- [!] Task 5: Update legacy tests that assert the old log shape | Files: internal/proxy/proxy_test.go, internal/yield/yield_test.go, internal/yield/displace_test.go, internal/job/worker_test.go | Deps: 4 | Parallelizable: no
- [!] Task 6: Name all remaining log calls and log broker.started | Files: cmd/broker/main.go, internal/yield/yield.go, internal/yield/displace.go, internal/job/service.go, internal/job/worker.go, internal/queue/gate.go, internal/queue/park.go, internal/detect/detect.go, internal/httpx/httpx.go, internal/tdarr/tdarr.go | Deps: 3, 4, 5 | Parallelizable: no
- [!] Task 7: Delete the shim | Files: cmd/broker/main.go, cmd/broker/logging_test.go | Deps: 6 | Parallelizable: no
- [!] Task 8: Pin logging-check.py and wire it into CI | Files: Makefile, docs/runbooks/logging.md | Deps:  | Parallelizable: yes
  attempt: 2
- [!] Task 9: Acceptance contract test through logging-check.py | Files: cmd/broker/canonical_logging_test.go | Deps: 2, 4, 7, 8 | Parallelizable: no
- [!] Task 10: Secret and redaction test | Files: cmd/broker/redaction_test.go | Deps: 9 | Parallelizable: no
- [!] Task 11: Guard the Loki label config | Files: cmd/broker/deploy_labels_test.go, cmd/broker/testdata/loki_labels.yml | Deps:  | Parallelizable: yes
