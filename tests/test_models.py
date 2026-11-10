"""Registry and lifecycle tests; no model weights are loaded."""
import contextlib
import copy
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch

from client import models, server
from client.model_config import ModelConfig, validate

def fixture():
    return {"version": 1, "gateway": {"listen": "127.0.0.1:8000"},
            "quotas": {"request_window_seconds": 60, "token_window_seconds": 3600,
                       "user": {"requests": 20, "tokens": 40000},
                       "global_ceiling": {"requests": 60, "tokens": 200000}},
            "models": [{"id": "local-llm", "aliases": ["default"],
                        "context_limit": 4096, "default_output_tokens": 128, "max_output_tokens": 1024,
                        "limits": {"requests": 30, "tokens": 100000},
                        "backend": {"runtime_id": "local-llm-runtime", "engine": "llama.cpp",
                                    "base_url": "http://127.0.0.1:8080", "expected_model": "local-llm"},
                        "launch": {"executable": "runtime/llama-server", "model_file": "models/test.gguf",
                                   "device": "none", "parallel": 1, "threads": 4}}]}

class ModelsTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.data = fixture()
        self.path = self.root / "gateway.config.json"
        self.write()

    def write(self):
        self.path.write_text(json.dumps(self.data))
        return ModelConfig(self.path)

    def test_alias_and_global_scaling(self):
        config = self.write()
        self.assertEqual(config.find("default")["id"], "local-llm")
        self.assertEqual(config.global_limits(), {"requests": 30, "tokens": 100000})
        second = copy.deepcopy(self.data["models"][0])
        second.update(id="second", aliases=[])
        second["backend"].update(runtime_id="second", base_url="http://127.0.0.1:8082")
        self.data["models"].append(second)
        self.assertEqual(self.write().global_limits(), {"requests": 60, "tokens": 200000})

    def test_execution_capacity_settings(self):
        self.data["gateway"]["max_active_requests"] = 2
        self.data["models"][0]["max_active_requests"] = 1
        self.assertEqual(self.write().data["gateway"]["max_active_requests"], 2)
        for value in (0, 2, True, None):
            with self.subTest(value=value):
                self.data["models"][0]["max_active_requests"] = value
                with self.assertRaises(ValueError):
                    self.write()

    def test_invalid_registrations(self):
        for change in (
            lambda d: d.update(version=True),
            lambda d: d["models"][0].update(aliases="alias"),
            lambda d: d["models"][0]["backend"].update(base_url="http://example.com:8080"),
            lambda d: d["models"][0]["backend"].update(base_url="http://127.0.0.1:8000"),
            lambda d: d["models"][0]["launch"].update(parallel=2),
            lambda d: d["models"][0].update(max_output_tokens=4096),
            lambda d: d["models"].append(copy.deepcopy(d["models"][0])),
            lambda d: d["quotas"]["user"].update(requests=0),
        ):
            data = copy.deepcopy(self.data); change(data)
            with self.assertRaises(ValueError):
                validate(data)
        self.path.write_text('{"version":1,"version":1}')
        with self.assertRaises(ValueError):
            ModelConfig(self.path)

    def test_removed_model_permissions_are_rejected(self):
        for users in (None, [], ["alice"]):
            with self.subTest(users=users):
                self.data["models"][0]["allowed_users"] = users
                with self.assertRaisesRegex(ValueError, "unsupported"):
                    self.write()

    def test_warning_never_edits_config_or_probes_remote(self):
        config = self.write(); before = self.path.read_bytes()
        self.assertIn("not registered", models.registration_warning(config, "http://127.0.0.1:8082", "unknown"))
        self.assertEqual(models.registration_warning(config, "http://localhost:8080", "default"), "")
        self.assertEqual(models.registration_warning(config, "http://127.0.0.1:8000", "local-llm"), "")
        self.assertEqual(models.registration_warning(config, "https://remote.example", "unknown"), "")
        with patch.object(models, "inspect", return_value=("ready", "unknown")) as probe, contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()) as errors:
            self.assertEqual(models.main(["check", "--config", str(self.path), "--base-url", "http://127.0.0.1:8082"]), 0)
            self.assertIn("unregistered", errors.getvalue())
            probe.assert_called_once()
        self.assertEqual(self.path.read_bytes(), before)

    def test_explicit_start_passes_model_file_and_verification(self):
        config = self.write(); model = config.find("local-llm")
        with patch.object(server, "ensure_server_ready", return_value=None) as launch:
            self.assertIn("reused", models.start(config, model, 10))
        arguments = launch.call_args.kwargs
        self.assertEqual(arguments["model_file"], self.root / model["launch"]["model_file"])
        self.assertEqual(arguments["model"], model["backend"]["expected_model"])
        self.assertEqual(arguments["server_options"]["ctx_size"], 4096)
        with patch.object(models, "inspect", return_value=("mismatch", "wrong identity")):
            with self.assertRaises(RuntimeError):
                arguments["verify_ready"]()

    def test_external_registration_is_not_started_or_stopped(self):
        self.data["models"][0].pop("launch")
        config = self.write(); model = config.find("local-llm")
        with patch.object(server, "ensure_server_ready") as launch:
            with self.assertRaises(ValueError): models.start(config, model, 1)
            with self.assertRaises(ValueError): models.stop(config, model, 1)
            launch.assert_not_called()

    def test_identity_retries_transient_empty_command_line(self):
        expected = models.process_identity(os.getpid())
        actual_argv = (Path("/proc") / str(os.getpid()) / "cmdline").read_bytes()
        with patch.object(Path, "read_bytes", side_effect=[b"", actual_argv]) as read, patch.object(models.time, "sleep"):
            self.assertEqual(models.process_identity(os.getpid()), expected)
            self.assertEqual(read.call_count, 2)

    def test_identity_never_accepts_permanently_empty_command_line(self):
        with patch.object(Path, "read_bytes", return_value=b""), patch.object(models.time, "monotonic", side_effect=[0, 2]):
            with self.assertRaisesRegex(RuntimeError, "could not be verified"):
                models.process_identity(os.getpid())

    def test_stop_only_recorded_process_and_refuse_identity_change(self):
        if not hasattr(os, "pidfd_open"):
            self.skipTest("Linux pidfd required")
        self.data["models"][0]["launch"]["executable"] = "/usr/bin/sleep"
        config = self.write(); model = config.find("local-llm")
        with patch.object(server, "PROJECT_ROOT", self.root):
            process = subprocess.Popen(["/usr/bin/sleep", "60"], cwd=self.root)
            try:
                record = models.process_identity(process.pid)
                record.update(model_file=str(config.resolve_path(model["launch"]["model_file"])), runtime_id=model["backend"]["runtime_id"], address="http://127.0.0.1:8080")
                path = models.record_path(model)
                bad = dict(record, start_ticks="wrong")
                models.save_record(path, bad)
                with self.assertRaisesRegex(RuntimeError, "identity changed"):
                    models.stop(config, model, 1)
                self.assertIsNone(process.poll())
                models.save_record(path, record)
                self.assertIn("Stopped", models.stop(config, model, 2))
                process.wait(timeout=2)
                self.assertFalse(path.exists())
            finally:
                if process.poll() is None: process.terminate()
                process.wait(timeout=2)

    def test_real_http_identity_context_and_no_redirect(self):
        state = {"identity": "local-llm", "context": 4096, "redirect": False}
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_args): pass
            def do_GET(self):
                if state["redirect"]:
                    self.send_response(302); self.send_header("Location", "/elsewhere"); self.end_headers(); return
                bodies = {"/health": {"status": "ok"}, "/v1/models": {"data": [{"id": state["identity"]}]}, "/slots": [{"n_ctx": state["context"]}]}
                body = json.dumps(bodies.get(self.path, {})).encode()
                self.send_response(200); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
        backend = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        worker = threading.Thread(target=backend.serve_forever, daemon=True); worker.start()
        try:
            model = copy.deepcopy(self.data["models"][0]); model.pop("launch")
            url = f"http://127.0.0.1:{backend.server_port}"
            self.assertEqual(models.inspect(url, model=model)[0], "ready")
            state["identity"] = "other"
            self.assertEqual(models.inspect(url, model=model)[0], "mismatch")
            state["identity"] = "local-llm"; state["context"] = 128
            self.assertEqual(models.inspect(url, model=model)[0], "mismatch")
            state["redirect"] = True
            self.assertEqual(models.inspect(url, model=model)[0], "unavailable")
        finally:
            backend.shutdown(); backend.server_close(); worker.join()

    def test_cli_preserves_prompt_syntax_and_dispatches_models(self):
        import cli
        with patch.object(models, "main", return_value=0) as subcommand:
            self.assertEqual(cli.main(["models", "list"]), 0)
            subcommand.assert_called_once_with(["list"])
        with patch.object(cli.LLMClient, "chat", return_value={"choices": [{"message": {"content": "ok"}}]}) as chat, contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(cli.main(["hello", "--config", str(self.path)]), 0)
            chat.assert_called_once()

if __name__ == "__main__":
    unittest.main()
