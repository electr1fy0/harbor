# Dashboard, daemon, configuration, and CLI bug audit

This ledger records 17 distinct defects corrected in these surfaces. Cases and routes that share one underlying failure are grouped together; parser depth and request-input limits are recorded as supporting hardening rather than separate defects.

1. **Invalid local health paths passed configuration validation.** `internal/config/config.go`: paths beginning with `//` or containing malformed escapes passed `Validate` but could not be probed by the loopback health checker, turning a configuration error into failed deployment attempts. Validation now rejects paths with URL authorities or parse errors. Regression tests: `TestValidateRejectsHealthPathsThatCannotBeProbedLocally`, `TestLoadValidatesHealthPathDuringDeployment`.

2. **Equivalent GitHub HTTPS URLs were rejected.** `internal/dashboard/api.go`: scheme and host comparison was case-sensitive and did not accept the standard explicit HTTPS port, so canonical-equivalent repository URLs failed validation. URL parsing now accepts case-insensitive `https://github.com` and port 443 while rejecting credentials, other hosts/ports, query strings, and fragments. Regression test: `TestValidGitHubRepoURLAcceptsEquivalentHTTPSAuthority`.

3. **A missing app could be returned as an empty successful app.** `internal/dashboard/api.go`: `getApp` treated each internal 404 as an optional missing subsection, including when every app resource was absent. It now returns not-found when inspect, releases, settings, and diagnostics all report 404. Regression test: `TestAPIGetAppReturnsNotFoundWhenNoAppResourcesExist`.

4. **Daemon clients accepted invalid response documents.** `internal/daemon/client.go`: `Execute` decoded only the first JSON value, accepted `null` as an empty response, and ignored trailing JSON or garbage. It now requires one non-null response object within the response bound. Regression test: `TestDecodeExecuteResponseRequiresOneBoundedObject`.

5. **Ping treated any HTTP 200 body as a healthy daemon.** `internal/daemon/client.go`: any text or an empty body from a different/misrouted Unix-socket service made `Ping` succeed. It now requires the daemon's `pong` payload and caps the health response. Regression test: `TestPingRequiresTheDaemonHealthPayload`.

6. **Large CLI environment input was read without an aggregate bound.** `cmd/paas/main.go`: `configure --env-stdin` scanned per-line-bounded input into an unbounded map before the daemon request limit was checked. Input is now read under a 64 KiB aggregate limit. Regression test: `TestReadEnvironmentRejectsAggregateInputBeyondDaemonRequestLimit`.

7. **`link` silently ignored extra positional arguments.** `cmd/paas/main.go`: `link APP SERVICE EXTRA` still linked the first two arguments and exited successfully. The parser now requires exactly two operands. Regression coverage: `TestParseCommandRequestRejectsInvalidArguments`.

8. **Some successful CLI commands hid stdout failures.** `cmd/paas/main.go`, `cmd/paas/export.go`: add/link and export-decrypt ignored errors while printing their success confirmation and exited 0 even when the caller could not receive the result. These paths now report the write failure and return nonzero. Regression tests: `TestRunReportsConfirmationWriterFailuresForServiceCommands`, `TestOfflineExportDecryptionAuthenticatesAndNeverOverwrites`.

9. **Duplicate JSON keys bypassed exact-input validation.** `internal/strictjson`, `internal/dashboard`, and `internal/daemon`: raw-map validation collapsed duplicate names while Go's struct decoder merged repeated nested objects, allowing the raw validator and effective request to see different values. A shared strict parser now rejects duplicate keys at every depth and bounds nesting; dashboard and daemon handlers use it before decoding. Regression tests: `TestAPIStrictJSONKeysAndNullRollbackNeverReachControl`, `TestPlatformRejectsMalformedRequests`, `TestValidateRejectsDuplicateKeysAtEveryDepth`, `TestValidateBoundsNestingAndRejectsMalformedClosings`.

10. **Concurrent daemon starts could remove a live socket.** `internal/daemon/server.go`: two processes could both classify the same socket as stale before either bound it, letting the later process unlink the first listener's path. A nonblocking per-socket lock now serializes startup and remains held for the listener lifetime. Regression tests: `TestConcurrentListenersSerializeStaleSocketReplacement`, `TestSocketLockLivesThroughCloseAndUnlink`.

11. **Daemon shutdown could unlink a replacement path.** `internal/daemon/server.go`: unconditional cleanup by path could remove a new file or socket installed after the daemon bound its listener. The listener now disables unconditional unlink and removes only the original socket inode while holding the path lock. Regression test: `TestServeDoesNotRemoveReplacementSocketPath`.

12. **Observation settings beyond the first 1,000 were silently ignored.** `internal/daemon/observability.go`: `allSettings` used a query limit with no matching write-side cap, so later apps lost restored traffic policy and sampling. The query now loads every saved app setting. Regression test: `TestObservationSettingsAreNotSilentlyCappedAtOneThousand`.

13. **Configure could race with observability forget cleanup.** `internal/daemon/observability.go`: `configure` persisted and applied traffic policies outside the mutex used by `sample` and `forget`, allowing an in-flight configure to recreate a policy after a successful forget. Configure now holds the same lock through its transaction and policy update. Regression test: `TestObserveConfigureSerializesWithForget`.

14. **Configuring notifications replayed existing deployment failures.** `internal/daemon/observability.go`: a newly configured webhook could receive failures that happened before the hook existed. Configure now atomically baselines states for deployments already in its history; legacy persisted settings are baselined at startup only when that app has no observation state, while genuinely new deployment IDs still notify on their first sample. Regression tests: `TestObservationFirstSampleBaselinesExistingFailures`, `TestObservationFirstSampleNotifiesNewFailureAfterConfiguration`.

15. **Dashboard checkout validation rejected valid Git refs and accepted invalid source roots.** `internal/dashboard/api.go`: its ASCII-only ref pattern rejected checkout-supported names such as `修复/naïve`, while allowing `.git` roots that the checkout layer later rejects. Dashboard validation now shares the checkout validator's ref and root rules. Regression test: `TestAPIInspectProjectUsesCheckoutReferenceRules`.

16. **Execute hid HTTP failures without an error payload.** `internal/daemon/client.go`: a non-200 response with a valid JSON object but no `Error` field became a blank CLI error. The client now falls back to the HTTP status. Regression test: `TestExecuteReportsStatusWhenErrorMessageIsMissing`.

17. **Untrusted source configuration had no read bound.** `internal/config/config.go`: `Load` used `os.ReadFile` on repository-controlled `paas.yaml`, allowing a very large config file to consume daemon memory before parsing. File reads and `LoadBytes` now reject documents over 1 MiB, with `TestLoadBoundsConfigurationFileSize`.
