#!/bin/sh
set -eu

if [ "$(uname -s)" != "Linux" ]; then
	echo "cgroup/OOM spike: SKIP (Linux-only; host is $(uname -s))"
	exit 0
fi
if ! command -v systemd-run >/dev/null 2>&1 || ! command -v systemctl >/dev/null 2>&1; then
	echo "cgroup/OOM spike: SKIP (systemd-run/systemctl is not installed)"
	exit 0
fi
if ! systemctl --user show-environment >/dev/null 2>&1; then
	echo "cgroup/OOM spike: SKIP (no usable user systemd manager)"
	exit 0
fi

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
go build -o "$tmp/memory-hog" "$root/examples/memory-hog"

unit="paas-spike-oom-$$.service"
set +e
systemd-run --user --unit="$unit" --wait \
	-p MemoryMax=64M -p MemorySwapMax=0 -p TasksMax=64 -p CPUQuota=50% \
	env MEMORY_HOG_MB=256 "$tmp/memory-hog" >"$tmp/stdout" 2>"$tmp/stderr"
run_status=$?
set -e

result=$(systemctl --user show "$unit" -p Result --value 2>/dev/null || true)
oom=$(systemctl --user show "$unit" -p OOMKilled --value 2>/dev/null || true)
systemctl --user reset-failed "$unit" >/dev/null 2>&1 || true

cat "$tmp/stdout" "$tmp/stderr" 2>/dev/null || true
if [ "$result" != "oom-kill" ] && [ "$oom" != "yes" ]; then
	echo "cgroup/OOM spike: expected oom-kill, got result=$result oom=$oom status=$run_status" >&2
	exit 1
fi
echo "cgroup/OOM spike: ok (result=$result oom=$oom)"
