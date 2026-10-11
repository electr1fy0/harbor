# Harbor dashboard

A local deployment control plane built with TanStack Start, Router, Query,
Form, and Table. UI primitives come from the Base UI shadcn preset `b6F9PjJnG`
(Nova, Figtree, neutral surfaces, yellow accent, Tabler icons).

## Run

Requires Bun 1.4+. Start Harbor's Go daemon from the repository root (see the
root README), then:

```sh
bun install
PAASD_SOCKET=/tmp/paasd.sock bun run dev
```

Open http://127.0.0.1:3000. The dashboard reads real application, release, log,
and service data through TanStack server functions over the daemon's Unix
socket. Offline and empty states are displayed when no daemon or projects are
available.

## GitHub import

Browse an owner's public repositories or paste `https://github.com/owner/repo`.
Set an app name, optional branch/tag/commit ref, and optional repository root.
The root must contain a valid `paas.yaml`; supported build/start contracts are
documented in the root README. The daemon checks out the repository and uses
the same pipeline as local CLI deploys.

To browse and deploy private repositories, provide `GITHUB_TOKEN` to **both**
the web process and Go daemon. Tokens stay server-side. With a token, repository
browsing includes repositories accessible to its user (filtered by owner).
This release supports manual deployment, not GitHub OAuth or automatic pushes.

## Configuration

Copy `.env.example` to `.env.local` for local Vite development, or export these
variables when starting production:

| Variable | Purpose |
| --- | --- |
| `PAASD_SOCKET` | Control socket; defaults to `/run/paas/paasd.sock` |
| `GITHUB_TOKEN` | Optional server-side GitHub API credential; also set on daemon for private clones |

The web process must be allowed to open the owner-only control socket. Keep the
dashboard on loopback; it is an administrator tool without user authentication.
Do not prefix secrets with `VITE_`.

## Verify and build

```sh
bun run generate-routes
bun run typecheck
bun run lint
bun run test
bun run build
PAASD_SOCKET=/tmp/paasd.sock bun run start
```

The Nitro build is written to `.output/`; `bun run start` binds it to
`127.0.0.1:3000`. `PORT` can select another port. Development uses Vite at the
same loopback address. Do not expose the dashboard publicly without adding
authentication and configuring a trusted origin.
