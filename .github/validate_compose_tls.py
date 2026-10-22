"""Fail closed if Compose merging exposes cleartext or separates the TLS relay."""
import argparse
import json
import re
import sys


def validate(version, config, uid, gid):
    validate_version(version)
    gateway = config["services"]["pi-gateway"]
    relay = config["services"]["compose-tls"]
    if gateway["environment"].get("PI_GATEWAY_HOST") != "127.0.0.1":
        raise ValueError("CI gateway must bind only shared-namespace loopback")
    validate_ports(gateway)
    validate_relay(relay, gateway, uid, gid)
    return True


def validate_version(version):
    match = re.fullmatch(r"v?(\d+)\.(\d+)\.(\d+)(?:[-+][A-Za-z\d.-]+)?", version.strip(), flags=re.ASCII)
    if not match or tuple(map(int, match.groups())) < (2, 24, 4):
        raise ValueError("CI TLS requires Docker Compose >= 2.24.4 for !override")


def validate_ports(gateway):
    ports = gateway.get("ports", [])
    if len(ports) != 1:
        raise ValueError("CI must replace, not append, gateway published ports")
    port = ports[0]
    if (port.get("host_ip") != "127.0.0.1" or int(port["target"]) != 18443
            or int(port["published"]) != 18443 or port.get("protocol") != "tcp"):
        raise ValueError("only the loopback TLS port may be published")


def validate_relay(relay, gateway, uid, gid):
    if relay.get("network_mode") != "service:pi-gateway" or relay.get("ports") or relay.get("networks"):
        raise ValueError("TLS relay must share the gateway network namespace")
    if relay.get("image") != gateway["image"] or relay.get("user") != f"{uid}:{gid}":
        raise ValueError("TLS relay must use the tested image and fixture file owner")
    mounts = {volume["target"]: volume for volume in relay.get("volumes", [])}
    for target in ("/ci/compose-tls", "/ci/certs"):
        if mounts.get(target, {}).get("read_only") is not True:
            raise ValueError("TLS fixture files must be mounted read-only")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    parser.add_argument("--version-only", action="store_true")
    parser.add_argument("--uid")
    parser.add_argument("--gid")
    args = parser.parse_args()
    validate_version(args.version)
    if args.version_only:
        return
    if not all((args.uid, args.gid)):
        parser.error("--uid and --gid are required for configuration validation")
    # Consume the Compose pipeline directly; this validator never opens a path
    # supplied by a caller or by the serialized configuration.
    config = json.load(sys.stdin)
    validate(args.version, config, args.uid, args.gid)
    print("Verified shared TLS namespace, private upstream and TLS-only host port.")


if __name__ == "__main__":
    main()
