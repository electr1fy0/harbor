# Harbor classroom demo (10–15 minutes)

Prepare the host, dashboard build, dependencies, and build caches beforehand.
Use a small local Go/Node fixture and generated `.localhost` URLs. Keep two successful releases ready so rollback is available immediately. For the
GitHub dashboard import flow, connect and test the repository in advance.

## Suggested walkthrough

| Time | Demonstration |
|---|---|
| 0–2 min | Explain source → build → release → health check → route; open the dashboard and a running app. |
| 2–5 min | Deploy a small change; show progress, events, logs, and the updated app. |
| 5–8 min | Trigger a prepared build failure; show the error and that the healthy version still serves. |
| 8–10 min | Roll back to a saved release and verify the old response. |
| 10–12 min | Save an environment key or resource preset; show pending settings. Toggle maintenance and restore normal traffic. |
| 12–15 min | Optional: demonstrate a prepared SQLite app or a manual backup, then take questions. |

Custom domains, DNS/HTTPS checks, primary-domain redirects, and application
certificate authorization have been removed. Local hostname routing remains.

## Features to explain or show with prepared evidence

| Feature | Why a live classroom demo is awkward |
|---|---|
| Hourly/daily/weekly scheduled backups and staleness | The natural schedule exceeds the session. Show scheduling plus an existing snapshot; manual backup is quick. |
| Full hour/day resource history | A fresh session cannot produce a representative history. Short live samples can still be shown on a supported host. |
| Signed GitHub push hooks and external CI callbacks | Require a publicly reachable endpoint, repository/CI configuration, secrets, and network access. Demonstrable if configured beforehand. |
| HTTPS notifications and threshold/cooldown delivery | Require a reachable public receiver plus a triggering event; private/loopback receivers are rejected. Show recorded delivery evidence. |
| Automatic recovery, repeated health failures, and crash-loop backoff | Require fault injection and waiting through probes/retries. A single prepared restart is possible; the full policy distracts from the core demo. |
| Daemon crash/reboot recovery and durable job recovery | Interrupt the dashboard and apps. Use a recording or describe saved desired state and queued-job recovery. |
| Linux namespace isolation, cgroup limits, and OOM classification | Need a prepared Linux/systemd host and a controlled workload. macOS local mode cannot prove these controls. |
| Release retention and build-cache pruning at scale | Need enough existing releases/cache data to make the result meaningful. A small seeded example is possible. |
| Encrypted full export and offline decrypt | Require a passphrase, downloaded archive, and a separate restore/decrypt explanation. A manual SQLite backup is easier to fit. |
| AI failure explanation | Depends on a configured OpenAI key and internet access. The ordinary error/log diagnostics work locally. |

Keep these features available; the table is a presentation scope, not a removal
list. Fit the first ten minutes around deploy, safe failure, and rollback.
