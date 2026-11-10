"""Explicit model management; never discovers ports or writes registrations."""
import argparse
from contextlib import contextmanager
import fcntl
import hashlib
import json
import math
import os
from pathlib import Path
import select
import signal
import sys
import tempfile
import time
from urllib.parse import urlsplit

import requests

from . import server
from .model_config import DEFAULT_CONFIG, ModelConfig, local_url

def query(session, address, path):
    with session.get(address + path, timeout=(2, 3), allow_redirects=False, stream=True) as response:
        if response.status_code != 200:
            raise RuntimeError("Runtime endpoint unavailable")
        data = bytearray()
        for chunk in response.iter_content(65536):
            data.extend(chunk)
            if len(data) > 8 << 20:
                raise RuntimeError("Runtime response too large")
        return json.loads(data)

def inspect(address, *, model=None, config=None):
    """Read only this explicitly supplied/configured address; no port scan."""
    address = local_url(address)
    try:
        with requests.Session() as session:
            session.trust_env = False
            if query(session, address, "/health").get("status") != "ok":
                return "unavailable", "Runtime is not ready"
            listing = query(session, address, "/v1/models")
            entries = listing.get("data")
            if not isinstance(entries, list) or len(entries) != 1 or not isinstance(entries[0], dict) or not isinstance(entries[0].get("id"), str):
                return "mismatch", "Expected one identifiable model"
            identity = entries[0]["id"]
            if model is None:
                return "ready", identity
            if identity != model["backend"]["expected_model"]:
                return "mismatch", "Running model identity differs from registration"
            slots = query(session, address, "/slots")
            if not isinstance(slots, list) or not slots or any(not isinstance(s, dict) or type(s.get("n_ctx")) is not int or s["n_ctx"] < model["context_limit"] for s in slots):
                return "mismatch", "Per-request context is smaller than registered or unverifiable"
            if model.get("launch") is not None and config is not None:
                props = query(session, address, "/props")
                reported = props.get("model_path")
                expected = config.resolve_path(model["launch"]["model_file"])
                if not isinstance(reported, str) or not Path(reported).is_absolute() or Path(reported).resolve() != expected:
                    return "mismatch", "Running model file differs from launch registration or cannot be verified"
            return "ready", identity
    except (requests.RequestException, OSError, ValueError, AttributeError, RuntimeError):
        return "unavailable", "Runtime could not be verified (connection, metadata, or endpoint failure)"

def registration_warning(config, address, expected_model):
    """A direct CLI URL does not imply permission to add a registration."""
    try:
        normalized = local_url(address)
    except ValueError:
        return ""  # Never probe a remote address for local discovery.
    gateway = local_url("http://" + config.data["gateway"]["listen"])
    if normalized == gateway:
        return ""
    matches = [m for m in config.models if local_url(m["backend"]["base_url"]) == normalized and expected_model in [m["id"], m["backend"]["expected_model"], *(m.get("aliases") or [])]]
    if matches:
        return ""
    return "Warning: this model/address is not registered. Direct access remains available, but gateway access requires an explicit configuration edit and reload. No registration or quota was changed."

def record_path(model):
    return server.PROJECT_ROOT / ".runtime" / "models" / (model["backend"]["runtime_id"] + ".json")

def process_identity(pid):
    process = Path("/proc") / str(pid)
    if process.stat().st_uid != os.geteuid():
        raise RuntimeError("Process belongs to another user")
    # Fields after the final ')' begin with field 3; starttime is field 22.
    start = (process / "stat").read_text().rsplit(")", 1)[1].split()[19]
    deadline = time.monotonic() + 1.0
    while True:
        argv = (process / "cmdline").read_bytes()
        executable = str((process / "exe").resolve(strict=True))
        cwd = str((process / "cwd").resolve(strict=True))
        if (process / "stat").read_text().rsplit(")", 1)[1].split()[19] != start:
            raise RuntimeError("Process identity changed during capture")
        if argv:
            return {"pid": pid, "start_ticks": start, "executable": executable,
                    "cwd": cwd, "argv_sha256": hashlib.sha256(argv).hexdigest()}
        # WSL can briefly expose an empty cmdline immediately after exec.
        # Never persist that as a usable ownership fingerprint.
        if time.monotonic() >= deadline:
            raise RuntimeError("Process command line could not be verified")
        time.sleep(0.01)

def save_record(path, data):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    descriptor, temporary = tempfile.mkstemp(dir=path.parent, prefix=".model-")
    try:
        with os.fdopen(descriptor, "w") as output:
            json.dump(data, output)
            output.flush(); os.fsync(output.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)

def start(config, model, timeout):
    launch = model.get("launch")
    if launch is None:
        raise ValueError("This registration is externally managed; start it yourself, then use models status")
    path = record_path(model)
    owned = []
    def observe(process, _log):
        identity = process_identity(process.pid)
        identity.update(model_file=str(config.resolve_path(launch["model_file"])), runtime_id=model["backend"]["runtime_id"], address=local_url(model["backend"]["base_url"]))
        save_record(path, identity)
        owned.append(identity)
    def verify():
        state, detail = inspect(model["backend"]["base_url"], model=model, config=config)
        if state != "ready":
            raise RuntimeError(detail + "; existing processes were not replaced")
    try:
        log = server.ensure_server_ready(model["backend"]["base_url"], model=model["backend"]["expected_model"], timeout=timeout,
            server_options={"ctx_size": model["context_limit"], "parallel": 1, "threads": launch["threads"]},
            model_file=config.resolve_path(launch["model_file"]), server_binary=config.resolve_path(launch["executable"]),
            device=launch["device"], on_start=observe, verify_ready=verify)
    except BaseException:
        if owned and path.exists():
            # Startup cleaned up only its own Popen; do not remove a newer record.
            try:
                if json.loads(path.read_text()) == owned[0]:
                    path.unlink()
            except (OSError, ValueError):
                pass
        raise
    return f"{model['id']}: ready ({'started' if log is not None else 'reused'})."

@contextmanager
def port_lock(address, timeout):
    port = urlsplit(local_url(address)).port
    path = server.PROJECT_ROOT / ".runtime" / f"llama-server-{port}.lock"
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a") as lock:
        deadline = time.monotonic() + timeout
        while True:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    raise RuntimeError("Timed out waiting for the model startup/stop lock")
                time.sleep(0.05)
        yield

def stop(config, model, timeout):
    if model.get("launch") is None:
        raise ValueError("Externally managed server: no process was stopped")
    if not hasattr(os, "pidfd_open") or not hasattr(signal, "pidfd_send_signal"):
        raise RuntimeError("Safe stopping requires Linux pidfd support")
    path = record_path(model)
    with port_lock(model["backend"]["base_url"], timeout):
        if not path.exists():
            return "No project-owned process record; no process was stopped."
        if path.is_symlink() or path.stat().st_uid != os.geteuid() or path.stat().st_mode & 0o077:
            raise RuntimeError("Unsafe process record; no process was stopped")
        record = json.loads(path.read_text())
        pid = record.get("pid")
        if type(pid) is not int or pid <= 1:
            raise RuntimeError("Invalid process record")
        try:
            descriptor = os.pidfd_open(pid)
        except ProcessLookupError:
            path.unlink()
            return "Owned process already exited; stale record removed."
        try:
            poller = select.poll(); poller.register(descriptor, select.POLLIN)
            if poller.poll(0):
                path.unlink(); return "Owned process already exited; stale record removed."
            identity = process_identity(pid)
            launch = model["launch"]
            if (any(record.get(k) != v for k, v in identity.items())
                    or identity["executable"] != str(config.resolve_path(launch["executable"]))
                    or identity["cwd"] != str(server.PROJECT_ROOT.resolve())
                    or record.get("runtime_id") != model["backend"]["runtime_id"]
                    or record.get("address") != local_url(model["backend"]["base_url"])
                    or record.get("model_file") != str(config.resolve_path(launch["model_file"]))):
                raise RuntimeError("Process identity changed; no signal sent")
            signal.pidfd_send_signal(descriptor, signal.SIGTERM)
            if not poller.poll(math.ceil(timeout * 1000)):
                raise RuntimeError("Graceful shutdown timed out; process was not force-killed")
            path.unlink()
            return f"Stopped {model['id']} (PID {pid})."
        finally:
            os.close(descriptor)

def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("list", "validate", "status", "check", "start", "stop"))
    parser.add_argument("model", nargs="?")
    parser.add_argument("--config", default=str(DEFAULT_CONFIG))
    parser.add_argument("--base-url", help="Explicit loopback URL, only for check")
    parser.add_argument("--timeout", type=float, default=120)
    args = parser.parse_args(argv)
    if not math.isfinite(args.timeout) or not 0 < args.timeout <= 3600:
        parser.error("timeout must be positive and at most 3600 seconds")
    if args.action in {"start", "stop"} and not args.model:
        parser.error("start/stop require a registered model name")
    if args.action == "check" and (not args.base_url or args.model):
        parser.error("check requires --base-url and no model name")
    if args.action != "check" and args.base_url:
        parser.error("--base-url is only supported by check")
    if args.action in {"list", "validate"} and args.model:
        parser.error("list/validate do not accept a model name")
    try:
        config = ModelConfig(args.config)
        if args.action == "validate":
            print(f"Configuration valid. Effective global allowances: {config.global_limits()}")
        elif args.action == "list":
            for model in config.models:
                print(f"{model['id']}\t{model['backend']['base_url']}\t{'managed' if model.get('launch') else 'external'}")
        elif args.action == "check":
            address = local_url(args.base_url)
            registered = next((m for m in config.models if local_url(m["backend"]["base_url"]) == address), None)
            state, detail = inspect(address, model=registered, config=config)
            print(f"{address}: {state} — {detail}")
            if registered is None:
                print("Warning: this address is unregistered. Direct access is unaffected; gateway access requires an explicit configuration edit and reload. Nothing was registered.", file=sys.stderr)
            return 0 if state == "ready" else 1
        elif args.action == "status":
            models = [config.find(args.model)] if args.model else config.models
            failed = False
            for model in models:
                state, detail = inspect(model["backend"]["base_url"], model=model, config=config)
                print(f"{model['id']}: {state} — {detail}")
                failed |= state != "ready"
            return int(failed)
        elif args.action == "start":
            print(start(config, config.find(args.model), args.timeout))
        elif args.action == "stop":
            print(stop(config, config.find(args.model), args.timeout))
        return 0
    except KeyboardInterrupt:
        print("Model operation interrupted.", file=sys.stderr); return 130
    except (OSError, ValueError, RuntimeError, requests.RequestException) as error:
        print(f"Model operation failed: {error}", file=sys.stderr); return 1
