# Tests

Portable package tests live next to their implementation so boundary behavior
is easy to discover. The scripts/tests checks exercise built binaries and
fixture failure semantics. Linux-specific runtime checks live under
scripts/spikes because they need the target host's namespaces, systemd, and
cgroup configuration.
