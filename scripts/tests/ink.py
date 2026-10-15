#!/usr/bin/env python3
"""Run a real Harbor deployment against a patched Ink checkout (host sandbox).

Build Harbor first, then run: python3 scripts/tests/ink.py /path/to/ink
Uses disposable Harbor state and a separate WAL directory; never contacts a
production daemon. Linux volume persistence requires an explicit host mount.
"""

import http.client
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import time


root = Path(__file__).resolve().parents[2]
ink = Path(sys.argv[1]).resolve()
assert (ink / "paas.yaml").is_file(), "Use the patched Ink checkout"


class UnixHTTP(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.host)


with tempfile.TemporaryDirectory(prefix="harbor-ink-", dir="/tmp") as temp:
    temp = Path(temp)
    sock = str(temp / "control.sock")
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    env = {k: v for k, v in os.environ.items() if not k.startswith("PAASD_")}
    log = (temp / "daemon.log").open("w+")
    daemon = subprocess.Popen([
        str(root / "bin/paasd"), "-socket", sock, "-data-dir", str(temp / "state"),
        "-listen", f"127.0.0.1:{port}", "-web-listen", "", "-sandbox", "host",
    ], stdout=log, stderr=log, env=env)

    def cli(*args, input=None, success=True):
        result = subprocess.run([str(root / "bin/paas"), "-socket", sock, *args],
                                input=input, capture_output=True, text=True, timeout=180)
        if success:
            assert result.returncode == 0, result.stdout + result.stderr
        else:
            assert result.returncode != 0, "Candidate unexpectedly succeeded"
        return result.stdout + result.stderr

    def request(method, path, value=None, expected=200):
        client = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
        body = json.dumps({"value": value}) if value is not None else None
        client.request(method, path, body, {"Host": "ink-smoke.localhost", "Content-Type": "application/json"})
        response = client.getresponse()
        data = response.read()
        client.close()
        assert response.status == expected, (method, path, response.status, data)
        return data

    def start():
        client = UnixHTTP(sock, timeout=30)
        client.request("POST", "/operations", json.dumps({"action": "start", "appName": "ink-smoke"}), {"Host": "localhost", "Content-Type": "application/json"})
        response = client.getresponse()
        data = response.read()
        client.close()
        assert response.status == 200, data

    try:
        for _ in range(100):
            if Path(sock).exists():
                break
            assert daemon.poll() is None, "Daemon exited"
            time.sleep(0.05)
        cli("ping")
        cli("deploy", str(ink), "--name", "ink-smoke")
        request("GET", "/health")
        cli("configure", "ink-smoke", "--env-stdin", input=f"INK_DATA_DIR={temp / 'wal'}\n")
        cli("stop", "ink-smoke")
        start()
        request("PUT", "/keep", "persisted")
        request("PUT", "/remove", "deleted")
        request("DELETE", "/remove", expected=204)
        cli("stop", "ink-smoke")
        start()
        assert json.loads(request("GET", "/keep"))["value"] == "persisted"
        request("GET", "/remove", expected=404)
        request("GET", "/")
        broken = temp / "broken"
        broken.mkdir()
        (broken / "go.mod").write_text("module startup-failure\n\ngo 1.23\n")
        (broken / "main.go").write_text('package main\nimport "os"\nfunc main() { os.Exit(7) }\n')
        (broken / "paas.yaml").write_text((ink / "paas.yaml").read_text())
        output = cli("deploy", str(broken), "--name", "ink-smoke", success=False)
        assert "exit code 7" in output, output
        assert json.loads(request("GET", "/keep"))["value"] == "persisted"
        print("PASS: Ink deploy, HTTP readiness, CRUD, WAL recovery after stop/start, and failed-candidate traffic safety")
    except BaseException:
        log.flush()
        log.seek(0)
        print(log.read(), file=sys.stderr)
        raise
    finally:
        daemon.terminate()
        try:
            daemon.wait(timeout=15)
        except subprocess.TimeoutExpired:
            daemon.kill()
            daemon.wait()
        log.close()
