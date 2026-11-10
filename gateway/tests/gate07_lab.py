"""Reusable gateway test scenarios and fixtures. Entry point starts postman_lab."""
import contextlib
import fcntl
import json
import os
from pathlib import Path
import secrets
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler

import requests
from test_checks import Checks

ROOT = Path(__file__).resolve().parents[2]
MODES = {"normal", "missing_usage", "invalid_usage", "hold", "stubborn", "stall"}
USERS = ("actual", "stream", "fallback", "busy", "cancel", "timeout")
USAGE = {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}


@contextlib.contextmanager
def lab_directory(verify=False, runtime=None):
    runtime = Path(runtime) if runtime is not None else ROOT / ".runtime"
    if verify:
        # Disposable checks must not look like another saved Postman lab.
        with tempfile.TemporaryDirectory(prefix="gate07-verify-") as directory:
            yield Path(directory)
        return
    runtime.mkdir(parents=True, exist_ok=True)
    work = runtime / "gate07-lab"
    work.mkdir(mode=0o700, exist_ok=True)
    if work.is_symlink() or not work.is_dir():
        raise RuntimeError("The lab state path must be a real directory")
    work.chmod(0o700)
    with (work / "lab.lock").open("a") as lock:
        os.chmod(lock.name, 0o600)
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise RuntimeError("The persistent Postman lab is already running") from None
        yield work


def saved_control_key(work):
    path = work / "control-key"
    if path.exists():
        value = path.read_text().strip()
        if len(value) < 32:
            raise RuntimeError("Saved lab control key is invalid")
        path.chmod(0o600)
        return value
    value = secrets.token_urlsafe(32)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as target:
        target.write(value)
    return value


def lab_keys(binary, keys_dir, base):
    def reusable():
        credentials_path = keys_dir / "credentials.json"
        records_path = keys_dir / "api-keys.json"
        credentials = json.loads(credentials_path.read_text()) if credentials_path.exists() else {"keys": {}}
        records = json.loads(records_path.read_text()) if records_path.exists() else {"keys": []}
        active = {r["id"]: r["user_id"] for r in records["keys"] if not r["revoked"]}
        return {entry["user_id"]: entry["api_key"] for key_id, entry in credentials["keys"].items()
                if active.get(key_id) == entry["user_id"] and entry["user_id"] in USERS}

    keys = reusable()
    for user in USERS:
        if user not in keys:
            subprocess.run([str(binary), "keys", "create", "--user", user, "--config-dir", str(keys_dir), "--base-url", base], check=True, stdout=subprocess.DEVNULL)
    return {user + "_key": token for user, token in reusable().items()}




class State:
    def __init__(self):
        self.lock = threading.Lock()
        self.mode, self.busy, self.calls = "normal", 0, 0
        self.release = threading.Event()
        self.release.set()
        self.control_key = secrets.token_urlsafe(32)

    def snapshot(self):
        with self.lock:
            return {"mode": self.mode, "busy": self.busy, "generation_calls": self.calls}


def handler_for(state):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def send_json(self, body, status=200):
            raw = json.dumps(body).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            try:
                self.wfile.write(raw)
            except (BrokenPipeError, ConnectionResetError):
                # A bounded gateway probe may time out while this fixture is
                # writing; that disconnect is expected during cancellation tests.
                pass

        def controls_allowed(self):
            value = self.headers.get("Authorization", "")
            return secrets.compare_digest(value, "Bearer " + state.control_key)

        def do_GET(self):
            if self.path == "/__test/state":
                self.send_json(state.snapshot() if self.controls_allowed() else {}, 200 if self.controls_allowed() else 401)
            elif self.path == "/health":
                self.send_json({"status": "ok"})
            elif self.path == "/v1/models":
                self.send_json({"data": [{"id": "local-llm"}]})
            elif self.path == "/slots":
                self.send_json([{"id": 0, "n_ctx": 4096, "is_processing": state.snapshot()["busy"] > 0}])
            else:
                self.send_json({}, 404)

        def do_POST(self):
            size = int(self.headers.get("Content-Length", "0"))
            if size > 1 << 20:
                self.send_json({}, 413)
                return
            try:
                body = json.loads(self.rfile.read(size))
            except (ValueError, TypeError):
                self.send_json({}, 400)
                return
            if self.path == "/__test/control":
                if not self.controls_allowed():
                    self.send_json({}, 401)
                    return
                mode = body.get("mode")
                if mode not in MODES:
                    self.send_json({"error": "invalid mode"}, 400)
                    return
                with state.lock:
                    if state.busy and mode != "normal":
                        self.send_json({"error": "release the current request first"}, 409)
                        return
                    state.mode = mode
                    if mode == "normal":
                        state.release.set()
                    else:
                        state.release.clear()
                self.send_json(state.snapshot())
            elif self.path == "/apply-template":
                self.send_json({"prompt": "fixture prompt"})
            elif self.path == "/tokenize":
                self.send_json({"tokens": [1, 2, 3]})
            elif self.path == "/v1/chat/completions":
                self.generate(body)
            else:
                self.send_json({}, 404)

        def generate(self, body):
            with state.lock:
                state.busy += 1
                state.calls += 1
                mode = state.mode
            usage = dict(USAGE)
            if mode == "invalid_usage":
                usage["total_tokens"] = 999
            common = {"id": "chat-lab-1", "created": 1, "model": "local-llm"}

            def event(choice, include_usage=False):
                value = dict(common, object="chat.completion.chunk", choices=choice)
                if include_usage:
                    value["usage"] = usage
                self.wfile.write(("data: " + json.dumps(value) + "\n\n").encode())
                self.wfile.flush()

            try:
                if body.get("stream"):
                    self.send_response(200)
                    self.send_header("Content-Type", "text/event-stream")
                    self.end_headers()
                    event([{"index": 0, "delta": {"content": "Hello world"}, "finish_reason": None}])
                    while mode in {"hold", "stubborn", "stall"} and not state.release.wait(0.1):
                        if mode != "stall":
                            self.wfile.write(b": heartbeat\n\n")
                            self.wfile.flush()
                    event([{"index": 0, "delta": {}, "finish_reason": "stop"}])
                    if mode != "missing_usage" and body.get("stream_options", {}).get("include_usage"):
                        event([], True)
                    self.wfile.write(b"data: [DONE]\n\n")
                    self.wfile.flush()
                else:
                    value = dict(common, object="chat.completion", choices=[{"index": 0, "message": {"role": "assistant", "content": "Hello world"}, "finish_reason": "stop"}])
                    if mode != "missing_usage":
                        value["usage"] = usage
                    self.send_json(value)
            except (BrokenPipeError, ConnectionResetError):
                pass
            finally:
                # Deliberately simulate compute that outlives the HTTP request.
                if mode in {"stubborn", "stall"}:
                    state.release.wait()
                with state.lock:
                    state.busy -= 1

    return Handler


def run_checks(base, backend, values, logs, on_check_complete=lambda name: None):
    checks = Checks()

    def control(mode):
        response = checks.request("set backend mode: " + mode, backend + "/__test/control",
                                  headers={"Authorization": "Bearer " + values["control_key"]},
                                  json={"mode": mode}, timeout=5)
        checks.status("set backend mode: " + mode, response, 200)

    def chat(user, cap=128, stream=False, model="local-llm"):
        return checks.request("send chat request for " + user, base + "/v1/chat/completions",
                              headers={"Authorization": "Bearer " + values[user + "_key"]},
                              json={"model": model, "messages": [{"role": "user", "content": "hello"}],
                                    "max_tokens": cap, "stream": stream,
                                    **({"stream_options": {"include_usage": True}} if stream else {})},
                              stream=stream, timeout=8)

    def rejection(response, code):
        checks.status("reject request while capacity is occupied", response, 503)
        body = checks.json("read capacity rejection", response)
        checks.equal("verify capacity error code", body["error"]["code"], code, response)

    try:
        checks.name = "actual_usage_refunds_and_retained_charges"
        for _ in range(2):
            r = chat("actual")
            checks.status("complete request with available token budget", r, 200)
            checks.settlement(r, logs, charged_tokens=5, usage_source="backend")
        checks.status("reject request after token budget is exhausted", chat("actual"), 429)
        on_check_complete(checks.name)

        checks.name = "streaming_usage_and_completion"
        r = chat("stream", stream=True)
        checks.status("start streaming request", r, 200)
        text = checks.text("read completed stream", r)
        checks.equal("require stream completion marker", "data: [DONE]" in text, True, r)
        checks.settlement(r, logs, charged_tokens=5)
        checks.status("reuse refunded token budget", chat("stream"), 200)
        checks.status("reject request after token budget is exhausted", chat("stream"), 429)
        on_check_complete(checks.name)

        checks.name = "missing_usage_conservative_charge"
        control("missing_usage")
        r = chat("fallback")
        checks.status("complete response without usage", r, 200)
        checks.settlement(r, logs, charged_tokens=131, usage_source="fallback")
        checks.status("reject request after conservative charge", chat("fallback"), 429)
        on_check_complete(checks.name)

        for mode, user in (("hold", "busy"), ("stubborn", "cancel")):
            checks.name = "runtime_and_global_capacity" if mode == "hold" else "cancellation_quarantine_and_recovery"
            control(mode)
            held = chat(user, cap=32, stream=True)
            checks.status("start held streaming request", held, 200)
            checks.first_event(held)
            rejected = chat(user, cap=32)
            rejection(rejected, "capacity_exceeded")
            checks.settlement(rejected, logs, charged_tokens=0)
            other = chat(user, cap=32, model="second")
            rejection(other, "capacity_exceeded")
            checks.settlement(other, logs, charged_tokens=0)
            held.close()
            record = checks.settlement(held, logs, charged_tokens=35, usage_source="fallback")
            if mode == "stubborn":
                checks.equal("keep cancelled runtime quarantined", record.get("capacity_state"), "quarantined_pending_idle", held)
                rejection(chat(user, cap=32), "capacity_unconfirmed")
            control("normal")
            time.sleep(0.15)
            checks.status("accept request after backend becomes idle", chat(user, cap=32), 200)
            on_check_complete(checks.name)

        checks.name = "timeout_without_false_completion_and_recovery"
        control("stall")
        r = chat("timeout", cap=32, stream=True)
        checks.status("start request that will time out", r, 200)
        text = checks.text("read timed-out stream", r)
        checks.equal("report backend idle timeout", "backend_idle_timeout" in text, True, r)
        checks.equal("omit success marker after timeout", "[DONE]" in text, False, r)
        checks.settlement(r, logs, capacity_state="quarantined_pending_idle")
        control("normal")
        time.sleep(0.15)
        checks.status("accept request after timeout recovery", chat("timeout", cap=32), 200)
        on_check_complete(checks.name)
        print("PASS: actual/stream usage, fallback, global/runtime busy refunds, cancellation, quarantine, timeout, recovery")
    finally:
        checks.close()


def main():
    from postman_lab import main as run_server
    run_server()


if __name__ == "__main__":
    main()
