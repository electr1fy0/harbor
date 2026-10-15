# Deploy Ink as a standalone node

The original `electr1fy0/ink` at commit
`4f7bada` builds successfully but cannot run under Harbor's generated Go
configuration. It requires a config ID argument, reads `config<ID>.yaml` from
the working directory, ignores Harbor's assigned `PORT`, and writes its WAL
beside the executable. Harbor starts `/app/server` without arguments, publishes
only build outputs, and mounts `/app` read-only. The immediate failure is
`Usage: ink <config_id>`; connection refusals are the resulting readiness symptom.

Standalone operation also exposes an infinite loop in `Ring.GetNodes` when
fewer than three physical nodes exist, and the original two-ack write quorum
rejects writes with only one node.

## Apply the Ink fix

The fix is available on Ink's `main` branch through
[pull request #1](https://github.com/electr1fy0/ink/pull/1). The same changes are
packaged as [ink-harbor.patch](../deploy/patches/ink-harbor.patch), based on commit
`4f7bada`. Alternatively, apply that patch in an unchanged Ink checkout:

```sh
git apply /path/to/harbor/deploy/patches/ink-harbor.patch
go test -race -timeout=60s ./...
go vet ./...
```

The patch includes `paas.yaml`, selects standalone mode when `PORT` is supplied
without a config argument, listens on `127.0.0.1:$PORT`, and uses one local
acknowledgement only in standalone mode. Existing config-ID cluster commands
retain two-ack writes. Ring traversal is bounded by one pass through its virtual
nodes, and the replication acknowledgement count is read under its mutex.
Startup checks that its WAL is writable before serving health checks.

In Harbor, select Ink's `main` branch and deploy it using the
included `paas.yaml`. A rollback to the original release still contains the
incompatible executable; it does not apply this source fix. The source fix is
published; redeploying Ink and updating the Harbor daemon are separate steps
needed to update running installations.

## Storage

Platform mode defaults to `/tmp/ink-standalone`. On Linux this is temporary
sandbox storage; stop/start and redeployment discard it. This default is suitable
for a demo. `INK_DATA_DIR` selects another directory, but does not create a
Harbor volume or mount. Durable operation requires an explicitly mounted,
writable directory outside the sandbox's temporary `/tmp`, with permissions for
the runtime account. Harbor currently exposes managed SQLite bindings rather
than a general app-volume setting; Ink's custom WAL does not use `DATABASE_URL`.

Use a single writer and stop the previous process before restarting against
the same persistent WAL. Overlapping live candidates can recover stale in-memory
state; Ink does not support sharing one WAL across simultaneous processes.

## Verification

The reproducible host test starts a separate Harbor daemon with disposable state:

```sh
make build
python3 scripts/tests/ink.py /path/to/patched/ink
```

It checks deployment and routing, HTTP readiness, standalone writes/reads/deletes,
WAL value and tombstone recovery across stop/start using an external host data
directory, and continued traffic to the working release after a candidate exits
with code 7. This host test does not claim Linux mount persistence.

Validation completed on 15 October 2026:

- Ink: `go test -race -count=1 -timeout=60s ./...` and `go vet ./...`, including
  the existing distributed-delete cluster test and new standalone/ring tests.
- Harbor: `make test` and `go test -race ./...` on macOS. The suite explicitly
  skips Linux-only acceptance cases on macOS.
- The real Ink deployment test above passed through Harbor's builder, release
  store, runtime, health checks, router, settings, and start/stop operations.
- A separate Ubuntu systemd/Bubblewrap probe used the Linux Ink executable,
  read-only `/app`, runtime user `nobody`, 128 MiB memory, 0.5 CPU, and 64 tasks.
  It checked readiness and CRUD, temporary WAL loss on restart, and WAL value
  and deletion recovery across restart with an explicit writable host mount.

Harbor's startup checks now inspect runtime liveness after a failed readiness
probe. A confirmed exit stops that readiness polling attempt and records its
exit code with a pointer to runtime logs. Its existing bounded start retries and
failed-candidate route protection remain in effect.
