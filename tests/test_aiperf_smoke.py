"""Opt-in integration test using real AIPerf and a fake local SSE server.

Run: RUN_AIPERF_SMOKE=1 python -m unittest discover -s tests -p test_aiperf_smoke.py -v
No model is started, no tokenizer is downloaded, and temporary artifacts are removed.
"""

from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import threading
import time
import unittest

from benchmarks.prepare import PROJECT_ROOT


@unittest.skipUnless(os.environ.get("RUN_AIPERF_SMOKE") == "1", "opt-in real AIPerf smoke test")
class AIPerfSmokeTests(unittest.TestCase):
    def test_cli_readiness_exact_prompt_caps_usage_and_exports(self):
        received = []
        health_checks = []

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *_args):
                pass

            def handle(self):
                try:
                    super().handle()
                except (ConnectionResetError, BrokenPipeError):
                    pass  # AIPerf may close an idle pooled connection.

            def reply(self, data, status=200, content_type="application/json"):
                body = data if isinstance(data, bytes) else json.dumps(data).encode()
                self.send_response(status)
                self.send_header("Content-Type", content_type)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                self.wfile.flush()

            def do_GET(self):
                if self.path == "/health":
                    health_checks.append(self.path)
                    self.reply({"status": "ok"})
                elif self.path == "/v1/models":
                    self.reply({"object": "list", "data": [{"id": "local-llm", "object": "model"}]})
                else:
                    self.reply({"error": "not found"}, 404)

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                if self.path != "/v1/chat/completions":
                    self.reply({"error": "not found"}, 404)
                    return
                received.append(body)
                events = [
                    {"choices": [{"index": 0, "delta": {"role": "assistant"}, "finish_reason": None}]},
                    {"choices": [{"index": 0, "delta": {"content": "Engineering"}, "finish_reason": None}]},
                    {"choices": [{"index": 0, "delta": {"content": " test"}, "finish_reason": None}]},
                    {"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]},
                    {"choices": [], "usage": {"prompt_tokens": 17, "completion_tokens": 2,
                                             "total_tokens": 19}},
                ]
                scenario = getattr(server, "scenario", "success")
                cached = 16 if body.get("cache_prompt") or scenario == "cache_ignored" else 0
                if scenario != "missing_backend":
                    events[-1]["timings"] = {
                        "cache_n": cached, "prompt_n": 17 - cached, "prompt_ms": 40,
                        "predicted_n": 2, "predicted_ms": 10,
                    }
                for event in events:
                    event.update(id="chatcmpl-smoke", object="chat.completion.chunk",
                                 created=1700000000, model="local-llm")
                chunks = [("data: " + json.dumps(event) + "\n\n").encode() for event in events]
                done = b"" if getattr(server, "scenario", "success") == "missing_done" else b"data: [DONE]\n\n"
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Content-Length", str(sum(map(len, chunks)) + len(done)))
                self.end_headers()
                for index, chunk in enumerate(chunks):
                    self.wfile.write(chunk)
                    self.wfile.flush()
                    if index == 0:
                        time.sleep(0.05)  # Role metadata arrives before generated text.
                if done:
                    time.sleep(0.3)  # Completion deliberately follows the last text chunk.
                    self.wfile.write(done)
                    self.wfile.flush()

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        def execute(command):
            with subprocess.Popen(command, cwd=PROJECT_ROOT, stdout=subprocess.PIPE,
                                  stderr=subprocess.STDOUT, text=True,
                                  start_new_session=True) as process:
                try:
                    logs, _ = process.communicate(timeout=120)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGTERM)
                    try:
                        logs, _ = process.communicate(timeout=10)
                    except subprocess.TimeoutExpired:
                        os.killpg(process.pid, signal.SIGKILL)
                        logs, _ = process.communicate()
                    self.fail(f"AIPerf smoke test timed out:\n{logs[-12000:]}")
                return process.returncode, logs
        try:
            with tempfile.TemporaryDirectory(prefix="aiperf-smoke-") as directory:
                root = Path(directory)
                prompt = "  Exact engineering prompt.\nPreserve whitespace. Ω  "
                source = root / "workload.json"
                source.write_text(json.dumps({
                    "prompts": [{"id": "smoke", "prompt": prompt}],
                    "test_config": {"repetitions": 2, "concurrency": 1},
                    "test_cases": [{"id": "smoke_4", "prompt_id": "smoke", "max_tokens": 4}],
                }), encoding="utf-8")
                output = root / "run"
                command = [
                    sys.executable, "-m", "benchmarks.run",
                    "--input", str(source), "--output", str(output),
                    "--base-url", f"http://127.0.0.1:{server.server_port}",
                    "--request-timeout", "10", "--tokenizer", "builtin", "--pdf",
                ]
                returncode, logs = execute(command)
                exported = output / "smoke_4/profile_export.jsonl"
                diagnostics = exported.read_text()[-12000:] if exported.is_file() else "No records"
                self.assertEqual(returncode, 0, logs[-8000:] + "\n" + diagnostics)
                self.assertTrue(health_checks, "Runner must check readiness through the actual CLI")
                self.assertEqual(len(received), 3, logs[-16000:])  # one warm-up + two measured
                for body in received:
                    self.assertEqual(body["messages"], [{"role": "user", "content": prompt}])
                    self.assertEqual(body["max_tokens"], 4)
                    self.assertEqual(body["temperature"], 0)
                    self.assertIs(body["cache_prompt"], False)
                    self.assertTrue(body["stream"])
                    self.assertTrue(body["stream_options"]["include_usage"])
                records = [json.loads(line) for line in
                           (output / "smoke_4/profile_export.jsonl").read_text().splitlines()]
                measured = [row for row in records
                            if row["metadata"]["benchmark_phase"] == "profiling"]
                warmup = [row for row in records
                          if row["metadata"]["benchmark_phase"] == "warmup"]
                self.assertEqual(len(measured), 2, logs[-16000:])
                self.assertEqual(len(warmup), 1)
                self.assertEqual({row["metadata"]["conversation_id"] for row in measured},
                                 {"smoke_4-r1", "smoke_4-r2"})
                for row in measured:
                    self.assertFalse(row.get("error"))
                    self.assertEqual(row["metrics"]["input_sequence_length"]["value"], 17)
                    self.assertEqual(row["metrics"]["output_sequence_length"]["value"], 2)
                self.assertTrue((output / "smoke_4/profile_export_aiperf.csv").is_file())
                self.assertTrue((output / "smoke_4/profile_export_aiperf.json").is_file())
                self.assertEqual(json.loads((output / "run.json").read_text())["status"], "completed")
                self.assertTrue((output / "report.pdf").read_bytes().startswith(b"%PDF-"))
                self.assertEqual(json.loads((output / "run.json").read_text())["pdf_export"]["status"],
                                 "completed")
                from benchmarks.report import load_report
                report = load_report(output)
                self.assertEqual(report["cases"][0]["outcomes"]["measured"], 2)
                metrics = {row[0]: row for row in report["cases"][0]["rows"]}
                self.assertEqual(metrics["Input tokens"][2], 17)
                self.assertEqual(metrics["Output tokens"][2], 2)
                compact = [json.loads(line) for line in
                           (output / "smoke_4/request_metrics.jsonl").read_text().splitlines()]
                measured_compact = [row for row in compact if row["benchmark_phase"] == "profiling"]
                self.assertEqual(len(measured_compact), 2)
                for row in measured_compact:
                    self.assertEqual(row["status"], "success")
                    self.assertTrue(row["completion_received"])
                    self.assertGreaterEqual(row["e2e_latency_ms"] - row["aiperf_request_latency_ms"], 150)
                    self.assertEqual(row["input_tokens"], 17)
                    self.assertEqual(row["output_tokens"], 2)
                    self.assertEqual(row["cached_input_tokens"], 0)
                    self.assertEqual(row["backend_prompt_tokens"], 17)
                    self.assertEqual(row["backend_prompt_ms"], 40)
                    self.assertEqual(row["backend_decode_ms"], 10)
                    self.assertIsNone(row["peak_ram_mib"])
                    self.assertEqual(row["ram_unavailable_reason"], "project_server_not_found")
                    self.assertIsNone(row["peak_vram_mib"])
                    self.assertEqual(row["vram_unavailable_reason"], "project_server_not_found")
                self.assertTrue((output / "smoke_4/request_metrics.csv").is_file())
                memory = json.loads((output / "smoke_4/memory_summary.json").read_text())
                self.assertEqual(memory["scope"], "server_process_case")
                self.assertGreater(memory["ram"]["unavailable_sample_count"], 0)
                self.assertGreater(metrics["Completion E2E latency"][2], metrics["AIPerf last-content latency"][2])

                for scenario in ("missing_done", "timeout", "cache_ignored", "missing_backend", "reuse"):
                    with self.subTest(scenario=scenario):
                        server.scenario = scenario
                        failed_output = root / scenario
                        failed_command = list(command)
                        failed_command[failed_command.index("--output") + 1] = str(failed_output)
                        if scenario == "timeout":
                            failed_command[failed_command.index("--request-timeout") + 1] = "0.15"
                        failed_command += ["--repetitions", "1", "--warmup-requests", "0"]
                        if scenario == "reuse":
                            failed_command += ["--prompt-cache", "reuse"]
                        returncode, failure_logs = execute(failed_command)
                        if scenario == "reuse":
                            self.assertEqual(returncode, 0, failure_logs[-12000:])
                            state = json.loads((failed_output / "run.json").read_text())
                            self.assertEqual(state["cases"][0]["cache_verification"]["cached_requests"], 1)
                            self.assertIs(received[-1]["cache_prompt"], True)
                            continue
                        self.assertEqual(returncode, 1, failure_logs[-12000:])
                        state = json.loads((failed_output / "run.json").read_text())
                        self.assertEqual(state["status"], "failed")
                        compact_path = failed_output / "smoke_4/request_metrics.jsonl"
                        self.assertTrue(compact_path.is_file(), failure_logs[-12000:])
                        failed = [json.loads(line) for line in compact_path.read_text().splitlines()]
                        self.assertEqual(len(failed), 1)
                        if scenario in {"cache_ignored", "missing_backend"}:
                            self.assertEqual(failed[0]["status"], "success")
                            self.assertIn("Uncached prompt policy", state["error"])
                            key = "cached_requests" if scenario == "cache_ignored" else "unverified_requests"
                            self.assertEqual(state["cases"][0]["cache_verification"][key], 1)
                            continue
                        self.assertNotEqual(failed[0]["status"], "success")
                        self.assertIsNone(failed[0]["e2e_latency_ms"])
                        self.assertFalse(failed[0]["completion_received"])
                        if scenario == "missing_done":
                            self.assertEqual(failed[0]["status"], "incomplete_stream")
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)


if __name__ == "__main__":
    unittest.main()
