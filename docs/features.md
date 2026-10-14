# Harbor operations and automation

These operational features extend Harbor's existing single-machine Go,
SQLite, immutable-release, systemd/Bubblewrap, and hostname-router design.

## Controls and deployment history

Start resumes a promoted artifact. Restart starts a candidate from that artifact
without rebuilding, health-checks it, and switches traffic only after readiness.
Candidate cancellation preserves production; Stop cancels app work and records
an explicit stopped desired state. Runtime settings have revisions; unapplied
changes appear on the project page. Project archive, favorite, tags, and required
environment keys are durable metadata. Archive does not stop the app.

All deployment attempts are paginated globally and per app, including failed
builds. Event timelines and stage timings explain progress without double-counting
startup/readiness or including later crashes in deployment duration. Repository
job history also records checkout time. Logs support
incremental cursors, deployment/source selection, pause/follow, search, and
redacted downloads. Systemd provides separate runtime logs; host execution uses
a shared file and cannot always separate build/runtime output.

Ongoing health probes are separate from deployment readiness. Automatic crash
recovery is opt-in, persisted, uses increasing delays and bounded attempts, and
respects explicit stop. Capacity preflight checks required keys and estimates
build/candidate memory overlap. Limits are ceilings, not reservations.

Retention preserves active, previous promoted, and in-flight releases. Cache
pruning coordinates with build locks. Bulk dotenv import previews key names and
never executes interpolation. Tiny/Small/Medium resource presets populate the
existing limit form for review before saving.

## GitHub and CI

The Automation tab edits repository, ref, root, build/start/health selection,
build timeout, and changed-path filters. Explicit build variables are stored
encrypted and redacted. A cache-free redeploy action is available. The encrypted
job registry survives daemon restart; queued work is recovered with bounds and
interrupted running jobs are reported as failed. Resolved Git metadata is
attached to repository deployment jobs.

Create a branch-specific hook and save its one-time secret. Listing returns
metadata only; rotation and revocation invalidate previous secrets.

* CI: POST `/api/hooks/deploy/<id>` with `Authorization: Bearer <secret>`.
  `Idempotency-Key` or `X-Delivery-ID` can deduplicate retries.
* GitHub: POST application/json to `/api/hooks/github/<id>`, select push events,
  and configure the secret in GitHub. Harbor validates the HMAC, branch and
  repository, applies path filters, deduplicates delivery/commit, and deploys
  the exact pushed SHA.

Callbacks retain trusted-host checks and use hook credentials instead of a
browser session. Project deletion cancels/removes jobs and source hooks before
removing the app. Private repositories use the existing GitHub connection.

## Data services

The Services page supports standalone SQLite creation, link/unlink, database
and WAL sizes, consistent manual backups, hourly/daily/weekly scheduled backups
with retention, integrity checks, download, and clone restore. Binding changes
apply on the next restart/deploy; unlink preserves files. Restore stages the
snapshot before atomically creating a new app/binding and never overwrites
production. Snapshots use SQLite VACUUM INTO and are checked before publication.

Snapshots are limited to 2 GiB. Inline downloads are limited to 2 MiB; larger
snapshots remain available for server-side verification and clone restore.
Encrypted Harbor export requires a 12–1024 byte passphrase and supports selected
app databases plus a consistent control snapshot, configuration, observability,
and required encryption keys. Exports use scrypt and AES-GCM and are bounded to
48 MiB before encryption. The export-only API response cap is 70 MiB; ordinary
responses remain limited to 4 MiB. Save the passphrase separately. Export is a
download; it does not upload backups to off-host storage. Decrypt an export
offline with `paas export-decrypt INPUT.enc OUTPUT.tar.gz --passphrase-stdin`.
Provide the passphrase as one line on stdin, ideally from a password manager
or protected input. Decryption authenticates the archive and writes a new file
with mode 0600; it never overwrites an existing file or extracts into live state.

## Observability and routing

Capacity history is sampled every 15 seconds, retained for up to 24 hours, and
plotted for memory, CPU, and tasks. Missing measurements remain unavailable.
Traffic counters include status codes, latency and response-body bytes for the
current daemon lifetime; hijacked stream bytes are excluded.

HTTPS notification URLs are encrypted and write-only. Select lifecycle events,
host memory/disk thresholds, and cooldowns. Delivery outcomes are recorded with
bounded retention. Outbound notification connections resolve and pin
public addresses and reject private targets. Diagnostic bundles include safe
settings, redacted logs/summary, and recent capacity history.

Maintenance serves HTTP 503 with Retry-After for each application's local route.
Project deletion clears stored routing/notification settings. Applications use
generated `.localhost` hostnames; custom domains, DNS/HTTPS diagnostics, and
primary-domain redirects have been removed.

## Feature coverage

| Requested features | Implementation/dashboard location |
|---|---|
| 1–3 | Start/restart, configuration revisions, pending banner |
| 4–5 | Permanent Redeploy, independent candidate/job cancellation |
| 6–7 | Explicit deletion/data choice, archive |
| 8–10 | Global/per-app attempt history, events, stage durations |
| 11–12 | CI bearer hooks, signed GitHub push callbacks |
| 13–18 | Source/build editor, encrypted build variables, timeout, cache-free deploy, dedupe, path filters |
| 19–20 | Durable jobs and resolved Git metadata |
| 21–25 | Cursor logs, deployment/source selection, search, redacted download |
| 26–30 | Historical charts, traffic metrics, notifications, thresholds, diagnostic bundle |
| 31–33 | Service creation, binding management, database/WAL sizes |
| 34–37 | Manual/scheduled snapshots, clone restore, encrypted export |
| 38–40 | Release retention, cache pruning, backup integrity/staleness |
| 41–43 | Dotenv import, required keys, resource presets |
| 44–45 | Removed: custom-domain diagnostics and primary redirects |
| 46 | Maintenance mode on the generated local route |
| 47–49 | Ongoing probes, opt-in crash recovery, capacity preflight |
| 50 | Project tags/favorites and archive/search filters |

## Verification

Run `make test`, `go test -race ./...`, and the frontend typecheck, lint, tests,
and build. Linux-specific systemd/cgroup and privileged tests require the
configured Linux environment and are explicitly skipped on macOS.
