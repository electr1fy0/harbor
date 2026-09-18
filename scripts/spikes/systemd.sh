#!/bin/sh
set -eu

if [ "$(uname -s)" != "Linux" ]; then
	echo "systemd spike: SKIP (Linux-only; host is $(uname -s))"
	exit 0
fi
if ! command -v systemd-run >/dev/null 2>&1 || ! command -v systemctl >/dev/null 2>&1; then
	echo "systemd spike: SKIP (systemd-run/systemctl is not installed)"
	exit 0
fi

if ! systemctl --user show-environment >/dev/null 2>&1; then
	echo "systemd spike: SKIP (no usable user systemd manager)"
	exit 0
fi

unit="paas-spike-$$.service"
cleanup() {
	systemctl --user stop "$unit" >/dev/null 2>&1 || true
	systemctl --user reset-failed "$unit" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

systemd-run --user --unit="$unit" /bin/sh -c 'sleep 30' >/dev/null
i=0
while [ "$i" -lt 20 ]; do
	if [ "$(systemctl --user show "$unit" -p ActiveState --value 2>/dev/null || true)" = "active" ]; then
		break
	fi
	i=$((i + 1))
done
if [ "$i" -ge 20 ]; then
	echo "systemd spike: unit did not become active" >&2
	exit 1
fi

state=$(systemctl --user show "$unit" -p ActiveState --value)
systemctl --user stop "$unit" >/dev/null
stopped=$(systemctl --user show "$unit" -p ActiveState --value)
[ "$state" = "active" ]
[ "$stopped" = "inactive" ]

exit_status_unit="paas-spike-exit-$$.service"
systemd-run --user --unit="$exit_status_unit" --wait /bin/sh -c 'exit 0' >/dev/null
result=$(systemctl --user show "$exit_status_unit" -p Result --value)
status=$(systemctl --user show "$exit_status_unit" -p ExecMainStatus --value)
systemctl --user reset-failed "$exit_status_unit" >/dev/null 2>&1 || true
[ "$result" = "success" ]
[ "$status" = "0" ]

echo "systemd spike: start=$state stop=$stopped result=$result exit=$status"
