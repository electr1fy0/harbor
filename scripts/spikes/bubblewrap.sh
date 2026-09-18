#!/bin/sh
set -eu

if [ "$(uname -s)" != "Linux" ]; then
	echo "bubblewrap spike: SKIP (Linux-only; host is $(uname -s))"
	exit 0
fi
if [ "$(id -u)" = "0" ]; then
	echo "bubblewrap spike: FAIL (run as an unprivileged user to prove user namespaces)"
	exit 1
fi
if ! command -v bwrap >/dev/null 2>&1; then
	echo "bubblewrap spike: SKIP (bwrap is not installed)"
	exit 0
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
mkdir -p "$tmp/app"
printf '%s\n' 'read-only app fixture' >"$tmp/app/marker"

host_user_ns=$(readlink /proc/self/ns/user)
host_pid_ns=$(readlink /proc/self/ns/pid)
host_ipc_ns=$(readlink /proc/self/ns/ipc)
host_uts_ns=$(readlink /proc/self/ns/uts)
host_mount_ns=$(readlink /proc/self/ns/mnt)
output=$(bwrap \
	--die-with-parent \
	--unshare-user --unshare-pid --unshare-ipc --unshare-uts \
	--dir /app \
	--uid 0 --gid 0 \
	--ro-bind /bin /bin \
	--ro-bind /etc /etc \
	--ro-bind /lib /lib \
	--ro-bind /lib64 /lib64 \
	--ro-bind /usr /usr \
	--ro-bind "$tmp/app" /app \
	--tmpfs /tmp \
	--proc /proc \
	--dev /dev \
	/bin/sh -eu -c '
		[ "$(id -u)" = "0" ]
		[ "$(readlink /proc/self/ns/user)" != "$1" ]
		[ "$(readlink /proc/self/ns/pid)" != "$2" ]
		[ "$(readlink /proc/self/ns/ipc)" != "$3" ]
		[ "$(readlink /proc/self/ns/uts)" != "$4" ]
		[ "$(readlink /proc/self/ns/mnt)" != "$5" ]
		[ "$(cat /app/marker)" = "read-only app fixture" ]
		if touch /app/must-not-write 2>/dev/null; then exit 10; fi
		touch /tmp/must-write
		printf "%s\n" "namespaces: user pid ipc uts mount"
		printf "%s\n" "app: read-only"
		printf "%s\n" "tmp: writable"
	' harbor-bwrap "$host_user_ns" "$host_pid_ns" "$host_ipc_ns" "$host_uts_ns" "$host_mount_ns"
)

printf '%s\n' "$output"
echo "bubblewrap spike: ok"
