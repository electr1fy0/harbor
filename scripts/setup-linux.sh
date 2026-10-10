#!/bin/sh
set -eu

[ "$(uname -s)" = Linux ] || { echo "Linux required" >&2; exit 1; }
[ "$(id -u)" = 0 ] || { echo "Run with sudo: sudo ./scripts/setup-linux.sh" >&2; exit 1; }
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
for command in go gcc git bwrap systemd-run systemctl curl; do
	command -v "$command" >/dev/null || { echo "Missing dependency: $command" >&2; exit 1; }
done
[ -f /sys/fs/cgroup/cgroup.controllers ] || { echo "cgroup v2 required" >&2; exit 1; }
systemctl show-environment >/dev/null
for account in paas-build paas-runtime; do
	if ! id "$account" >/dev/null 2>&1; then
		useradd --system --no-create-home --shell /usr/sbin/nologin "$account"
	fi
	[ "$(id -u "$account")" -ne 0 ] || { echo "$account must not be root" >&2; exit 1; }
done
install -d -m 0755 /var/lib/paas /var/lib/paas/releases
install -d -m 0700 /var/lib/paas/logs
install -d -m 0755 /run/paas

if [ "$(sysctl -n kernel.apparmor_restrict_unprivileged_userns 2>/dev/null || echo 0)" = 1 ]; then
	command -v apparmor_parser >/dev/null || { echo "apparmor_parser required" >&2; exit 1; }
	profile=/etc/apparmor.d/harbor-bwrap
	if [ -f "$profile" ] && ! cmp -s "$root/deploy/apparmor/harbor-bwrap" "$profile"; then
		echo "Existing $profile differs; inspect it before installing this profile" >&2
		exit 1
	fi
	install -m 0644 "$root/deploy/apparmor/harbor-bwrap" "$profile"
	apparmor_parser -r "$profile"
fi
echo "Setup complete: build/runtime users, data paths, and Bubblewrap policy configured."
echo "Start: sudo ./bin/paasd"
echo "CLI:   sudo ./bin/paas ping"
