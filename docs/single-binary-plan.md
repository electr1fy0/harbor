# Single-binary Harbor dashboard migration

## Objective

Ship the complete dashboard and deployment engine in the same `paasd` executable
and process. Production needs no Bun/Node runtime, source checkout, separate
frontend directory, or dashboard service. Keep React and the existing dashboard
experience. Bun remains a frontend development/build dependency.

## Architecture and boundaries

- Compile the frontend as a Vite React SPA using TanStack Router and Query.
  Embed its complete `web/dist` output in the Go executable with `go:embed`.
- Add a dedicated numeric-loopback dashboard listener (default 127.0.0.1:3000)
  inside paasd, alongside its existing app router and optional TLS ask listener.
  Caddy continues to terminate HTTPS; the dashboard does not share the app router.
- Implement Go auth/session endpoints and a dashboard API. Dispatch dashboard
  operations directly to the daemon's existing handler inside the process;
  neither a Bun server nor Unix-socket forwarding is needed for the dashboard.
- Keep the Unix socket for CLI administration, including owner/group permissions.
  Do not expose the raw control API over the dashboard listener.
- Preserve safe dashboard response projections: no host artifact paths, process
  identifiers, raw deployment errors, GitHub credentials, or environment values.
- The dashboard now executes inside the privileged daemon. Preserve strict
  Host/Origin checks, signed cookies, fail-closed configuration, bounded login
  attempts/hash concurrency, bounded JSON, and explicit operation routing.
- Keep existing public auth environment names and scrypt password hash format.
  Provide password hashing through the Go executable, removing target-host Bun.

## Work packages

1. Frontend (Luna, max): replace Start/Nitro and server functions with an SPA
   entry and typed same-origin fetch functions; preserve validation, views and
   route/query behavior; add transport tests and update frontend instructions.
2. Dashboard API (Luna, max): implement endpoint validation and Go projections
   matching existing frontend types, including settings, diagnostics, capacity,
   GitHub browsing and deployment job workflows; add mapping/boundary tests.
3. Auth and assets (Luna, max): implement Go authentication, session cookies,
   request access checks and static/SPA serving with tests; embed generated assets.
4. Integration (primary): expose the daemon handler for trusted in-process use,
   add listener lifecycle and flags to paasd, update build and service packaging,
   migrate installation/configuration documentation, and add process-level tests.
5. Review and validation (primary, with Luna follow-ups as needed): inspect all
   changes for auth bypass, unsafe response data, SPA routing, stale runtime
   dependencies, startup/shutdown regressions, and frontend/API contract drift.
   Fix findings before committing the reviewed implementation.

## HTTP contract

- GET /api/auth/status -> {mode: "local" | "public", authenticated: boolean}
- POST /api/auth/login {password} -> {ok: boolean, message?: string}
- POST /api/auth/logout {} -> {ok: true}
- POST /api/dashboard/<operation> takes the existing frontend input object
  directly and returns its existing typed result. Errors use a non-2xx status
  with {error: string}. Requests use same-origin browser credentials.
- Operations: getOverview, getApp, getAppSettings, configureApp, getCapacity,
  deployApp, retryGitHubDeployment, getDeploymentJob, stopApp, rollbackApp,
  listGitHubRepos, getGitHubRepo, getGitHubConnection, connectGitHub,
  disconnectGitHub, inspectProject, submitOnboardingJob, getOnboardingJob,
  cancelOnboardingJob.

## Verification and acceptance

- Frontend: generate routes, TypeScript, lint, unit tests and production build.
- Go: vet, unit tests, race tests and existing local integration/acceptance suites.
  Linux-specific privilege/systemd probes may skip on macOS and must be reported.
- Security: public login/logout, incorrect passwords, signed-session rejection,
  Origin/Host checks, partial config, unauthorized API access, body/rate bounds,
  missing asset/API behavior and response secret/path filtering.
- Executable smoke: build assets and paasd; copy only the executable into an empty
  temporary directory, run without a frontend server, and request real HTML,
  compiled JS/CSS, deep links, auth and live dashboard API/CLI data. Confirm
  graceful shutdown and listener/config errors.
- Inspect the actual SPA in a browser against the Go listener if browser tooling
  is available. Check a deep link, login flow and empty/connected dashboard.
- Update packaging to run only paasd behind the existing Caddy dashboard target.
  Commit source and tests after review and successful applicable checks.

## Expected outcome and measurement limits

The executable grows by the frontend asset size. Runtime removes the separate
JavaScript server and its forwarding/rendering overhead. This change does not
claim a measured memory or latency improvement; browser React execution remains.

## Implementation and verification results

Completed the SPA, Go auth/static handler, all 19 dashboard operations, in-process
control dispatch, public/private GitHub browsing, listener lifecycle, hidden
password-hash command, and single-service deployment packaging. Removed the Bun
production server and documented the migration from harbor-web.service.

Luna agents at max reasoning implemented the three work packages and performed
cross-reviews. The primary agent reviewed their changes and added executable,
listener, strict-input, hashing-concurrency and live-deployment API tests. Review
findings fixed before commit include normal Host ports, index.html redirect
loops, unsupported regex syntax, null/case-folded JSON validation, and queue
backpressure status propagation.

Passed verification:

- Frontend route generation, frozen install, typecheck, lint, 13 tests, build.
- `make test`: vet/build/unit checks, CLI/fixture/router checks, single-binary
  local/public smoke, and all applicable deployment integration checks.
- `go test -race ./...`; focused race checks repeated after validation fixes.
- Clean source-tree `go test ./...` using only the committed embed scaffold;
  the compiled-assets listener test explicitly skips until assets are built.
- Live deployment integration through both CLI and dashboard: matching active
  release, write-only environment settings, safe response projection and stop.
- Final executable smoke from an empty directory without Bun/Node on PATH.
- Browser verification of rendered projects, capacity data and a project deep
  link, with no browser warnings/errors observed.

Linux-only Bubblewrap, systemd/cgroup/OOM and Node runtime probes skipped on
macOS; privileged Linux acceptance was not run. Vite reports a non-blocking large
JavaScript chunk advisory. No measured memory/latency improvement is claimed.
