#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

make web
make build

exec ./bin/paasd \
	-socket /tmp/paasd.sock \
	-data-dir /tmp/harbor-dev \
	-listen 127.0.0.1:8080
