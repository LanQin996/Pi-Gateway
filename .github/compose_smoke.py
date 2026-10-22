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
from urllib.request import Request, build_opener, ProxyHandler
from uuid import uuid4

ACCOUNT_GROUPS_PATH = "/api/account-groups"
KEYS_PATH = "/api/keys"
MODELS_PATH = "/v1/models"

class Probe:
    def __init__(self, base_url, password):
        parsed = urlsplit(base_url)
        if parsed.scheme != "http" or parsed.hostname not in ("127.0.0.1", "::1") or parsed.path not in ("", "/"):
            raise ValueError("the Compose probe only accepts a loopback HTTP endpoint")
        self.base_url = base_url.rstrip("/")
        self.password = password
        self.token = ""
        # Do not send disposable credentials through ambient HTTP proxy settings.
        self.client = build_opener(ProxyHandler({}))

    def request(self, method, path, payload=None, *, token=None, status=200, raw=False):
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
        key = self.request("POST", KEYS_PATH, {
            "name": marker, "group_ids": [group["id"]], "max_concurrency": 2,
        })["key"]
        # Exercise update as well as insert and ensure it survives restart.
        self.request("PATCH", f'/api/keys/{key["id"]}', {"name": marker + "-updated"})
        self.request("GET", MODELS_PATH, token=key["key"])
        self.request("GET", MODELS_PATH, token="invalid-" + uuid4().hex, status=401)
        self.request("POST", "/api/auth/logout")
        self.token = ""
        return {"marker": marker, "group_id": group["id"], "key_id": key["id"], "client_key": key["key"]}

    def verify(self, state):
        if not re.fullmatch(r"compose-ci-[0-9a-f]{32}", state["marker"]):
            raise AssertionError("not a disposable Compose probe state")
        self.health_and_login()
        groups = self.request("GET", ACCOUNT_GROUPS_PATH)["groups"]
        group = next((entry for entry in groups if entry["id"] == state["group_id"]), None)
        if group is None or group["name"] != state["marker"] or group["notes"] != "must survive recreation" or group["disabled_models"] != ["blocked-ci-model"]:
            raise AssertionError("group data or model restrictions did not survive container recreation")
        keys = self.request("GET", KEYS_PATH)["keys"]
        key = next((entry for entry in keys if entry["id"] == state["key_id"]), None)
        if key is None or key["name"] != state["marker"] + "-updated" or key["group_ids"] != [state["group_id"]] or key["max_concurrency"] != 2:
            raise AssertionError("key data or group binding did not survive container recreation")
        self.request("GET", MODELS_PATH, token=state["client_key"])
        self.request("PATCH", f'/api/keys/{state["key_id"]}', {"enabled": False})
        self.request("GET", MODELS_PATH, token=state["client_key"], status=401)
        self.request("DELETE", f'/api/keys/{state["key_id"]}')
        self.request("DELETE", f'/api/account-groups/{state["group_id"]}')
        if any(entry["id"] == state["key_id"] for entry in self.request("GET", KEYS_PATH)["keys"]):
            raise AssertionError("deleted probe key still exists")
        if any(entry["id"] == state["group_id"] for entry in self.request("GET", ACCOUNT_GROUPS_PATH)["groups"]):
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
