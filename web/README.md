# Harbor dashboard

The dashboard is a React single-page app using TanStack Router, Query, Form,
and Table. The Go daemon serves the same-origin `/api` endpoints and the built
static files from `web/dist`; the browser never connects to the control socket
or GitHub directly.

## Development

Start the Go daemon with its dashboard listener on `127.0.0.1:3000` using the
local development instructions in the [root README](../README.md). Then run:

```sh
cd web
bun install
bun run dev
```

Open <http://127.0.0.1:5173>. Vite proxies `/api` requests to the Go dashboard
listener and keeps the browser's local Host and Origin headers intact.

## GitHub import

The dashboard can browse public repositories by owner and accepts GitHub HTTPS
repository URLs. A user can connect a personal access token to browse private
repositories and clone them. The token is sent to the Go API and stored by the
daemon in its private data directory; the API returns connection metadata only.
Legacy `GITHUB_TOKEN` configuration is also read by the daemon. GitHub OAuth is not implemented. The project Automation tab supports signed
GitHub push callbacks and authenticated CI deployment hooks.

The daemon inspects `paas.yaml` or detects supported Go and Node layouts, then
lets the user review build settings, runtime environment values, and an optional
managed SQLite service before starting an asynchronous deployment. See
[`../docs/onboarding.md`](../docs/onboarding.md) for repository permissions and
API details.

## Authentication and deployment

Public dashboard authentication is configured on the Go daemon with
`PAASD_PUBLIC_ORIGIN`, `PAASD_ADMIN_PASSWORD_HASH`, and `PAASD_SESSION_SECRET`.
Keep these values out of frontend environment variables and bundles. Generate a
compatible scrypt hash with `./bin/paasd -hash-password`; deployment setup is
documented in [`../deploy/README.md`](../deploy/README.md).

Optional failure explanations use `OPENAI_API_KEY` in the Go daemon's
environment and the OpenAI `gpt-5.4-mini` model. The project diagnostics button
sends redacted evidence only when clicked. Never set the key in Vite variables
or application settings. See [diagnostics](../docs/diagnostics.md#ai-failure-explanations)
for configuration and behavior.

The frontend build writes static files to `web/dist`. `make build` installs and
builds the SPA before compiling `paasd`, which embeds those assets. Production
serving uses the Go binary and does not need a Bun server process.

## Operations, data services, and observability

Project tabs expose operations, all deployment attempts and events, repository
automation, incremental redacted logs, historical metrics, maintenance and
primary-domain routing, notifications, and diagnostic exports. The Services
page manages SQLite bindings, snapshots, scheduled retention, verification,
clone restore, and encrypted Harbor exports. See [`../docs/features.md`](../docs/features.md)
for behavior, authentication, limits, and the first 50 feature coverage map.

## Checks

```sh
bun run generate-routes
bun run typecheck
bun run lint
bun run test
bun run build
```

The browser client calls `GET /api/auth/status`, `POST /api/auth/login`,
`POST /api/auth/logout`, and the allowlisted
`POST /api/dashboard/<functionName>` endpoints. Dashboard request bodies contain
the function input directly. The API returns typed frontend data and reports
errors as JSON; a 404 becomes a browser `HarborNotFoundError`.
