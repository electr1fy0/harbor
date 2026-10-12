# Managed SQLite implementation plan

Replace the managed PostgreSQL service with file-backed SQLite. Harbor's own
SQLite state database remains separate from application databases.

## Design

- `paas add sqlite --name NAME` provisions a persistent service volume without
  an OCI image, database server, listening port, or database credentials.
- Each application/service binding gets an isolated database and writable
  directory inside the service volume. Re-linking preserves the database.
- Deploy, redeploy, rollback, and recovery inject a SQLite file URI through
  `DATABASE_URL` and expose the binding directory at the same absolute path
  across host, Bubblewrap, and systemd runtimes. Mount the directory so SQLite
  journals and WAL sidecars work. Root deployments grant the runtime identity
  access without granting the build identity access to application data.
- SQLite readiness checks validate existing files and never silently replace
  missing or corrupt databases during recovery. Use WAL and a busy timeout for
  overlapping deployments and concurrent requests.
- New managed services support SQLite only. Existing PostgreSQL records and
  volumes are preserved, marked unavailable, and reported as requiring an
  explicit application/data migration. PostgreSQL data is not SQLite data.
- Onboarding and the web UI use `sqliteService` / `createSqlite`; documentation
  and examples describe the file-based driver contract.

## Implementation assignments

1. Luna max: SQLite provider, service lifecycle and binding provisioning,
   shared path/URI helpers, PostgreSQL runtime removal, daemon construction,
   and focused lifecycle/binding tests.
2. Luna max: deployment injection and writable mounts, runtime permissions,
   redaction, SQLite demo fixture and build wiring, persistence acceptance tests,
   and documentation.
3. Luna max: onboarding request/validation/orchestration, web form and API
   schema, associated backend/frontend tests.

## Review and validation

Review all diffs for persistent data preservation, idempotent linking,
missing/corrupt database failures, permissions, binding isolation, and recovery.
Run formatting, Go vet/unit/race checks and the repository acceptance workflow;
run frontend tests, type checking, and build. Linux-only verification is reported
as skipped if the current host cannot run it. Commit the reviewed change after
checks pass.

## Completed implementation and review

Three Luna agents at max reasoning implemented the assignments above. The
primary agent reviewed their changes and required corrections for provisioning
retry cleanup, unsafe sidecar files and hard links during root permission
changes, stale READY state after canceled recovery, and the acceptance harness
reading an old router address after daemon restart. Obsolete service event
writes were removed because the event table requires deployment identities.

The SQLite driver is pinned to `go-sqlite3` v1.14.52 (bundled SQLite 3.53.4),
which includes the WAL-reset corruption fix. Other application drivers must
configure their own equivalent busy timeout and foreign-key settings.

Validation completed on macOS:

- `make test`: vet, builds, Go unit tests, CLI smoke tests, example checks,
  router probes, and acceptance tests passed. The SQLite acceptance counter
  persisted through re-link, redeploy, rollback, stop/redeploy, and daemon
  restart. Linux-only probes and acceptance cases skipped on this host.
- `go test -race ./...`: passed.
- Frontend: all 25 tests, type checking, production build, and formatting of
  changed files passed. The build emitted dependency directive
  warnings and completed successfully.
- Go formatting and `git diff --check`: passed.

Linux systemd/Bubblewrap runtime access and privileged assertions are covered
by the implementation and acceptance tests but were not executed on macOS.
Existing PostgreSQL volumes are preserved and require an explicit schema/data
migration into a replacement application and SQLite service.
