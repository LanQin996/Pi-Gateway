"""Exercise a disposable Compose gateway before/after container recreation.

Only loopback is accepted. No external model/OAuth calls are made. State contains
one disposable client key, is written owner-only, and is removed after verify.
"""

import argparse
import json
import os
from pathlib import Path
import re
import sys
import tempfile
from urllib.error import HTTPError
from urllib.parse import urlsplit
from urllib.request import Request, build_opener, ProxyHandler, HTTPRedirectHandler
from uuid import uuid4

ACCOUNT_GROUPS_PATH = "/api/account-groups"
KEYS_PATH = "/api/keys"
MODELS_PATH = "/v1/models"

def entity_id(value):
    """State and API responses may select only positive signed-64-bit IDs."""
    if type(value) is not int or not 0 < value <= (1 << 63) - 1:
        raise ValueError("probe entity IDs must be positive signed-64-bit integers")
    return int(value)


def entity_path(collection, value):
    # Numeric conversion prevents file content from supplying URL delimiters,
    # even if an upstream state/schema validation is changed later.
    identifier = entity_id(value)
    if collection == KEYS_PATH:
        return "/api/keys/" + str(identifier)
    if collection == ACCOUNT_GROUPS_PATH:
        return "/api/account-groups/" + str(identifier)
    raise ValueError("unsupported probe collection")


def request_path(path):
    # No encoded separators, traversal segments, queries, fragments or origins.
    if not isinstance(path, str):
        raise ValueError("unsupported Compose probe request path")
    static_paths = ("/", "/healthz", "/api/auth/login", "/api/auth/logout",
                    MODELS_PATH, KEYS_PATH, ACCOUNT_GROUPS_PATH)
    entity_pattern = r"/api/(?:keys|account-groups)/[1-9]\d{0,18}"
    asset_pattern = r"/assets/[A-Za-z0-9_-][A-Za-z0-9_.-]*\.(?:js|css)"
    if not (path in static_paths
            or re.fullmatch(entity_pattern, path, flags=re.ASCII)
            or re.fullmatch(asset_pattern, path, flags=re.ASCII)):
        raise ValueError("unsupported Compose probe request path")
    return path


class NoRedirects(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        # A local server must not redirect disposable bearer credentials elsewhere.
        return None


class Probe:
    def __init__(self, base_url, password):
        parsed = urlsplit(base_url)
        if (parsed.scheme != "http" or parsed.hostname not in ("127.0.0.1", "::1")
                or parsed.path not in ("", "/") or parsed.query or parsed.fragment
                or parsed.username is not None or parsed.password is not None):
            raise ValueError("the Compose probe only accepts a loopback HTTP endpoint")
        host = "[::1]" if parsed.hostname == "::1" else "127.0.0.1"
        port = "" if parsed.port is None else ":" + str(int(parsed.port))
        self.base_url = "http://" + host + port
        self.password = password
        self.token = ""
        # Do not send disposable credentials through ambient HTTP proxy settings.
        self.client = build_opener(ProxyHandler({}), NoRedirects())

    def request(self, method, path, payload=None, *, token=None, status=200, raw=False):
        path = request_path(path)
        headers = {}
        if token is None:
            token = self.token
        if token:
            headers["Authorization"] = "Bearer " + token
        data = None if payload is None else json.dumps(payload).encode()
        if data is not None:
            headers["Content-Type"] = "application/json"
        request = Request(self.base_url + path, data=data, headers=headers, method=method)
        try:
            response = self.client.open(request, timeout=15)
        except HTTPError as error:
            response = error
        with response:
            if response.status != status:
                # Avoid leaking generated client keys or login response bodies.
                raise AssertionError(f"{method} {path}: HTTP {response.status}, wanted {status}")
            body = response.read(2 << 20)
        return body if raw else json.loads(body)

    def health_and_login(self):
        self.request("GET", "/healthz", token="")
        html = self.request("GET", "/", token="", raw=True)
        assets = re.findall(rb'(?:src|href)="(/assets/[^\"]+)"', html)
        if not assets:
            raise AssertionError("image does not serve the compiled frontend assets")
        self.request("GET", assets[0].decode(), token="", raw=True)
        result = self.request("POST", "/api/auth/login", {
            "username": "admin", "password": self.password,
        }, token="")
        self.token = result["token"]
        if not self.token:
            raise AssertionError("login returned no token")

    def seed(self):
        self.health_and_login()
        marker = "compose-ci-" + uuid4().hex
        group = self.request("POST", ACCOUNT_GROUPS_PATH, {
            "name": marker, "notes": "must survive recreation", "disabled_models": ["blocked-ci-model"],
        })["group"]
        group_id = entity_id(group["id"])
        key = self.request("POST", KEYS_PATH, {
            "name": marker, "group_ids": [group_id], "max_concurrency": 2,
        })["key"]
        key_id = entity_id(key["id"])
        # Exercise update as well as insert and ensure it survives restart.
        self.request("PATCH", entity_path(KEYS_PATH, key_id), {"name": marker + "-updated"})
        self.request("GET", MODELS_PATH, token=key["key"])
        self.request("GET", MODELS_PATH, token="invalid-" + uuid4().hex, status=401)
        self.request("POST", "/api/auth/logout")
        self.token = ""
        return {"marker": marker, "group_id": group_id, "key_id": key_id, "client_key": key["key"]}

    def verify(self, state):
        if not re.fullmatch(r"compose-ci-[0-9a-f]{32}", state["marker"]):
            raise AssertionError("not a disposable Compose probe state")
        # Validate file-derived identifiers before any authenticated request.
        group_id = entity_id(state["group_id"])
        key_id = entity_id(state["key_id"])
        group_path = entity_path(ACCOUNT_GROUPS_PATH, group_id)
        key_path = entity_path(KEYS_PATH, key_id)
        self.health_and_login()
        groups = self.request("GET", ACCOUNT_GROUPS_PATH)["groups"]
        group = next((entry for entry in groups if entry["id"] == group_id), None)
        if group is None or group["name"] != state["marker"] or group["notes"] != "must survive recreation" or group["disabled_models"] != ["blocked-ci-model"]:
            raise AssertionError("group data or model restrictions did not survive container recreation")
        keys = self.request("GET", KEYS_PATH)["keys"]
        key = next((entry for entry in keys if entry["id"] == key_id), None)
        if key is None or key["name"] != state["marker"] + "-updated" or key["group_ids"] != [group_id] or key["max_concurrency"] != 2:
            raise AssertionError("key data or group binding did not survive container recreation")
        self.request("GET", MODELS_PATH, token=state["client_key"])
        self.request("PATCH", key_path, {"enabled": False})
        self.request("GET", MODELS_PATH, token=state["client_key"], status=401)
        self.request("DELETE", key_path)
        self.request("DELETE", group_path)
        if any(entry["id"] == key_id for entry in self.request("GET", KEYS_PATH)["keys"]):
            raise AssertionError("deleted probe key still exists")
        if any(entry["id"] == group_id for entry in self.request("GET", ACCOUNT_GROUPS_PATH)["groups"]):
            raise AssertionError("deleted probe group still exists")
        self.request("POST", "/api/auth/logout")


def state_path(value):
    """Confine CLI-selected files to the trusted runner's temporary directory."""
    root = Path(os.environ.get("RUNNER_TEMP") or tempfile.gettempdir()).resolve(strict=True)
    path = Path(value)
    if path.parent.resolve(strict=True) != root or not re.fullmatch(r"pi-gateway-ci-[A-Za-z0-9_-]+\.json", path.name):
        raise ValueError("state must be a pi-gateway-ci-*.json file directly in RUNNER_TEMP (or the system temporary directory)")
    if path.is_symlink():
        raise ValueError("state must not be a symbolic link")
    return root / path.name


def read_state(path):
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
    fd = os.open(path, flags)
    with os.fdopen(fd, "r", encoding="utf-8") as source:
        return json.load(source)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("phase", choices=("seed", "verify"))
    parser.add_argument("--url", default="http://127.0.0.1:8317")
    parser.add_argument("--state", type=Path, required=True)
    args = parser.parse_args()
    args.state = state_path(args.state)
    password = os.environ.get("PI_GATEWAY_ADMIN_PASSWORD")
    if not password:
        raise ValueError("PI_GATEWAY_ADMIN_PASSWORD is required for the disposable probe")
    probe = Probe(args.url, password)
    if args.phase == "seed":
        state = probe.seed()
        # Refuse to overwrite another run's state or follow a pre-existing symlink.
        fd = os.open(args.state, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as output:
            json.dump(state, output)
        print("Compose health, frontend, authentication, and seed CRUD passed.")
    else:
        probe.verify(read_state(args.state))
        args.state.unlink()
        print("Compose recreation, persisted bindings, live revocation, and cleanup passed.")


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        # Do not dump HTTP response bodies, environment variables or state keys.
        print(f"Compose probe failed: {error}", file=sys.stderr)
        sys.exit(1)
