"""Opt-in full command smoke test: Go worker + actual gateway + fake llama.cpp."""

import json
import os
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from benchmarks.evaluation.report import seal

ROOT = Path(__file__).resolve().parents[1]


@unittest.skipUnless(os.environ.get("RUN_EVALUATION_SMOKE") == "1", "set RUN_EVALUATION_SMOKE=1; requires Go")
class EvaluationSmoke(unittest.TestCase):
    def test_complete_command_and_offline_audit(self):
        slot_lock = threading.Lock()
        busy = threading.Event()

        class RuntimeHandler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *args):
                pass

            def handle(self):
                try:
                    super().handle()
                except ConnectionResetError:
                    pass  # Go closes pooled test connections when the worker exits.

            def respond(self, value):
                body = json.dumps(value).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_GET(self):
                values = {"/health": {"status": "ok"}, "/props": {"model_path": "synthetic-model"},
                          "/slots": [{"id": 0, "n_ctx": 4096, "is_processing": busy.is_set()}],
                          "/v1/models": {"data": [{"id": "local-llm"}]}}
                self.respond(values[self.path])

            def do_POST(self):
                self.rfile.read(int(self.headers.get("Content-Length", 0)))
                if self.path != "/v1/chat/completions":
                    values = {"/apply-template": {"prompt": "hello"}, "/tokenize": {"tokens": [1, 2, 3]},
                              "/slots/0?action=erase": {"id_slot": 0, "n_erased": 0}}
                    self.respond(values[self.path])
                    return
                with slot_lock:
                    busy.set()
                    chunk = {"id": "chat-test", "object": "chat.completion.chunk", "created": 1, "model": "local-llm",
                             "choices": [{"index": 0, "delta": {"content": "hello"}, "finish_reason": None}]}
                    first = "data: " + json.dumps(chunk) + "\n\n"
                    chunk["choices"] = [{"index": 0, "delta": {}, "finish_reason": "stop"}]
                    chunk["usage"] = {"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4}
                    last = "data: " + json.dumps(chunk) + "\n\ndata: [DONE]\n\n"
                    self.send_response(200)
                    self.send_header("Content-Type", "text/event-stream")
                    self.send_header("Content-Length", str(len((first + last).encode())))
                    self.end_headers()
                    self.wfile.write(first.encode()); self.wfile.flush()
                    time.sleep(0.1)
                    busy.clear()
                    self.wfile.write(last.encode()); self.wfile.flush()

        server = ThreadingHTTPServer(("127.0.0.1", 0), RuntimeHandler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary)
                config = json.loads((ROOT / "gateway.config.json").read_text())
                config["models"][0]["backend"]["base_url"] = f"http://127.0.0.1:{server.server_port}"
                (directory / "config.json").write_text(json.dumps(config))
                workload = {"version": 1, "traces": [{"name": "contended", "jobs": [
                    {"id": "bg1", "at_seconds": 0, "class": "background", "case": "engineering_short_64"},
                    {"id": "bg2", "at_seconds": .02, "class": "background", "case": "engineering_short_64"},
                    {"id": "int1", "at_seconds": .04, "class": "interactive", "case": "engineering_short_64"}]}]}
                (directory / "workload.json").write_text(json.dumps(workload))
                output = directory / "run"
                command = [sys.executable, "-m", "benchmarks.evaluate", "--config", str(directory / "config.json"),
                           "--workload", str(directory / "workload.json"), "--repetitions", "1", "--output", str(output)]
                result = subprocess.run(command, cwd=ROOT, capture_output=True, text=True, timeout=180)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                summary = json.loads((output / "summary.json").read_text())
                self.assertTrue(summary["complete"])
                self.assertTrue(summary["classification_valid"])
                self.assertEqual(summary["finished_trials"], 3)
                for row in summary["summaries"]:
                    self.assertEqual(row["completion_percent"]["value"], 100)
                for name in ("report.html", "summary.csv", "requests.csv", "source.zip", "worker", "manifest.json"):
                    self.assertTrue((output / name).is_file(), name)
                audit = subprocess.run([sys.executable, "-m", "benchmarks.evaluate", "--report-only", str(output)],
                                       cwd=ROOT, capture_output=True, text=True, timeout=30)
                self.assertEqual(audit.returncode, 0, audit.stdout + audit.stderr)
                # Simulate the original bug while retaining internally consistent
                # file hashes: report auditing must reject the class mismatch.
                event_path = output / "events.jsonl"
                events = [json.loads(line) for line in event_path.read_text().splitlines()]
                for event in events:
                    if event["event"] == "gateway":
                        event["record"]["request_class"] = "interactive"
                event_path.write_text("".join(json.dumps(event) + "\n" for event in events))
                seal(output)
                invalid = subprocess.run([sys.executable, "-m", "benchmarks.evaluate", "--report-only", str(output)],
                                         cwd=ROOT, capture_output=True, text=True, timeout=30)
                self.assertEqual(invalid.returncode, 1, invalid.stdout + invalid.stderr)
                invalid_summary = json.loads((output / "summary.json").read_text())
                self.assertFalse(invalid_summary["classification_valid"])
                self.assertTrue(invalid_summary["classification_issues"])
                self.assertIn("INVALID POLICY COMPARISON", (output / "report.html").read_text())
                # The same command preserves a report even when runtime preflight fails.
                config["models"][0]["context_limit"] = 8192
                (directory / "config.json").write_text(json.dumps(config))
                command[-1] = str(directory / "failed")
                failed = subprocess.run(command, cwd=ROOT, capture_output=True, text=True, timeout=180)
                self.assertNotEqual(failed.returncode, 0)
                failed_summary = json.loads((directory / "failed/summary.json").read_text())
                self.assertFalse(failed_summary["complete"])
                self.assertTrue(failed_summary["warnings"])
        finally:
            server.shutdown(); server.server_close(); thread.join()


if __name__ == "__main__":
    unittest.main()
