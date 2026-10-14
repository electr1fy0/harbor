#!/usr/bin/env python3
"""Exercise the installed executable without a checkout or JavaScript runtime."""
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[2]


def request(address, path, body=None, host=None, origin=None, cookie=None, method=None):
    headers = {}
    if host:
        headers["Host"] = host
    if origin:
        headers["Origin"] = origin
        headers["Sec-Fetch-Site"] = "same-origin"
    if cookie:
        headers["Cookie"] = cookie
    if body is not None:
        headers["Content-Type"] = "application/json"
        body = json.dumps(body)
    connection = http.client.HTTPConnection(address, timeout=20)
    connection.request(method or ("POST" if body is not None else "GET"), path, body, headers)
    response = connection.getresponse()
    result = response.status, dict(response.getheaders()), response.read()
    connection.close()
    return result


def run(public):
    with tempfile.TemporaryDirectory(prefix="harbor-dashboard-") as work:
        work = Path(work)
        binary = work / "paasd"
        shutil.copy2(ROOT / "bin/paasd", binary)
        env = dict(os.environ)
        for name in ("PAASD_PUBLIC_ORIGIN", "PAASD_ADMIN_PASSWORD_HASH", "PAASD_SESSION_SECRET", "GITHUB_TOKEN"):
            env.pop(name, None)
        # The executable serves all assets from an empty working directory and
        # does not need to find Bun/Node on PATH.
        env["PATH"] = "/usr/bin:/bin"
        password = "harbor-smoke-password"
        host = "dashboard.example.com" if public else None
        origin = "https://dashboard.example.com" if public else None
        if public:
            salt = bytes(range(32))
            digest = hashlib.scrypt(password.encode(), salt=salt, n=32768, r=8, p=1, dklen=32, maxmem=64 << 20)
            env.update(PAASD_PUBLIC_ORIGIN=origin,
                       PAASD_ADMIN_PASSWORD_HASH=f"scrypt$32768$8$1${salt.hex()}${digest.hex()}",
                       PAASD_SESSION_SECRET="42" * 32)
        log_path = work / "daemon.log"
        with log_path.open("wb") as log:
            process = subprocess.Popen([str(binary), "-socket", str(work / "control.sock"),
                                        "-data-dir", str(work / "data"), "-listen", "127.0.0.1:0",
                                        "-web-listen", "127.0.0.1:0", "-sandbox", "host"],
                                       cwd=work, env=env, stdout=log, stderr=log)
        try:
            address = None
            deadline = time.monotonic() + 20
            while time.monotonic() < deadline:
                content = log_path.read_text()
                match = re.search(r"dashboard ready: http://(\S+)", content)
                if match:
                    address = match[1]
                    break
                if process.poll() is not None:
                    raise AssertionError(f"daemon exited during startup:\n{content}")
                time.sleep(0.05)
            assert address, f"no dashboard listener:\n{log_path.read_text()}"
            if not public:
                origin = "http://" + address
            cookie = None
            if public:
                status, headers, _ = request(address, "/", host=host)
                assert status == 302 and headers.get("Location") == "/login", (status, headers)
                status, _, _ = request(address, "/api/dashboard/getOverview", {}, host, origin)
                assert status == 401, status
                status, _, body = request(address, "/api/auth/login", {"password": "incorrect"}, host, origin)
                assert status == 200 and not json.loads(body)["ok"], (status, body)
                status, headers, body = request(address, "/api/auth/login", {"password": password}, host, origin)
                assert status == 200 and json.loads(body)["ok"], (status, body)
                issued = headers["Set-Cookie"]
                assert all(flag in issued.lower() for flag in ("secure", "httponly", "samesite=lax", "path=/")), issued
                cookie = issued.split(";", 1)[0]
            status, _, html = request(address, "/login" if public else "/", host=host, cookie=cookie)
            assert status == 200 and b'<div id="root"' in html, (status, html[:100])
            assets = re.findall(r'(?:src|href)="(/assets/[^\"]+)"', html.decode())
            assert any(path.endswith(".js") for path in assets), html
            assert any(path.endswith(".css") for path in assets), html
            for asset in assets:
                status, headers, body = request(address, asset, host=host)
                assert status == 200 and len(body) > 0, (asset, status)
                assert "immutable" in headers.get("Cache-Control", ""), headers
            status, _, body = request(address, "/projects/example", host=host, cookie=cookie)
            assert status == 200 and body == html, (status, body[:100])
            status, _, body = request(address, "/api/dashboard/getOverview", {}, host, origin, cookie)
            overview = json.loads(body)
            assert status == 200 and overview["connected"] and overview["apps"] == [], (status, overview)
            assert overview["services"] == [], overview
            status, _, body = request(address, "/api/dashboard/getCapacity", {}, host, origin, cookie)
            assert status == 200 and "host" in json.loads(body), (status, body)
            status, _, _ = request(address, "/api/dashboard/getOverview", {}, host, "https://evil.example", cookie)
            assert status == 403, status
            status, _, _ = request(address, "/api/auth/status", host="evil.example")
            assert status == 403, status
            for missing in ("/assets/missing.js", "/api/missing"):
                status, _, _ = request(address, missing, host=host, cookie=cookie)
                assert status == 404, (missing, status)
            if public:
                status, headers, _ = request(address, "/api/auth/logout", {}, host, origin, cookie)
                assert status == 200 and "Max-Age=0" in headers["Set-Cookie"], (status, headers)
                status, _, body = request(address, "/api/auth/status", host=host)
                assert status == 200 and not json.loads(body)["authenticated"], (status, body)
        finally:
            if process.poll() is None:
                process.send_signal(signal.SIGTERM)
                try:
                    process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
                    raise AssertionError("daemon did not shut down")
        assert process.returncode == 0, log_path.read_text()


if __name__ == "__main__":
    run(public=False)
    run(public=True)
    print("single-binary dashboard smoke: local and public passed")
