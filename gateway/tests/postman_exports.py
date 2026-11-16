"""Stable Postman exports and local file links, independent of server state."""

import json
import os
from pathlib import PureWindowsPath
import subprocess
import tempfile
import uuid

import gate07_lab as fixtures


def save_json(path, value):
    """Keep identical exports untouched; replace changed files atomically."""
    if path.exists():
        try:
            matches = json.loads(path.read_text()) == value
        except (ValueError, UnicodeError):
            matches = False
        if matches:
            path.chmod(0o600)
            return "unchanged"
    status = "changed" if path.exists() else "created"
    fd, temporary = tempfile.mkstemp(prefix=".export-", dir=path.parent)
    try:
        with os.fdopen(fd, "w") as target:
            json.dump(value, target, indent=2)
            target.write("\n")
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)
    return status


def collection_links(path, label="postman_collection"):
    """Resolve a local import link, using the Windows host path under WSL."""
    links = {label + "_uri": path.resolve().as_uri()}
    if os.environ.get("WSL_DISTRO_NAME"):
        try:
            windows_path = subprocess.run(
                ["wslpath", "-w", str(path.resolve())],
                check=True,
                capture_output=True,
                text=True,
                timeout=3,
            ).stdout.strip()
            if PureWindowsPath(windows_path).is_absolute():
                links[label + "_windows_path"] = windows_path
                links[label + "_uri"] = PureWindowsPath(windows_path).as_uri()
        except (OSError, subprocess.SubprocessError):
            pass
    return links


def export_collection(work, base, key):
    source = fixtures.ROOT / "gateway/tests/gate07.postman_collection.json"
    collection = json.loads(source.read_text())
    collection["info"]["_postman_id"] = str(
        uuid.uuid5(uuid.NAMESPACE_URL, "llm-gateway-scheduler/reusable-tests")
    )
    collection["variable"] = [
        {"key": "gateway_test_url", "value": base, "type": "string"},
        {"key": "gateway_test_key", "value": key, "type": "string"},
    ]
    path = work / "postman.collection.json"
    return path, save_json(path, collection)


def export_environment(work, base, key):
    environment = {
        "id": str(
            uuid.uuid5(uuid.NAMESPACE_URL, "llm-gateway-scheduler/reusable-test-environment")
        ),
        "name": "Gateway tests — reusable",
        "values": [
            {"key": "gateway_test_url", "value": base, "type": "default", "enabled": True},
            {"key": "gateway_test_key", "value": key, "type": "secret", "enabled": True},
        ],
        "_postman_variable_scope": "environment",
    }
    path = work / "postman.environment.json"
    return path, save_json(path, environment)
