"""Local verification of the Compose probe, not a substitute for Docker tests."""

import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import unittest
from urllib.error import URLError

from compose_smoke import Probe, state_path
from unittest.mock import patch


ROOT = Path(__file__).resolve().parent.parent


class ComposeProbeTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.scratch = tempfile.TemporaryDirectory(prefix="compose-probe-", dir=ROOT / ".github")
        cls.addClassCleanup(cls.scratch.cleanup)
        cls.binary = Path(cls.scratch.name) / ("pi-gateway.exe" if os.name == "nt" else "pi-gateway")
        subprocess.run(["go", "build", "-o", str(cls.binary), "./cmd/pi-gateway"], cwd=ROOT, check=True, timeout=180)

    def start_gateway(self, config):
        env = {name: value for name, value in os.environ.items() if not name.startswith("PI_GATEWAY_")}
        # Every path and credential is private to this temporary test instance.
        process = subprocess.Popen([str(self.binary), "--config", str(config)], cwd=self.scratch.name,
                                   env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.addCleanup(self.stop_gateway, process)
        return process

    @staticmethod
    def stop_gateway(process):
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=20)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)

    def wait_ready(self, process, probe):
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            if process.poll() is not None:
                self.fail(f"private gateway exited with status {process.returncode}")
            try:
                probe.request("GET", "/healthz", token="")
                return
            except (URLError, OSError):
                time.sleep(0.05)
        self.fail("private gateway did not become ready")

    def test_seed_reopen_verify(self):
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        root = Path(self.scratch.name)
        config = root / "probe.yaml"
        # JSON is valid YAML and avoids platform-specific quoting of Windows paths.
        config.write_text(json.dumps({
            "server": {"host": "127.0.0.1", "port": port},
            "data": {"driver": "sqlite", "database": str(root / "probe.db")},
            "redis": {"enabled": False},
            "admin": {"username": "admin", "password": "disposable-probe-password"},
            "logging": {"level": "error", "file": ""},
        }), encoding="utf-8")
        probe = Probe(f"http://127.0.0.1:{port}", "disposable-probe-password")
        first = self.start_gateway(config)
        self.wait_ready(first, probe)
        state = probe.seed()
        self.stop_gateway(first)
        second = self.start_gateway(config)
        self.wait_ready(second, probe)
        probe.verify(state)
        # A successful verify must delete its own fixtures, not merely check HTTP.
        with self.assertRaisesRegex(AssertionError, "did not survive"):
            probe.verify(state)
        self.stop_gateway(second)

    def test_probe_rejects_nonlocal_targets(self):
        for url in ("https://127.0.0.1", "http://example.com", "http://127.0.0.1/admin"):
            with self.subTest(url=url), self.assertRaises(ValueError):
                Probe(url, "unused")


class StatePathTest(unittest.TestCase):
    def test_confines_state_to_runner_temp(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {"RUNNER_TEMP": directory}):
            root = Path(directory)
            accepted = root / "pi-gateway-ci-test.json"
            self.assertEqual(state_path(accepted), accepted.resolve())
            for candidate in (root / "secret.json", root / ".." / accepted.name, root / "subdir" / accepted.name):
                with self.subTest(path=candidate), self.assertRaises((ValueError, FileNotFoundError)):
                    state_path(candidate)
            target = root / "target"
            target.write_text("private", encoding="utf-8")
            accepted.symlink_to(target)
            with self.assertRaisesRegex(ValueError, "symbolic link"):
                state_path(accepted)


if __name__ == "__main__":
    unittest.main()
