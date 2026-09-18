#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
if "$root/examples/broken-build/build.sh" >/tmp/harbor-broken-build.out 2>&1; then
	echo "broken-build fixture unexpectedly succeeded" >&2
	exit 1
fi

echo "broken-build fixture: rejected as expected"
