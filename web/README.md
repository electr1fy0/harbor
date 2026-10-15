# Harbor dashboard

The dashboard uses [templ](https://templ.guide/) components rendered by the Go
daemon, plain CSS, and a small browser script. There is no npm, Bun, Node,
React, Vite, CDN, or frontend package installation in the dashboard workflow.
Node applications deployed *by* Harbor remain supported independently.

## Build and develop

A fresh checkout contains the generated Go components. Build and run the daemon:

```sh
make build
./bin/paasd -socket /tmp/paasd.sock -data-dir /tmp/harbor-dev -listen 127.0.0.1:8080 -web-listen 127.0.0.1:3000 -sandbox host
```

For local development, `./scripts/dev.sh` regenerates templates, rebuilds, and
starts the daemon in one command. To enable optional failure explanations:

```sh
export OPENAI_API_KEY="your-key-here"
./scripts/dev.sh
```

Replace the placeholder with your key, stop any existing daemon first, and
restart after code or environment changes. The script inherits exported
variables; it does not read `.env` files. For Linux/WSL setup and the root
invocation, see the [local development guide](../README.md#local-development-with-scriptsdevsh).

Open <http://127.0.0.1:3000>. The same process renders every page, serves embedded
assets, authenticates requests, and calls the control plane in process.

Edit `dashboard.templ` for page structure, `view.go` for typed component inputs,
and `assets/app.css` / `assets/app.js` for styling and browser enhancements.
Page data and form definitions live in `internal/dashboard/frontend*.go`.
After editing templates:

```sh
make web        # run the pinned Go-based templ generator
make build      # rebuild embedded assets and Go components
```

Restart `paasd` to load changes. Commit `.templ` files and their generated
`*_templ.go` together. Normal builds and tests do not run the generator or
require a globally installed templ executable. The pinned templ release supports
the project's Go 1.23 baseline.

## Behavior

Pages render usable project status on the server. Browser JavaScript submits
allowlisted JSON actions and refreshes live sections every five seconds without
overwriting forms being edited. Successful saves preserve other drafts, show
contextual feedback, and refresh selector choices. Import reveals detected build
settings after inspection; advanced controls use native disclosure panels.
Services, backups, and encrypted exports have separate views, and unavailable
actions explain their prerequisites. The log viewer follows redacted log deltas every
two seconds, caps retained output at 1 MiB, and supports source/deployment
selection, pause, search, and download. Asset URLs carry content hashes for safe
caching across upgrades; HTML is never cached.

GitHub repository inspection precedes deployment and becomes invalid when its
source or build settings change. Runtime/build secrets and GitHub tokens are
write-only and cleared from forms after a successful save. Backup downloads,
encrypted exports, signed deployment hooks, operations, and diagnostic exports
use the existing authenticated Go API.

## Authentication and deployment

Public access uses `PAASD_PUBLIC_ORIGIN`, `PAASD_ADMIN_PASSWORD_HASH`, and
`PAASD_SESSION_SECRET` in the Go daemon. Generate a compatible password hash with
`./bin/paasd -hash-password`. See [deployment](../deploy/README.md).

Private repositories use a personal access token connected through Settings.
The daemon stores it encrypted and returns connection metadata only. Legacy
`GITHUB_TOKEN` is supported. GitHub OAuth is not implemented.

Optional failure explanations use `OPENAI_API_KEY` in the daemon environment.
The Explain failure action sends redacted evidence only when requested. See
[diagnostics](../docs/diagnostics.md#ai-failure-explanations).

## Checks

```sh
make web-check
make test
```

The API contract remains `GET /api/auth/status`, `POST /api/auth/login`,
`POST /api/auth/logout`, and `POST /api/dashboard/<functionName>`. Request bodies
contain function input directly. Host/origin/session checks, strict request
validation, bounds, and response redaction remain in Go.
