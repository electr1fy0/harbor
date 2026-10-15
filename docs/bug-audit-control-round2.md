# Control-plane audit, round 2

This round records eight additional reproducible defects found after the 17
items in `bug-audit-api.md`. Earlier ledger entries are not recounted.

## Findings

1. **A production deployment could fall out of notification sampling.**
   `internal/daemon/observability.go` sampled only the latest 20 attempts. More
   than 20 newer failed candidates could push an older still-serving
   production deployment out of that page, so a later transition of that
   deployment would never be stored or notified. Sampling and baselining now
   include every unfinished attempt and follow previously observed active IDs
   through terminal transitions, even after a crash removes the route.
   `TestObservationSamplesProductionBeyondRecentAttempts` calls `Store.Finish`
   to exercise route removal and verifies notifications for an old production
   crash and an off-page candidate after 21 newer failed attempts.

2. **The CLI could cancel a deployment before the daemon's build deadline.**
   `cmd/paas/main.go` imposed a six-minute request deadline while
   `paasd -build-timeout` accepts longer durations. The dashboard also imposed
   a ten-minute deadline on its synchronous deploy and rollback calls. A build
   allowed by daemon configuration could therefore be canceled by its caller
   first. CLI deploy and dashboard deploy/rollback requests now have no fixed
   client deadline and still inherit caller cancellation; all other CLI and
   dashboard control deadlines remain unchanged. Regression tests
   `TestRequestContextLeavesDeployDeadlineToDaemon` and
   `TestSynchronousDeployAndRollbackUseCallerCancellation` cover the timeout
   policies and cancellation behavior.

3. **An empty daemon deploy source could resolve to the daemon working
   directory.** `internal/daemon/platform.go` accepted a request with no
   `SourceDir` and no `RepoURL`; `config.Load("")` then resolves `paas.yaml`
   relative to the daemon's current directory. The endpoint now rejects such
   requests before deployment. `TestPlatformDeployAcceptsGitHubFieldsAndRejectsMixedSources`
   includes the empty-source request.

4. **SIGTERM during daemon startup bypassed runtime cleanup.**
   `cmd/paasd/main.go` registered signal handling only after managed services
   had been restored and apps recovered, phases that can run for up to 11
   minutes. The default signal action could terminate the process before its
   deferred listener cleanup and explicit runtime/service shutdown ran. Signal
   handling now begins before startup restoration, cancels both startup
   contexts, skips recovery after an interrupted restore, and enters the normal
   cleanup path without serving traffic. `TestSIGTERMInterruptsStartupRestoreAndRecovery`
   sends SIGTERM while each phase is blocked and verifies cancellation.

5. **Fractional observation timestamps were not lexically chronological.**
   `time.RFC3339Nano` omits trailing fractional zeroes, but SQLite compares
   the stored TEXT values directly for history cutoffs and 24-hour retention.
   For example, `2026-10-15T12:00:00.1Z` sorts after the later
   `2026-10-15T12:00:00.19Z`; a cutoff at `.15Z` could therefore include the
   older sample and omit the newer one, while retention can make the inverse
   mistake. New samples and cutoffs use fixed-width UTC nanoseconds, and
   startup normalizes at most the retained 5760 legacy rows without losing
   subsecond precision. `TestObservationSampleTimestampFormatPreservesNanosecondChronology`
   and `TestObservationSampleTimestampMigrationNormalizesLegacyFractionalTimes`
   cover the ordering and on-disk upgrade.

6. **Observation deployment-state rows accumulated for the app's lifetime.**
   Every sampled deployment state was retained forever, and each pass scanned
   all rows to find deployments that had previously been active. Repeated
   deployments therefore caused unbounded metrics-database growth and
   increasingly expensive samples. After a complete pass, state retention now
   follows the recent history page plus active attempts and previously active
   deployments whose terminal transition was sampled. Failed or canceled
   passes do not prune state. `TestObservationStatesStayBoundedAcrossDeploymentHistory`
   verifies repeated batches stay within the history bound, and
   `TestObservationStatePruningWaitsForCompletePass` verifies an incomplete
   pass preserves state for a retry.

7. **The Node example exposed its application port on every host interface.**
   `examples/hello-node/server.js` listened on `0.0.0.0`, although the README
   requires applications to bind to `127.0.0.1` because app runtimes share
   the host network namespace and Harbor routes requests through its loopback
   proxy. The example now binds to loopback. `examples/hello-node/server.test.js`
   starts the real server on an ephemeral port, checks `server.address()` is
   `127.0.0.1`, and requests the health endpoint; `scripts/tests/examples.sh`
   runs it when Node is installed.

8. **The example smoke test could pass against another process and raced with
   parallel runs.** `scripts/tests/examples.sh` assumed ports 39081/39082 and
   shared predictable `/tmp` log and response files. If another listener on
   39081 returned the expected fixture responses, the test could pass even
   though its own `hello-go` process failed to bind; concurrent runs could also
   overwrite each other's files. The script now chooses two loopback ephemeral
   ports and creates a private temporary workspace that its cleanup removes.
   Two simultaneous runs both passed with distinct hello-node ephemeral ports.

## Validation

- `go test ./cmd/paas ./internal/daemon ./internal/dashboard` — passed after
  the shared request-envelope cap and dashboard timeout changes.
- `go test ./internal/daemon ./internal/deployment ./internal/store` — passed
  after extending observation to all unfinished attempts and route-removed
  transitions.
- `go test ./cmd/paasd` — passed with startup signal cancellation coverage.
- `go test ./internal/daemon -run 'TestObservation(State|SampleTimestamp|SamplesProduction)' -count=1` — passed.
