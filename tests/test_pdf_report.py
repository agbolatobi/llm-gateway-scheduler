"""PDF reporting checks using explicit synthetic AIPerf fixtures."""

import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from benchmarks import report


def make_run(root, case_count=2):
    """Synthetic data for tests/visual QA only; never represents model performance."""
    root = Path(root)
    (root / "inputs").mkdir(parents=True)
    cases, specs = [], []
    for index in range(case_count):
        key = f"synthetic_engineering_{index + 1}_64"
        directory = root / key
        directory.mkdir()
        cases.append({"id": key, "status": "completed"})
        specs.append({"id": key, "prompt_id": f"engineering_{index + 1}",
                      "max_tokens": 64, "target_input_tokens": 128, "request_count": 3})
        records = [{"metadata": {"benchmark_phase": "warmup"}}]
        records += [{"metadata": {"benchmark_phase": "profiling"}} for _ in range(3)]
        (directory / "profile_export.jsonl").write_text(
            "".join(json.dumps(row) + "\n" for row in records), encoding="utf-8")
        completion_records = [
            {"schema_version": 1, "request_id": f"{key}-{index}",
             "benchmark_phase": "warmup" if index == 0 else "profiling",
             "status": "success", "completion_received": True,
             "e2e_latency_ms": 1100 + index, "ttft_ms": 120 + index}
            for index in range(4)
        ]
        (directory / "request_metrics.jsonl").write_text(
            "".join(json.dumps(row) + "\n" for row in completion_records), encoding="utf-8")
        summary = {
            "schema_version": "1.4", "aiperf_version": "0.13.0", "is_complete": True,
            "time_to_first_token": {"unit": "ms", "avg": 120.5 + index, "p50": 119,
                                    "p95": 130.25, "p99": 135.5},
            "request_latency": {"unit": "ms", "avg": 950.25, "p50": 930, "p95": 995, "p99": 1000},
            "inter_token_latency": {"unit": "ms", "avg": 15.2, "p50": 15, "p95": 16.5, "p99": 17},
            "request_throughput": {"unit": "requests/sec", "avg": 1.05},
            "output_token_throughput": {"unit": "tokens/sec", "avg": 52.5},
            "input_sequence_length": {"unit": "tokens", "avg": 137, "p50": 137, "p95": 137, "p99": 137},
            "output_sequence_length": {"unit": "tokens", "avg": 50, "p50": 50, "p95": 53, "p99": 54},
            "request_count": {"unit": "requests", "avg": 3},
            "error_request_count": {"unit": "requests", "avg": 0},
            "warmup_metrics": {"time_to_first_token": {"unit": "ms", "avg": 99999}},
        }
        (directory / "profile_export_aiperf.json").write_text(json.dumps(summary), encoding="utf-8")
    run = {"schema_version": 1, "created_at": "2026-10-25T18:00:00+00:00",
           "status": "completed", "aiperf_version": "0.13.0", "source_sha256": "a" * 64,
           "settings": {"model": "SYNTHETIC TEST DATA - NOT MODEL PERFORMANCE",
                        "base_url": "http://127.0.0.1:8080", "temperature": 0},
           "warmup_requests_per_case": 1, "measured_request_count": 3 * case_count, "cases": cases}
    manifest = {"schema_version": 1, "source_sha256": "a" * 64, "concurrency": 1,
                "repetitions": 3, "cases": specs}
    (root / "run.json").write_text(json.dumps(run), encoding="utf-8")
    (root / "inputs/manifest.json").write_text(json.dumps(manifest), encoding="utf-8")
    return root


class PDFReportTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = make_run(Path(temporary.name) / "run")
        self.case = self.root / "synthetic_engineering_1_64"

    def change_json(self, path, update):
        value = json.loads(path.read_text())
        update(value)
        path.write_text(json.dumps(value), encoding="utf-8")

    def test_native_values_units_and_warmup_separation(self):
        data = report.load_report(self.root)
        case = data["cases"][0]
        self.assertEqual(case["outcomes"], {"measured": 3, "errors": 0, "warmup": 1, "warmup_errors": 0})
        native = next(row for row in case["rows"] if row[0] == "Time to first token")
        self.assertEqual(native, ["Time to first token", "ms", 120.5, 119, 130.25, 135.5])
        e2e = next(row for row in case["rows"] if row[0] == "Completion E2E latency")
        self.assertEqual(e2e[2], 1102)
        self.assertEqual(case["completion"]["e2e_latency"]["count"], 3)
        self.assertEqual(data["manifest"]["concurrency"], 1)
        self.assertEqual(case["warnings"], [])

    def test_case_memory_values_and_unavailable_markers(self):
        summary = {
            "schema_version": 1, "scope": "server_process_case", "unit": "MiB",
            "server_pid": 42, "sampling_interval_seconds": 1,
            "ram": {"baseline_mib": 1024, "peak_mib": 1200, "sample_count": 12,
                    "status": "available", "unavailable_reason": None},
            "vram": {"baseline_mib": None, "peak_mib": None, "sample_count": 0,
                     "status": "unavailable", "unavailable_reason": "cpu_only_server"},
        }
        (self.case / "memory_summary.json").write_text(json.dumps(summary))
        case = report.load_report(self.root)["cases"][0]
        self.assertEqual(case["memory_rows"][0], ["Server RSS", 1024, 1200, 12, "available", None])
        self.assertEqual(case["memory_rows"][1][-1], "cpu_only_server")
        self.assertTrue(report.export_pdf(self.root).is_file())

    def test_older_reports_mark_memory_uncollected(self):
        case = report.load_report(self.root)["cases"][0]
        self.assertIsNone(case["memory"])
        self.assertTrue(all(row[1:4] == [None, None, None] for row in case["memory_rows"]))

    def test_invalid_memory_summary_is_rejected(self):
        (self.case / "memory_summary.json").write_text('{"schema_version": 1}')
        with self.assertRaisesRegex(ValueError, "memory summary"):
            report.load_report(self.root)
        with self.assertRaisesRegex(ValueError, "memory value"):
            report.memory_rows({"ram": {"peak_mib": float("nan")}})

    def test_missing_values_and_non_finite_values_are_unavailable(self):
        self.assertEqual(report.number(None), "N/A")
        self.assertEqual(report.number(float("nan")), "N/A")
        self.assertEqual(report.number(float("inf")), "N/A")
        self.assertEqual(report.number(True), "N/A")
        self.assertEqual(report.number("12"), "N/A")
        self.assertEqual(report.number(0), "0")
        self.assertEqual(report.number(47569.387), "47,569")
        self.assertEqual(report.number(1_000_000), "1.0e+06")
        self.assertEqual(report.metric_rows({})[0][1:], ["N/A", None, None, None, None])

    def test_backend_phase_values_and_cache_coverage(self):
        path = self.case / "request_metrics.jsonl"
        rows = [json.loads(line) for line in path.read_text().splitlines()]
        for row in rows:
            row.update(input_tokens=17, output_tokens=2, backend_prompt_tokens=17,
                       cached_input_tokens=0, backend_output_tokens=2,
                       backend_prompt_ms=80, backend_decode_ms=100)
        path.write_text("".join(json.dumps(row) + "\n" for row in rows))
        case = report.load_report(self.root)["cases"][0]
        self.assertEqual(case["backend"]["backend_prompt_ms"]["p50"], 80)
        self.assertEqual(case["backend"]["cached_input_tokens"]["p50"], 0)
        self.assertEqual(case["cache_verification"]["uncached_requests"], 3)
        self.assertTrue(report.export_pdf(self.root).is_file())

    def test_pdf_is_created_without_changing_sources(self):
        before = {p: p.read_bytes() for p in self.root.rglob("*") if p.is_file()}
        with patch("subprocess.run") as process:
            destination = report.export_pdf(self.root)
        process.assert_not_called()
        content = destination.read_bytes()
        self.assertTrue(content.startswith(b"%PDF-"))
        self.assertTrue(content.rstrip().endswith(b"%%EOF"))
        self.assertGreater(len(content), 2000)
        for path, original in before.items():
            self.assertEqual(path.read_bytes(), original)

    def test_existing_pdf_is_never_overwritten(self):
        path = self.root / "report.pdf"
        path.write_bytes(b"keep this")
        with self.assertRaises(FileExistsError):
            report.export_pdf(self.root)
        self.assertEqual(path.read_bytes(), b"keep this")

    def test_custom_output_path(self):
        path = self.root / "exports/custom.pdf"
        self.assertEqual(report.export_pdf(self.root, path), path)
        self.assertFalse((self.root / "report.pdf").exists())

    def test_invalid_output_extension_is_rejected(self):
        with self.assertRaises(ValueError):
            report.export_pdf(self.root, self.root / "run.json")

    def test_dry_run_cannot_be_exported(self):
        self.change_json(self.root / "run.json", lambda value: value.update(status="dry_run"))
        with self.assertRaisesRegex(ValueError, "no benchmark results"):
            report.export_pdf(self.root)
        self.assertFalse((self.root / "report.pdf").exists())

    def test_partial_case_and_missing_summary_are_explicit(self):
        self.change_json(self.root / "run.json", lambda value: value.update(status="failed"))
        self.change_json(self.root / "run.json",
                         lambda value: value["cases"][0].update(status="failed"))
        (self.case / "profile_export_aiperf.json").unlink()
        data = report.load_report(self.root)
        self.assertEqual(data["run"]["status"], "failed")
        self.assertGreaterEqual(len(data["cases"][0]["warnings"]), 2)
        self.assertTrue(report.export_pdf(self.root).is_file())

    def test_record_errors_and_count_mismatch_are_flagged(self):
        (self.case / "request_metrics.jsonl").unlink()
        records = [{"metadata": {"benchmark_phase": "profiling"}, "error": {"message": "timeout"}}]
        (self.case / "profile_export.jsonl").write_text(json.dumps(records[0]) + "\n")
        case = report.load_report(self.root)["cases"][0]
        self.assertEqual(case["outcomes"]["errors"], 1)
        self.assertTrue(any("differs" in message for message in case["warnings"]))
        self.assertTrue(any("Failed" in message for message in case["warnings"]))

    def test_missing_records_are_not_zero_successes(self):
        (self.case / "profile_export.jsonl").unlink()
        (self.case / "request_metrics.jsonl").unlink()
        case = report.load_report(self.root)["cases"][0]
        self.assertIsNone(case["outcomes"])
        self.assertTrue(any("unverified" in message for message in case["warnings"]))

    def test_legacy_runs_show_unavailable_completion_metrics(self):
        (self.case / "request_metrics.jsonl").unlink()
        case = report.load_report(self.root)["cases"][0]
        self.assertIsNone(case["completion"])
        e2e = next(row for row in case["rows"] if row[0] == "Completion E2E latency")
        self.assertEqual(e2e[2:], [None, None, None, None])
        self.assertTrue(any("E2E metrics are N/A" in warning for warning in case["warnings"]))

    def test_completion_failures_override_native_success_coverage(self):
        path = self.case / "request_metrics.jsonl"
        rows = [json.loads(line) for line in path.read_text().splitlines()]
        rows[1].update(status="incomplete_stream", completion_received=False, e2e_latency_ms=None)
        path.write_text("".join(json.dumps(row) + "\n" for row in rows))
        case = report.load_report(self.root)["cases"][0]
        self.assertEqual(case["outcomes"]["errors"], 1)
        self.assertEqual(case["completion"]["e2e_latency"]["count"], 2)
        self.assertTrue(any("Completion verification failed" in warning for warning in case["warnings"]))

    def test_malformed_summary_fails_without_creating_pdf(self):
        (self.case / "profile_export_aiperf.json").write_text("not JSON")
        with self.assertRaises(ValueError):
            report.export_pdf(self.root)
        self.assertFalse((self.root / "report.pdf").exists())

    def test_no_results_fails_without_creating_pdf(self):
        for path in self.root.glob("synthetic*/*.json*"):
            path.unlink()
        with self.assertRaisesRegex(ValueError, "No saved"):
            report.export_pdf(self.root)

    def test_case_paths_cannot_escape_run_directory(self):
        self.change_json(self.root / "run.json", lambda value: value["cases"][0].update(id="../outside"))
        with self.assertRaises(ValueError):
            report.load_report(self.root)

    def test_symlinked_result_outside_run_is_rejected(self):
        source = self.case / "profile_export_aiperf.json"
        source.unlink()
        source.symlink_to(self.root.parent / "outside.json")
        with self.assertRaisesRegex(ValueError, "escapes"):
            report.load_report(self.root)

    def test_long_names_unicode_and_markup_are_safe(self):
        model = 'test <img src="http://invalid.example/private"/> & Ω 中文 ' + "very_long_alias_" * 25
        self.change_json(self.root / "run.json",
                         lambda value: value["settings"].update(model=model))
        self.assertTrue(report.export_pdf(self.root).is_file())

    def test_cli_reports_errors(self):
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(report.main([str(self.root / "missing")]), 1)


if __name__ == "__main__":
    unittest.main()
