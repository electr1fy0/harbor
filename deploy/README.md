# Public Harbor deployment

This guide runs the Harbor control plane on a Linux host behind Caddy. Caddy
terminates public HTTPS; the dashboard and deployment daemon stay bound to
loopback. The web process runs as `paas-web` and can access the daemon socket
through the dedicated `paas-admins` group. The web process does not run as
root.

## Prerequisites

- A Linux host with systemd, Bun 1.4+, Python 3, and Caddy 2.11 or newer.
- A public DNS name for the dashboard and DNS names for each application.
- Ports 80 and 443 reachable on the host. Caddy obtains normal public HTTPS
  certificates for the dashboard and only application domains explicitly
  configured in Harbor.
- A deployment checkout/build installed under `/opt/harbor` and the daemon
  binary installed at `/usr/local/bin/paasd`.

Create a dedicated admin group and unprivileged web account. Add only trusted
operators who need to manage deployments to `paas-admins`:

```sh
sudo groupadd --system paas-admins
sudo useradd --system --home-dir /var/lib/harbor-web --create-home \
  --shell /usr/sbin/nologin --gid paas-admins paas-web
cd /opt/harbor
sudo install -d -o root -g paas-admins -m 0750 /etc/harbor
sudo install -o root -g paas-admins -m 0640 deploy/harbor.env.example /etc/harbor/harbor.env
```

Generate a password hash on the target host from the dashboard source checkout.
The utility reads the password from a hidden TTY prompt and prints only the
scrypt hash:

```sh
cd /opt/harbor/web
bun run hash-password
```

Set `PAASD_PUBLIC_ORIGIN` to the exact dashboard origin (for example
`https://dashboard.example.com`), paste the generated value as
`PAASD_ADMIN_PASSWORD_HASH`, and set `PAASD_SESSION_SECRET` to 32 random bytes
encoded as 64 lowercase hexadecimal characters. Generate it with
`openssl rand -hex 32`. Keep `/etc/harbor/harbor.env` root-owned and readable
only by root and `paas-admins`; never put the password itself in the file,
command line, or source tree. Also configure the optional `GITHUB_TOKEN` there
if the web dashboard should browse private repositories.

Public mode locks the dashboard if any auth variable is set without all valid
values. With all three auth variables unset, only loopback requests are
accepted and login is disabled for local development.

## Install and start

Install the service units from the deployment directory and reload systemd:

```sh
cd /opt/harbor/deploy
sudo install -m 0644 paasd.service /etc/systemd/system/paasd.service
sudo install -m 0644 harbor-web.service /etc/systemd/system/harbor-web.service
sudo systemctl daemon-reload
sudo systemctl enable --now paasd harbor-web
```

The daemon service listens on `127.0.0.1:8080`, serves the TLS authorization
check on `127.0.0.1:8081`, and creates `/run/paas/paasd.sock` with mode `0660`
for `paas-admins`. The web service connects through that socket and binds to
`127.0.0.1:3000`. Keep both services private; do not expose their ports through
a firewall or a second proxy. The daemon's TLS ask listener is loopback-only.

Create a Caddyfile using the exact same origin as the web environment:

```sh
cd /opt/harbor
PAASD_PUBLIC_ORIGIN=https://dashboard.example.com \
  ./deploy/render-caddyfile.sh | sudo tee /etc/caddy/Caddyfile >/dev/null
sudo caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
sudo systemctl reload caddy
```

Caddy's first site serves the dashboard at the configured dashboard hostname.
The HTTPS catch-all routes application traffic to the loopback app router and
uses Caddy on-demand TLS. When Caddy needs a new certificate, it calls Harbor's
`/tls/ask` endpoint with the requested hostname before issuance; cached
certificates do not require a check on every handshake. The endpoint accepts
requests only over loopback and approves only exact domains configured on an
application. Unconfigured hostnames are denied before certificate issuance.
Removing a domain prevents future certificate issuance and renewal, while an
already issued certificate remains valid until it expires. Keep the ask listener
and the app router loopback-only. Caddy's normal
certificate storage must remain persistent and writable by its service user.

## Configure domains and runtime settings

In a project's Settings tab, add exact DNS names that already resolve to this
host. Harbor validates names and the TLS check authorizes those configured
names only. Changes to environment keys/values, domains, and resource limits
are sent through the daemon's configure API; secret values are write-only and
are not returned to the browser. Environment and resource changes take effect
on the next deployment. Domain changes update the TLS allowlist for future
certificate authorization checks; Caddy can continue serving a cached
certificate for a removed domain until that certificate expires.

A running application's project detail link uses HTTPS only when the daemon's
settings response contains an exact configured domain. Otherwise it continues
to use the loopback development URL.

## Operations and recovery

Use `systemctl status paasd harbor-web caddy` and `journalctl -u paasd -u harbor-web -u caddy` to inspect service health. Keep `/etc/harbor/harbor.env` and Caddy's certificate data backed up securely. Rotate `PAASD_SESSION_SECRET` to invalidate all existing sessions. To disable public access, stop Caddy and unset the three public auth variables before restarting the web service; loopback local mode then becomes available.
