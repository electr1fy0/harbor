# Public Harbor deployment

This guide runs the complete Harbor control plane and dashboard in one
`paasd` binary and process behind Caddy. Caddy terminates public HTTPS; the
embedded dashboard and application router bind to loopback. The dashboard API executes inside the root-owned deployment daemon.
The Unix socket remains private for CLI administration.

## Prerequisites

- A Linux host with systemd, Python 3, and Caddy 2.11 or newer.
- A public DNS name for the dashboard.
- Ports 80 and 443 reachable on the host.
- A release `paasd` binary installed at `/usr/local/bin/paasd`, plus Harbor's
  runtime prerequisites from the root README. No Bun/Node runtime or frontend
  checkout is needed on the target host.

When building a release from source, run `make build` on the build machine.
Only Go/C tooling is required: templ components and embedded CSS/JavaScript
compile directly into `bin/paasd`. Generated Go components are checked in, so
`go build ./cmd/paasd` from a fresh checkout includes a working dashboard.
After editing `.templ` files, run `make web` and include the regenerated Go files.

Create a dedicated admin group containing only trusted CLI operators, and a
root-only dashboard configuration file:

```sh
sudo groupadd --system paas-admins
sudo install -d -o root -g root -m 0700 /etc/harbor
sudo install -o root -g root -m 0600 deploy/harbor.env.example /etc/harbor/harbor.env
```

Generate the dashboard password hash with the installed binary. It reads a
hidden terminal prompt and prints only the scrypt hash:

```sh
paasd -hash-password
```

Set `PAASD_PUBLIC_ORIGIN` to the exact dashboard origin (for example
`https://dashboard.example.com`), paste the generated value as
`PAASD_ADMIN_PASSWORD_HASH`, and set `PAASD_SESSION_SECRET` to 32 random bytes
encoded as 64 lowercase hexadecimal characters. Generate it with
`openssl rand -hex 32`. Keep `/etc/harbor/harbor.env` root-owned and readable
only by root; never put the password itself in the file,
command line, or source tree. Connect a GitHub personal access token through the dashboard to browse and clone
private repositories. Harbor stores it encrypted in the daemon data directory;
see [GitHub onboarding](../docs/onboarding.md) for repository permissions.

Dashboard startup fails if any auth variable is set without all valid values. With all three auth variables unset, only loopback requests are
accepted and login is disabled for local development.

To enable **Explain failure**, set `OPENAI_API_KEY` in the same root-only
configuration file and restart `paasd`. The daemon calls the OpenAI Responses
API with `gpt-5.4-mini` only when an operator requests an explanation. The key
must stay out of application environment settings and frontend configuration.
Without it, the dashboard and existing diagnostics still work; requesting an
explanation reports that the feature is not configured. See
[failure explanations](../docs/diagnostics.md#ai-failure-explanations) for the
evidence sent to OpenAI and cache behavior.

## Install and start

Install the service units from the deployment directory and reload systemd:

```sh
cd /opt/harbor/deploy
sudo install -m 0644 paasd.service /etc/systemd/system/paasd.service
sudo systemctl daemon-reload
sudo systemctl enable --now paasd
```

The daemon service listens on `127.0.0.1:8080` and creates
`/run/paas/paasd.sock` with mode `0660` for `paas-admins`. The same daemon
serves the dashboard on `127.0.0.1:3000`. Keep both TCP listeners private.

Create a Caddyfile using the exact same origin as the web environment:

```sh
cd /opt/harbor
PAASD_PUBLIC_ORIGIN=https://dashboard.example.com \
  ./deploy/render-caddyfile.sh | sudo tee /etc/caddy/Caddyfile >/dev/null
sudo caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
sudo systemctl reload caddy
```

Caddy serves the authenticated dashboard at the configured dashboard hostname.
Applications use generated `http://APP.localhost:8080` URLs on the Harbor host.
The generated Caddyfile publishes only the dashboard.

## Configure runtime settings

Use a project's Settings tab to update environment keys/values and resource
limits. Secret values are write-only and are not returned to the browser.
Changes take effect on the next deployment, rollback, or daemon recovery.

## Upgrade from custom-domain routing

Regenerate and reload the Caddyfile and install the updated `paasd.service`
before restarting the upgraded daemon. The `-tls-ask-listen` flag and TLS
authorization endpoint have been removed. The database upgrade removes custom
hostname mappings and retains each application's generated `.localhost` route,
runtime settings, and deployment history. Existing application custom URLs
will no longer serve through this configuration.

## Operations and recovery

Use `systemctl status paasd caddy` and `journalctl -u paasd -u caddy` to inspect service health. Keep `/etc/harbor/harbor.env` and Caddy's certificate data backed up securely. Rotate `PAASD_SESSION_SECRET` to invalidate all existing sessions. To disable public access, stop Caddy and unset the three public auth variables before restarting `paasd`; loopback local mode then becomes available.


## Upgrade from the separate Bun dashboard

Build and install the new `paasd` executable before changing the running services.
Keep the existing public origin, password hash and session secret in the
configuration file; the scrypt hash format is compatible. Change the file and
parent directory to `root:root`, modes `0600` and `0700` respectively. Install the
updated `paasd.service`, which now reads that environment file. Leave the Caddy
dashboard upstream at `127.0.0.1:3000`.

```sh
sudo systemctl disable --now harbor-web
sudo rm -f /etc/systemd/system/harbor-web.service
sudo systemctl daemon-reload
sudo systemctl restart paasd
```

Stopping the old dashboard releases port 3000 for the embedded listener. A
`paasd` restart also restarts managed applications under its existing lifecycle.
Check dashboard login and CLI access after the restart. The old `paas-web`
account and `/opt/harbor/web` installation are no longer used; remove them only
when no other local work depends on them.

Use `-web-listen ''` to run the daemon without its dashboard. The CLI socket and
application router continue to work. All listeners shut down with the same
process, so updating the dashboard means updating and restarting `paasd`.
