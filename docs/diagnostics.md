# Deployment diagnostics

Harbor tracks the release currently serving production separately from the
newest deployment attempt. A failed candidate does not replace a healthy active
deployment, so an app can report a running production release and a failed
latest attempt at the same time.

`POST /apps` keeps the existing `Deployment` field for compatibility and adds
`Production` and `LatestAttempt`. `Production` is the deployment currently
mapped to the app's `.localhost` production route. `LatestAttempt` is the most
recent deployment record, including failed candidates. Either value is `null`
when no corresponding deployment exists.

`POST /diagnostics` accepts `{"Name":"app-name"}` and returns a `diagnostics`
object with `production`, `latestAttempt`, `summary`, an optional bounded
`excerpt`, and `actions`. The summary uses the recorded state and backend
capabilities: it recommends checking the injected `PORT` and configured health
path after readiness failures, inspecting runtime logs after a crash, and
increasing memory before redeploy after an OOM kill only when the selected
systemd sandbox recorded an enforced memory ceiling. Other hosts are described
as not enforcing that ceiling. Build and runtime error text is never used to
invent a root cause.

Actions indicate whether a safe operation applies. Settings remain available
for changing runtime limits and environment keys. A GitHub retry is available
only when Harbor has a persisted GitHub source for the app. Rollback is offered
only when production is not serving and a different previously promoted
release exists. UI stop and rollback actions continue to require confirmation.

`POST /logs` accepts the existing `Name` field and optional `DeploymentID`.
When supplied, Harbor returns that deployment's logs only if the deployment
belongs to the named app. Omitting it selects the latest deployment attempt.
The dashboard uses this endpoint to inspect the failed candidate while leaving
active production intact.

Environment values and managed-service credentials are redacted before any
daemon log response or diagnostic excerpt is returned. The service also keeps
encrypted redaction history so replacing or removing a value does not expose it
in older release logs. If the encryption key or history cannot be read, log
diagnostics fail closed with a generic error instead of returning unredacted
text. The dashboard receives only safe deployment metadata, summary text, and
redacted log excerpts.
