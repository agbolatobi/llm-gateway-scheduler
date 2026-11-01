"""Fast checks: no model startup, tokenizer download, or inference."""

import contextlib
import copy
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch

import cli
from benchmarks import prepare, run


def fixture():
    return {
        "prompts": [{"id": "short", "prompt": "  Exact engineering text.\nSecond line. Ω  ",
                     "target_input_tokens": 128, "actual_input_tokens": None}],
        "test_config": {"repetitions": 3, "concurrency": 1},
        "test_cases": [
            {"id": "short_64", "prompt_id": "short", "max_tokens": 64},
            {"id": "short_256", "prompt_id": "short", "max_tokens": 256},
        ],
    }


class BenchmarkTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.source = self.root / "prompts.json"
        self.data = fixture()
        self.write_source()
        # Unit profiles are synthetic; never inspect or poll a user's live server.
        self.enterContext(patch("benchmarks.memory.find_target",
                                return_value=(None, "project_server_not_found")))
        for stream in ("stdout", "stderr"):
            manager = contextlib.redirect_stdout if stream == "stdout" else contextlib.redirect_stderr
            self.enterContext(manager(io.StringIO()))

    def write_source(self):
        self.source.write_text(json.dumps(self.data), encoding="utf-8")

    def args(self, *extra):
        return ["--input", str(self.source), "--output", str(self.root / "run"), *extra]

    def test_exact_text_caps_repetitions_and_source_preserved(self):
        original = self.source.read_bytes()
        workload = prepare.load_workload(self.source)
        output = self.root / "prepared"
        manifest = prepare.write_prepared(workload, output)
        self.assertEqual(manifest["request_count"], 6)
        self.assertEqual(manifest["concurrency"], 1)
        for case in manifest["cases"]:
            lines = [json.loads(line) for line in (output / case["input_file"]).read_text().splitlines()]
            self.assertEqual(len(lines), 3)
            for index, row in enumerate(lines, 1):
                self.assertEqual(row["text_input"], self.data["prompts"][0]["prompt"])
                self.assertEqual(row["output_length"], case["max_tokens"])
                self.assertEqual(row["session_id"], f"{case['id']}-r{index}")
                self.assertNotIn("input_length", row)
        self.assertEqual(self.source.read_bytes(), original)

    def test_selection_and_overrides(self):
        workload = prepare.load_workload(self.source, case_ids=["short_256"],
                                         repetitions=1, concurrency=2)
        self.assertEqual(len(workload["cases"]), 1)
        self.assertEqual(workload["repetitions"], 1)
        self.assertEqual(workload["concurrency"], 2)

    def test_compare_flag_exports_after_success(self):
        with patch.object(run, "installed_executable", return_value=Path("/fake/aiperf")), \
             patch.object(run.subprocess, "run", side_effect=self.fake_profile), \
             patch.object(run, "export_comparison", return_value={"meas_02_complete": True, "gaps": []}) as export:
            self.assertEqual(run.main(self.args("--compare")), 0)
        export.assert_called_once_with(self.root / "run")
        record = json.loads((self.root / "run/run.json").read_text())
        self.assertIs(record["settings"]["compare"], True)
        self.assertNotIn("meas_02", record["settings"])
        self.assertEqual(record["comparison"]["status"], "completed")

    def test_compare_export_is_opt_in(self):
        with patch.object(run, "installed_executable", return_value=Path("/fake/aiperf")), \
             patch.object(run.subprocess, "run", side_effect=self.fake_profile), \
             patch.object(run, "export_comparison") as export:
            self.assertEqual(run.main(self.args()), 0)
        export.assert_not_called()

    def test_compare_dry_run_does_not_export_or_start_processes(self):
        with patch.object(run.subprocess, "run") as execute, \
             patch.object(run, "export_comparison") as export:
            self.assertEqual(run.main(self.args("--compare", "--dry-run")), 0)
        execute.assert_not_called()
        export.assert_not_called()
        record = json.loads((self.root / "run/run.json").read_text())
        self.assertIs(record["settings"]["compare"], True)
        self.assertEqual(record["status"], "dry_run")

    def test_help_exposes_functional_compare_flag(self):
        output = io.StringIO()
        with contextlib.redirect_stdout(output), self.assertRaises(SystemExit) as exit:
            run.main(["--help"])
        self.assertEqual(exit.exception.code, 0)
        self.assertIn("--compare", output.getvalue())
        self.assertNotIn("--meas-02", output.getvalue())

    def test_pdf_flag_exports_after_success(self):
        with patch.object(run, "installed_executable", return_value=Path("/fake/aiperf")), \
             patch.object(run.subprocess, "run", side_effect=self.fake_profile), \
             patch.object(run, "export_pdf", return_value=self.root / "run/report.pdf") as export:
            self.assertEqual(run.main(self.args("--pdf")), 0)
        export.assert_called_once_with(self.root / "run")
        record = json.loads((self.root / "run/run.json").read_text())
        self.assertEqual(record["pdf_export"]["status"], "completed")

    def test_pdf_failure_preserves_successful_benchmark_status(self):
        with patch.object(run, "installed_executable", return_value=Path("/fake/aiperf")), \
             patch.object(run.subprocess, "run", side_effect=self.fake_profile), \
             patch.object(run, "export_pdf", side_effect=OSError("disk full")):
            self.assertEqual(run.main(self.args("--pdf")), 1)
        record = json.loads((self.root / "run/run.json").read_text())
        self.assertEqual(record["status"], "completed")
        self.assertEqual(record["pdf_export"]["status"], "failed")
        self.assertTrue((self.root / "run/short_64/profile_export.jsonl").is_file())

    def test_pdf_dry_run_needs_no_dependency_or_inference(self):
        with patch.object(run, "require_pdf_support") as support, \
             patch.object(run, "export_pdf") as export, \
             patch.object(run.subprocess, "run") as execute:
            self.assertEqual(run.main(self.args("--pdf", "--dry-run")), 0)
        support.assert_not_called()
        export.assert_not_called()
        execute.assert_not_called()

    def test_missing_pdf_dependency_fails_before_startup(self):
        with patch.object(run, "require_pdf_support", side_effect=RuntimeError("missing")), \
             patch.object(run.subprocess, "run") as execute:
            self.assertEqual(run.main(self.args("--pdf")), 1)
        execute.assert_not_called()
        self.assertFalse((self.root / "run").exists())

    def test_invalid_workloads_fail_before_output(self):
        variants = []
        for field, value in (("repetitions", 0), ("concurrency", True)):
            data = fixture()
            data["test_config"][field] = value
            variants.append(data)
        data = fixture()
        data["test_cases"][0]["prompt_id"] = "missing"
        variants.append(data)
        data = fixture()
        data["test_cases"][0]["id"] = "../escape"
        variants.append(data)
        data = fixture()
        data["test_cases"][0]["max_tokens"] = -1
        variants.append(data)
        data = fixture()
        data["prompts"].append(copy.deepcopy(data["prompts"][0]))
        variants.append(data)
        data = fixture()
        data["test_cases"].append(copy.deepcopy(data["test_cases"][0]))
        variants.append(data)
        data = fixture()
        data["prompts"][0]["prompt"] = "   "
        variants.append(data)
        for data in variants:
            with self.subTest(data=data):
                self.data = data
                self.write_source()
                self.assertEqual(prepare.main(self.args()), 1)
                self.assertFalse((self.root / "run").exists())

    def test_unknown_case_rejected(self):
        with self.assertRaisesRegex(ValueError, "Unknown case"):
            prepare.load_workload(self.source, case_ids=["missing"])

    def test_output_never_overwritten(self):
        output = self.root / "prepared"
        workload = prepare.load_workload(self.source)
        prepare.write_prepared(workload, output)
        before = (output / "manifest.json").read_bytes()
        with self.assertRaises(FileExistsError):
            prepare.write_prepared(workload, output)
        self.assertEqual((output / "manifest.json").read_bytes(), before)

    def test_dry_run_never_starts_processes_or_requires_aiperf(self):
        with patch.object(run, "installed_executable") as installed:
            with patch.object(run.subprocess, "run") as execute:
                self.assertEqual(run.main(self.args("--dry-run")), 0)
        installed.assert_not_called()
        execute.assert_not_called()
        record = json.loads((self.root / "run/run.json").read_text())
        self.assertEqual(record["status"], "dry_run")
        self.assertIn("--server-only", record["startup_command"])
        self.assertEqual(record["measured_request_count"], 6)
        command = record["cases"][0]["command"]
        for flag in ("--streaming", "--use-server-token-count", "--use-legacy-max-tokens"):
            self.assertIn(flag, command)
        self.assertEqual(command[command.index("--export-level") + 1], "raw")
        self.assertEqual(command[command.index("--tokenizer") + 1], "builtin")
        self.assertIs(json.loads(command[command.index("--extra-inputs") + 1])["cache_prompt"], False)
        self.assertEqual(record["settings"]["prompt_cache"], "off")

    def fake_profile(self, command, **kwargs):
        if "--artifact-dir" in command:
            output = Path(command[command.index("--artifact-dir") + 1])
            output.mkdir(parents=True)
            count = int(command[command.index("--request-count") + 1])
            from test_latency import pair, write_jsonl
            pairs = [pair(f"request-{index}") for index in range(count)]
            (output / "profile_export.jsonl").write_text(
                "".join(json.dumps(native) + "\n" for _, native in pairs), encoding="utf-8")
            write_jsonl(output / "profile_export_raw.jsonl", [raw for raw, _ in reversed(pairs)])
        return subprocess.CompletedProcess(command, 0)

    def test_run_uses_cli_then_each_case(self):
        with patch.object(run, "installed_executable", return_value=Path("/fake/aiperf")):
            with patch.object(run.subprocess, "run", side_effect=self.fake_profile) as execute:
                self.assertEqual(run.main(self.args()), 0)
        commands = [call.args[0] for call in execute.call_args_list]
        self.assertEqual(len(commands), 3)
        self.assertIn(str(run.PROJECT_ROOT / "cli.py"), commands[0])
        self.assertIn("--start-server", commands[0])
        self.assertIn("--server-only", commands[0])
        self.assertEqual(commands[1][0], str(Path("/fake/aiperf")))
        record = json.loads((self.root / "run/run.json").read_text())
        self.assertEqual(record["status"], "completed")
        self.assertTrue(all(case["status"] == "completed" for case in record["cases"]))
        self.assertTrue(all(case["completion_outcomes"]["errors"] == 0 for case in record["cases"]))
        self.assertTrue((self.root / "run/short_64/request_metrics.jsonl").is_file())
        self.assertTrue((self.root / "run/short_64/memory_samples.jsonl").is_file())
        summary = json.loads((self.root / "run/short_64/memory_summary.json").read_text())
        self.assertEqual(summary["ram"]["unavailable_reason"], "project_server_not_found")
        row = json.loads((self.root / "run/short_64/request_metrics.jsonl").read_text().splitlines()[0])
        self.assertIsNone(row["peak_ram_mib"])
        self.assertEqual(row["ram_unavailable_reason"], "project_server_not_found")

    def test_native_success_without_done_is_a_failed_case(self):
        def incomplete(command, **kwargs):
            result = self.fake_profile(command, **kwargs)
            if "--artifact-dir" in command:
                output = Path(command[command.index("--artifact-dir") + 1])
                path = output / "profile_export_raw.jsonl"
                rows = [json.loads(line) for line in path.read_text().splitlines()]
                for row in rows:
                    row["responses"].pop()
                from test_latency import write_jsonl
                write_jsonl(path, rows)
            return result
        with patch.object(run, "installed_executable", return_value=Path("/fake/aiperf")), \
             patch.object(run.subprocess, "run", side_effect=incomplete) as execute:
            self.assertEqual(run.main(self.args()), 1)
        self.assertEqual(execute.call_count, 2)
        record = json.loads((self.root / "run/run.json").read_text())
        self.assertEqual(record["status"], "failed")
        self.assertEqual(record["cases"][0]["completion_outcomes"]["errors"], 3)
        rows = [json.loads(line) for line in
                (self.root / "run/short_64/request_metrics.jsonl").read_text().splitlines()]
        self.assertTrue(all(row["status"] == "incomplete_stream" for row in rows))

    def test_aiperf_failure_still_exports_available_raw_error_records(self):
        def failed_profile(command, **kwargs):
            result = self.fake_profile(command, **kwargs)
            if "--artifact-dir" in command:
                output = Path(command[command.index("--artifact-dir") + 1])
                path = output / "profile_export_raw.jsonl"
                rows = [json.loads(line) for line in path.read_text().splitlines()]
                for row in rows:
                    row["error"] = {"type": "TimeoutError", "message": "timed out"}
                from test_latency import write_jsonl
                write_jsonl(path, rows)
                raise subprocess.CalledProcessError(1, command)
            return result
        with patch.object(run, "installed_executable", return_value=Path("/fake/aiperf")), \
             patch.object(run.subprocess, "run", side_effect=failed_profile):
            self.assertEqual(run.main(self.args()), 1)
        record = json.loads((self.root / "run/run.json").read_text())
        self.assertEqual(record["cases"][0]["exit_code"], 1)
        self.assertEqual(record["cases"][0]["completion_outcomes"]["errors"], 3)
        self.assertTrue((self.root / "run/short_64/request_metrics.csv").is_file())

    def test_no_start_option_does_not_invoke_cli(self):
        with patch.object(run, "installed_executable", return_value=Path("/fake/aiperf")):
            with patch.object(run.subprocess, "run", side_effect=self.fake_profile) as execute:
                self.assertEqual(run.main(self.args("--no-start-server", "--warmup-requests", "0")), 0)
        self.assertEqual(execute.call_count, 2)
        self.assertTrue(all("--warmup-request-count" not in c.args[0]
                            for c in execute.call_args_list))

    def test_failed_startup_prevents_profiling(self):
        failure = subprocess.CalledProcessError(1, ["cli.py"])
        with patch.object(run, "installed_executable", return_value=Path("/fake/aiperf")):
            with patch.object(run.subprocess, "run", side_effect=failure) as execute:
                self.assertEqual(run.main(self.args()), 1)
        self.assertEqual(execute.call_count, 1)
        self.assertEqual(json.loads((self.root / "run/run.json").read_text())["status"], "failed")

    def test_failed_profile_prevents_remaining_cases(self):
        results = [subprocess.CompletedProcess([], 0),
                   subprocess.CalledProcessError(1, ["aiperf"])]
        with patch.object(run, "installed_executable", return_value=Path("/fake/aiperf")):
            with patch.object(run.subprocess, "run", side_effect=results) as execute:
                self.assertEqual(run.main(self.args()), 1)
        self.assertEqual(execute.call_count, 2)
        record = json.loads((self.root / "run/run.json").read_text())
        self.assertEqual(record["cases"][0]["status"], "failed")
        self.assertEqual(record["cases"][1]["status"], "pending")
        self.assertTrue((self.root / "run/short_64/memory_summary.json").is_file())

    def test_profile_interrupt_stops_sampler_and_retains_memory(self):
        results = [subprocess.CompletedProcess([], 0), KeyboardInterrupt()]
        with patch.object(run, "installed_executable", return_value=Path("/fake/aiperf")), \
             patch.object(run.subprocess, "run", side_effect=results):
            self.assertEqual(run.main(self.args()), 130)
        record = json.loads((self.root / "run/run.json").read_text())
        self.assertEqual(record["cases"][0]["status"], "interrupted")
        self.assertEqual(record["cases"][0]["memory"]["ram"]["status"], "unavailable")
        self.assertTrue((self.root / "run/short_64/memory_samples.jsonl").is_file())

    def test_invalid_memory_interval_rejected_before_execution(self):
        for value in ("0", "0.01", "61", "nan", "inf"):
            with self.subTest(value=value), self.assertRaises(SystemExit), \
                 patch.object(run.subprocess, "run") as execute:
                run.main(self.args("--memory-interval", value))
            execute.assert_not_called()

    def test_interrupt_keeps_manifest(self):
        with patch.object(run, "installed_executable", return_value=Path("/fake/aiperf")):
            with patch.object(run.subprocess, "run", side_effect=KeyboardInterrupt):
                self.assertEqual(run.main(self.args()), 130)
        self.assertEqual(json.loads((self.root / "run/run.json").read_text())["status"], "interrupted")

    def test_missing_aiperf_does_not_start_server_or_create_run(self):
        with patch.object(run, "installed_executable", side_effect=RuntimeError("missing")):
            with patch.object(run.subprocess, "run") as execute:
                self.assertEqual(run.main(self.args()), 1)
        execute.assert_not_called()
        self.assertFalse((self.root / "run").exists())

    def test_missing_record_export_is_not_success(self):
        with patch.object(run, "installed_executable", return_value=Path("/fake/aiperf")):
            with patch.object(run.subprocess, "run", return_value=subprocess.CompletedProcess([], 0)):
                self.assertEqual(run.main(self.args()), 1)

    def test_remote_auto_start_is_rejected(self):
        with patch.object(run.subprocess, "run") as execute:
            self.assertEqual(run.main(self.args("--dry-run", "--base-url", "http://example.com")), 1)
        execute.assert_not_called()
        self.assertFalse((self.root / "run").exists())

    def test_record_outcomes_separate_warmup_and_errors(self):
        records = [
            {"metadata": {"benchmark_phase": "warmup"}},
            {"metadata": {"benchmark_phase": "profiling"}},
            {"metadata": {"benchmark_phase": "profiling"}, "error": {"message": "timeout"}},
            {"metadata": {"benchmark_phase": "profiling", "was_cancelled": True}},
            {"metadata": {"benchmark_phase": "profiling", "context_overflow_skip": True}},
        ]
        path = self.root / "records.jsonl"
        path.write_text("".join(json.dumps(row) + "\n" for row in records))
        self.assertEqual(run.request_outcomes(path),
                         {"measured": 4, "errors": 3, "warmup": 1, "warmup_errors": 0})

    def test_incomplete_or_failed_exports_do_not_report_success(self):
        for outcomes in (
            {"measured": 2, "errors": 0},
            {"measured": 3, "errors": 1},
        ):
            with self.subTest(outcomes=outcomes):
                destination = self.root / f"outcome-{outcomes['errors']}"
                with patch.object(run, "installed_executable", return_value=Path("/fake/aiperf")), \
                     patch.object(run.subprocess, "run", return_value=subprocess.CompletedProcess([], 0)), \
                     patch.object(run, "request_outcomes", return_value=outcomes), \
                     patch.object(run, "export_request_metrics", return_value={
                         "outcomes": outcomes, "cache_verification": {
                             "cached_requests": 0, "unverified_requests": 0}}):
                    self.assertEqual(run.main(["--input", str(self.source), "--output", str(destination)]), 1)
                record = json.loads((destination / "run.json").read_text())
                self.assertEqual(record["cases"][0]["status"], "failed")

    def test_invalid_record_export_rejected(self):
        path = self.root / "invalid.jsonl"
        for content in ("{}\n", "not JSON\n", "null\n",
                        '{"metadata":{"benchmark_phase":"unknown"}}\n'):
            with self.subTest(content=content):
                path.write_text(content)
                with self.assertRaises(ValueError):
                    run.request_outcomes(path)


class StartupOnlyCLITests(unittest.TestCase):
    def setUp(self):
        self.enterContext(contextlib.redirect_stdout(io.StringIO()))
        self.enterContext(contextlib.redirect_stderr(io.StringIO()))

    def test_startup_only_checks_readiness_without_inference(self):
        with patch.object(cli, "ensure_server_ready", return_value=None) as ready:
            with patch.object(cli, "LLMClient") as client:
                self.assertEqual(cli.main(["--start-server", "--server-only"]), 0)
        ready.assert_called_once_with("http://127.0.0.1:8080", model="local-llm", timeout=120)
        client.return_value.chat.assert_not_called()
        client.return_value.stream.assert_not_called()

    def test_startup_errors_are_reported(self):
        with patch.object(cli, "ensure_server_ready", side_effect=RuntimeError("failed")):
            self.assertEqual(cli.main(["--start-server", "--server-only"]), 1)

    def test_invalid_startup_only_combinations(self):
        for args in ([], ["--server-only"], ["--start-server", "--server-only", "hello"],
                     ["--start-server", "--server-only", "--stream"]):
            with self.subTest(args=args), self.assertRaises(SystemExit) as error:
                cli.main(args)
            self.assertEqual(error.exception.code, 2)

    def test_normal_chat_remains_supported(self):
        client = Mock()
        client.chat.return_value = {"choices": [{"message": {"content": "hello"}}]}
        with patch.object(cli, "LLMClient", return_value=client):
            self.assertEqual(cli.main(["hello"]), 0)
        client.chat.assert_called_once()


if __name__ == "__main__":
    unittest.main()
