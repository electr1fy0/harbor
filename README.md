# Harbor PaaS

Harbor is a single-node PaaS built as a sequence of working vertical slices.
The current repository is the Cycle 1 / Person 1 pass: repository contracts, a
Unix-socket daemon ping, example applications, a hostname router spike, and
Linux feasibility probes.

The target production host is Ubuntu/Debian Linux x86_64 with systemd, cgroup
v2, Bubblewrap, unprivileged user namespaces, Go, Git, Skopeo, umoci, and
SQLite. This development host may be non-Linux; Linux-only probes skip
explicitly in that case and must be rerun on the target host before the Cycle 1
handoff.

## Quick start

Build the binaries:

    make build

Start the daemon. The default socket follows the plan and may require root on
Linux; use a temporary socket for local development:

    ./bin/paasd -socket /tmp/paasd.sock
    ./bin/paas -socket /tmp/paasd.sock ping

Expected output:

    pong

Run the complete portable check suite:

    make test

Run the router spike:

    ./scripts/spikes/router.sh

## Cycle 1 / Person 1 boundaries

| Boundary | Current responsibility | Next-cycle extension |
| --- | --- | --- |
| internal/daemon | HTTP-over-Unix-socket control-plane ping | deployment API and lifecycle operations |
| internal/contracts | Builder, Sandbox, Router, and ServiceProvider interfaces | concrete builders, sandbox, and service providers |
| internal/router | host-normalized reverse proxy with synchronized route table | persistent route promotion and rollback |
| cmd/paas | thin CLI client for daemon commands | deploy/apps/inspect/logs/stop |
| cmd/paasd | thin daemon process and signal handling | orchestration service |
| examples | healthy, unhealthy, failed-build, and memory-limit fixtures | builder and runtime integration tests |
| scripts/spikes | target-host feasibility probes | production runtime implementation |

The daemon protocol is deliberately small in this pass. GET /ping returns
pong; the Unix socket path is configurable so tests and unprivileged local
development do not need /run/paas.

## Linux handoff

Run these on the intended Linux host before tagging the pass:

    make test
    ./scripts/spikes/bubblewrap.sh
    ./scripts/spikes/systemd.sh
    ./scripts/spikes/cgroup-oom.sh

The scripts fail on Linux when a required capability or probe is broken. They
only skip when the host is not Linux or a Linux-only dependency is absent; the
output identifies the reason.
