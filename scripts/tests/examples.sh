#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
if command -v node >/dev/null 2>&1; then
	node --test "$root/examples/hello-node/server.test.js"
fi
tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/harbor-examples.XXXXXX")
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
	rm -rf "$tmpdir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

ports=$(python3 - <<'PY'
import socket

listeners = []
ports = []
for _ in range(2):
    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    listeners.append(listener)
    ports.append(listener.getsockname()[1])
print(*ports)
for listener in listeners:
    listener.close()
PY
)
hello_port=${ports%% *}
unhealthy_port=${ports#* }

PORT=$hello_port "$root/bin/hello-go" >"$tmpdir/hello.log" 2>&1 &
hello_pid=$!
PORT=$unhealthy_port "$root/bin/unhealthy" >"$tmpdir/unhealthy.log" 2>&1 &
unhealthy_pid=$!

i=0
while [ "$i" -lt 50 ]; do
	if curl -fsS "http://127.0.0.1:$hello_port/health" >"$tmpdir/health.out" 2>/dev/null; then
		break
	fi
	i=$((i + 1))
	sleep 0.05
done
[ "$i" -lt 50 ]
[ "$(tr -d '\r\n' <"$tmpdir/health.out")" = "ok" ]
[ "$(curl -fsS "http://127.0.0.1:$hello_port/" | tr -d '\r\n')" = "hello from harbor" ]

unhealthy_status=$(curl -sS -o "$tmpdir/unhealthy.out" -w '%{http_code}' "http://127.0.0.1:$unhealthy_port/health")
[ "$unhealthy_status" = "503" ]

MEMORY_HOG_MB=8 "$root/bin/memory-hog" >"$tmpdir/hog.out" 2>&1 &
hog_pid=$!
i=0
while [ "$i" -lt 50 ]; do
	if grep -q 'allocated 8 MiB' "$tmpdir/hog.out" 2>/dev/null; then
		break
	fi
	i=$((i + 1))
	sleep 0.05
done
[ "$i" -lt 50 ]

echo "example apps: hello-go health/root, unhealthy 503, memory-hog allocation: ok"
