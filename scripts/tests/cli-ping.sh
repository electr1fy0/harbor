#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
socket=$(mktemp -u "/tmp/paasd.XXXXXX.sock")
daemon_pid=""
cleanup() {
	if [ -n "$daemon_pid" ]; then
		kill "$daemon_pid" 2>/dev/null || true
		wait "$daemon_pid" 2>/dev/null || true
	fi
	rm -f "$socket" "$root/.paasd-test.log" "$root/.paas-ping.out"
}
trap cleanup EXIT INT TERM

"$root/bin/paasd" -socket "$socket" >"$root/.paasd-test.log" 2>&1 &
daemon_pid=$!

ready=0
i=0
while [ "$i" -lt 50 ]; do
	if "$root/bin/paas" -socket "$socket" ping >"$root/.paas-ping.out" 2>/dev/null; then
		ready=1
		break
	fi
	i=$((i + 1))
done

if [ "$ready" -ne 1 ]; then
	cat "$root/.paasd-test.log" >&2
	exit 1
fi

if [ "$(tr -d '\r\n' <"$root/.paas-ping.out")" != "pong" ]; then
	cat "$root/.paas-ping.out" >&2
	exit 1
fi

echo "cli ping: ok"
