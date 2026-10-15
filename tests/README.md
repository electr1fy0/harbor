# Tests

Portable package tests live next to their implementation so boundary behavior
is easy to discover. The scripts/tests checks exercise built binaries and
fixture failure semantics. Linux-specific runtime checks live under
scripts/spikes because they need the target host's namespaces, systemd, and
cgroup configuration.

For the Ink standalone compatibility fix and its optional real-deployment test,
see [docs/ink.md](../docs/ink.md). It requires a separate patched Ink checkout
and does not run as part of the default suite.
