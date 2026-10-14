# Harbor PaaS

Harbor deploys HTTP applications to a single machine:

    source → unprivileged build → immutable release → Bubblewrap + systemd → health check → hostname route

The target is Ubuntu/Debian Linux with Go, a C compiler (SQLite), Git, systemd,
cgroup v2, Bubblewrap, and curl. Linux runtimes use namespace isolation and
systemd resource limits. Go and Node runtimes, managed SQLite services, and
GitHub HTTPS source deployments are supported. Caddy provides public HTTPS for
configured custom domains and an authenticated dashboard. Apps and operators
must be trusted; applications share the host network and runtime account.

## Web dashboard

The complete React dashboard is embedded in the Go `paasd` executable. The same
process serves the UI, authenticates dashboard API requests, and manages
applications. Production needs no separate web service or Bun/Node runtime.
The UI uses TanStack Router, Query, Form, and Table with the Base UI shadcn preset.

Build from source with `make build` (Bun 1.4+ is required on the build machine),
then start the daemon using the local development commands below. Open
<http://127.0.0.1:3000>. The default dashboard listener is numeric loopback;
`-web-listen` selects another loopback address and `-web-listen ''` disables it.
The CLI continues to use the private Unix socket.

For frontend development, run `bun install` and `bun run dev` inside `web/`.
Vite serves <http://127.0.0.1:5173> and proxies API calls to the daemon on port
3000. See [web/README.md](web/README.md) for the development and verification steps.

Import a repository by browsing a GitHub owner or entering its HTTPS URL. Connect
a fine-grained personal access token in the dashboard to browse and clone private
repositories; public URLs work without a connection. Harbor stores the token
encrypted in its private data directory and returns only connection metadata.

Select a branch, tag, or full commit SHA and a repository-relative root directory.
Harbor uses an existing `paas.yaml`, or detects supported Go/Node layouts and lets
you review generated settings. Add runtime environment values and optionally
create or bind a managed SQLite service, then follow asynchronous deployment
progress. Saved GitHub source details support retry without resetting application
settings. See [docs/onboarding.md](docs/onboarding.md) for permissions and API details.
GitHub OAuth and webhook-triggered deployments are not implemented. Legacy daemon
`GITHUB_TOKEN` configuration is supported; a dashboard connection is preferred.
Repository checkouts reject symlinks, path traversal, non-GitHub hosts, and embedded
URL credentials; Git must be installed on the daemon host.

Public access requires an exact HTTPS dashboard origin, an admin password hash,
and a session signing secret; invalid or partial configuration fails startup.
Generate the compatible scrypt hash with `paasd -hash-password`. See
[deploy/README.md](deploy/README.md) for the single systemd service and Caddy setup.
The dashboard API executes within the privileged daemon process and retains
strict Host/Origin validation, signed sessions and bounded login attempts.

Project settings support globally unique custom domains, write-only runtime
environment values, and resource limits. Domain changes update routing
immediately. Environment and resource changes apply on the next deployment,
rollback, or daemon recovery. Values and their redaction history are encrypted
with a daemon-owned `environment.key` (mode `0600`); keep that key with secure
backups of the data directory. Settings responses contain environment key names
only, and application diagnostics redact configured values, including previous
values after rotation or removal.

The **Capacity** page reports host memory/CPU, filesystem and Harbor storage
growth, app limits and measured usage, and active systemd build usage. Unsupported
measurements stay unavailable. See [docs/capacity.md](docs/capacity.md) for the
sampling boundaries and the deployment headroom estimate.

Project diagnostics show the active production deployment separately from the
latest attempt. Failed candidates keep the healthy version serving traffic and
show a bounded, redacted log excerpt with available retry, settings, and rollback
actions. See [docs/diagnostics.md](docs/diagnostics.md) for the evidence and action
boundaries.

## Linux setup and demo

    make build
    sudo ./scripts/setup-linux.sh
    sudo ./bin/paasd

The daemon runs in the foreground and prints its socket/router address once
ready. Startup normally takes less than a second; no application is built
until deploy. In another terminal:

    sudo ./bin/paas ping
    sudo ./bin/paas deploy ./examples/hello-go --name hello
    curl -H 'Host: hello.localhost' http://127.0.0.1:8080/
    sudo ./bin/paas apps
    sudo ./bin/paas releases hello
    sudo ./bin/paas inspect hello
    sudo ./bin/paas logs hello
    sudo ./bin/paas deploy ./examples/hello-go --name hello
    sudo ./bin/paas rollback hello
    sudo ./bin/paas stop hello

The control socket is owner-only by default, so the CLI also uses sudo with a
root daemon. `-socket-group GROUP` grants a validated existing group access with
mode `0660`; use a dedicated group containing trusted administrators only.
Builds run as `paas-build` in disposable workspaces with a timeout and systemd
build limits (512 MiB, 128 tasks, one CPU, no swap). Apps run as `paas-runtime`.
The default build timeout is five minutes, configurable with `-build-timeout`;
cold Go builds on small VMs can take minutes even though daemon startup is quick.
Releases under `/var/lib/paas/releases` are daemon-owned and read-only; deleting
the original source does not affect a running app. The setup script creates
accounts/directories and, where needed, a scoped Bubblewrap AppArmor profile
for the runtime and Linux probes; it does not disable global user-namespace restrictions.
Go module/build caches persist under the daemon data directory. Node builds also
keep npm, pnpm, and Yarn caches in per-application directories there, selected
through `npm_config_cache`, `npm_config_store_dir`, `pnpm_config_store_dir`,
`YARN_CACHE_FOLDER`, and `YARN_GLOBAL_FOLDER`. These are build caches; runtime
dependencies still need to be present in the published release.
Runtime supervision keeps a 250 ms polling interval; systemd-backed runtimes are
queried in batches of up to 32 units per manager call. Build caches persist until
manually pruned. Remove `build-cache` only when no builds are active; it is
recreated automatically by the next build.

## Application contract

Every source directory needs `paas.yaml`. A module-based Go app can use:

    name: hello
    runtime: go
    build:
      command: go build -o "$PAAS_OUT/server" .
    start:
      command: /app/server
    resources:
      memory: 128Mi
      cpu: 0.5
      tasks: 64
    health:
      path: /health

The current start contract is exactly `/app/server`; the build must create an
executable at `$PAAS_OUT/server`. Examples compile `main.go` instead of `.`
because they share the repository's module. Source/output symlinks and special
files are rejected. Apps must honor injected `PORT` and bind to `127.0.0.1`.
The default Linux backend enforces memory, CPU, and task limits through cgroup v2. Omit `health.path`
for TCP readiness; otherwise HTTP 200–399 is accepted. Routing follows readiness.

Deploy is synchronous; inspect can show intermediate states while it runs.
Success returns a release ID, port, and URL. Failed deployments persist
BUILD_FAILED, START_FAILED, or UNHEALTHY with errors/logs, and get no route.
Unexpected runtime exits become CRASHED; memory-limit kills become OOM_KILLED.
Exits of the active runtime withdraw its route. Unknown hosts
return 404. The logs command combines build output with up to 500 journal entries, bounded to 1 MiB; rotation/streaming are not implemented.

Redeployment builds and checks a candidate on a separate loopback port before
atomically switching traffic. Candidate failures preserve the active version.
The proxy tracks in-flight requests to the replaced runtime and waits for them
to finish before stopping it, with a 30-second drain deadline. Longer requests
and streams may be interrupted at that deadline. Rollback (`paas rollback NAME
[--release ID]`) starts an immutable release without recompilation and runs its
saved readiness check before switching traffic. Release history and promotion
timestamps persist in SQLite; lifecycle event write failures are logged. Logs
shows the newest deployment attempt, including failed candidates. Legacy releases
without recorded readiness settings must be redeployed before rollback.

Graceful daemon shutdown stops its apps, then managed databases. Startup restores
managed databases before recovering desired-running apps from their last
promoted immutable releases. Failed service recovery leaves dependent apps with
diagnostics while independent apps recover. Explicitly stopped apps stay stopped.
Stop preserves diagnostics from completed failed attempts and cleanup failures.
Linux defaults to the systemd backend and fails startup if its prerequisites
are missing. `-sandbox=bubblewrap` provides namespace isolation without resource
limits; `-sandbox=host` explicitly selects host execution without those controls.
Inspect reports the selected backend and whether it enforces resource limits.
Non-root Linux execution requires a user systemd manager with delegated CPU,
memory, and task controllers; missing delegation is a deployment error.
Run `make test-privileged` after setup to verify the root daemon configuration.
Applications share the host network and runtime account.

Transient systemd units can survive an abrupt daemon crash. The next startup
reclaims persisted application units before marking their deployments stopped;
a cleanup failure prevents startup rather than losing ownership of a live unit.
Graceful shutdown stops and waits for each runtime. Failed cleanup is recorded
and retried, and replacement is blocked until it succeeds.

## Managed SQLite services

Create and link a service with `paas add sqlite --name db` and
`paas link APP db`. Each application binding has its own persistent SQLite file
and writable directory. Harbor injects a `file:` SQLite URI in `DATABASE_URL`
with read/write mode and the `_busy_timeout=5000` and `_foreign_keys=on` options
used by the included `go-sqlite3` example. The binding directory is mounted
writable at the same absolute path in the app runtime so SQLite can create its
WAL and journal sidecar files. Relinking an application, redeploying, rolling
back, and daemon recovery all reuse that file.

Applications must use a SQLite driver that accepts SQLite file URIs and honors
the driver options in `DATABASE_URL`; PostgreSQL drivers and PostgreSQL URLs do
not work with this service. Other SQLite drivers must accept the file URI and
`mode=rw`, then configure a five-second busy timeout and foreign keys on each
connection using that driver's supported options. The example uses
[`github.com/mattn/go-sqlite3` v1.14.52](https://github.com/mattn/go-sqlite3),
which bundles SQLite 3.53.4 and requires CGO plus a C compiler at build time.
Keep bundled SQLite libraries current: the WAL-reset corruption issue described
in the [SQLite WAL documentation](https://www.sqlite.org/wal.html#walreset) is
fixed in SQLite 3.51.3 and later. Store Harbor's SQLite volumes on a local
filesystem; SQLite WAL does not support network filesystems.

Existing PostgreSQL service records and data volumes are preserved, but Harbor
does not start PostgreSQL or convert PostgreSQL data into SQLite. To migrate an
existing application, create a replacement application and SQLite service,
transfer its schema and data explicitly, then cut over after validation.

## Local macOS development

    make build
    ./bin/paasd -socket /tmp/paasd.sock -data-dir /tmp/harbor-dev -listen 127.0.0.1:8080
    ./bin/paas --socket /tmp/paasd.sock deploy ./examples/hello-go --name hello

Non-root development runs builds/apps under your account, without Linux
privilege separation or systemd build limits. A writable data directory is
required; `/var/lib/paas` is the Linux default. `PAASD_SOCKET` selects the CLI
socket as an alternative to repeating `--socket`.

## Testing on WSL

Use WSL 2 with Ubuntu, systemd enabled, and cgroup v2. Keep the checkout in
the Linux filesystem (for example `~/harbor`), not under `/mnt/c`.
Check `wsl --list --verbose` in PowerShell. Inside Ubuntu, check:

    ps -p 1 -o comm=
    test -f /sys/fs/cgroup/cgroup.controllers && echo "cgroup v2 available"

PID 1 should be `systemd`. If it is not, edit `/etc/wsl.conf` with sudo and
merge these settings into the existing file:

    [boot]
    systemd=true

Then run `wsl --shutdown` in PowerShell and reopen Ubuntu. This shuts down
all running WSL distributions. See Microsoft's
[WSL systemd instructions](https://learn.microsoft.com/en-us/windows/wsl/systemd).

Install dependencies in Ubuntu:

    sudo apt update
    sudo apt install -y build-essential git curl bubblewrap systemd apparmor apparmor-utils

Install Go 1.23 or newer using the
[official Linux installation instructions](https://go.dev/doc/install).
Use a system-wide installation: both your shell and `sudo` must find Go
(for an installation under `/usr/local/go`, expose its executable in
`/usr/local/bin`). Confirm `go version` and `sudo go version` work.

    git clone https://github.com/electr1fy0/harbor.git
    cd harbor
    make build
    sudo ./scripts/setup-linux.sh
    make test
    go test -race ./...
    make test-privileged

Run the suites sequentially. `make test` includes a real deployment test;
`test-privileged` additionally verifies dedicated identities, effective
systemd build constraints, and denied release writes. Linux probes may
report SKIP if WSL lacks a usable user systemd manager; that is not proof
that those capabilities passed. Bubblewrap failures indicate a host setup problem. Cycle 2 acceptance checks
actual namespaces, runtime identity, cgroup limits, journal output, and OOM
classification on Linux.
WSL-specific acceptance has not yet been run on a WSL installation.

After the suites finish, use the Linux setup/demo commands above for a
manual deployment. Keep the daemon running in one Ubuntu terminal and use
the CLI and curl in another. No Windows hosts-file change is needed when
curl supplies the `Host` header explicitly.

## Verification

    make test
    go test -race ./...
    make integration
    make test-privileged

Run setup first on Linux. `make test` runs vet, unit tests, CLI smoke tests,
fixture checks, router/Linux feasibility probes, and real deployment acceptance
tests. Linux-only probes explicitly skip on macOS. `test-privileged` tests the
root daemon, dedicated build/runtime identities, and denied release writes.
Acceptance covers source-independent runtime, immutable artifacts, health-gated
routing, unknown hosts, failures, inspect/logs/stop, crash withdrawal, persisted
history, candidate failure safety, continuous redeploy/rollback traffic, and
duplicate-daemon protection. A fresh clone reproduces the demo
using the setup/build commands above.

The examples stay as regression fixtures: healthy Go server, broken build,
unhealthy server, and memory hog. The memory hog verifies OOM classification with the default Linux backend.
Host and direct Bubblewrap backends do not enforce runtime resource limits.
