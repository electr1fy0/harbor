# Tests

Portable package tests live next to their implementation so boundary behavior
is easy to discover. The scripts/tests checks exercise built binaries and
fixture failure semantics. Linux-specific runtime checks live under
scripts/spikes because they need the target host's namespaces, systemd, and
cgroup configuration.

`TestCycle4NodeDeploymentAndConcurrency` deploys the same Node fixture on
macOS host mode and Linux's default systemd/Bubblewrap backend. It checks
readiness, hostname routing, concurrent Go/Node requests, the expected PID
isolation, and route withdrawal on stop. The relative `node server.js` start
command must work in both environments.

For WSL 2, follow the [WSL setup and acceptance steps](../README.md#wsl-2-windows-subsystem-for-linux).
Run the Linux suites with the system manager after setup; missing user-manager
delegation is not a reason to skip privileged runtime acceptance. Passing these
tests on a regular Linux VM does not establish WSL verification.

For the Ink standalone compatibility fix and its optional real-deployment test,
see [docs/ink.md](../docs/ink.md). It requires a separate patched Ink checkout
and does not run as part of the default suite.
