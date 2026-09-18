# memory-hog

Allocates and touches MEMORY_HOG_MB MiB (default 256) and then holds it. The
cgroup spike runs it with a low memory limit and verifies an OOM result.
