#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
"$root/scripts/spikes/bubblewrap.sh"
"$root/scripts/spikes/systemd.sh"
"$root/scripts/spikes/cgroup-oom.sh"
