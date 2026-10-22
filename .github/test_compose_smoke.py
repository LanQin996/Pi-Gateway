"""Local verification of the Compose probe, not a substitute for Docker tests."""

from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
from threading import Thread
import unittest
from urllib.error import URLError

from compose_smoke import Probe, state_path, entity_path, KEYS_PATH, ACCOUNT_GROUPS_PATH, NoRedirects, request_path
from unittest.mock import patch, Mock


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
        for url in ("https://127.0.0.1", "http://example.com", "http://127.0.0.1/admin",
                    "http://127.0.0.1?next=admin", "http://127.0.0.1#admin",
                    "http://user:password@127.0.0.1", "http://127.0.0.1:99999"):
            with self.subTest(url=url), self.assertRaises(ValueError):
                Probe(url, "unused")



class ProbeBoundaryTest(unittest.TestCase):
    def test_request_path_preserves_static_allowlist(self):
        for path in ("/", "/healthz", "/api/auth/login", "/api/auth/logout",
                     "/v1/models", "/api/keys", "/api/account-groups"):
            with self.subTest(path=path):
                self.assertEqual(request_path(path), path)
        for path in (None, 1, [], "/api/auth", "/api/auth/login/", "/healthz\n"):
            with self.subTest(path=path), self.assertRaises(ValueError):
                request_path(path)

    def test_request_path_entity_digits_remain_ascii_and_bounded(self):
        for collection in (KEYS_PATH, ACCOUNT_GROUPS_PATH):
            for identifier in ("1", "123", "9223372036854775807", "9999999999999999999"):
                path = collection + "/" + identifier
                with self.subTest(path=path):
                    self.assertEqual(request_path(path), path)
            for identifier in ("", "0", "01", "-1", "+1", "1" * 20,
                               "١", "1١", "１", "1１", "1²", "1\n", "1/", "%31"):
                with self.subTest(collection=collection, identifier=identifier), self.assertRaises(ValueError):
                    request_path(collection + "/" + identifier)

    def test_request_path_assets_keep_same_filename_boundary(self):
        for filename in ("a.js", "Z9_-x.css", "_entry.abc-123.js", "-entry.css", "a..js"):
            path = "/assets/" + filename
            with self.subTest(path=path):
                self.assertEqual(request_path(path), path)
        for filename in (".hidden.js", "../secret.js", "a/b.js", "é.js", "a١.js",
                         "a.JS", "a.json", "a.js?x=1", "a.js#x", "a.js\n"):
            with self.subTest(filename=filename), self.assertRaises(ValueError):
                request_path("/assets/" + filename)

    def test_state_ids_cannot_supply_api_path_components(self):
        for field in ("group_id", "key_id"):
            for invalid in ("1", "1/../../auth/logout", "%2f..%2f", "1?enabled=true", "1#fragment", 0, -1, True, 1.0, None, 1 << 63):
                with self.subTest(field=field, value=invalid):
                    state = {"marker": "compose-ci-" + "a" * 32, "group_id": 1, "key_id": 2, "client_key": "private"}
                    state[field] = invalid
                    probe = Probe("http://127.0.0.1:8317", "unused")
                    probe.client = Mock()
                    with self.assertRaises(ValueError):
                        probe.verify(state)
                    probe.client.open.assert_not_called()

    def test_entity_paths_have_only_numeric_segments(self):
        self.assertEqual(entity_path(KEYS_PATH, 123), "/api/keys/123")
        self.assertEqual(entity_path(ACCOUNT_GROUPS_PATH, (1 << 63) - 1), "/api/account-groups/9223372036854775807")
        with self.assertRaises(ValueError):
            entity_path("/api/auth", 1)

    def test_redirect_response_never_forwards_authorization(self):
        received = []

        class RedirectServer(BaseHTTPRequestHandler):
            def do_GET(self):
                received.append((self.path, self.headers.get("Authorization")))
                self.send_response(302)
                self.send_header("Location", "/api/auth/logout")
                self.end_headers()

            def log_message(self, format, *args):
                # Keep private probe credentials and URLs out of test logs.
                return

        with HTTPServer(("127.0.0.1", 0), RedirectServer) as server:
            worker = Thread(target=server.serve_forever, daemon=True)
            worker.start()
            try:
                probe = Probe(f"http://127.0.0.1:{server.server_port}", "unused")
                with self.assertRaisesRegex(AssertionError, "HTTP 302"):
                    probe.request("GET", "/healthz", token="disposable-test-token")
                self.assertEqual(received, [("/healthz", "Bearer disposable-test-token")])
            finally:
                server.shutdown()
                worker.join(timeout=5)

    def test_request_path_rejects_traversal_and_redirects(self):
        probe = Probe("http://127.0.0.1:8317/", "unused")
        probe.client = Mock()
        for path in ("//example.invalid", "http://example.invalid", "/api/keys/1/../2", "/api/keys/1?x=1", "/api/keys/1#x", "/assets/../secret.js", "/assets/%2e%2e%2fsecret.js", "/assets/a.js?next=/admin", "/api/keys/1\n"):
            with self.subTest(path=path), self.assertRaises(ValueError):
                probe.request("GET", path)
        probe.client.open.assert_not_called()
        self.assertIsNone(NoRedirects().redirect_request(None, None, 302, "Found", {}, "http://example.invalid"))


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
