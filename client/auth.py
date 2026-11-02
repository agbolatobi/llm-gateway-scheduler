"""Load a locally saved gateway key only for its configured loopback origin."""

import json
import os
from pathlib import Path
import re
import stat
from urllib.parse import urlsplit


def _origin(value):
    if not isinstance(value, str):
        return None
    try:
        url = urlsplit(value)
        if (url.scheme != "http" or url.username is not None
                or url.password is not None or url.path not in ("", "/")
                or url.query or url.fragment
                or url.hostname not in {"localhost", "127.0.0.1", "::1"}
                or url.port is None or url.port == 0):
            return None
        host = "127.0.0.1" if url.hostname == "localhost" else url.hostname
        return url.scheme, host, url.port
    except ValueError:
        return None


def saved_api_key(base_url):
    """Return the selected key, or None when no key matches this server."""
    origin = _origin(base_url)
    if origin is None:
        return None
    path = Path(os.environ.get(
        "LLM_GATEWAY_CREDENTIALS",
        str(Path.home() / ".config" / "llm-gateway-scheduler" / "credentials.json"),
    ))
    try:
        parent = path.parent.lstat()
        if not stat.S_ISDIR(parent.st_mode) or parent.st_mode & 0o077:
            raise ValueError("Gateway credentials directory must have mode 0700.")
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    except FileNotFoundError:
        return None
    with os.fdopen(fd, "r", encoding="utf-8") as source:
        info = os.fstat(source.fileno())
        if (not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077
                or (hasattr(os, "getuid") and info.st_uid != os.getuid())):
            raise ValueError("Gateway credentials must be an owner-only regular file (0600).")
        raw = source.read((1 << 20) + 1)
    if len(raw) > 1 << 20:
        raise ValueError("Gateway credentials file exceeds 1 MiB.")
    try:
        saved = json.loads(raw)
        if not isinstance(saved, dict) or saved.get("version") != 1:
            raise ValueError
        keys = saved["keys"]
        if not isinstance(keys, dict):
            raise ValueError
        selected = saved.get("active_key_id")
        if not selected:
            return None
        key = keys[selected]
        if not isinstance(key, dict):
            raise ValueError
        if _origin(key.get("base_url")) != origin:
            return None
        token = key["api_key"]
        if not isinstance(token, str) or not re.fullmatch(r"lgw_[0-9a-f]{16}_[A-Za-z0-9_-]{43}", token):
            raise ValueError
        return token
    except (ValueError, KeyError, TypeError):
        raise ValueError("Invalid gateway credentials; create or select a saved key.") from None
