"""MEAS-02 provenance and comparison tests using synthetic records."""

import contextlib
import hashlib
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

from benchmarks import compare, latency, prepare, run, runtime
from test_latency import pair, write_jsonl


def make_comparison(root):
    source = root / "source.json"
    data = {"prompts": [{"id": f"p{index}", "prompt": f"Synthetic engineering input {index}."}
                        for index in range(3)],
            "test_config": {"repetitions": 3, "concurrency": 1},
            "test_cases": [{"id": f"p{index}_{cap}", "prompt_id": f"p{index}", "max_tokens": cap}
                           for index in range(3) for cap in (64, 256)]}
    source.write_text(json.dumps(data))
    workload = prepare.load_workload(source)
    output = root / "run"
    output.mkdir()
    manifest = prepare.write_prepared(workload, output / "inputs")
    controls = {"process": {"pid": 42}, "model": {"sha256": "synthetic"},
                "build_info": "SYNTHETIC TEST DATA"}
    point = {"status": "available", "controls": controls, "fingerprint": runtime.digest(controls)}
    (output / "runtime.json").write_text(json.dumps(point))
    cases = []
    for spec in manifest["cases"]:
        directory = output / spec["id"]
        directory.mkdir()
        index = int(spec["prompt_id"][1:])
        total, cap = 17 + index * 100, spec["max_tokens"]
        records = []
        for repetition in range(4):
            raw, native = pair(f"{spec['id']}-{repetition}",
                               "warmup" if repetition == 0 else "profiling")
            native["metrics"]["usage_prompt_tokens"]["value"] = total
            native["metrics"]["usage_completion_tokens"]["value"] = cap
            raw["responses"][-2]["packets"][0]["value"] = json.dumps({
                "choices": [], "timings": {"prompt_n": total, "cache_n": 0,
                                           "predicted_n": cap, "prompt_ms": total / 2,
                                           "predicted_ms": cap}})
            records.append(latency.request_metrics(raw, native))
        write_jsonl(directory / "request_metrics.jsonl", records)
        runtime.save_snapshot(directory, point, point)
        cases.append({"id": spec["id"], "status": "completed"})
    record = {"schema_version": 1, "status": "completed", "cases": cases,
              "source_sha256": manifest["source_sha256"], "warmup_requests_per_case": 1,
              "settings": {"prompt_cache": "off", "temperature": 0}}
    (output / "run.json").write_text(json.dumps(record))
    return output


class RuntimeTests(unittest.TestCase):
    def test_only_allowlisted_launch_options_are_recorded(self):
        options = runtime.launch_options(["server", "--model=model.gguf", "--threads", "6",
                                          "--api-key", "DO_NOT_RECORD", "--port", "8080"])
        self.assertEqual(options["--threads"], "6")
        self.assertEqual(options["--model"], "model.gguf")
        self.assertNotIn("DO_NOT_RECORD", json.dumps(options))
        self.assertNotIn("--api-key", options)

    def test_missing_server_is_explicit_and_never_queries_properties(self):
        with patch.object(runtime.memory, "find_target", return_value=(None, "project_server_not_found")), \
             patch.object(runtime.requests, "Session") as session:
            value = runtime.RuntimeRecorder("http://localhost:8080").capture()
        self.assertEqual(value["unavailable_reason"], "project_server_not_found")
        session.assert_not_called()

    def test_file_fingerprints_are_reused_and_invalidated(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "model"
            path.write_bytes(b"synthetic")
            recorder = runtime.RuntimeRecorder("http://localhost:8080")
            with patch.object(runtime.hashlib, "file_digest", wraps=hashlib.file_digest) as hash_file:
                first = recorder.fingerprint_file(path)
                self.assertEqual(first, recorder.fingerprint_file(path))
                self.assertEqual(hash_file.call_count, 1)
                path.write_bytes(b"changed synthetic model")
                second = recorder.fingerprint_file(path)
                self.assertEqual(hash_file.call_count, 2)
                self.assertNotEqual(first["sha256"], second["sha256"])

    def test_real_proc_identity_and_safe_mock_properties(self):
        with tempfile.TemporaryDirectory() as temporary:
            model = Path(temporary) / "model.gguf"
            model.write_bytes(b"SYNTHETIC MODEL")
            props = {"model_path": str(model), "model_alias": "synthetic", "model_ftype": "synthetic",
                     "build_info": "synthetic", "total_slots": 1, "chat_template": "template",
                     "default_generation_settings": {"n_ctx": 4096, "params": {
                         "temperature": 0.8, "top_p": 0.95, "generation_prompt": "DO_NOT_RECORD"}}}
            response = Mock()
            response.json.return_value = props
            with patch.object(runtime.memory, "find_target", return_value=(os.getpid(), None)), \
                 patch.object(runtime.requests, "Session") as session:
                session.return_value.__enter__.return_value.get.return_value = response
                recorder = runtime.RuntimeRecorder("http://localhost:8080")
                first, second = recorder.capture(), recorder.capture()
            self.assertEqual(first["status"], "available")
            self.assertEqual(first["fingerprint"], second["fingerprint"])
            self.assertNotIn("DO_NOT_RECORD", json.dumps(first))
            self.assertEqual(first["controls"]["context_size"], 4096)

    def test_snapshot_rejects_changed_or_missing_controls(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary)
            first_result = {"status": "available", "fingerprint": "a"}
            second_result = {"status": "available", "fingerprint": "b"}
            self.assertFalse(runtime.save_snapshot(path, first_result, second_result)["unchanged"])
            original = (path / "runtime_snapshot.json").read_bytes()
            with self.assertRaises(FileExistsError):
                runtime.save_snapshot(path, first_result, first_result)
            self.assertEqual((path / "runtime_snapshot.json").read_bytes(), original)


class ComparisonTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = make_comparison(Path(temporary.name))

    def change(self, path, update):
        value = json.loads(path.read_text())
        update(value)
        path.write_text(json.dumps(value))

    def test_complete_matrix_selects_controlled_groups(self):
        data = compare.analyze(self.root)
        self.assertTrue(data["meas_02_complete"], data["gaps"])
        self.assertEqual(data["input_comparison"], ["p0_64", "p1_64", "p2_64"])
        self.assertEqual(data["output_comparison"], ["p0_64", "p0_256"])
        self.assertEqual(data["cases"][0]["requests"], 3)

    def test_exports_json_csv_and_observed_writeup_without_modifying_sources(self):
        before = {path: path.read_bytes() for path in self.root.rglob("*") if path.is_file()}
        data = compare.export_comparison(self.root)
        text = (self.root / "comparison.md").read_text()
        self.assertIn("Acceptance: complete", text)
        self.assertIn("Input-length comparison", text)
        self.assertIn("Output-length comparison", text)
        self.assertIn("not pure prefill", text)
        self.assertIn("per-case repetitions", text)
        self.assertTrue((self.root / "comparison.csv").is_file())
        for path, content in before.items():
            self.assertEqual(path.read_bytes(), content)
        with self.assertRaises(FileExistsError):
            compare.export_comparison(self.root)

    def test_runtime_change_prevents_completion(self):
        path = self.root / "p1_64/runtime_snapshot.json"
        self.change(path, lambda item: item["after"].update(fingerprint="different"))
        data = compare.analyze(self.root)
        self.assertFalse(data["meas_02_complete"])
        self.assertTrue(any("runtime settings" in gap for gap in data["gaps"]))

    def test_runtime_reference_tampering_is_detected(self):
        self.change(self.root / "runtime.json", lambda value: value["controls"].update(changed=True))
        self.assertFalse(compare.analyze(self.root)["meas_02_complete"])

    def test_cache_hits_and_missing_counters_prevent_completion(self):
        path = self.root / "p0_64/request_metrics.jsonl"
        rows = list(latency.read_jsonl(path))
        rows[1].update(cached_input_tokens=16, backend_prompt_tokens=1)
        rows[2].update(backend_decode_ms=None)
        write_jsonl(path, rows)
        data = compare.analyze(self.root)
        self.assertFalse(data["meas_02_complete"])
        self.assertTrue(any("uncached" in gap for gap in data["gaps"]))
        self.assertTrue(any("backend_decode_ms" in gap for gap in data["gaps"]))

    def test_missing_runtime_does_not_invent_configuration(self):
        (self.root / "runtime.json").unlink()
        data = compare.analyze(self.root)
        self.assertFalse(data["meas_02_complete"])
        self.assertTrue(any("configuration" in gap for gap in data["gaps"]))

    def test_wrong_concurrency_policy_and_repetitions_prevent_completion(self):
        self.change(self.root / "run.json", lambda value: value["settings"].update(prompt_cache="reuse"))
        self.change(self.root / "inputs/manifest.json", lambda value: value.update(concurrency=2, repetitions=1))
        data = compare.analyze(self.root)
        self.assertFalse(data["meas_02_complete"])
        self.assertGreaterEqual(len(data["gaps"]), 3)

    def test_missing_warmup_and_failed_request_prevent_completion(self):
        path = self.root / "p0_64/request_metrics.jsonl"
        rows = list(latency.read_jsonl(path))[1:]
        rows[0].update(status="error", e2e_latency_ms=None)
        write_jsonl(path, rows)
        self.assertFalse(compare.analyze(self.root)["meas_02_complete"])

    def test_path_traversal_is_rejected(self):
        self.change(self.root / "run.json", lambda value: value["cases"][0].update(id="../escape"))
        with self.assertRaises(ValueError):
            compare.analyze(self.root)

    def test_missing_comparison_groups_are_reported(self):
        manifest = json.loads((self.root / "inputs/manifest.json").read_text())
        manifest["cases"] = manifest["cases"][:1]
        (self.root / "inputs/manifest.json").write_text(json.dumps(manifest))
        self.change(self.root / "run.json", lambda value: value.update(cases=value["cases"][:1]))
        data = compare.analyze(self.root)
        self.assertFalse(data["meas_02_complete"])
        self.assertFalse(data["input_comparison"])
        self.assertFalse(data["output_comparison"])

    def test_compare_preflight_rejects_invalid_controls_without_startup(self):
        for flag in (["--concurrency", "2"], ["--repetitions", "2"],
                     ["--warmup-requests", "0"], ["--prompt-cache", "reuse"]):
            with contextlib.redirect_stderr(io.StringIO()), patch.object(run.subprocess, "run") as execute:
                self.assertEqual(run.main(["--compare", *flag]), 1)
            execute.assert_not_called()


if __name__ == "__main__":
    unittest.main()
