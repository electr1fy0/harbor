# Frontend and onboarding bug audit

This ledger records 34 distinct, reproducible issues fixed in `web/**`,
`internal/source/**`, and `internal/onboarding/**`. Closely related UI variants
and shared truncation helpers are grouped once. GitHub URL normalization and
Unicode Git-ref acceptance are cross-layer issues; coordinate their counts
with the API/dashboard ledger rather than counting each layer separately.

## Findings

1. **GitHub repository URLs rejected valid normalized HTTPS forms.**
   `web/src/lib/validation.ts`, `web/src/routes/new.tsx`,
   `internal/source/github.go`. Uppercase scheme/host and the default `:443`
   port were rejected by validation or canonicalization even though they
   identify the same HTTPS GitHub host. Validation and canonicalization now
   accept these forms and the source parser canonicalizes them. Coverage:
   `TestCanonicalGitHubURLNormalizesSchemeHostAndDefaultPort` and
   `Harbor input validation / accepts GitHub URLs with normalized host casing
   and explicit HTTPS port`. **Cross-reference API/dashboard URL-normalization
   finding; count once across layers.**

2. **The browser's Git-ref rules rejected valid refs and measured length in
   UTF-16 instead of UTF-8 bytes.** `web/src/lib/validation.ts`. The ASCII-only
   allowlist blocked valid Unicode names, while JavaScript's `.max(255)` alone
   would accept a multibyte name that exceeded the daemon's 255-byte limit.
   Validation now mirrors Git's forbidden syntax and caps UTF-8 bytes.
   Coverage: `Harbor input validation / accepts valid Unicode Git refs that
   the daemon supports` and `limits Git ref length in UTF-8 bytes like the
   daemon`. Dashboard validation shares the checkout validator; **count once
   across layers**.

3. **An empty data directory could resolve GitHub credential paths from the
   current working directory.** `internal/source/github_credentials.go`.
   Calling token/disconnect operations on a store constructed without a data
   directory could address credential filenames relative to `.`. The store
   now fails closed. Coverage:
   `TestGitHubCredentialStoreWithEmptyDataDirectoryNeverUsesWorkingDirectory`.

4. **An empty data directory could read or write onboarding source settings in
   the current working directory.** `internal/onboarding/sources.go`. The
   source store now keeps an empty path and fails closed instead of resolving
   `onboarding-sources.json` relative to `.`. Coverage:
   `TestSourceStoreWithoutDataDirectoryFailsClosed`.

5. **Webhook path-filter glob conversion diverged from path semantics.**
   `internal/onboarding/path_filters.go`. The regex translator split Unicode
   literals into bytes, used dot-based `**` matching that failed on newline
   path components, and compiled negated character classes that could consume
   `/`. Matching now uses `path.Match` on individual components and a
   component-level globstar matcher, while rejecting invalid UTF-8. Negation
   normalization only rewrites the first class character, preserving literal
   `[` and `!` members. These related conversion failures are grouped as one
   glob-matching issue.
   Coverage: `TestWebhookPathFiltersMatchUnicodePaths` includes Unicode
   literals, newline paths, zero-directory `**`, and negated-class separator
   cases, and literal class members.

6. **Cancel and Forget left stale job IDs in the bounded queue.**
   `internal/onboarding/manager.go`. Repeatedly canceling queued jobs or
   forgetting an app could leave dead IDs in the channel and make later
   submissions fail despite no active work. Both paths now compact queued IDs.
   Coverage: `TestForgetRemovesQueuedJobsFromBoundedQueue`,
   `TestManagerConcurrentQueuedCancelSubmitAndWorkerStart`, and
   `TestManagerQueueBoundAndJobHistoryLimit`.

7. **Truncating logs before secret redaction could expose a secret suffix.**
   `internal/onboarding/manager.go`. If the beginning of a secret fell before
   the tail boundary, truncation removed the part needed for detection and
   left the suffix visible. Logs are now redacted before tail truncation.
   Coverage: `TestSafeLogTailRedactsSecretsBeforeTruncating`.

8. **Bounded package-manifest decoding could accept only a valid JSON prefix.**
   `internal/onboarding/project.go`. Decoding just the first JSON value from a
   capped reader ignored trailing JSON/garbage and could miss bytes beyond the
   cap. The reader now consumes at most limit+1 and unmarshals the complete
   bounded document. Coverage: `TestInspectRejectsTrailingPackageJSONData`
   and `TestPackageManifestDecoderRejectsDataPastSizeLimit`.

9. **A valid environment sample line exactly at the read limit was discarded.**
   `internal/onboarding/project.go`. The scanner's token limit equaled the
   whole-file cap, so a line using the full allowance was reported too long
   and its key omitted. The scanner now permits the cap plus one byte and
   reports actual over-limit input. Coverage:
   `TestEnvironmentSampleAcceptsSingleLineAtSizeLimit` and
   `TestEnvironmentSampleWarnsWhenKeyListIsTruncated`.

10. **Unknown GitHub webhook IDs caused their request bodies to be read.**
    `internal/onboarding/automation.go`. The handler consumed up to the body
    limit before checking whether the hook existed, allowing unauthenticated
    traffic to force avoidable reads. Hook lookup now precedes body reading.
    Coverage: `TestUnknownGitHubHookDoesNotReadRequestBody`.

11. **GitHub repository browsing silently returned an incomplete list above
    1,000 repositories.** `internal/source/github_api.go`. Hitting the page
    ceiling looked like a complete result to the UI. The API now probes beyond
    the ceiling and returns an explicit overflow error; credential setup uses
    a one-item scope probe instead of enumerating all repositories. Coverage:
    `TestGitHubRepositoryListReportsWhenPageCapWouldTruncateResults` and
    `TestGitHubCredentialStoreEncryptsTokenAndUsesPrivateFiles`.

12. **Byte-bounded text truncation could split Unicode characters.**
    `internal/source/github_api.go`, `internal/onboarding/manager.go`.
    Repository descriptions, branch metadata, and log tails were sliced at
    arbitrary byte boundaries, yielding invalid UTF-8/replacement characters.
    Both prefix and suffix truncation now stop at rune boundaries. Coverage:
    `TestBoundedTextDoesNotSplitUTF8AtLimit` and
    `TestTailStringDoesNotSplitUTF8AtLimit`.

13. **The observability history selector did not affect the request.**
    `web/src/components/app/app-observability.tsx`, `web/src/lib/features.ts`.
    The query key changed when the user selected “Last 24 hours,” but the
    request always asked the daemon for its default one-hour window. The
    selected window is now sent. Coverage: `observability history requests /`
    `includes the selected history window in the daemon request`.

14. **Background refresh errors hid previously loaded data.**
    `web/src/components/app/app-observability.tsx`,
    `web/src/components/app/app-operations.tsx`,
    `web/src/routes/deployments.tsx`, `web/src/routes/services.tsx`, and
    `web/src/routes/projects.$name.tsx`. A transient refresh error could replace
    usable cached settings, history, service lists, or job status with an error
    screen/unavailable badge. Screens now retain cached data and show a refresh
    warning. This is one repeated React Query error-state handling defect.
    The test environment is Node-only (no DOM/component harness); query helper
    behavior is covered by `deploymentJobStatus` tests, while screen-level
    rendering was checked by typecheck/build rather than DOM regression tests.

15. **A transient deployment-job poll error permanently stopped polling.**
    `web/src/lib/queries.ts`. The interval callback returned `false` whenever
    `query.state.error` was set, so an in-progress deployment could remain
    stale forever until manual reload. Polling now retries more slowly on
    errors and stops only for terminal states. Coverage:
    `deployment job polling / retries transient errors and stops only for
    terminal states`.

16. **Searching logs repeatedly lowercased the full output for every match.**
    `web/src/components/app/advanced-logs.tsx`, `web/src/lib/features.ts`.
    For a large log with many matches, the old loop repeatedly allocated and
    scanned the entire bounded output, causing work proportional to
    text-size × match-count. Search now uses one escaped global case-insensitive
    regex pass and caps highlights at 2,000. Coverage:
    `log search / returns bounded, non-overlapping, case-insensitive source
    spans` (no timing-sensitive performance test).

17. **Case-insensitive log highlighting used transformed-string offsets.**
    `web/src/components/app/advanced-logs.tsx`, `web/src/lib/features.ts`.
    Lowercasing can expand characters such as `İ`, so adding the search string
    length to a lowercased match offset skipped original characters and
    corrupted highlighted output. The new matcher returns spans against the
    original string. Coverage: `log search / keeps source offsets correct when
    lowercase conversion expands Unicode`.

18. **Successful unrelated source actions cleared the build-environment draft.**
    `web/src/components/app/app-automation.tsx`. The shared success callback
    cleared the dotenv editor after actions such as redeploy/ref updates even
    when no environment replacement was submitted. It now clears only after a
    successful source update containing `buildEnvironment`. Coverage:
    `build environment draft handling / clears the draft only after a
    successful environment replacement`.

19. **Runtime secret drafts were cleared before the save result.**
    `web/src/components/app/project-settings.tsx`. The form cleared the
    key/value immediately after starting a mutation, losing the only visible
    copy on failure. It now clears after a successful environment update only.
    Coverage: `build environment draft handling / preserves runtime secret
    drafts after unrelated settings updates`.

20. **In-flight form success could overwrite edits typed after submission.**
    `web/src/components/app/app-automation.tsx`,
    `web/src/components/app/app-observability.tsx`,
    `web/src/components/app/environment-import.tsx`,
    `web/src/components/app/service-tools.tsx`. Inputs whose successful
    mutation resets or replaces local state are now disabled while pending, so
    a user cannot type a second draft that the first result then clears,
    including the service-name field whose create success clears it. There
    is no DOM test environment; this UI-only behavior was verified by code
    review and typecheck/build, not a component regression test.

21. **Selecting an invalid environment file left an earlier parsed import
    available.** `web/src/components/app/environment-import.tsx`. Oversized or
    unreadable replacement files could leave the prior parsed values and
    “Import” action active, causing the wrong file's secrets to be submitted.
    New selection now clears old preview/parsed values before reading and
    reports read failures. The UI flow has no DOM regression test in the
    current Node-only Vitest setup.

22. **Changing GitHub owners left the old repository URL in the deploy form.**
    `web/src/routes/new.tsx`. Owner change cleared the selected repository but
    left the prior URL in form state; the stale URL could still be submitted
    despite no visible repository selection. The URL is now cleared with the
    selection. The UI flow has no DOM regression test in the current Node-only
    Vitest setup.

23. **Late asynchronous callbacks overwrote newer source/environment drafts.**
    `web/src/components/app/app-automation.tsx`, `web/src/routes/new.tsx`, and
    `web/src/components/app/environment-import.tsx`. A repository inspection
    could finish after its source draft changed, or a slow `File.text()` could
    replace a later file selection or text typed while it was reading. Inspection
    results now compare their request snapshot, and file reads use a generation
    guard before applying success or error state. Coverage: `async inspection
    and request bounds / rejects inspection results for a stale source draft`
    and `ignores file reads and failures after a newer draft replaces them`.
    The async guard is unit-tested; file-input rendering itself is not covered
    because this Vitest setup has no DOM/component harness.

24. **Valid dotenv source text could expand beyond the daemon's JSON body cap.**
    `web/src/lib/features.ts`, `web/src/components/app/app-automation.tsx`.
    Escapes not decoded by the dotenv parser (such as `\\u`) are escaped again
    by `JSON.stringify`, so a document below the source-byte limit could become
    an over-64-KiB API request. Separately, Zod's string `.max()` counted
    UTF-16 code units for runtime environment values while the daemon caps
    UTF-8 bytes, so multibyte values could pass the browser and fail server-side.
    The parser checks serialized environment size with room for the app name,
    source settings check the complete serialized action, and shared value
    validation enforces the daemon's byte cap. Coverage: `async inspection and
    request bounds / bounds JSON-encoded dotenv values, not only their source
    text` and `Harbor input validation / limits environment values in UTF-8
    bytes like the daemon`. The same envelope mismatch also affected dashboard
    configure and onboarding calls and `paas configure --env-stdin`: per-value
    and raw-input limits did not include the wrapped daemon request fields. A
    shared `contracts.MaxControlRequestBytes` now defines the daemon cap; the
    dashboard rejects oversized serialized control requests with 413 before
    dispatch, and the CLI checks the encoded request before connecting.
    Coverage: `TestEnvironmentRequestEnvelopeOversizeIsRejectedBeforeControl`
    and `TestRunConfigureRejectsSerializedRequestAboveDaemonLimitBeforeConnecting`.

25. **The dotenv preview accepted environment keys the daemon would reject.**
    `web/src/lib/features.ts`. The importer accepted arbitrarily long names,
    while shared request validation and daemon configuration limit keys to 128
    ASCII characters. Preview now rejects longer names before submission.
    Coverage: `dotenv import / matches the daemon's 128-character environment
    key limit`.

26. **Changing log views let late responses corrupt the active cursor.**
    `web/src/components/app/advanced-logs.tsx`, `web/src/lib/features.ts`. One
    mutable cursor was shared across deployment/source selections, so an older
    request could replace the new view's cursor and make later polls restart
    from the tail. Cursors are now isolated by view identity and older responses
    cannot overwrite a newer request for that view. Coverage: `advanced log
    cursors / isolates deployment/source identities and ignores late responses`.
    The cursor helper is unit-tested; the screen has no DOM/component harness.

27. **An explicit Node package-manager declaration was overridden by another
    tool's lockfile.** `internal/onboarding/project.go`. When package.json named
    npm but the checkout also had a pnpm lockfile, lockfile priority selected
    pnpm and generated pnpm commands, ignoring the declared toolchain. The
    declared manager now takes precedence and only its own lockfile selects a
    frozen install; unsupported declarations fail even when another lockfile
    exists. Coverage: `TestDeclaredNodePackageManagerWinsOverOtherLockfiles`
    and `TestUnsupportedDeclaredNodePackageManagerIsNotHiddenByLockfile`.

28. **A malformed persisted AES-GCM nonce could panic the daemon.**
    `internal/onboarding/protected_store.go`. Corrupt encrypted state with a
    nil, short, or long nonce reached `cipher.AEAD.Open`, which panics on a
    nonce-size mismatch. The store now validates the nonce length and returns
    a storage error. Coverage:
    `TestProtectedStoreRejectsMalformedNonceWithoutPanicking`. The GitHub
    credential store already validates its nonce length before decrypting.

29. **Git subprocess diagnostics were retained without a size bound.**
    `internal/source/github.go`. `exec.Cmd.CombinedOutput` accumulated all
    stdout and stderr in memory, even if a remote sent a huge error stream.
    Git output is now continuously drained into a fixed-size buffer, reports
    truncation, preserves process cancellation, and redacts tokens even when a
    token crosses the retention boundary. Coverage:
    `TestRunGitBoundsCommandOutputAndRedactsPartialTokenAtLimit` and
    `TestRunGitCancellationKillsForkedChildWithinBound`.

30. **Redeployment ignored explicit source configuration when the repository
    contained `paas.yaml`.** `internal/onboarding/project.go` and
    `internal/onboarding/manager.go`. Inspection defaults were persisted with
    source settings, and configured preparation returned a selected config
    only in memory; the deploy lifecycle then reloaded the repository file and
    discarded explicit build/start/health settings. The code now preserves
    repository authority for inspection defaults, persists explicit source
    overrides into the disposable checkout before deployment, and records
    override provenance so legacy auto-saved defaults remain compatible after
    upgrade. Coverage: `TestPrepareConfiguredAppliesPersistedSelectionOverRepositoryConfig`,
    `TestManagerRedeployUsesSavedSourceAndPreservesConfiguredSettings`, and
    `TestManagerRedeployAppliesExplicitSavedSourceOverrides`.

31. **Source settings and their encrypted build environment could be observed
    or persisted in mismatched versions.** `internal/onboarding/manager.go`,
    `internal/onboarding/sources.go`, and `internal/onboarding/automation.go`.
    A write failure between the regular source file and encrypted store could
    leave a partial update, and source readers could combine different
    versions during redeploy/webhook processing. Updates now serialize under
    the manager lock, restore the prior store on a failed second write, and
    load source plus environment from one snapshot; Forget stages job/hook
    removals and compacts the queue only after persisted changes commit, with
    rollback errors returned and the process failing closed if rollback also
    fails. Hook creation shares the same lock, so a concurrent Forget cannot
    leave a hook for a deleted source. Coverage: `TestUpdateSourceLeavesSettingsUnchangedWhenProtectedStoreIsUnreadable`,
    `TestUpdateSourceRollsBackSecretsWhenSourceSaveFails`,
    `TestSaveSourceForCurrentJobDoesNotPartiallyUpdateSettings`,
    `TestForgetRestoresSourceAndInMemoryJobsWhenProtectedCommitFails`,
    `TestForgetRemovesQueuedJobsFromBoundedQueue`, and
    `TestCreateHookSerializesWithForget`.

32. **Valid GitHub repository names beginning with punctuation were rejected.**
    `internal/source/github.go`, `internal/dashboard/api.go`,
    `internal/dashboard/api_mappers.go`, and `web/src/routes/new.tsx`.
    Names such as `.github`, `_repo`, and `-repo` are valid path segments, but
    the repository validators required an alphanumeric first character. Source,
    dashboard, and browser validation now accept leading punctuation while
    rejecting `.` and `..` path segments. Coverage:
    `TestGitHubRepositoryMapperAcceptsLeadingPunctuationNames`,
    `TestGitHubRepositoryValidationAcceptsLeadingDotNames`, and
    `Harbor input validation / accepts valid GitHub repository names beginning
    with punctuation` (the shared browser parser is also used by the
    new-deployment route). The API/dashboard implementation is cross-owned;
    count this finding once across layers.

33. **An active oldest job prevented terminal history from being pruned at the
    100-job cap.** `internal/onboarding/manager.go`. When the oldest record was
    still active, pruning stopped rather than removing later terminal records,
    so retained history could exceed the documented cap while that job ran.
    Pruning now skips active records and removes old terminal history without
    discarding live jobs. Coverage:
    `TestPruneKeepsActiveOldestJobAndRemovesOldTerminalHistory`. This is a
    bounded history-cap violation, not an unbounded-growth claim.

34. **Unreadable saved source settings were reported as if no source existed.**
    `internal/onboarding/manager.go`, `internal/daemon/onboarding_routes.go`,
    `web/src/lib/features.ts`, and `web/src/components/app/app-automation.tsx`.
    Corrupt or inaccessible source storage was mapped to `ErrJobNotFound` and
    returned as a normal empty state, hiding an operational failure with no
    retry path. Only an absent record now maps to not-found; storage failures
    use a server-error status, and the UI shows the failure with a retry action.
    Coverage: `TestGetSourceDistinguishesMissingSettingsFromUnavailableStorage`,
    `TestAutomationGetSourceDistinguishesMissingSettingsFromCorruptStorage`,
    and `saved source load status / treats a missing source as an empty state
    and storage failures as errors`. The Node-only frontend test setup has no
    DOM harness, so retry rendering is covered by typecheck/code review rather
    than a component test.

## Validation

- `go test ./internal/onboarding ./internal/source ./internal/daemon` — passed.
- `go test -race ./internal/onboarding ./internal/source` — passed.
- `cd web && bun run typecheck` — passed.
- `cd web && bun run test` — passed, 42 tests across 5 files.
- Targeted `bunx prettier --check` for changed frontend files — passed.
- Full repository checks are left to the parent integration pass; a broader
  frontend Prettier check previously reported existing issues in untouched
  `src/components/ui/sonner.tsx` and generated `src/routeTree.gen.ts`.
