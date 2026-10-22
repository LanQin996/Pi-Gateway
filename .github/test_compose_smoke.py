"""Local verification of the Compose probe, not a substitute for Docker tests."""

from contextlib import contextmanager
from copy import deepcopy
from io import StringIO
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os
from pathlib import Path
import socket
import ssl
import subprocess
import tempfile
import time
from threading import Thread
import unittest
from urllib.error import URLError

from compose_smoke import Probe, state_path, entity_path, KEYS_PATH, ACCOUNT_GROUPS_PATH, NoRedirects, request_path
from unittest.mock import patch, Mock
from validate_compose_tls import validate, main as validate_main


ROOT = Path(__file__).resolve().parent.parent


class TLSFixture(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.scratch = tempfile.TemporaryDirectory(prefix="compose-probe-", dir=ROOT / ".github")
        cls.addClassCleanup(cls.scratch.cleanup)
        root = Path(cls.scratch.name)
        cls.tls_binary = root / ("compose-tls.exe" if os.name == "nt" else "compose-tls")
        subprocess.run(["go", "build", "-o", str(cls.tls_binary), "./.github/compose_tls"],
                       cwd=ROOT, check=True, timeout=180)
        cls.certs = root / "certs"
        subprocess.run([str(cls.tls_binary), "certs", "--dir", str(cls.certs)], check=True, timeout=15)
        cls.ca_file = cls.certs / "ca.crt"

    def tls_server(self, server, certificates=None):
        certificates = certificates or self.certs
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        context.load_cert_chain(certificates / "server.crt", certificates / "server.key")
        server.socket = context.wrap_socket(server.socket, server_side=True)
        return server

    @contextmanager
    def running_tls_server(self, handler, certificates=None):
        with self.tls_server(HTTPServer(("127.0.0.1", 0), handler), certificates) as server:
            server.received = []
            worker = Thread(target=server.serve_forever, daemon=True)
            worker.start()
            try:
                yield server
            finally:
                server.shutdown()
                worker.join(timeout=5)


class HealthServer(BaseHTTPRequestHandler):
    def do_GET(self):
        self.server.received.append((self.path, self.headers.get("Authorization")))
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b"{}")

    def log_message(self, format, *args):
        return


class ComposeProbeTest(TLSFixture):
    @classmethod
    def setUpClass(cls):
        super().setUpClass()
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
            except (URLError, OSError, AssertionError):
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
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            tls_port = listener.getsockname()[1]
        relay = subprocess.Popen([str(self.tls_binary), "serve", "--dir", str(self.certs),
                                  "--listen", f"127.0.0.1:{tls_port}", "--backend-port", str(port)],
                                 stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.addCleanup(self.stop_gateway, relay)
        probe = Probe(f"https://127.0.0.1:{tls_port}", "disposable-probe-password", self.ca_file)
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
        for url in ("http://127.0.0.1", "https://example.com", "https://127.0.0.1/admin",
                    "https://127.0.0.1?next=admin", "https://127.0.0.1#admin",
                    "https://user:password@127.0.0.1", "https://127.0.0.1:99999"):
            with self.subTest(url=url), self.assertRaises(ValueError):
                Probe(url, "unused")



class ProbeBoundaryTest(TLSFixture):
    def test_tls_rejects_untrusted_ca_before_sending_credentials(self):
        other = Path(self.scratch.name) / "other-ca"
        subprocess.run([str(self.tls_binary), "certs", "--dir", str(other)], check=True, timeout=15)
        with self.running_tls_server(HealthServer) as server:
            url = f"https://127.0.0.1:{server.server_port}"
            for authority in (None, other / "ca.crt"):
                probe = Probe(url, "unused", authority)
                with self.subTest(authority=authority), self.assertRaises(URLError) as raised:
                    probe.request("GET", "/healthz", token="disposable")
                self.assertIsInstance(raised.exception.reason, ssl.SSLCertVerificationError)
            self.assertEqual(server.received, [])
            self.assertEqual(Probe(url, "unused", self.ca_file).request("GET", "/healthz"), {})

    def test_tls_rejects_wrong_ip_san_before_sending_credentials(self):
        other = Path(self.scratch.name) / "wrong-ip"
        subprocess.run([str(self.tls_binary), "certs", "--dir", str(other), "--san-ip", "127.0.0.2"],
                       check=True, timeout=15)
        with self.running_tls_server(HealthServer, other) as server:
            probe = Probe(f"https://127.0.0.1:{server.server_port}", "unused", other / "ca.crt")
            with self.assertRaises(URLError) as raised:
                probe.request("GET", "/healthz", token="disposable")
            self.assertIsInstance(raised.exception.reason, ssl.SSLCertVerificationError)
            self.assertEqual(server.received, [])

    def test_tls_ignores_ambient_proxy(self):
        with self.running_tls_server(HealthServer) as server:
            # Any attempted proxy connection would fail, not silently go direct.
            environment = {"HTTP_PROXY": "http://127.0.0.1:1", "HTTPS_PROXY": "http://127.0.0.1:1",
                           "http_proxy": "http://127.0.0.1:1", "https_proxy": "http://127.0.0.1:1",
                           "NO_PROXY": "", "no_proxy": ""}
            with patch.dict(os.environ, environment):
                probe = Probe(f"https://127.0.0.1:{server.server_port}", "unused", self.ca_file)
                self.assertEqual(probe.request("GET", "/healthz", token="disposable"), {})
            self.assertEqual(server.received, [("/healthz", "Bearer disposable")])

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
                    probe = Probe("https://127.0.0.1:18443", "unused", self.ca_file)
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

        with self.tls_server(HTTPServer(("127.0.0.1", 0), RedirectServer)) as server:
            worker = Thread(target=server.serve_forever, daemon=True)
            worker.start()
            try:
                probe = Probe(f"https://127.0.0.1:{server.server_port}", "unused", self.ca_file)
                with self.assertRaisesRegex(AssertionError, "HTTP 302"):
                    probe.request("GET", "/healthz", token="disposable-test-token")
                self.assertEqual(received, [("/healthz", "Bearer disposable-test-token")])
            finally:
                server.shutdown()
                worker.join(timeout=5)

    def test_request_path_rejects_traversal_and_redirects(self):
        probe = Probe("https://127.0.0.1:18443/", "unused", self.ca_file)
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


class ComposeTLSConfigTest(unittest.TestCase):
    def configuration(self):
        return {"services": {
            "pi-gateway": {"image": "pi-gateway:ci", "environment": {"PI_GATEWAY_HOST": "127.0.0.1"},
                           "ports": [{"host_ip": "127.0.0.1", "target": 18443, "published": "18443", "protocol": "tcp"}]},
            "compose-tls": {"image": "pi-gateway:ci", "user": "1001:1001", "network_mode": "service:pi-gateway",
                            "volumes": [{"target": "/ci/compose-tls", "read_only": True}, {"target": "/ci/certs", "read_only": True}]}}}

    def test_supported_version_and_namespace(self):
        for version in ("2.24.4", "v2.40.1", "2.40.1-desktop.1", "3.0.0"):
            with self.subTest(version=version):
                self.assertTrue(validate(version, self.configuration(), "1001", "1001"))
        for version in ("2.24.3", "1.29.2", "", "unknown"):
            config = self.configuration()
            with self.subTest(version=version), self.assertRaises(ValueError):
                validate(version, config, "1001", "1001")

    def test_cli_consumes_json_without_a_filename(self):
        arguments = ["validate_compose_tls.py", "--version", "2.24.4", "--uid", "1001", "--gid", "1001"]
        source = StringIO(json.dumps(self.configuration()))
        with patch("sys.argv", arguments), patch("sys.stdin", source), patch("sys.stdout", new_callable=StringIO) as output:
            validate_main()
        self.assertIn("Verified shared TLS namespace", output.getvalue())

    def test_cli_rejects_filename_arguments(self):
        arguments = ["validate_compose_tls.py", "--version", "2.24.4", "--uid", "1001", "--gid", "1001",
                     "--config", "caller-supplied.json"]
        with patch("sys.argv", arguments), patch("sys.stderr", new_callable=StringIO) as output:
            with self.assertRaises(SystemExit) as raised:
                validate_main()
        self.assertEqual(raised.exception.code, 2)
        self.assertIn("unrecognized arguments: --config", output.getvalue())

    def test_cleartext_or_external_ports_rejected(self):
        base = self.configuration()
        extra = deepcopy(base["services"]["pi-gateway"]["ports"])
        extra.append({"host_ip": "127.0.0.1", "target": 8317, "published": "8317", "protocol": "tcp"})
        for ports in (extra, [], [{"host_ip": "0.0.0.0", "target": 18443, "published": "18443", "protocol": "tcp"}],
                      [{"host_ip": "127.0.0.1", "target": 8317, "published": "18443", "protocol": "tcp"}]):
            config = deepcopy(base)
            config["services"]["pi-gateway"]["ports"] = ports
            with self.subTest(ports=ports), self.assertRaises(ValueError):
                validate("2.24.4", config, "1001", "1001")

    def test_gateway_binding_rejected(self):
        config = self.configuration()
        config["services"]["pi-gateway"]["environment"]["PI_GATEWAY_HOST"] = "0.0.0.0"
        with self.assertRaises(ValueError):
            validate("2.24.4", config, "1001", "1001")

    def test_relay_namespace_owner_and_readonly_mounts_required(self):
        for field, value in (("network_mode", "bridge"), ("ports", [{}]), ("networks", ["default"]),
                             ("user", "0:0"), ("image", "untested"), ("volumes", [])):
            config = self.configuration()
            config["services"]["compose-tls"][field] = value
            with self.subTest(field=field), self.assertRaises(ValueError):
                validate("2.24.4", config, "1001", "1001")


if __name__ == "__main__":
    unittest.main()
