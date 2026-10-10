# Harbor PaaS

Harbor deploys local Go HTTP applications to a single machine:

    source → unprivileged build → immutable release → host process → health check → hostname route

The target is Ubuntu/Debian Linux with Go, a C compiler (SQLite), Git, systemd,
cgroup v2, Bubblewrap, and curl. Runtime namespace isolation, runtime resource
limits, safe replacement/rollback, PostgreSQL, Node, Git URL deployment, and
public HTTPS are not implemented yet. Apps must be trusted: this is a local
development platform, not multi-tenant hosting.

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
    sudo ./bin/paas releases hello
    sudo ./bin/paas inspect hello
    sudo ./bin/paas logs hello
    sudo ./bin/paas rollback hello
    sudo ./bin/paas stop hello

The control socket is owner-only, so the CLI also uses sudo with a root daemon.
Builds run as `paas-build` in disposable workspaces with a timeout and systemd
build limits (512 MiB, 128 tasks, one CPU, no swap). Apps run as `paas-runtime`.
The default build timeout is five minutes, configurable with `-build-timeout`;
cold Go builds on small VMs can take minutes even though daemon startup is quick.
Releases under `/var/lib/paas/releases` are daemon-owned and read-only; deleting
the original source does not affect a running app. The setup script creates
accounts/directories and, where needed, a scoped Bubblewrap AppArmor profile
for the Linux probes; it does not disable global user-namespace restrictions.

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
Runtime resource fields are validated but not enforced yet. Omit `health.path`
for TCP readiness; otherwise HTTP 200–399 is accepted. Routing follows readiness.

Deploy is synchronous; inspect can show intermediate states while it runs.
Success returns a release ID, port, and URL. Failed deployments persist
BUILD_FAILED, START_FAILED, or UNHEALTHY with errors/logs, and get no route.
Unexpected runtime exits become CRASHED and withdraw the route. Unknown hosts
return 404. Logs returns the last 1 MiB; rotation/streaming are not implemented.

Deployments support safe, zero-downtime candidate redeployment: new versions
are built, started, and health-checked on an isolated loopback port before traffic
is atomically switched. Failed candidate builds or unhealthy startups never disturb
the active version. Replaced deployments undergo a graceful draining grace period
before receiving SIGTERM. Rollback (`paas rollback <name>`) instantly reverts to a
previous release from immutable history without recompilation. Structured lifecycle
events and release history are audited persistently in SQLite.

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
that those capabilities passed. Bubblewrap failures indicate a host setup
problem even though the current application runtime does not use it.
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
history, and duplicate-daemon protection. A fresh clone reproduces the demo
using the setup/build commands above.

The examples stay as regression fixtures: healthy Go server, broken build,
unhealthy server, and memory hog. Do not deploy the memory hog expecting runtime
memory enforcement yet; its cgroup spike verifies host feasibility separately.
