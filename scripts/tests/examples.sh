#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
hello_pid=""
unhealthy_pid=""
hog_pid=""
cleanup() {
	if [ -n "$hello_pid" ]; then
		kill "$hello_pid" 2>/dev/null || true
		wait "$hello_pid" 2>/dev/null || true
	fi
	if [ -n "$unhealthy_pid" ]; then
		kill "$unhealthy_pid" 2>/dev/null || true
		wait "$unhealthy_pid" 2>/dev/null || true
	fi
	if [ -n "$hog_pid" ]; then
		kill "$hog_pid" 2>/dev/null || true
		wait "$hog_pid" 2>/dev/null || true
	fi
	rm -f /tmp/harbor-example-hello.log /tmp/harbor-example-unhealthy.log
}
trap cleanup EXIT INT TERM

PORT=39081 "$root/bin/hello-go" >/tmp/harbor-example-hello.log 2>&1 &
hello_pid=$!
PORT=39082 "$root/bin/unhealthy" >/tmp/harbor-example-unhealthy.log 2>&1 &
unhealthy_pid=$!

i=0
while [ "$i" -lt 50 ]; do
	if curl -fsS http://127.0.0.1:39081/health >/tmp/harbor-example-health.out 2>/dev/null; then
		break
	fi
	i=$((i + 1))
	sleep 0.05
done
[ "$i" -lt 50 ]
[ "$(tr -d '\r\n' </tmp/harbor-example-health.out)" = "ok" ]
[ "$(curl -fsS http://127.0.0.1:39081/ | tr -d '\r\n')" = "hello from harbor" ]

unhealthy_status=$(curl -sS -o /tmp/harbor-example-unhealthy.out -w '%{http_code}' http://127.0.0.1:39082/health)
[ "$unhealthy_status" = "503" ]

MEMORY_HOG_MB=8 "$root/bin/memory-hog" >/tmp/harbor-example-hog.out 2>&1 &
hog_pid=$!
i=0
while [ "$i" -lt 50 ]; do
	if rg -q 'allocated 8 MiB' /tmp/harbor-example-hog.out 2>/dev/null; then
		break
	fi
	i=$((i + 1))
	sleep 0.05
done
[ "$i" -lt 50 ]

echo "example apps: hello-go health/root, unhealthy 503, memory-hog allocation: ok"
