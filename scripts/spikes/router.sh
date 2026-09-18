#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
binary=$(mktemp "/tmp/harbor-router.XXXXXX")
log_file=$(mktemp "/tmp/harbor-router-log.XXXXXX")
router_pid=""
cleanup() {
	if [ -n "$router_pid" ]; then
		kill "$router_pid" 2>/dev/null || true
		wait "$router_pid" 2>/dev/null || true
	fi
	rm -f "$binary" "$log_file" /tmp/harbor-router-a.out
}
trap cleanup EXIT INT TERM

go build -o "$binary" "$root/cmd/router-spike"
"$binary" -listen 127.0.0.1:18080 -demo >"$log_file" 2>&1 &
router_pid=$!

i=0
while [ "$i" -lt 50 ]; do
	if curl -fsS -H 'Host: a.localhost' http://127.0.0.1:18080/ >/tmp/harbor-router-a.out 2>/dev/null; then
		break
	fi
	i=$((i + 1))
	sleep 0.05
done

if [ "$i" -ge 50 ]; then
	cat "$log_file" >&2
	exit 1
fi

a=$(tr -d '\r\n' </tmp/harbor-router-a.out)
b=$(curl -fsS -H 'Host: b.localhost:8080' http://127.0.0.1:18080/ | tr -d '\r\n')
[ "$a" = "server A" ]
[ "$b" = "server B" ]
echo "router spike: a.localhost -> $a; b.localhost -> $b"
