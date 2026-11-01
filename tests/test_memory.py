"""Memory sampling checks; no model or GPU is required."""

import csv
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import unittest
from unittest.mock import patch

from benchmarks import memory
from benchmarks.latency import export_request_metrics
from test_latency import pair, write_jsonl


def sample(start=100, end=110, ram=12, vram=None, ram_reason=None,
           vram_reason="cpu_only_server"):
    return {"start_perf_ns": start, "end_perf_ns": end, "ram_mib": ram,
            "vram_mib": vram, "ram_unavailable_reason": ram_reason,
            "vram_unavailable_reason": vram_reason}


class MemoryTests(unittest.TestCase):
    def monitor(self):
        with patch.object(memory, "find_target", return_value=(None, "project_server_not_found")):
            return memory.MemoryMonitor("http://127.0.0.1:8080", 0.1)

    def gpu(self, output, code=0):
        result = subprocess.CompletedProcess([], code, stdout=output, stderr="")
        with patch.object(memory.subprocess, "run", return_value=result) as run:
            value = memory.gpu_memory_mib("/fake/nvidia-smi", 42)
        self.assertEqual(run.call_args.kwargs["timeout"], 1)
        self.assertIn("--query-compute-apps=pid,gpu_uuid,used_gpu_memory", run.call_args.args[0])
        return value

    def test_gpu_sums_only_selected_pid_across_devices(self):
        self.assertEqual(self.gpu("42, GPU-a, 128\n99, GPU-a, 999\n42, GPU-b, 256\n"),
                         (384, None))

    def test_gpu_unavailable_is_never_zero(self):
        for output in ("", "99, GPU-a, 999\n", "42, GPU-a, N/A\n",
                       "42, GPU-a, nan\n", "42, GPU-a, -1\n",
                       "42, GPU-a, 12\n42, GPU-a, 12\n", "unsupported"):
            with self.subTest(output=output):
                value, reason = self.gpu(output)
                self.assertIsNone(value)
                self.assertTrue(reason)
        self.assertEqual(memory.gpu_memory_mib(None, 42), (None, "nvidia_smi_not_found"))
        self.assertEqual(self.gpu("", 1), (None, "gpu_query_failed_or_unsupported"))

    def test_reported_zero_is_valid(self):
        self.assertEqual(self.gpu("42, GPU-a, 0\n"), (0, None))

    def test_gpu_timeout_and_os_error(self):
        for error, reason in ((subprocess.TimeoutExpired("nvidia-smi", 1), "gpu_query_timeout"),
                              (OSError("missing"), "gpu_query_failed")):
            with patch.object(memory.subprocess, "run", side_effect=error):
                self.assertEqual(memory.gpu_memory_mib("/fake", 42), (None, reason))

    def test_remote_endpoint_does_not_scan_local_processes(self):
        with patch.object(memory, "_find_server") as find:
            self.assertEqual(memory.find_target("http://example.com:8080"),
                             (None, "nonlocal_or_unsupported_endpoint"))
        find.assert_not_called()

    def test_local_identity_ambiguity_and_absence_are_marked(self):
        for returned, error, reason in ((None, None, "project_server_not_found"),
                                        (None, RuntimeError("multiple"), "server_identity_unavailable"),
                                        (42, None, None)):
            with patch.object(memory, "_find_server", return_value=returned, side_effect=error):
                self.assertEqual(memory.find_target("http://localhost:8080"), (returned, reason))

    def test_real_proc_counters_for_only_this_test_process(self):
        self.assertGreater(memory.rss_mib(os.getpid()), 0)
        self.assertGreater(memory.process_start(os.getpid()), 0)

    def test_rss_units_and_missing_counter(self):
        with patch.object(Path, "read_text", return_value="Name: test\nVmRSS: 2048 kB\n"):
            self.assertEqual(memory.rss_mib(42), 2)
        for data in ("Name: test", "VmRSS: NaN kB", "VmRSS: 2048 MB"):
            with patch.object(Path, "read_text", return_value=data), self.assertRaises(ValueError):
                memory.rss_mib(42)

    def test_cpu_only_requires_explicit_runtime_flag(self):
        self.assertTrue(memory.cpu_only(["llama-server", "--device", "none"]))
        self.assertTrue(memory.cpu_only(["llama-server", "--device=none"]))
        for args in ([], ["--device", "CUDA0"], ["--model", "none"], ["--device"],
                     ["--device", "none", "--device", "CUDA0"]):
            self.assertFalse(memory.cpu_only(args))

    def test_missing_server_never_queries_gpu(self):
        monitor = self.monitor()
        with patch.object(memory, "gpu_memory_mib") as gpu:
            monitor.sample()
        gpu.assert_not_called()
        summary = monitor.summary()
        for name in ("ram", "vram"):
            self.assertIsNone(summary[name]["peak_mib"])
            self.assertEqual(summary[name]["unavailable_reason"], "project_server_not_found")

    def test_cpu_and_wsl_paths_do_not_query_gpu(self):
        for cpu, wsl, reason in (("cpu_only_server", None, "cpu_only_server"),
                                 (None, "wsl_process_vram_unsupported", "wsl_process_vram_unsupported")):
            monitor = self.monitor()
            monitor.pid, monitor.target_reason = 42, None
            monitor.cpu_reason, monitor.gpu_reason = cpu, wsl
            with patch.object(monitor, "_same_process", return_value=True), \
                 patch.object(memory, "rss_mib", return_value=12), \
                 patch.object(memory, "gpu_memory_mib") as gpu:
                monitor.sample()
            gpu.assert_not_called()
            self.assertEqual(monitor.samples[-1]["ram_mib"], 12)
            self.assertEqual(monitor.samples[-1]["vram_unavailable_reason"], reason)

    def test_identity_change_discards_all_readings(self):
        monitor = self.monitor()
        monitor.pid, monitor.target_reason = 42, None
        monitor.cpu_reason = "cpu_only_server"
        with patch.object(monitor, "_same_process", side_effect=[True, False]), \
             patch.object(memory, "rss_mib", return_value=12):
            monitor.sample()
        self.assertIsNone(monitor.samples[-1]["ram_mib"])
        self.assertEqual(monitor.samples[-1]["ram_unavailable_reason"],
                         "server_exited_or_identity_changed")

    def test_partial_samples_keep_available_peak_and_error_counts(self):
        monitor = self.monitor()
        monitor.samples = [sample(ram=None, ram_reason="rss_counter_unavailable"),
                           sample(ram=24), sample(ram=12)]
        result = monitor.summary()["ram"]
        self.assertEqual(result["peak_mib"], 24)
        self.assertIsNone(result["baseline_mib"])
        self.assertEqual(result["status"], "partial")
        self.assertEqual(result["sample_count"], 2)
        self.assertEqual(result["unavailable_sample_count"], 1)

    def test_request_peaks_exclude_baseline_outside_and_straddling_samples(self):
        monitor = self.monitor()
        monitor.samples = [sample(10, 20, 999), sample(95, 105, 888),
                           sample(110, 120, 20), sample(150, 160, 40), sample(195, 205, 777)]
        row = monitor.request_fields({"request_start_perf_ns": 100, "completion_perf_ns": 200})
        self.assertEqual(row["peak_ram_mib"], 40)
        self.assertEqual(row["ram_sample_count"], 2)
        self.assertEqual(row["memory_scope"], "server_process_request_window")
        self.assertEqual(row["vram_unavailable_reason"], "cpu_only_server")
        self.assertEqual(monitor.summary()["ram"]["peak_mib"], 999)

    def test_short_and_incomplete_request_markers(self):
        monitor = self.monitor()
        monitor.samples = [sample(100, 110)]
        for start, end, reason in ((101, 109, "no_samples_in_request_window"),
                                    (100, None, "request_window_unavailable"),
                                    (True, 200, "request_window_unavailable")):
            fields = monitor.request_fields({"request_start_perf_ns": start, "completion_perf_ns": end})
            self.assertIsNone(fields["peak_ram_mib"])
            self.assertEqual(fields["ram_unavailable_reason"], reason)

    def test_overlapping_windows_share_process_observations(self):
        monitor = self.monitor()
        monitor.samples = [sample(150, 160, 24)]
        for start, end in ((100, 200), (140, 220)):
            self.assertEqual(monitor.request_fields(
                {"request_start_perf_ns": start, "completion_perf_ns": end})["peak_ram_mib"], 24)

    def test_thread_stops_on_exception_and_exports_are_no_clobber(self):
        monitor = self.monitor()
        with self.assertRaisesRegex(RuntimeError, "profile failed"):
            with monitor:
                self.assertTrue(monitor.thread.is_alive())
                threading.Event().wait(0.12)
                raise RuntimeError("profile failed")
        self.assertFalse(monitor.thread.is_alive())
        self.assertGreaterEqual(len(monitor.samples), 3)
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary)
            monitor.save(path)
            original = (path / "memory_samples.jsonl").read_bytes()
            self.assertEqual(len(original.splitlines()), len(monitor.samples))
            self.assertEqual(json.loads((path / "memory_summary.json").read_text())["scope"],
                             "server_process_case")
            with self.assertRaises(FileExistsError):
                monitor.save(path)
            self.assertEqual((path / "memory_samples.jsonl").read_bytes(), original)

    def test_interval_validation(self):
        for interval in (0, 0.01, 61, float("nan"), float("inf")):
            with self.assertRaises(ValueError):
                memory.MemoryMonitor("http://localhost:8080", interval)

    def test_memory_fields_in_jsonl_csv_with_explicit_unavailable_values(self):
        monitor = self.monitor()
        monitor.samples = [sample(2_110_000_000, 2_120_000_000, 42)]
        raw, native = pair("request")
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary)
            write_jsonl(path / "profile_export.jsonl", [native])
            write_jsonl(path / "profile_export_raw.jsonl", [raw])
            export_request_metrics(path, memory=monitor)
            row = json.loads((path / "request_metrics.jsonl").read_text())
            self.assertEqual(row["peak_ram_mib"], 42)
            self.assertIsNone(row["peak_vram_mib"])
            self.assertEqual(row["vram_unavailable_reason"], "cpu_only_server")
            with (path / "request_metrics.csv").open(newline="") as stream:
                row = next(csv.DictReader(stream))
            self.assertEqual(row["peak_vram_mib"], "")
            self.assertEqual(row["vram_unavailable_reason"], "cpu_only_server")

    def test_uncollected_defaults_are_explicit(self):
        fields = memory.uncollected_fields()
        self.assertIsNone(fields["peak_ram_mib"])
        self.assertEqual(fields["ram_unavailable_reason"], "memory_not_collected")

    def test_background_polling_reads_real_test_process_memory(self):
        import time
        with patch.object(memory, "find_target", return_value=(os.getpid(), None)):
            monitor = memory.MemoryMonitor("http://localhost:8080", 0.1)
        # Attribute only this test process; never launch a model or query a GPU.
        monitor.cpu_reason = "cpu_only_server"
        with monitor:
            start = time.perf_counter_ns()
            threading.Event().wait(0.35)
            end = time.perf_counter_ns()
        self.assertFalse(monitor.thread.is_alive())
        row = monitor.request_fields({"request_start_perf_ns": start, "completion_perf_ns": end})
        self.assertGreater(row["peak_ram_mib"], 0)
        self.assertGreater(row["ram_sample_count"], 0)
        self.assertEqual(row["memory_server_pid"], os.getpid())
        self.assertEqual(row["vram_unavailable_reason"], "cpu_only_server")


if __name__ == "__main__":
    unittest.main()
