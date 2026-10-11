#!/bin/sh
set -eu

: "${PAASD_PUBLIC_ORIGIN:?Set PAASD_PUBLIC_ORIGIN to the exact HTTPS dashboard origin, for example https://dashboard.example.com}"
dashboard_host=$(python3 - "$PAASD_PUBLIC_ORIGIN" <<'PY'
import ipaddress
import re
import sys
from urllib.parse import urlsplit

origin = sys.argv[1]
try:
    parsed = urlsplit(origin)
    host = parsed.hostname or ""
    port = parsed.port
    is_ip = False
    try:
        ipaddress.ip_address(host)
        is_ip = True
    except ValueError:
        pass
    valid_domain = bool(
        re.fullmatch(
            r"(?=.{1,253}$)(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?",
            host,
        )
    )
    if (
        parsed.scheme != "https"
        or not host
        or is_ip
        or not valid_domain
        or port not in (None, 443)
        or parsed.username is not None
        or parsed.password is not None
        or parsed.path not in ("", "/")
        or parsed.query
        or parsed.fragment
        or origin != f"https://{host}"
    ):
        raise ValueError
except (ValueError, UnicodeError):
    print(
        "PAASD_PUBLIC_ORIGIN must be exactly https:// followed by a public DNS name, without a path or nonstandard port.",
        file=sys.stderr,
    )
    sys.exit(1)

print(host)
PY
)

cat <<CADDY
{
	on_demand_tls {
		ask http://127.0.0.1:8081/tls/ask
	}
}

${dashboard_host} {
	reverse_proxy 127.0.0.1:3000
}

https:// {
	tls {
		on_demand
	}
	reverse_proxy 127.0.0.1:8080
}
CADDY
