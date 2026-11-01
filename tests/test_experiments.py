"""Controlled sweeps: synthetic evidence and lifecycle mocks, never a live model."""

import contextlib
import copy
import io
import json
from pathlib import Path
import socket
import subprocess
import tempfile
import unittest
from unittest.mock import MagicMock, Mock, patch

import cli
from client import server
from benchmarks import experiment_results as results, experiments, runtime


def write(path, data):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(data), encoding="utf-8")


def stage_fixture(root, plan, concurrency=1, context=4096):
    flags = runtime.launch_options(server.launch_options({**plan["server"], "ctx_size": context}))
    controls = {"process": {"pid": 42, "start_ticks": 20, "argv_sha256": "argv", "launch_options": flags},
                "context_size": context // 4, "slots": 4, "model": {"sha256": "synthetic"}}
    point = {"status": "available", "controls": controls, "fingerprint": runtime.digest(controls)}
    write(root / "run/runtime.json", point)
    write(root / "run/run.json", {"status": "completed", "settings": {"temperature": 0, "prompt_cache": "off"}})
    write(root / "run/inputs/manifest.json", {"source_sha256": "synthetic", "concurrency": concurrency})
    directory = root / "run" / plan["case"]
    write(directory / "runtime_snapshot.json", {"before": point, "after": point, "unchanged": True})
    write(root / "input_budget.json", {"input_tokens": 17, "output_cap": 64})
    rows = []
    for index in range(plan["repetitions"]):
        start = 10_000_000_000 + (index // concurrency) * 1_000_000_000
        rows.append({"benchmark_phase": "profiling", "status": "success", "input_tokens": 17,
                     "output_tokens": 2, "cached_input_tokens": 0, "backend_prompt_tokens": 17,
                     "request_start_perf_ns": start, "completion_perf_ns": start + 1_000_000_000,
                     "ttft_ms": 200, "e2e_latency_ms": 1000})
    rows += [{"benchmark_phase": "warmup", "status": "success"} for _ in range(plan["warmup_requests"])]
    (directory / "request_metrics.jsonl").write_text("".join(json.dumps(row) + "\n" for row in rows))
    memory = {"ram": {"status": "available", "baseline_mib": 100, "peak_mib": 200},
              "vram": {"peak_mib": None, "unavailable_reason": "cpu_only_server"}}
    for path in (directory, root / "startup", root / "idle"):
        write(path / "memory_summary.json", memory)
    return rows


class ExperimentTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.plan = json.loads(experiments.DEFAULT_CONFIG.read_text())
        self.source = self.root / "prompts.json"
        write(self.source, {"prompts": [{"id": "short", "prompt": "Engineering test"}],
                            "test_cases": [{"id": self.plan["case"], "prompt_id": "short", "max_tokens": 64}]})

    def config(self, data=None):
        path = self.root / "config.json"
        write(path, self.plan if data is None else data)
        return path

    def test_plan_is_valid_and_default_has_both_sweeps(self):
        self.assertEqual(experiments.load_plan(self.config(), self.source), self.plan)
        self.assertEqual(self.plan["concurrency"], [1, 2, 4])
        self.assertEqual(len(self.plan["context"]), 2)

    def test_invalid_plans_are_rejected_before_startup(self):
        variants = []
        for key, value in (("repetitions", 3), ("concurrency", [1, 4, 2]),
                           ("context", [1024, 4096]), ("idle_seconds", 0), ("memory_interval", float("nan"))):
            item = copy.deepcopy(self.plan)
            item[key] = value
            variants.append(item)
        item = copy.deepcopy(self.plan)
        item["limits"]["max_ram_mib"] = -1
        variants.append(item)
        for item in variants:
            with self.subTest(item=item), self.assertRaises(ValueError):
                experiments.load_plan(self.config(item), self.source)

    def test_dry_run_is_read_only_and_never_starts_a_server(self):
        destination = self.root / "new"
        with patch.object(experiments, "OwnedServer") as owned, \
             patch.object(experiments, "require_free_port") as port, contextlib.redirect_stdout(io.StringIO()):
            code = experiments.main(["all", "--dry-run", "--input", str(self.source),
                                     "--config", str(self.config()), "--output", str(destination)])
        self.assertEqual(code, 0)
        self.assertFalse(destination.exists())
        owned.assert_not_called()
        port.assert_not_called()

    def test_existing_output_and_primary_port_are_refused(self):
        for extra in (["--output", str(self.root)], ["--port", "8080"]):
            with patch.object(experiments, "OwnedServer") as owned, contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(experiments.main(["all", "--input", str(self.source),
                                                   "--config", str(self.config()), *extra]), 1)
            owned.assert_not_called()

    def test_profile_uses_same_case_and_fixed_settings_without_compare_flag(self):
        command = experiments.command(self.plan, self.source, self.root, 8081, 4)
        for flag, value in (("--case", self.plan["case"]), ("--concurrency", "4"),
                            ("--prompt-cache", "off"), ("--temperature", "0")):
            self.assertEqual(command[command.index(flag) + 1], value)
        self.assertIn("--no-start-server", command)
        self.assertNotIn("--compare", command)

    def test_throughput_uses_measured_wall_time_and_verifies_overlap(self):
        stage_fixture(self.root, self.plan, concurrency=4)
        value = results.summarize_stage(self.root, self.plan, 4, 4096)
        self.assertTrue(value["passed"], value["gaps"])
        self.assertEqual(value["duration_seconds"], 2)
        self.assertEqual(value["output_tokens_per_second"], 8)
        self.assertEqual(value["max_observed_inflight"], 4)
        self.assertEqual(value["warmups"], 1)
        self.assertIsNone(value["peak_vram_mib"])
        self.assertEqual(value["vram_unavailable_reason"], "cpu_only_server")

    def test_missing_terminal_time_never_fabricates_throughput(self):
        rows = stage_fixture(self.root, self.plan)
        rows[0]["completion_perf_ns"] = None
        self.assertEqual(results.measurement_window(rows[:-1]), (None, None))

    def test_bad_cache_runtime_or_safety_threshold_prevents_acceptance(self):
        stage_fixture(self.root, self.plan)
        path = self.root / "run" / self.plan["case"] / "request_metrics.jsonl"
        rows = [json.loads(line) for line in path.read_text().splitlines()]
        rows[0].update(cached_input_tokens=16, backend_prompt_tokens=1, e2e_latency_ms=40_000)
        path.write_text("".join(json.dumps(row) + "\n" for row in rows))
        value = results.summarize_stage(self.root, self.plan, 1, 4096)
        self.assertFalse(value["passed"])
        self.assertTrue(any("Uncached" in gap for gap in value["gaps"]))
        self.assertTrue(any("threshold" in gap for gap in value["gaps"]))

    def test_expected_context_and_concurrency_are_verified_not_assumed(self):
        stage_fixture(self.root, self.plan)
        value = results.summarize_stage(self.root, self.plan, 4, 8192)
        self.assertFalse(value["passed"])
        self.assertTrue(any("--ctx-size" in gap for gap in value["gaps"]))
        self.assertTrue(any("target concurrency" in gap for gap in value["gaps"]))

    def test_context_normalization_only_ignores_intended_changes(self):
        stage_fixture(self.root, self.plan)
        value = results.summarize_stage(self.root, self.plan, 1, 4096)["runtime_controls"]
        other = copy.deepcopy(value)
        other["context_size"] = 2048
        other["process"].update(pid=99, start_ticks=100, argv_sha256="different")
        other["process"]["launch_options"]["--ctx-size"] = "8192"
        self.assertEqual(results.normalized_controls(value, context=True), results.normalized_controls(other, context=True))
        other["process"]["launch_options"]["--threads"] = "2"
        self.assertNotEqual(results.normalized_controls(value, context=True), results.normalized_controls(other, context=True))

    def test_sweep_exports_all_formats_and_selects_highest_tested_concurrency(self):
        stages = []
        for concurrency in (1, 2, 4):
            root = self.root / str(concurrency)
            stage_fixture(root, self.plan, concurrency)
            stages.append({"experiment": "concurrency", "path": str(concurrency),
                           "result": results.summarize_stage(root, self.plan, concurrency, 4096)})
        record = {"status": "completed", "plan": self.plan, "experiments": ["concurrency"], "stages": stages}
        data = results.export_results(self.root, record)
        self.assertEqual(data["gaps"], [])
        self.assertEqual(data["selected_concurrency"], 4)
        for suffix in ("json", "csv", "md"):
            self.assertTrue((self.root / ("summary." + suffix)).is_file())
        record["stages"].pop()
        self.assertTrue(results.export_results(self.root, record)["gaps"])

    def test_host_headroom_guard_fails_closed(self):
        with patch.object(experiments, "available_ram_mib", return_value=10), self.assertRaises(RuntimeError):
            experiments.require_headroom(self.plan)

    def test_cleanup_still_stops_owned_server_if_memory_export_fails(self):
        owned = experiments.OwnedServer(self.root, self.plan, 8081, 4096)
        owned.process = Mock(pid=42)
        owned.process.poll.return_value = None
        owned.ticks = 10
        owned.monitor = MagicMock()
        owned.monitor.save.side_effect = OSError("disk full")
        with patch.object(experiments.memory, "process_start", return_value=10), \
             patch.object(experiments, "_terminate") as stop, self.assertRaises(OSError):
            owned.close()
        stop.assert_called_once_with(42, 8081, 10)

    def test_no_cleanup_signal_is_sent_to_changed_process_identity(self):
        owned = experiments.OwnedServer(self.root, self.plan, 8081, 4096)
        owned.process = Mock(pid=42)
        owned.process.poll.return_value = None
        owned.ticks = 10
        with patch.object(experiments.memory, "process_start", return_value=11), \
             patch.object(experiments, "_terminate") as stop, self.assertRaises(RuntimeError):
            owned.close()
        stop.assert_not_called()

    def test_cli_forwards_explicit_startup_settings(self):
        with patch.object(cli, "ensure_server_ready") as ready, contextlib.redirect_stdout(io.StringIO()), \
             contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(cli.main(["--start-server", "--server-only", "--ctx-size", "8192", "--parallel", "4"]), 0)
        self.assertEqual(ready.call_args.kwargs["server_options"], {"ctx_size": 8192, "parallel": 4})

    def test_server_startup_options_are_bounded_and_boolean_flags_are_explicit(self):
        values = server.launch_options(self.plan["server"])
        self.assertIn("--no-kv-unified", values)
        self.assertIn("--cont-batching", values)
        for options in ({"ctx_size": 0}, {"threads": True}, {"ubatch_size": 1024, "batch_size": 512}, {"unknown": 1}):
            with self.assertRaises(ValueError):
                server.launch_options(options)

    def test_budget_includes_chat_template_and_rejects_overflow_or_busy_slots(self):
        def check(slots, count):
            responses = []
            for data in (slots, {"prompt": "TEMPLATE:Engineering test:ASSISTANT"}, {"tokens": list(range(count))}):
                response = Mock()
                response.json.return_value = data
                responses.append(response)
            session = MagicMock()
            session.request.side_effect = responses
            with patch.object(experiments.requests, "Session") as create:
                create.return_value.__enter__.return_value = session
                value = experiments.check_input_budget("http://127.0.0.1:8081", self.plan, self.source)
            return value, session
        value, session = check([{"is_processing": False, "n_ctx": 1024}], 17)
        self.assertEqual(value["input_tokens"], 17)
        body = session.request.call_args_list[-1].kwargs["json"]
        self.assertEqual(body["content"], "TEMPLATE:Engineering test:ASSISTANT")
        self.assertIs(body["parse_special"], True)
        with self.assertRaisesRegex(ValueError, "exceeds"):
            check([{"is_processing": False, "n_ctx": 64}], 17)
        with self.assertRaisesRegex(RuntimeError, "not idle"):
            check([{"is_processing": True, "n_ctx": 1024}], 17)

    def test_changed_snapshot_and_missing_memory_prevent_acceptance(self):
        stage_fixture(self.root, self.plan)
        path = self.root / "run" / self.plan["case"]
        snapshot = json.loads((path / "runtime_snapshot.json").read_text())
        snapshot["unchanged"] = False
        write(path / "runtime_snapshot.json", snapshot)
        data = json.loads((self.root / "idle/memory_summary.json").read_text())
        data["ram"]["status"] = "unavailable"
        write(self.root / "idle/memory_summary.json", data)
        result = results.summarize_stage(self.root, self.plan, 1, 4096)
        self.assertFalse(result["passed"])
        self.assertTrue(any("Runtime changed" in gap for gap in result["gaps"]))
        self.assertTrue(any("process-RSS" in gap for gap in result["gaps"]))

    def test_existing_service_is_never_adopted_or_stopped(self):
        owned = experiments.OwnedServer(self.root / "stage", self.plan, 8081, 4096)
        with patch.object(experiments, "require_free_port"), patch.object(experiments, "require_headroom"), \
             patch.object(experiments.server, "ensure_server_ready", return_value=None), \
             patch.object(experiments, "_terminate") as stop, self.assertRaisesRegex(RuntimeError, "not adopted"):
            owned.__enter__()
        stop.assert_not_called()

    def test_port_probe_uses_reuseaddr_but_refuses_a_live_listener(self):
        with socket.socket() as listener:
            listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            listener.bind(("127.0.0.1", 0))
            listener.listen()
            port = listener.getsockname()[1]
            with self.assertRaises(OSError):
                experiments.require_free_port(port)
        experiments.require_free_port(port)
        with patch.object(experiments.socket, "socket") as create:
            experiments.require_free_port(8081)
        create.return_value.__enter__.return_value.setsockopt.assert_called_once_with(
            socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)

    def test_full_orchestrator_freezes_only_a_successful_complete_sweep(self):
        destination = self.root / "suite"
        def create_owned(root, plan, port, context):
            root.mkdir()
            owned = MagicMock(context=context, address=f"http://127.0.0.1:{port}")
            owned.__enter__.return_value = owned
            return owned
        def profile(command, root, plan, owned):
            concurrency = int(command[command.index("--concurrency") + 1])
            stage_fixture(root, plan, concurrency, owned.context)
        with patch.object(experiments, "OwnedServer", side_effect=create_owned), \
             patch.object(experiments, "profile_stage", side_effect=profile), \
             patch.object(experiments, "check_input_budget", return_value={"input_tokens": 17}), \
             patch.object(experiments, "require_free_port"), patch.object(experiments, "require_headroom"), \
             patch.object(experiments.run, "installed_executable"), \
             patch.object(experiments, "experiment_lock", return_value=contextlib.nullcontext()), \
             contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(experiments.main(["all", "--config", str(self.config()),
                                               "--input", str(self.source), "--output", str(destination)]), 0)
        data = json.loads((destination / "summary.json").read_text())
        self.assertEqual(len(data["stages"]), 5)
        self.assertFalse(data["gaps"])
        frozen = json.loads((destination / "frozen_config.json").read_text())
        self.assertEqual(frozen["concurrency"], 4)
        self.assertEqual(frozen["server_options"]["ctx_size"], 4096)

    def test_startup_and_idle_memory_have_phase_specific_scope(self):
        with patch.object(experiments.memory, "find_target", return_value=(None, "project_server_not_found")):
            for phase in ("startup", "idle"):
                monitor = experiments.PhaseMemoryMonitor("http://127.0.0.1:8081", 0.2, phase)
                value = monitor.summary()
                self.assertEqual(value["scope"], "server_process_" + phase)
                self.assertNotIn("AIPerf case", value["window"])
                self.assertIn("phase", value["baseline_kind"])

    def test_new_server_options_and_observer_reach_the_owned_process(self):
        process = Mock()
        process.poll.return_value = None
        callback = Mock()
        with patch.object(server, "PROJECT_ROOT", self.root), \
             patch.object(server.Path, "is_file", return_value=True), \
             patch.object(server, "_health", side_effect=["missing", "missing", "ready"]), \
             patch.object(server.subprocess, "Popen", return_value=process) as launch:
            server.ensure_server_ready("http://127.0.0.1:8081", server_options=self.plan["server"], on_start=callback)
        command = launch.call_args.args[0]
        self.assertEqual(command[command.index("--parallel") + 1], "4")
        self.assertEqual(command[command.index("--batch-size") + 1], "512")
        callback.assert_called_once_with(process, self.root / ".runtime/llama-server-8081.log")


if __name__ == "__main__":
    unittest.main()
