"""Completion-boundary tests using AIPerf's serialized raw SSE structure."""

import copy
import csv
import json
from pathlib import Path
import tempfile
import unittest

from benchmarks import latency


def event(offset_ms, payload):
    return {"perf_ns": 2_000_000_000 + int(offset_ms * 1_000_000),
            "packets": [{"name": "data", "value": payload if isinstance(payload, str)
                          else json.dumps(payload)}]}


def pair(key="request-1", phase="profiling"):
    metadata = {"x_request_id": key, "conversation_id": "same-case-r1",
                "benchmark_phase": phase, "was_cancelled": False}
    raw = {"metadata": copy.deepcopy(metadata), "start_perf_ns": 2_000_000_000,
           "status": 200, "responses": [
               event(20, {"choices": [{"delta": {"role": "assistant"}}]}),
               event(40, {"choices": [{"delta": {"content": ""}}]}),
               event(100, {"choices": [{"delta": {"content": "Engineering"}}]}),
               event(200, {"choices": [{"delta": {"content": " test"}}]}),
               event(300, {"choices": [], "usage": {"prompt_tokens": 17, "completion_tokens": 2},
                           "timings": {"cache_n": 0, "prompt_n": 17, "prompt_ms": 80,
                                       "predicted_n": 2, "predicted_ms": 100}}),
               event(500, "[DONE]"),
           ]}
    native = {"metadata": copy.deepcopy(metadata), "metrics": {
        "request_latency": {"value": 200, "unit": "ms"},
        "usage_prompt_tokens": {"value": 17, "unit": "tokens"},
        "usage_completion_tokens": {"value": 2, "unit": "tokens"},
    }}
    return raw, native


def write_jsonl(path, rows):
    path.write_text("".join(json.dumps(row) + "\n" for row in rows), encoding="utf-8")


class CompletionLatencyTests(unittest.TestCase):
    def test_done_delay_is_included_and_native_latency_is_preserved(self):
        raw, native = pair()
        row = latency.request_metrics(raw, native)
        self.assertEqual(row["ttft_ms"], 100)
        self.assertEqual(row["e2e_latency_ms"], 500)
        self.assertEqual(row["aiperf_request_latency_ms"], 200)
        self.assertEqual(row["status"], "success")
        self.assertTrue(row["completion_received"])
        self.assertEqual(row["input_tokens"], 17)
        self.assertEqual(row["output_tokens"], 2)

    def test_missing_done_and_finish_reason_are_not_successful_completion(self):
        raw, native = pair()
        raw["responses"] = raw["responses"][:-1]
        raw["responses"].append(event(400, {"choices": [{"delta": {}, "finish_reason": "stop"}]}))
        row = latency.request_metrics(raw, native)
        self.assertEqual(row["status"], "incomplete_stream")
        self.assertFalse(row["completion_received"])
        self.assertIsNone(row["e2e_latency_ms"])
        self.assertEqual(row["e2e_unavailable_reason"], "incomplete_stream")
        self.assertEqual(row["ttft_ms"], 100)

    def test_empty_output_has_unavailable_ttft(self):
        raw, native = pair()
        raw["responses"] = [event(20, {"choices": [{"delta": {"role": "assistant"}}]}),
                            event(100, "[DONE]")]
        row = latency.request_metrics(raw, native)
        self.assertIsNone(row["ttft_ms"])
        self.assertEqual(row["ttft_unavailable_reason"], "no_generated_text")
        self.assertEqual(row["e2e_latency_ms"], 100)

    def test_whitespace_is_generated_text_but_role_reasoning_and_usage_are_not(self):
        raw, native = pair()
        raw["responses"] = [
            event(10, {"choices": [{"delta": {"reasoning_content": "thinking"}}]}),
            event(20, {"choices": []}),
            event(50, {"choices": [{"delta": {"content": " "}}]}), event(100, "[DONE]")]
        self.assertEqual(latency.request_metrics(raw, native)["ttft_ms"], 50)

    def test_http_errors_timeouts_and_cancellation_have_no_success_latency(self):
        for kind in ("http", "timeout", "cancelled", "native_error", "context"):
            with self.subTest(kind=kind):
                raw, native = pair()
                if kind == "http":
                    raw["status"] = 503
                elif kind == "timeout":
                    raw["error"] = {"type": "TimeoutError", "message": "request timed out"}
                elif kind == "cancelled":
                    native["metadata"]["was_cancelled"] = True
                elif kind == "native_error":
                    native["error"] = {"message": "parse error"}
                else:
                    native["metadata"]["context_overflow_skip"] = True
                row = latency.request_metrics(raw, native)
                self.assertNotEqual(row["status"], "success")
                self.assertIsNone(row["e2e_latency_ms"])

    def test_invalid_clock_values_are_rejected(self):
        for start in (None, -1, True, 1.5):
            with self.subTest(start=start):
                raw, native = pair()
                raw["start_perf_ns"] = start
                row = latency.request_metrics(raw, native)
                self.assertEqual(row["status"], "invalid_timing")
                self.assertIsNone(row["e2e_latency_ms"])
        raw, native = pair()
        raw["responses"][3]["perf_ns"] = raw["start_perf_ns"] - 1
        self.assertEqual(latency.request_metrics(raw, native)["status"], "invalid_timing")

    def test_malformed_events_and_explicit_stream_errors(self):
        for payload, expected in (("not JSON", "invalid_stream"),
                                  ({"error": {"message": "bad"}}, "stream_error")):
            raw, native = pair()
            raw["responses"] = [event(100, payload), event(200, "[DONE]")]
            self.assertEqual(latency.request_metrics(raw, native)["status"], expected)
        raw, native = pair()
        raw["responses"][0]["packets"] = [{"name": "event", "value": "error"}]
        self.assertEqual(latency.request_metrics(raw, native)["status"], "stream_error")

    def test_heartbeats_and_multiple_data_fields(self):
        raw, native = pair()
        raw["responses"].insert(0, {"perf_ns": raw["start_perf_ns"] + 1,
                                   "packets": [{"name": "comment", "value": "keepalive"}]})
        message = raw["responses"][3]
        message["packets"] = [{"name": "data", "value": '{"choices":'},
                              {"name": "data", "value": '[{"delta":{"content":"Hello"}}]}'}]
        self.assertEqual(latency.request_metrics(raw, native)["ttft_ms"], 100)

    def test_missing_usage_is_explicit_and_no_tokenizer_fallback_occurs(self):
        raw, native = pair()
        native["metrics"] = {"input_sequence_length": {"value": 999, "unit": "tokens"}}
        row = latency.request_metrics(raw, native)
        self.assertIsNone(row["input_tokens"])
        self.assertIsNone(row["output_tokens"])
        self.assertEqual(row["input_tokens_unavailable_reason"], "server_usage_not_reported")

    def test_missing_and_conflicting_pairs(self):
        raw, native = pair()
        self.assertEqual(latency.request_metrics(None, native)["status"], "missing_raw")
        self.assertEqual(latency.request_metrics(raw, None)["status"], "missing_metrics")
        native["metadata"]["conversation_id"] = "wrong"
        with self.assertRaises(ValueError):
            latency.request_metrics(raw, native)

    def test_warmups_and_failures_do_not_enter_success_percentiles(self):
        raw, native = pair()
        good = latency.request_metrics(raw, native)
        warm_raw, warm_native = pair("warmup", "warmup")
        warm_raw["responses"][-1] = event(90_000, "[DONE]")
        warm = latency.request_metrics(warm_raw, warm_native)
        bad_raw, bad_native = pair("bad")
        bad_raw["responses"] = bad_raw["responses"][:-1]
        bad = latency.request_metrics(bad_raw, bad_native)
        summary = latency.summarize([warm, good, bad])
        self.assertEqual(summary["outcomes"], {"measured": 2, "errors": 1, "warmup": 1, "warmup_errors": 0})
        self.assertEqual(summary["e2e_latency"]["count"], 1)
        self.assertEqual(summary["e2e_latency"]["avg"], 500)
        self.assertEqual(summary["e2e_latency"]["p99"], 500)

    def test_summary_rejects_invalid_success_values_and_duplicate_ids(self):
        raw, native = pair()
        good = latency.request_metrics(raw, native)
        for value in (None, float("nan"), float("inf"), -1, True):
            row = dict(good, e2e_latency_ms=value)
            with self.subTest(value=value), self.assertRaises(ValueError):
                latency.summarize([row])
        with self.assertRaises(ValueError):
            latency.summarize([good, good])
        with self.assertRaisesRegex(ValueError, "completion marker"):
            latency.summarize([dict(good, completion_received=False)])

    def test_linear_percentiles_and_empty_samples(self):
        self.assertEqual(latency.statistics([])["avg"], None)
        values = latency.statistics([0, 100])
        self.assertEqual(values["p50"], 50)
        self.assertEqual(values["p95"], 95)
        self.assertEqual(values["p99"], 99)


class CompletionExportTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.raw1, self.native1 = pair("one")
        self.raw2, self.native2 = pair("two")
        self.raw2["responses"][-1] = event(1000, "[DONE]")
        write_jsonl(self.root / "profile_export.jsonl", [self.native1, self.native2])
        write_jsonl(self.root / "profile_export_raw.jsonl", [self.raw2, self.raw1])

    def test_join_uses_request_id_despite_different_order_and_same_conversation(self):
        rows = latency.derive_records(self.root)
        self.assertEqual({r["request_id"]: r["e2e_latency_ms"] for r in rows},
                         {"one": 500, "two": 1000})

    def test_orphan_records_are_retained_as_errors(self):
        write_jsonl(self.root / "profile_export_raw.jsonl", [self.raw1])
        rows = latency.derive_records(self.root)
        self.assertEqual(len(rows), 2)
        self.assertEqual(latency.summarize(rows)["outcomes"]["errors"], 1)

    def test_duplicate_ids_are_rejected(self):
        write_jsonl(self.root / "profile_export_raw.jsonl", [self.raw1, self.raw1])
        with self.assertRaisesRegex(ValueError, "Duplicate raw"):
            latency.derive_records(self.root)

    def test_worker_staging_files_are_used_when_aggregate_is_missing(self):
        original = self.root / "profile_export_raw.jsonl"
        staging = self.root / "raw_records"
        staging.mkdir()
        original.rename(staging / "raw_records_worker1.jsonl")
        rows = latency.derive_records(self.root)
        self.assertEqual({row["request_id"] for row in rows}, {"one", "two"})
        self.assertTrue((staging / "raw_records_worker1.jsonl").is_file())

    def test_absent_raw_file_keeps_every_native_outcome(self):
        (self.root / "profile_export_raw.jsonl").unlink()
        summary = latency.export_request_metrics(self.root)
        self.assertEqual(summary["outcomes"]["measured"], 2)
        self.assertEqual(summary["outcomes"]["errors"], 2)
        rows = list(latency.read_jsonl(self.root / "request_metrics.jsonl"))
        self.assertTrue(all(row["status"] == "missing_raw" for row in rows))
        self.assertTrue(all(row["e2e_latency_ms"] is None for row in rows))

    def test_exports_are_compact_preserve_sources_and_refuse_overwrite(self):
        sources = {p: p.read_bytes() for p in self.root.glob("profile*.jsonl")}
        self.raw1["payload"] = {"messages": [{"content": "PRIVATE PROMPT"}]}
        write_jsonl(self.root / "profile_export_raw.jsonl", [self.raw2, self.raw1])
        sources[self.root / "profile_export_raw.jsonl"] = (self.root / "profile_export_raw.jsonl").read_bytes()
        summary = latency.export_request_metrics(self.root)
        self.assertEqual(summary["e2e_latency"]["avg"], 750)
        text = (self.root / "request_metrics.jsonl").read_text()
        self.assertNotIn("PRIVATE PROMPT", text)
        self.assertNotIn("Engineering", text)
        with (self.root / "request_metrics.csv").open(newline="") as stream:
            rows = list(csv.DictReader(stream))
        self.assertEqual(len(rows), 2)
        self.assertEqual(rows[0]["completion_received"], "True")
        for path, original in sources.items():
            self.assertEqual(path.read_bytes(), original)
        with self.assertRaises(FileExistsError):
            latency.export_request_metrics(self.root)
