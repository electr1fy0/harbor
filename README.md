# Harbor PaaS

<p align="center">
  <strong>Lightweight, single-machine Platform-as-a-Service for Linux</strong><br>
  Deploy Go and Node.js applications with unprivileged builds, namespace isolation, systemd cgroup v2 limits, managed SQLite services, zero-downtime routing, and an embedded web dashboard.
</p>

<p align="center">
  <img src="https://img.shields.io/badge/go-1.23%2B-blue.svg" alt="Go Version">
  <img src="https://img.shields.io/badge/platform-linux%20%7C%20systemd%20%7C%20cgroup%20v2-lightgrey.svg" alt="Platform Linux">
  <img src="https://img.shields.io/badge/isolation-bubblewrap-orange.svg" alt="Isolation Bubblewrap">
  <img src="https://img.shields.io/badge/database-embedded%20sqlite-blue.svg" alt="Database SQLite">
  <img src="https://img.shields.io/badge/reverse--proxy-caddy%202.11%2B-brightgreen.svg" alt="Reverse Proxy Caddy">
</p>

---

## Table of Contents

- [Overview](#overview)
- [Architecture & Security Model](#architecture--security-model)
  - [Deployment Lifecycle](#deployment-lifecycle)
  - [Dual Privilege Separation & Isolation](#dual-privilege-separation--isolation)
  - [Sandbox Isolation Matrix](#sandbox-isolation-matrix)
  - [Secret Storage & Redaction](#secret-storage--redaction)
  - [Network & Routing Model](#network--routing-model)
- [System Requirements & Prerequisites](#system-requirements--prerequisites)
  - [Linux Host](#linux-host)
  - [WSL 2 (Windows Subsystem for Linux)](#wsl-2-windows-subsystem-for-linux)
  - [macOS Development](#macos-development)
- [Installation & Quick Start](#installation--quick-start)
  - [1. Build Binaries from Source](#1-build-binaries-from-source)
  - [2. Configure Linux Host Dependencies](#2-configure-linux-host-dependencies)
  - [3. Start the Control-Plane Daemon](#3-start-the-control-plane-daemon)
  - [4. Deploy Your First Application](#4-deploy-your-first-application)
  - [5. Test the Deployed Application](#5-test-the-deployed-application)
- [Application Specification (`paas.yaml`)](#application-specification-paasyaml)
  - [Schema & Syntax](#schema--syntax)
  - [Resource Limits & Memory Suffixes](#resource-limits--memory-suffixes)
  - [Go Application Contract](#go-application-contract)
  - [Node.js Application Contract](#nodejs-application-contract)
  - [Environment Variables (Build vs. Runtime)](#environment-variables-build-vs-runtime)
- [CLI Reference (`paas`)](#cli-reference-paas)
  - [Global Options](#global-options)
  - [Commands Summary](#commands-summary)
  - [Command Details & Examples](#command-details--examples)
- [Daemon Reference (`paasd`)](#daemon-reference-paasd)
  - [Command-Line Flags](#command-line-flags)
  - [Environment Variables](#environment-variables)
  - [Local Mode vs. Public Production Mode](#local-mode-vs-public-production-mode)
- [Web Dashboard](#web-dashboard)
  - [Embedded Architecture](#embedded-architecture)
  - [Dashboard Views & Features](#dashboard-views--features)
  - [Frontend Development & Rebuilding](#frontend-development--rebuilding)
- [Managed SQLite Services](#managed-sqlite-services)
  - [Provisioning & Application Bindings](#provisioning--application-bindings)
  - [Connection URI Format](#connection-uri-format)
  - [Driver Compatibility & WAL Mode](#driver-compatibility--wal-mode)
  - [Backups, Verification & Clone Restore](#backups-verification--clone-restore)
  - [Encrypted Harbor Archive Exports](#encrypted-harbor-archive-exports)
- [GitHub Integration & CI/CD Automation](#github-integration--cicd-automation)
  - [Repository Onboarding & Token Permissions](#repository-onboarding--token-permissions)
  - [CI Deployment Webhook](#ci-deployment-webhook)
  - [Signed GitHub Push Webhook](#signed-github-push-webhook)
  - [Path Filtering & Deduplication](#path-filtering--deduplication)
- [Observability, Capacity & Operations](#observability-capacity--operations)
  - [Capacity Reporting & Headroom Estimation](#capacity-reporting--headroom-estimation)
  - [Traffic Metrics & Historical Sampling](#traffic-metrics--historical-sampling)
  - [Alerting Webhooks & Thresholds](#alerting-webhooks--thresholds)
  - [Maintenance Mode](#maintenance-mode)
  - [Automatic Crash Recovery](#automatic-crash-recovery)
- [AI Failure Explanations (`Explain Failure`)](#ai-failure-explanations-explain-failure)
- [Production Deployment Guide](#production-deployment-guide)
  - [Systemd Daemon Service (`paasd.service`)](#systemd-daemon-service-paasdservice)
  - [Public HTTPS Reverse Proxy with Caddy](#public-https-reverse-proxy-with-caddy)
  - [Security Hardening Checklist](#security-hardening-checklist)
- [Repository Structure & Example Applications](#repository-structure--example-applications)
- [Verification & Testing](#verification--testing)

---

## Overview

Harbor is a self-contained, single-host Platform-as-a-Service (PaaS) engineered for Linux environments. It deploys HTTP applications directly from source directories or GitHub repositories onto a single machine with zero external orchestration dependencies (no Kubernetes, Docker daemon, or external container runtimes).

The core operational pipeline executes as follows:

```
Source Directory / Git Repo
           │
           ▼
Unprivileged Build (systemd-run, paas-build user, 512MB/1CPU/128Tasks)
           │
           ▼
Immutable Release Published (/var/lib/paas/releases/<id>/app, mode 0555)
           │
           ▼
Isolated Runtime (Bubblewrap namespaces + systemd transient unit, paas-runtime user)
           │
           ▼
Readiness Health Check (HTTP 200–399 or TCP handshake on injected $PORT)
           │
           ▼
Atomic Route Switch (HostRouter: http://<name>.localhost:8080)
           │
           ▼
Drain Previous Runtime (30-second in-flight drain deadline before SIGTERM/SIGKILL)
```

Harbor combines a Go-based control plane, SQLite metadata persistence, Bubblewrap namespaces, systemd cgroup v2 resource limits, a built-in reverse proxy router, and an embedded [templ](https://templ.guide/) web dashboard inside a single compiled binary: `paasd`.

---

## Architecture & Security Model

### Deployment Lifecycle

Harbor guarantees zero-downtime rolling deployments and candidate safety:

1. **Staging & Validation**: The source tree is copied to a temporary workspace. Symlinks and platform entries (`.git`, `.paas`, root `bin/`) are strictly filtered.
2. **Resource-Constrained Build**: When executed as root, builds execute in transient systemd units under the dedicated `paas-build` user account with enforced limits: 512 MiB RAM, 100% CPU quota (1 core), 128 tasks, no swap, and a configurable build timeout (default 5 minutes).
3. **Immutable Release Artifact**: Output artifacts are copied to `/var/lib/paas/releases/<release_id>/app` with a JSON manifest and frozen to read-only permissions (`0555` directories/executables, `0444` files). Deleting or modifying the original source tree never impacts active or past releases.
4. **Candidate Execution**: Candidate runtimes launch on an unallocated loopback port (`127.0.0.1:<port>`).
5. **Health Gating**: Traffic is not forwarded until the candidate passes configured health checks (HTTP `health.path` returning 200–399 or TCP socket readiness).
6. **Atomic Upstream Switch**: Traffic for `<app>.localhost` switches instantly in memory within the `HostRouter`.
7. **Connection Draining**: The old runtime enters retirement. The proxy tracks active in-flight requests and waits up to 30 seconds for them to drain before stopping the old process.
8. **Candidate Failure Isolation**: If a new deployment attempt fails at the build, start, or health-check stage, the active production deployment remains untouched, serving live traffic without interruption. The failure is recorded in deployment history with redacted logs.
9. **Fast Rollback**: Rollbacks re-instantiate existing immutable release artifacts directly. They require no recompilation, run health checks on a separate loopback port, and switch traffic atomically once healthy.

### Dual Privilege Separation & Isolation

When Harbor runs as root on Linux, it enforces strict privilege separation across distinct non-root system accounts:

```
┌────────────────────────────────────────────────────────────────────────┐
│                        Root Control Plane (paasd)                      │
│  - Binds /run/paas/paasd.sock (0660 paas-admins)                       │
│  - Owns /var/lib/paas state, releases, SQLite databases, secret keys   │
│  - Manages systemd transient units & HostRouter reverse proxy          │
└───────────────────┬────────────────────────────────┬───────────────────┘
                    │                                │
      systemd-run   │                  systemd-run   │
      (MemoryMax=512M)                 (User limits) │
                    ▼                                ▼
┌──────────────────────────────────────┐  ┌──────────────────────────────┐
│       Build Worker (paas-build)      │  │  Runtime Sandbox (paas-runtime)│
│  - Disposable /tmp/paas-build-*      │  │  - Bubblewrap namespaces     │
│  - CGO_ENABLED=0 (default)           │  │  - Read-only /app mount      │
│  - Go/Node build caches              │  │  - cgroup v2 memory/cpu/tasks│
│  - Systemd limits: 512MB, 1 CPU      │  │  - Host network namespace    │
└──────────────────────────────────────┘  └──────────────────────────────┘
```

Startup fails immediately if `paas-build` and `paas-runtime` have not been created or share UIDs with root or each other.

### Sandbox Isolation Matrix

Each running application is isolated using Bubblewrap (`bwrap`) executed inside a transient systemd cgroup v2 unit (`paas-deploy-<id>.service`):

| Boundary | Enforcement | Implementation Details |
|---|---|---|
| **User Identity** | Enforced | Drops to `paas-runtime` UID/GID via `--unshare-user` |
| **Process Hierarchy** | Enforced | `--unshare-pid` (isolated PID 1 inside sandbox, cannot view or signal host processes) |
| **Filesystem Mounts** | Enforced | Root filesystem mounted read-only (`/bin`, `/usr`, `/lib`, `/lib64`); private tmpfs `/tmp`; private devfs `/dev` |
| **Application Code** | Enforced | Release directory mounted read-only at `/app` (`--ro-bind <dir> /app`) |
| **Host Secrets** | Enforced | Host `/etc` is not mounted; only `/etc/resolv.conf`, `/etc/hosts`, `/etc/nsswitch.conf`, `/etc/ssl/certs`, `/etc/ca-certificates` are mounted read-only |
| **Capabilities** | Enforced | `--cap-drop ALL` (all Linux capabilities dropped) |
| **IPC & Hostname** | Enforced | `--unshare-ipc`, `--unshare-uts` |
| **Network** | Shared (`--share-net`) | Applications share the host network stack and must bind to loopback (`127.0.0.1`) on `$PORT` |
| **Memory Limit** | Enforced | cgroup v2 `MemoryMax`, `MemorySwapMax=0` (no swap space allowed) |
| **CPU Limit** | Enforced | cgroup v2 `CPUQuota` (e.g., `50%` for 0.5 CPU cores) |
| **Process Count** | Enforced | cgroup v2 `TasksMax` (prevents fork-bombs) |
| **OOM Handling** | Enforced | `OOMPolicy=kill`, `KillMode=control-group` (kernel terminates entire unit when memory ceiling is breached) |

> [!IMPORTANT]
> **Host Network Sharing**: Applications run with `--share-net`. They share the host network namespace and the `paas-runtime` UID. Applications and operators must be mutually trusted. Apps must bind explicitly to `127.0.0.1` and listen on the dynamically allocated `PORT` environment variable.

### Secret Storage & Redaction

- **Environment Encryption at Rest**: Application environment variables and build secrets are encrypted using AES-256-GCM under `/var/lib/paas/environment.key` (mode `0600`).
- **Write-Only Operations**: Secrets are write-only. Neither the CLI nor the web dashboard ever returns secret values; only variable names/keys are reported back to users.
- **Redaction History**: Harbor retains a cryptographic hash index of past secrets. When secrets are updated or deleted, past values remain masked (`[redacted]`) across historical build outputs, diagnostic excerpts, and runtime logs.
- **Fail-Closed Logs**: If `environment.key` cannot be loaded, log inspection fails closed with a generic error rather than risking unredacted credential exposure.

### Network & Routing Model

- **Built-in HostRouter**: An in-process HTTP reverse proxy maps incoming `Host` headers to upstream internal loopback sockets.
- **Local Hostnames**: Applications receive generated URLs in the format `http://<app-name>.localhost:8080`.
- **Atomic Route Switching**: `HostRouter.SwitchRoute` installs candidate upstream mappings in memory while tracking in-flight connections on the previous mapping.
- **Public Traffic**: In production, an external reverse proxy (Caddy) terminates public HTTPS and forwards dashboard requests to `127.0.0.1:3000`.

---

## System Requirements & Prerequisites

### Linux Host

Harbor is designed and verified for Debian 12 (Bookworm) and Ubuntu 22.04 / 24.04 LTS:

- **Linux Kernel**: cgroup v2 unified hierarchy enabled (`/sys/fs/cgroup/cgroup.controllers`).
- **Init System**: systemd as PID 1.
- **Required Host Packages**:
  - `go` (1.23 or newer)
  - `gcc` / `build-essential` (required for CGO and SQLite)
  - `git` (2.25+)
  - `bwrap` (Bubblewrap 0.4.1+)
  - `systemd`, `systemd-sysv`, `curl`
  - `apparmor`, `apparmor-utils` (if user namespace restrictions are enabled)
- **Production Package (Optional)**: Caddy (v2.11+) for public HTTPS reverse proxying.

### WSL 2 (Windows Subsystem for Linux)

Harbor runs inside WSL 2 with Ubuntu provided systemd and cgroup v2 are active:

1. Confirm systemd is running as PID 1:
   ```sh
   ps -p 1 -o comm=
   # Must output: systemd
   ```
   If not, enable it in `/etc/wsl.conf`:
   ```ini
   [boot]
   systemd=true
   ```
   Then restart WSL from PowerShell (`wsl --shutdown`).
2. Verify cgroup v2 availability:
   ```sh
   test -f /sys/fs/cgroup/cgroup.controllers && echo "cgroup v2 available"
   ```
3. Always keep your checkout inside the Linux filesystem (e.g., `~/harbor`), **never** under `/mnt/c/`.

### macOS Development

macOS is supported for local development and testing without systemd isolation:
- Runtimes run directly as host processes using the `-sandbox host` flag.
- Privilege separation and cgroup limits are bypassed on macOS.
- Filesystem reporting is available, while Linux cgroup metrics return `null`.

---

## Installation & Quick Start

### 1. Build Binaries from Source

Ensure Go 1.23+ and a C compiler are installed:

```sh
git clone https://github.com/electr1fy0/harbor.git
cd harbor
make build
```

This compiles binaries into `./bin/`:
- `bin/paasd`: The control-plane daemon, router, and embedded web server.
- `bin/paas`: The command-line client.
- `bin/router-spike`: Routing test utility.
- Compiled test fixtures (`bin/hello-go`, `bin/sqlite-app`, `bin/unhealthy`, `bin/memory-hog`).

### 2. Configure Linux Host Dependencies

Run the automated system configuration script with `sudo`:

```sh
sudo ./scripts/setup-linux.sh
```

This script:
1. Verifies prerequisite binaries (`go`, `gcc`, `git`, `bwrap`, `systemd-run`, `systemctl`, `curl`).
2. Confirms cgroup v2 controller availability (`cpu`, `memory`, `pids`).
3. Provisions dedicated system accounts: `paas-build` and `paas-runtime` (with `nologin` shells).
4. Creates state directories: `/var/lib/paas`, `/var/lib/paas/releases`, `/var/lib/paas/logs`, and `/run/paas`.
5. Loads and parses the scoped AppArmor profile (`/etc/apparmor.d/harbor-bwrap`) if unprivileged user namespace restrictions are active.

### 3. Start the Control-Plane Daemon

Start the daemon in the foreground:

```sh
sudo ./bin/paasd
```

Expected startup output:
```
runtime sandbox=systemd
dashboard ready: http://127.0.0.1:3000
paasd ready: socket=/run/paas/paasd.sock router=http://127.0.0.1:8080 data=/var/lib/paas
```

### 4. Deploy Your First Application

In another terminal, test connectivity using the CLI:

```sh
# Verify the control socket
sudo ./bin/paas ping
# Output: pong

# Deploy the included Go example
sudo ./bin/paas deploy ./examples/hello-go --name hello
```

Expected output:
```
State: RUNNING
Release: rel_0123456789ab
Port: 38471
URL: http://hello.localhost:8080
```

### 5. Test the Deployed Application

Send an HTTP request via the router port (`8080`) specifying the application's virtual host:

```sh
curl -H "Host: hello.localhost" http://127.0.0.1:8080/
# Output: hello from harbor
```

You can now inspect runtime details, view logs, and manage releases:

```sh
# View running apps
sudo ./bin/paas apps

# View runtime inspection details (cgroups, PID, sandbox isolation)
sudo ./bin/paas inspect hello

# View unified build and runtime journal logs
sudo ./bin/paas logs hello

# Stop the application
sudo ./bin/paas stop hello
```

Open your browser to <http://127.0.0.1:3000> to interact with the embedded web dashboard.

---

## Application Specification (`paas.yaml`)

Every application deployed to Harbor requires a `paas.yaml` configuration file in its repository root.

### Schema & Syntax

Harbor uses strict YAML decoding (`KnownFields(true)`). Any unknown fields or multiple YAML documents in a single file cause validation failure.

```yaml
# Application identifier: lowercase letters, numbers, hyphens (1–63 chars, must start with a letter)
name: my-app

# Runtime environment: must be "go" or "node"
runtime: go

# Build instructions
build:
  # Shell command executed in an unprivileged build workspace
  command: go build -o "$PAAS_OUT/server" main.go

# Start instructions
start:
  # For Go: must be exactly "/app/server"
  # For Node: command to execute (e.g., "node /app/server.js")
  command: /app/server

# Runtime resource limits (optional; defaults shown)
resources:
  memory: 128Mi   # Default: 128Mi
  cpu: 0.5        # Default: 0.5 (quota in cores, e.g., 0.5 = 50% CPU)
  tasks: 64       # Default: 64 (maximum threads/processes)

# Readiness health probe (optional)
health:
  path: /health   # HTTP path for readiness check. If omitted, TCP readiness on $PORT is used
```

### Resource Limits & Memory Suffixes

Harbor's memory parser accepts positive binary byte values:
- Suffixes supported: `Ki`, `Mi`, `Gi`, `Ti`, `Pi`, `Ei`, `K`, `M`, `G`, `T`, `P`, `E`, `B`, or unitless bytes.
- All unit conversions use base-2 arithmetic (e.g., `128Mi` = `128 * 1024 * 1024` bytes; `1M` = `1 * 1024 * 1024` bytes).
- CPU quota must be a finite positive number representing fractional cores (e.g., `0.5` = 50% of one core; `2.0` = 200%).
- Tasks limit must be a positive integer ($1$).

### Go Application Contract

- **Output Binary**: Go builds must compile an executable placed at `$PAAS_OUT/server`.
- **Start Command**: `start.command` must be configured as `/app/server`.
- **CGO & SQLite**: By default, the builder injects `CGO_ENABLED=0` into the build environment. If your Go application relies on CGO (such as `github.com/mattn/go-sqlite3`), prefix the build command with `CGO_ENABLED=1`:
  ```yaml
  build:
    command: CGO_ENABLED=1 go build -o "$PAAS_OUT/server" main.go
  ```
- **Port Binding**: The application must read the `PORT` environment variable and bind to `127.0.0.1:$PORT`.

### Node.js Application Contract

- **Output Artifacts**: The build command can output directly to `$PAAS_OUT`. If `$PAAS_OUT` remains empty after the build command finishes, Harbor automatically copies the built source tree (including `node_modules`) into the release artifact.
- **Start Command**: `start.command` can be any valid shell invocation, such as:
  ```yaml
  start:
    command: node /app/server.js
  ```
- **Symlink Validation**: Package manager symlinks (e.g., `node_modules/.bin`) are preserved during artifact copy, but are strictly validated before publication. Any symlink with an absolute target or one that escapes `/app` will fail the deployment.

### Environment Variables (Build vs. Runtime)

#### Build Environment Variables
During execution of `build.command`, the following variables are provided:
- `PAAS_OUT`: Absolute path to the destination directory where build artifacts must be stored.
- `PATH`: System paths (`/usr/local/bin:/usr/local/go/bin:/usr/bin:/bin`) and `~/.local/bin`.
- `HOME`: Dedicated temporary home directory inside the workspace.
- `TMPDIR`: Dedicated temporary directory inside the workspace.
- `GOCACHE`: Path to persistent Go build cache (`/var/lib/paas/build-cache/go-build`).
- `GOMODCACHE`: Path to persistent Go module cache (`/var/lib/paas/build-cache/go/pkg/mod`).
- `GOFLAGS=-p=1` & `GOMAXPROCS=1`: Pinned to enforce build concurrency stability.
- `npm_config_cache`, `npm_config_store_dir`, `pnpm_config_store_dir`, `YARN_CACHE_FOLDER`: Configured to point to per-application persistent package caches under `/var/lib/paas/build-cache/node/<app>/`.

> [!WARNING]
> The above variable names are reserved and cannot be overwritten by custom build environment settings.

#### Runtime Environment Variables
During execution inside the runtime sandbox:
- `PORT`: Loopback port dynamically assigned to this container instance.
- `DATABASE_URL`: Injected automatically if the application is linked to a managed SQLite service (e.g., `file:/var/lib/paas/volumes/<svc_id>/db_<app_id>/database.sqlite?mode=rw&_busy_timeout=5000&_foreign_keys=on`).
- `PATH`: Preserved system paths.
- User-configured environment keys stored securely via `paas configure` or the dashboard Settings tab.

---

## CLI Reference (`paas`)

The `paas` binary is a thin client that communicates with `paasd` via HTTP requests over the control Unix domain socket.

### Global Options

All commands accept:
- `-socket <path>` or `--socket <path>`: Path to daemon socket (default: `/run/paas/paasd.sock` or `PAASD_SOCKET` environment variable).

### Commands Summary

```
paas [-socket PATH] {ping|deploy SOURCE [--name NAME]|apps|inspect NAME|logs NAME|stop NAME|releases NAME|rollback NAME [--release ID]|settings NAME|configure NAME [--env-stdin] [--remove-env KEY] [--memory LIMIT] [--cpu N] [--tasks N]|add TYPE --name NAME|link APP SERVICE|services|export-decrypt INPUT OUTPUT --passphrase-stdin}
```

### Command Details & Examples

#### `ping`
Test communication with the running daemon.
```sh
sudo paas ping
# pong
```

#### `deploy SOURCE [--name NAME]`
Deploy an application from a local directory containing `paas.yaml`.
```sh
sudo paas deploy ./examples/hello-go --name hello
```
*Outputs release ID, allocated port, and generated URL.*

#### `apps`
List all registered applications and their current runtime status.
```sh
sudo paas apps
# NAME       STATE    RELEASE          PORT
# hello      RUNNING  rel_a1b2c3d4e5f6 38491
```

#### `inspect NAME`
Display detailed runtime metadata, including PID, systemd unit, cgroup limits, and sandbox isolation parameters.
```sh
sudo paas inspect hello
```
*Example Output:*
```
Application: hello
Release: rel_a1b2c3d4e5f6
Deployment: dep_9876543210fe
State: RUNNING
PID: 14205
Port: 38491
Last error: -

Sandbox:
  Backend: systemd
  PID isolation:     yes
  Mount isolation:   yes
  IPC isolation:     yes
  UTS isolation:     yes
  User isolation:    yes
  Network isolation: no

Resources:
  Memory limit: 128Mi
  CPU quota:    50%
  Tasks limit:  64

Systemd:
  Unit: paas-deploy-dep_9876543210fe.service
```

#### `logs NAME`
Fetch combined build logs and up to 500 runtime journal entries (capped at 1 MiB).
```sh
sudo paas logs hello
```

#### `stop NAME`
Gracefully terminate the application runtime and withdraw its route.
```sh
sudo paas stop hello
# Stopped hello
```

#### `releases NAME`
List all published immutable releases for an application.
```sh
sudo paas releases hello
# RELEASE          SOURCE_HASH  CREATED               ACTIVE
# rel_a1b2c3d4e5f6 8f9b2a1c0d4e 2026-10-15T10:00:00Z  yes
# rel_7e8d9c0b1a2f 4a5b6c7d8e9f 2026-10-14T18:30:00Z  no
```

#### `rollback NAME [--release ID]`
Instantly switch to the previously promoted release (or a specified release ID) without rebuilding.
```sh
# Roll back to the immediately previous promoted release
sudo paas rollback hello

# Roll back to an explicit release ID
sudo paas rollback hello --release rel_7e8d9c0b1a2f
```

#### `settings NAME`
View current environment keys (values redacted) and resource limits.
```sh
sudo paas settings hello
# Environment keys: API_KEY, LOG_LEVEL
# Memory limit: 128Mi
# CPU quota: 0.5
# Tasks limit: 64
```

#### `configure NAME [options]`
Update runtime limits and write-only environment variables. Changes take effect on the next deployment, rollback, or daemon recovery.
```sh
# Set resource limits
sudo paas configure hello --memory 256Mi --cpu 1.0 --tasks 128

# Set secrets securely via stdin (KEY=VALUE lines)
cat <<EOF | sudo paas configure hello --env-stdin
API_KEY=secret_value_123
LOG_LEVEL=info
EOF

# Remove an environment variable
sudo paas configure hello --remove-env LOG_LEVEL
```

#### `add TYPE --name NAME`
Create a managed stateful service (currently `sqlite`).
```sh
sudo paas add sqlite --name main-db
# ✓ main-db READY (sqlite)
```

#### `link APP SERVICE`
Link an application to a managed service. Provisions an isolated SQLite database file and injects `DATABASE_URL`.
```sh
sudo paas link hello main-db
# ✓ Injected DATABASE_URL into hello
```

#### `services`
List all managed stateful services.
```sh
sudo paas services
# NAME     TYPE    STATE  PORT
# main-db  sqlite  READY  -
```

#### `export-decrypt INPUT OUTPUT --passphrase-stdin`
Offline utility to authenticate and decrypt an encrypted Harbor backup archive. The passphrase must be provided on standard input (12 to 1024 bytes). The output file must not already exist and is written with mode `0600`.
```sh
sudo paas export-decrypt /backup/harbor-export.enc /backup/harbor-export.tar.gz --passphrase-stdin
```

---

## Daemon Reference (`paasd`)

The `paasd` daemon coordinates application lifecycles, builds, sandbox execution, routing, and the dashboard.

### Command-Line Flags

| Flag | Default | Description |
|---|---|---|
| `-socket <path>` | `/run/paas/paasd.sock` | Path to control Unix domain socket |
| `-socket-group <group>` | `""` | Existing system group granted socket access with mode `0660` |
| `-data-dir <path>` | `/var/lib/paas` | Persistent storage directory for databases, releases, and keys |
| `-listen <address>` | `127.0.0.1:8080` | Internal loopback address for the application router |
| `-web-listen <address>` | `127.0.0.1:3000` | Loopback address for embedded dashboard; `""` disables dashboard |
| `-hash-password` | `false` | Interactive terminal utility: prompts for admin password, prints scrypt hash, and exits |
| `-build-user <user>` | `paas-build` | Unprivileged system user for running builds when daemon is root |
| `-runtime-user <user>`| `paas-runtime` | Unprivileged system user for sandboxed runtimes when daemon is root |
| `-build-timeout <dur>` | `5m0s` | Maximum allowable duration for a single build step |
| `-sandbox <type>` | `default` | Sandbox backend: `default` (systemd on Linux, host on macOS), `systemd`, `bubblewrap`, or `host` |

### Environment Variables

Configuration options read by `paasd`:

- `PAASD_PUBLIC_ORIGIN`: Exact canonical HTTPS origin for the public dashboard (e.g., `https://dashboard.example.com`). Required for public mode.
- `PAASD_ADMIN_PASSWORD_HASH`: Scrypt hash formatted as `scrypt$32768$8$1$<salt_hex>$<hash_hex>`. Generated via `paasd -hash-password`.
- `PAASD_SESSION_SECRET`: Exactly 32 bytes encoded as 64 lowercase hexadecimal characters. Used to HMAC-sign session cookies. Generate with `openssl rand -hex 32`.
- `GITHUB_TOKEN`: Legacy fallback GitHub personal access token (connecting via Dashboard Settings is preferred).
- `OPENAI_API_KEY`: Server-side API key enabling AI-powered **Explain failure** diagnostics via `gpt-5.4-mini`.

### Local Mode vs. Public Production Mode

Harbor operates in one of two security modes based on authentication environment variables:

1. **Local Development Mode**:
   When `PAASD_PUBLIC_ORIGIN`, `PAASD_ADMIN_PASSWORD_HASH`, and `PAASD_SESSION_SECRET` are all unset:
   - Dashboard listens on loopback (`127.0.0.1:3000`).
   - Authentication is disabled; no login is required.
   - Non-loopback requests are rejected.
2. **Public Production Mode**:
   When authentication variables are configured:
   - All three variables must be set with valid values, or `paasd` fails startup immediately.
   - Access requires login at `/login`.
   - Browser sessions use signed `__Host-harbor_session` cookies (8-hour expiration, HttpOnly, SameSite=Strict).
   - Login attempts are bounded to 5 attempts per 15 minutes to prevent brute-force attacks.

---

## Web Dashboard

### Embedded Architecture

The Harbor dashboard is written in Go using [templ](https://templ.guide/) HTML components.
- Components, CSS, and client-side JavaScript are compiled directly into the `paasd` binary using Go's `embed` package.
- **Zero Frontend Build Dependencies**: No Node.js, Bun, npm, React, Vite, or external CDN assets are required on the production host.
- The control plane and dashboard share one process and memory space.

### Dashboard Views & Features

- **Projects (`/`)**: Overview of all deployed services, running state, latest attempts, and quick search filters (by name, tag, favorite, or archived).
- **New Project (`/new`)**: Repository import workflow. Enter a public GitHub URL or browse repositories from a connected personal access token. Review auto-detected Go/Node settings, configure environment variables, and optionally link a managed SQLite database.
- **Project Detail (`/projects/{name}`)**:
  - *Overview Tab*: Live runtime metrics, production release, latest attempt status, quick actions (Start, Restart, Redeploy, Stop, Rollback, Delete).
  - *Settings Tab*: Manage write-only runtime environment variables, cgroup resource limits, crash recovery policies, and release retention count.
  - *Automation Tab*: Configure repository branch/ref, root directories, build timeouts, path inclusion/exclusion filters, and generate CI/GitHub webhook tokens.
  - *Logs Tab*: Real-time log follower (polled every 2 seconds, bounded to 1 MiB in-memory buffer), search filter, pause/resume, and redacted text download.
- **Deployments (`/deployments`)**: Global and per-app deployment attempt history, timeline events, and stage timing breakdowns (checkout, build, start, health check).
- **Services (`/services`)**: Manage SQLite databases, inspect WAL sizes, initiate consistent manual snapshots, configure automated hourly/daily/weekly backup policies, download snapshots, and execute clone restores.
- **Capacity (`/capacity`)**: Real-time host memory and CPU utilization, disk consumption (Harbor data scan up to 20,000 entries), per-app cgroup usage, and estimated deployment headroom.
- **Settings (`/settings`)**: Platform-wide settings, GitHub token connection status, encrypted outbound alert webhooks, and maintenance mode toggle.

### Frontend Development & Rebuilding

To modify dashboard templates:
```sh
# 1. Edit web/dashboard.templ or assets/app.css
# 2. Run templ generator
make web

# 3. Verify templ and dashboard tests
make web-check

# 4. Rebuild binaries
make build
```

---

## Managed SQLite Services

Harbor provides single-tenant, file-backed managed SQLite services.

### Provisioning & Application Bindings

- Create a service with `paas add sqlite --name <db-name>` or via the Services tab.
- Linking an application (`paas link <app-name> <db-name>`) creates an isolated SQLite file under `/var/lib/paas/volumes/<svc_id>/db_<app_id>/database.sqlite`.
- The directory containing the database is mounted read-write into the application's sandbox container at the exact same absolute path. This allows SQLite to create its `-wal` (Write-Ahead Log) and `-shm` (Shared Memory) sidecar files.

### Connection URI Format

Harbor injects an RFC-compliant SQLite file URI into `DATABASE_URL`:
```
file:/var/lib/paas/volumes/svc_01234567/db_app_89abcdef/database.sqlite?mode=rw&_busy_timeout=5000&_foreign_keys=on
```
Configured driver parameters:
- `mode=rw`: Opens existing database in read-write mode.
- `_busy_timeout=5000`: Sets a 5-second lock wait timeout to prevent transient SQLite lock errors.
- `_foreign_keys=on`: Enforces relational foreign key constraints.

### Driver Compatibility & WAL Mode

- **Driver Requirements**: Applications must use a driver supporting SQLite file URIs and URI query parameters (such as `github.com/mattn/go-sqlite3` for Go, or `better-sqlite3` for Node). PostgreSQL drivers are not compatible.
- **Local Storage**: Harbor's SQLite volumes must reside on local filesystems; SQLite WAL mode does not support network file systems (NFS/CIFS).
- **SQLite Library Version**: It is recommended to use modern SQLite libraries (3.51.3+) to ensure protection against known WAL-reset edge cases.

### Backups, Verification & Clone Restore

1. **Consistent Snapshots**: Backups utilize SQLite's online vacuum API (`VACUUM INTO`), generating a bit-level consistent snapshot without locking write transactions.
2. **Integrity Checks**: Snapshots are verified using SQLite integrity checks (`PRAGMA integrity_check`) before publication.
3. **Automated Scheduling**: Scheduled backups run hourly, daily, or weekly with automated release retention pruning.
4. **Clone Restore**: Restoring from a backup snapshot provisions a brand-new application with a staged copy of the database. Production data is never overwritten in place.

### Encrypted Harbor Archive Exports

Operators can generate encrypted backups of platform configuration and selected databases:
- Backups are encrypted using AES-256-GCM with a key derived via scrypt using a mandatory 12–1024 byte passphrase.
- Export size is bounded to 48 MiB unencrypted.
- Decrypt offline using the CLI:
  ```sh
  paas export-decrypt backup.enc output.tar.gz --passphrase-stdin
  ```

---

## GitHub Integration & CI/CD Automation

### Repository Onboarding & Token Permissions

Harbor supports deploying from GitHub repositories:
- **Public Repositories**: Cloned over HTTPS without credentials.
- **Private Repositories**: Connect a GitHub Personal Access Token (PAT) via Dashboard Settings.
- **Recommended Token**: Fine-grained PAT scoped to the specific repositories Harbor needs to deploy.
- **Required Permissions**:
  - `Metadata: read`: List repositories and branch metadata.
  - `Contents: read`: Clone repository source trees.
- **Credential Storage**: Tokens are stored encrypted in `/var/lib/paas/github_credentials.enc` (AES-256-GCM) with an owner-only key (`0600`).

### CI Deployment Webhook

Trigger deployments from external CI/CD pipelines (e.g., GitHub Actions, GitLab CI):
```sh
curl -X POST https://dashboard.example.com/api/hooks/deploy/<hook_id> \
  -H "Authorization: Bearer <hook_secret>" \
  -H "Idempotency-Key: $(git rev-parse HEAD)"
```
- Deduplicated via `Idempotency-Key` or `X-Delivery-ID`.
- Executes asynchronously without exposing a web dashboard session.

### Signed GitHub Push Webhook

Configure GitHub repository push webhooks to deploy automatically on `git push`:
1. In Harbor's Automation tab, create a Webhook for the target branch and copy the one-time secret.
2. In GitHub Repository Settings -> Webhooks:
   - Payload URL: `https://dashboard.example.com/api/hooks/github/<hook_id>`
   - Content type: `application/json`
   - Secret: *Paste the webhook secret*
   - Events: Just the `push` event.
3. Harbor verifies the HMAC-SHA256 signature (`X-Hub-Signature-256`), checks the branch name, applies path filters, and deploys the pushed commit SHA.

### Path Filtering & Deduplication

Configure path filters in the Automation tab to prevent unnecessary builds:
- `includePaths`: Build triggers only if pushed commits modify files under these relative paths (e.g., `services/api/**`).
- `ignorePaths`: Ignores changes matching specific patterns (e.g., `docs/**`, `*.md`).

---

## Observability, Capacity & Operations

### Capacity Reporting & Headroom Estimation

Open the **Capacity** dashboard view or query `POST /capacity` to inspect resource metrics:
- **Host Metrics**: Total RAM (`MemTotal`), Available RAM (`MemAvailable`), and CPU core counts.
- **Runtime Metrics**: Measured directly from cgroup v2 controllers (`memory.current`, `memory.peak`, CPU time counter deltas, task counts).
- **Filesystem Scan**: Tracks disk usage and file growth under `/var/lib/paas` (bounded to 20,000 scanned entries).
- **Deployment Headroom**: Estimated as:
  $$\text{Memory Headroom} = \text{MemAvailable} - (0.10 \times \text{MemTotal})$$
  *(Resource limits represent ceilings, not static reservations).*

### Traffic Metrics & Historical Sampling

- **15-Second Sampling**: System capacity is sampled every 15 seconds and stored in a rotating 24-hour historical window (up to 5,760 samples).
- **Traffic Counters**: In-process reverse proxy monitors request counts, HTTP status breakdown (2xx, 3xx, 4xx, 5xx), response bytes, and latency percentiles (p50, p90, p99).

### Alerting Webhooks & Thresholds

Configure automated outbound alerting in Platform Settings:
- **Event Alerts**: Dispatched on application events (`CRASHED`, `OOM_KILLED`, `DEPLOY_FAILED`).
- **Threshold Alerts**: Triggers when host memory or disk consumption exceeds configured percentages (with cooldown timeouts).
- **SSRF Protection**: Outbound alert requests resolve and pin public IP addresses; connections to loopback or private RFC-1918 subnets are blocked.

### Maintenance Mode

Applications can be placed into maintenance mode via the Settings page:
- `HostRouter` serves an immediate `HTTP 503 Service Unavailable`.
- Injects a `Retry-After: 60` HTTP response header and custom maintenance message.
- Upstream runtime processes remain alive and undisturbed.

### Automatic Crash Recovery

Harbor provides opt-in, exponential backoff crash recovery:
- If an active application process exits unexpectedly, Harbor automatically attempts a restart up to a configured threshold (default 5 attempts).
- Backoff increases progressively (e.g., 5s, 10s, 20s, 40s).
- If an application repeatedly fails, it enters a `CrashLoop` state requiring manual operator intervention. Explicitly stopped applications are never restarted automatically.

---

## AI Failure Explanations (`Explain Failure`)

When a deployment attempt fails (`BUILD_FAILED`, `START_FAILED`, `UNHEALTHY`, `CRASHED`, or `OOM_KILLED`), the dashboard provides an **Explain failure** action powered by OpenAI's `gpt-5.4-mini`.

```
Failed Deployment (BUILD_FAILED / CRASHED / OOM_KILLED)
                          │
                          ▼
Operator clicks "Explain failure" in Dashboard
                          │
                          ▼
Sanitize & Redact Diagnostic Log Excerpts + State Metadata
                          │
                          ▼
Call OpenAI Responses API (model: gpt-5.4-mini, store: false)
                          │
                          ▼
Validate Returned Citations & Evidence Against Local Logs
                          │
                          ▼
Render Concise Failure Summary, Verified Evidence, and Suggested Next Step
```

- **Opt-In & Server-Side**: Requires `OPENAI_API_KEY` set in `/etc/harbor/harbor.env`.
- **Privacy Guarantees**: Calls the OpenAI Responses API with `store: false`. Sent payloads include only sanitized deployment metadata and redacted log excerpts. Application source trees and environment variables are never sent.
- **Evidence Verification**: Harbor verifies that citations in the model's response match actual lines in local diagnostic logs.
- **In-Memory Caching**: Responses are cached in memory for identical deployment evidence. No model calls run automatically or on page refresh.

---

## Production Deployment Guide

In production, run Harbor as a single root-owned systemd service behind Caddy.

### Systemd Daemon Service (`paasd.service`)

1. Install system dependencies:
   ```sh
   sudo apt update
   sudo apt install -y build-essential git curl bubblewrap systemd apparmor apparmor-utils python3 caddy
   ```
2. Build and install `paasd`:
   ```sh
   make build
   sudo install -m 0755 bin/paasd /usr/local/bin/paasd
   sudo install -m 0755 bin/paas /usr/local/bin/paas
   sudo ./scripts/setup-linux.sh
   ```
3. Create admin group and directory:
   ```sh
   sudo groupadd --system paas-admins
   sudo install -d -o root -g root -m 0700 /etc/harbor
   ```
4. Generate the scrypt admin password hash:
   ```sh
   paasd -hash-password
   ```
5. Configure environment variables in `/etc/harbor/harbor.env` (mode `0600`, owned by `root:root`):
   ```ini
   PAASD_PUBLIC_ORIGIN=https://dashboard.example.com
   PAASD_ADMIN_PASSWORD_HASH=scrypt$32768$8$1$...
   PAASD_SESSION_SECRET=abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789
   OPENAI_API_KEY=sk-...
   ```
6. Install and start the systemd service:
   ```sh
   sudo install -m 0644 deploy/paasd.service /etc/systemd/system/paasd.service
   sudo systemctl daemon-reload
   sudo systemctl enable --now paasd
   ```

### Public HTTPS Reverse Proxy with Caddy

Render and install the Caddy configuration to publish the dashboard:

```sh
PAASD_PUBLIC_ORIGIN=https://dashboard.example.com \
  ./deploy/render-caddyfile.sh | sudo tee /etc/caddy/Caddyfile >/dev/null

sudo caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
sudo systemctl reload caddy
```

Caddy manages TLS certificates automatically and forwards traffic for `dashboard.example.com` to `127.0.0.1:3000`.

### Security Hardening Checklist

- Keep `/etc/harbor/harbor.env` permissions restricted to `0600` owned by `root:root`.
- Restrict socket access by assigning trusted operators to the `paas-admins` group.
- Keep the application router listener (`127.0.0.1:8080`) and dashboard listener (`127.0.0.1:3000`) bound strictly to loopback addresses.
- Backup `/var/lib/paas/environment.key` alongside any system backups; losing this key prevents decryption of stored secrets.

---

## Repository Structure & Example Applications

```
harbor/
├── cmd/
│   ├── paas/                # CLI client implementation
│   ├── paasd/               # Control-plane daemon, router, and dashboard runner
│   └── router-spike/        # Standalone routing validation tool
├── internal/
│   ├── builder/             # Go & Node unprivileged build engine and release store
│   ├── capacity/            # cgroup v2 & host capacity sampling
│   ├── config/              # paas.yaml parser and schema validation
│   ├── contracts/           # Interfaces and data contracts between subsystems
│   ├── daemon/              # HTTP control-plane API endpoints and routes
│   ├── dashboard/           # Dashboard backend handlers, API mappers, auth, AI diagnostics
│   ├── deployment/          # Core deployment orchestration, lifecycle, rollback, drain
│   ├── health/              # HTTP and TCP readiness check probes
│   ├── model/               # Persistence domain models
│   ├── onboarding/          # GitHub repo browsing, config auto-detection, async jobs
│   ├── router/              # In-process reverse proxy and Host header router
│   ├── sandbox/             # Bubblewrap & systemd cgroup v2 process supervisors
│   ├── service/             # Managed stateful SQLite services and volume bindings
│   ├── source/              # Git operations, credential encryption, GitHub API client
│   └── store/               # SQLite database access layer
├── web/                     # templ components, CSS styling, and browser assets
├── deploy/                  # Production systemd units, Caddyfile template, AppArmor rules
├── scripts/                 # Host setup and test verification scripts
└── examples/                # Reference applications:
    ├── hello-go/            # Basic Go web application
    ├── hello-node/          # Basic Node.js web application
    ├── sqlite-app/          # Go app using managed SQLite via DATABASE_URL
    ├── memory-hog/          # Test fixture verifying cgroup OOM kill classification
    ├── unhealthy/           # Test fixture verifying health check failure handling
    └── broken-build/        # Test fixture verifying build failure isolation
```

---

## Verification & Testing

Harbor includes a test suite covering unit logic, CLI behavior, router traffic, and privileged Linux isolation:

```sh
# 1. Static code analysis and vet
make vet

# 2. Unit and smoke tests
make test

# 3. Frontend templ component checks
make web-check

# 4. End-to-end integration suite
make integration

# 5. Linux privileged acceptance suite (requires sudo)
# Verifies non-root identities, cgroup enforcement, and release write restrictions
make test-privileged
```

---

## License

Harbor is open-source software licensed under the MIT License.
