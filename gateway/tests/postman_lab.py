"""One stable Postman entry point; manages only its own test gateway/backends."""
import argparse
import json
import os
from pathlib import Path, PureWindowsPath
import secrets
import socket
import subprocess
import tempfile
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import requests
import gate07_lab as fixtures
from test_checks import CheckFailure
from manual_requests import handle_manual


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
            windows_path = subprocess.run(["wslpath", "-w", str(path.resolve())],
                                          check=True, capture_output=True, text=True,
                                          timeout=3).stdout.strip()
            if PureWindowsPath(windows_path).is_absolute():
                links[label + "_windows_path"] = windows_path
                links[label + "_uri"] = PureWindowsPath(windows_path).as_uri()
        except (OSError, subprocess.SubprocessError):
            pass
    return links


def export_collection(work, base, key):
    source = fixtures.ROOT / "gateway/tests/gate07.postman_collection.json"
    collection = json.loads(source.read_text())
    collection["info"]["_postman_id"] = str(uuid.uuid5(uuid.NAMESPACE_URL, "llm-gateway-scheduler/reusable-tests"))
    collection["variable"] = [{"key": "gateway_test_url", "value": base, "type": "string"},
                              {"key": "gateway_test_key", "value": key, "type": "string"}]
    path = work / "postman.collection.json"
    return path, save_json(path, collection)


def export_environment(work, base, key):
    environment = {
        "id": str(uuid.uuid5(uuid.NAMESPACE_URL, "llm-gateway-scheduler/reusable-test-environment")),
        "name": "Gateway tests — reusable",
        "values": [
            {"key": "gateway_test_url", "value": base, "type": "default", "enabled": True},
            {"key": "gateway_test_key", "value": key, "type": "secret", "enabled": True},
        ],
        "_postman_variable_scope": "environment",
    }
    path = work / "postman.environment.json"
    return path, save_json(path, environment)


class TestLab:
    def __init__(self, work, base):
        self.work, self.base = work, base
        self.key = fixtures.saved_control_key(work)
        self.collection, self.export_status = export_collection(work, base, self.key)
        self.collection_links = collection_links(self.collection)
        self.environment, self.environment_status = export_environment(work, base, self.key)
        self.environment_links = collection_links(self.environment, "postman_environment")
        self.logs = work / "gateway.log"
        self.report = work / "latest-report.json"
        self.process = None
        self.output = None
        self.states, self.servers = [], []
        self.run_lock = threading.Lock()
        self.manual_requests = 0
        self.view_lock = threading.Lock()
        self.state = "starting"
        self.last_report = json.loads(self.report.read_text()) if self.report.exists() else None
        self.values = None

    def start(self):
        for _ in range(2):
            state = fixtures.State()
            state.control_key = self.key
            server = ThreadingHTTPServer(("127.0.0.1", 0), fixtures.handler_for(state))
            server.daemon_threads = True
            self.states.append(state)
            self.servers.append(server)
            threading.Thread(target=server.serve_forever, daemon=True).start()
        self.reset()

    def stop_gateway(self):
        if self.process is not None and self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=8)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait()
        self.process = None
        if self.output is not None:
            self.output.close()
            self.output = None

    def reset(self):
        # Only this lab's child process is restarted. Its saved keys are retained.
        _, environment_status = export_environment(self.work, self.base, self.key)
        if environment_status != "unchanged":
            self.environment_status = environment_status
        with self.view_lock:
            self.state = "resetting"
        for state in self.states:
            state.release.set()
        self.stop_gateway()
        end = time.monotonic() + 3
        while any(s.snapshot()["busy"] for s in self.states):
            if time.monotonic() > end:
                raise RuntimeError("Test backend did not drain")
            time.sleep(0.02)
        for state in self.states:
            with state.lock:
                state.mode, state.calls = "normal", 0
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1", 0))
            port = reservation.getsockname()[1]
        self.gateway = f"http://127.0.0.1:{port}"
        backends = [f"http://127.0.0.1:{s.server_port}" for s in self.servers]
        models = [{"id": name, "aliases": [alias], "max_active_requests": 1,
                   "context_limit": 4096, "default_output_tokens": 128, "max_output_tokens": 1024,
                   "limits": {"requests": 1000, "tokens": 200000},
                   "backend": {"runtime_id": name + "-runtime", "engine": "llama.cpp", "base_url": url, "expected_model": "local-llm"}}
                  for name, alias, url in zip(("local-llm", "second"), ("default", "engineer"), backends)]
        config = {"version": 1, "gateway": {"listen": f"127.0.0.1:{port}", "max_active_requests": 1},
                  "quotas": {"request_window_seconds": 60, "token_window_seconds": 3600,
                             "user": {"requests": 1000, "tokens": 140},
                             "global_ceiling": {"requests": 1000, "tokens": 200000}}, "models": models}
        config_path = self.work / "gateway.config.json"
        save_json(config_path, config)
        binary = fixtures.ROOT / ".runtime/gateway"
        keys_dir = self.work / "keys"
        self.values = {"control_key": self.key, **fixtures.lab_keys(binary, keys_dir, self.gateway)}
        self.output = self.logs.open("w")
        self.logs.chmod(0o600)
        self.process = subprocess.Popen([str(binary), "--config", str(config_path), "--config-dir", str(keys_dir), "--backend-idle-timeout", "1s"], stdout=self.output, stderr=self.output)
        end = time.monotonic() + 10
        while time.monotonic() < end:
            if self.process.poll() is not None:
                raise RuntimeError("Test gateway exited; inspect gateway.log")
            try:
                response = requests.get(self.gateway + "/v1/models", headers={"Authorization": "Bearer " + self.values["actual_key"]}, timeout=0.3)
                if response.status_code == 200:
                    with self.view_lock:
                        self.state = "ready"
                    return
            except requests.RequestException:
                pass
            time.sleep(0.05)
        raise RuntimeError("Test gateway startup timed out")

    def files(self):
        return {"postman_collection": str(self.collection), "latest_report": str(self.report),
                **self.collection_links,
                "postman_environment": str(self.environment), **self.environment_links,
                "gateway_log": str(self.logs),
                "test_cases": str(fixtures.ROOT / "gateway/tests/gate07_lab.py"),
                "test_server": str(Path(__file__).resolve()),
                "guide": str(fixtures.ROOT / "gateway/README.lifecycle.md")}

    def status(self):
        with self.view_lock:
            return {"state": self.state, "base_url": self.base, "files": self.files(), "last_run": self.last_report}

    def failure_info(self, error, step):
        if isinstance(error, CheckFailure):
            failure = error.details
        else:
            failure = {"check": "test_setup" if step == "prepare test gateway" else "test_server",
                       "step": step, "reason": str(error) or type(error).__name__,
                       "expected": "Operation completes successfully",
                       "actual": {"error_type": type(error).__name__}}

        # Scrub credentials in nested diagnostics and response bodies, including
        # exceptions raised while setting up the test gateway.
        secrets_to_hide = [self.key, *(self.values or {}).values()]
        def redact(value):
            if isinstance(value, str):
                for secret in secrets_to_hide:
                    if isinstance(secret, str) and secret:
                        value = value.replace(secret, "[redacted]")
                return value[:4096]
            if isinstance(value, dict):
                return {redact(k): redact(v) for k, v in value.items()}
            if isinstance(value, list):
                return [redact(v) for v in value]
            return value
        failure = redact(failure)
        summary = f"{failure['check']} / {failure['step']}: {failure['reason']}"
        return {"error": summary, "failure": failure}

    def run(self):
        checks = []
        started = time.monotonic()
        run_id = str(uuid.uuid4())
        step = "prepare test gateway"
        try:
            self.reset()
            with self.view_lock:
                self.state = "running"
            def record(name):
                checks.append({"name": name, "passed": True})
                print("PASS " + name, flush=True)
            backend = f"http://127.0.0.1:{self.servers[0].server_port}"
            step = "execute test suite"
            fixtures.run_checks(self.gateway, backend, self.values, self.logs, record)
            result = {"status": "passed", "run_id": run_id, "checks": checks}
        except Exception as error:
            info = self.failure_info(error, step)
            checks.append({"name": info["failure"]["check"], "passed": False,
                           "failure": info["failure"]})
            result = {"status": "failed", "run_id": run_id, "checks": checks,
                      **info}
            print("FAIL " + info["error"], flush=True)
        finally:
            for state in self.states:
                state.release.set()
        result.update(duration_seconds=round(time.monotonic() - started, 3), files=self.files())
        save_json(self.report, result)
        with self.view_lock:
            self.last_report = result
            self.state = "ready" if result["status"] == "passed" else "failed"
        print("Test report: " + str(self.report), flush=True)
        return result

    def close(self):
        for state in self.states:
            state.release.set()
        self.stop_gateway()
        for server in self.servers:
            server.shutdown()
            server.server_close()


def handler_for(lab):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def reply(self, status, body):
            raw = json.dumps(body).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            try:
                self.wfile.write(raw)
            except (BrokenPipeError, ConnectionResetError):
                pass

        def authenticated(self):
            supplied = self.headers.get_all("Authorization", [])
            if len(supplied) != 1 or not secrets.compare_digest(supplied[0], "Bearer " + lab.key):
                self.reply(401, {"error": "Invalid test API key"})
                return False
            return True

        def do_GET(self):
            if not self.authenticated():
                return
            if handle_manual(self, lab):
                return
            if self.path == "/tests/status":
                self.reply(200, lab.status())
            else:
                self.reply(404, {"error": "Not found"})

        def do_POST(self):
            if not self.authenticated():
                return
            if handle_manual(self, lab):
                return
            if self.path not in ("/tests/run", "/tests/reset"):
                self.reply(404, {"error": "Not found"})
                return
            if not lab.run_lock.acquire(blocking=False):
                self.reply(409, {"error": "A test run or reset is already in progress"})
                return
            try:
                if lab.manual_requests:
                    self.reply(409, {"error": "Manual requests are active; finish/cancel them before running or resetting tests"})
                    return
                if self.path == "/tests/run":
                    result = lab.run()
                    self.reply(200 if result["status"] == "passed" else 500, result)
                else:
                    lab.reset()
                    self.reply(200, {"status": "reset", "files": lab.files()})
            except Exception as error:
                with lab.view_lock:
                    lab.state = "failed"
                step = "reset test state" if self.path == "/tests/reset" else "write test report"
                self.reply(500, {"status": "failed", **lab.failure_info(error, step), "files": lab.files()})
            finally:
                lab.run_lock.release()
    return Handler


def main():
    if not __debug__:
        raise RuntimeError("Run the test server without Python -O so test assertions stay enabled")
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--port", "--gateway-port", dest="port", type=int, default=8005, help="The only Postman-facing port")
    parser.add_argument("--verify", action="store_true", help="Run checks in temporary state without changing your saved collection")
    args = parser.parse_args()
    if not 1024 <= args.port <= 65535:
        parser.error("port must be between 1024 and 65535")
    base = f"http://127.0.0.1:{args.port}"
    with fixtures.lab_directory(args.verify) as work:
        lab = TestLab(work, base)
        server = None
        try:
            if not args.verify:
                server = ThreadingHTTPServer(("127.0.0.1", args.port), handler_for(lab))
                # Let an in-progress run finish before child processes are stopped.
                server.daemon_threads = False
            lab.start()
            if args.verify:
                result = lab.run()
                if result["status"] != "passed":
                    raise RuntimeError(result["error"])
                return
            print(f"Postman test server: {base}\nCollection {lab.export_status}: {lab.collection}", flush=True)
            print(f"Environment {lab.environment_status}: {lab.environment}", flush=True)
            print("Import the collection and environment once; select 'Gateway tests — reusable'. Reimport the environment only if deleted in Postman or settings changed.", flush=True)
            print("Postman environment link: " + lab.environment_links["postman_environment_uri"], flush=True)
            if "postman_environment_windows_path" in lab.environment_links:
                print("Windows environment import path: " + lab.environment_links["postman_environment_windows_path"], flush=True)
            print("Postman setup link (saved URL and key): " + lab.collection_links["postman_collection_uri"], flush=True)
            if "postman_collection_windows_path" in lab.collection_links:
                print("Windows import path: " + lab.collection_links["postman_collection_windows_path"], flush=True)
            for label, path in lab.files().items():
                print(f"{label}: {path}", flush=True)
            print("Send 'Run all tests' repeatedly. Each run resets test state and keeps the same URL/key. Ctrl+C stops this test server only.", flush=True)
            server.serve_forever()
        except KeyboardInterrupt:
            pass
        finally:
            if server is not None:
                server.server_close()
            lab.close()


if __name__ == "__main__":
    main()
