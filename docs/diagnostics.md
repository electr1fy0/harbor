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

## AI failure explanations

The latest failed attempt has an **Explain failure** button for `BUILD_FAILED`,
`START_FAILED`, `UNHEALTHY`, `CRASHED`, and `OOM_KILLED`. It returns a short
explanation, supporting evidence, and one suggested next step. Existing log,
settings, retry, and rollback controls remain available; the explanation does
not execute actions or modify application code or settings.

Set `OPENAI_API_KEY` in the daemon's environment to enable the feature. The key
is server-only. Harbor makes one OpenAI Responses API request using
`gpt-5.4-mini` with structured output and `store: false`. No model request runs
automatically on deployment, page load, or polling. Without a key, requesting
an explanation reports that it is not configured.

The request includes bounded, redacted diagnostic log evidence and selected
deployment facts, including failure state, readiness configuration, resource
limits, and production status. It does not include repository source or
environment variable values. Logs can still contain application data beyond
the credentials Harbor knows how to redact; the button discloses that redacted
diagnostic evidence is sent to OpenAI. `store: false` disables Responses API
response storage; it does not override OpenAI's other data retention policies.

Harbor checks that the requested deployment is still the application's latest
failed attempt and validates evidence references against the supplied facts
and log lines. Model explanations can be uncertain; inspect the cited evidence
before following the suggested step. Unavailable providers, incomplete answers,
or invalid evidence return a local error while ordinary diagnostics remain
usable.

Successful explanations use a bounded in-memory cache scoped to the deployment
and evidence content. Harbor reloads redacted diagnostics before using a cached
answer, so changed evidence does not reuse the old answer. Concurrent requests
for the same evidence share one provider call. Errors are not cached, and a
daemon restart clears the cache.
