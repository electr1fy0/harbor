# Harbor robustness audit

The audit fixes **102 distinct defects**, counted by underlying failure rather
than input variants, repeated screens, or regression-test count.

| Area | Component findings | Evidence |
| --- | ---: | --- |
| Storage, router, capacity, health | 18 | [Core ledger](bug-audit-core.md) |
| Deployment, services, builder, runtime, sandbox | 27 | [Deployment ledger](bug-audit-deployment.md) |
| Dashboard, daemon, configuration, CLI | 17 | [API ledger](bug-audit-api.md) |
| Frontend, source, onboarding | 34 | [Frontend/onboarding ledger](bug-audit-frontend-onboarding.md) |
| Additional control-plane and example findings | 8 | [Round-two ledger](bug-audit-control-round2.md) |
| Cross-layer duplicates removed | -2 | GitHub HTTPS URL normalization and Git-ref/root validation |
| **Distinct total** | **102** | |

Each component ledger identifies the original faulty behavior, the change, and
regression coverage. Startup-constructor cancellation extends round-two item 4;
deployment log redaction extends frontend/onboarding item 7. Neither is counted
again. Repository-name validation is recorded only in frontend/onboarding item 32.

Implementation agents used Luna at max reasoning. Parent review and agent
cross-review checked their changes and corrected draft regressions involving
GitHub permission probing, notification baselines, socket cleanup, webhook glob
semantics, source-setting compatibility, log cursor reuse, and superseded restart
timers. Corrections to audit drafts are excluded from the defect count.

## Verification

The original Go package suite passed before changes. New core regression tests
were also run against a clean archive of the original revision and reproduced
forwarded-header spoofing, invalid route commits, DNS trailing-dot failures,
protocol/flush accounting errors, partial application records, cross-app data
associations, deletion and pagination failures, restart-plan errors, lost rollback
artifacts, incorrect capacity availability, and the nil-context health panic.

Final validation passed:

- `make test`: frontend production build, typecheck, 42 unit tests across five
  files, Go vet, Go package tests, CLI/dashboard/broken-build/example smoke
  checks, router checks, and all portable integration tests.
- `go test -race ./internal/... ./cmd/...`: passed.
- Portable integration covered zero-downtime redeployment and rollback,
  candidate-failure safety, SQLite persistence, and the complete deployment
  lifecycle, including crash handling and build timeout.
- Focused regression suites were repeated where scheduling or concurrency was
  involved; the example smoke script also passed in two concurrent runs.
- `git diff --check`: passed. A final read-only cross-review found no actionable
  regressions in restart admission/retry, health-policy snapshots, startup
  cancellation, stage timings, or scheduled backup behavior.

Some frontend changes are covered through request/schema/query helpers rather
than a DOM component harness. The frontend ledger identifies UI behavior that was
reviewed and typechecked without component regression tests. Privileged
namespace/systemd/cgroup acceptance checks require a prepared Linux host; the
current host is macOS and its Docker engine is unavailable.

## Additional hardening

- The normal Makefile test target now typechecks and unit-tests the frontend.
- Strict JSON parsing rejects duplicate keys and excessive nesting.
- Schema version 10 enforces immutable application ownership for releases and
  deployments and consistent event ownership. Existing rows are preserved.
- Configuration reads, diagnostic output, completed-process metadata, retained
  jobs, observation state, and filesystem scans have explicit bounds.
