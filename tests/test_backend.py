"""Backend phase/caching checks; synthetic records only."""

import copy
import json
import unittest

from benchmarks import backend, latency
from test_latency import event, pair


class BackendTests(unittest.TestCase):
    def row(self, timings=None, phase="profiling"):
        raw, native = pair(phase=phase)
        if timings is not None:
            raw["responses"][-2]["packets"][0]["value"] = json.dumps(
                {"choices": [], "timings": timings})
        return latency.request_metrics(raw, native)

    def test_extracts_backend_values_without_using_client_ttft(self):
        row = self.row()
        self.assertEqual(row["backend_prompt_ms"], 80)
        self.assertEqual(row["backend_decode_ms"], 100)
        self.assertEqual(row["ttft_ms"], 100)
        self.assertEqual(row["cached_input_tokens"], 0)
        self.assertEqual(row["backend_prompt_tokens"], 17)
        self.assertEqual(row["backend_output_tokens"], 2)

    def test_cached_prefix_is_distinct_from_total_input_usage(self):
        row = self.row({"cache_n": 16, "prompt_n": 1, "prompt_ms": 5,
                        "predicted_n": 2, "predicted_ms": 100})
        self.assertEqual(row["input_tokens"], 17)
        self.assertEqual(row["backend_prompt_tokens"], 1)
        self.assertEqual(row["cached_input_tokens"], 16)
        summary = latency.summarize([row])
        self.assertEqual(summary["cache_verification"]["cached_requests"], 1)

    def test_invalid_values_have_explicit_reasons_and_cannot_verify_off(self):
        for value in (-1, True, 0.5, float("nan"), "0"):
            with self.subTest(value=value):
                row = self.row({"cache_n": value, "prompt_n": 17, "prompt_ms": -1})
                self.assertIsNone(row["cached_input_tokens"])
                self.assertEqual(row["cached_input_tokens_unavailable_reason"], "invalid_backend_counter")
                self.assertIsNone(row["backend_prompt_ms"])
                self.assertEqual(latency.summarize([row])["cache_verification"]["unverified_requests"], 1)

    def test_missing_counters_are_not_zero(self):
        row = self.row({})
        self.assertIsNone(row["cached_input_tokens"])
        self.assertEqual(row["cached_input_tokens_unavailable_reason"], "backend_counter_not_reported")

    def test_mismatched_input_accounting_cannot_verify_uncached(self):
        row = self.row({"cache_n": 0, "prompt_n": 1})
        self.assertIsNone(row["cached_input_tokens"])
        self.assertEqual(row["cached_input_tokens_unavailable_reason"], "backend_input_count_mismatch")

    def test_integral_usage_float_is_supported(self):
        raw, native = pair()
        native["metrics"]["usage_prompt_tokens"]["value"] = 17.0
        row = latency.request_metrics(raw, native)
        self.assertEqual(latency.summarize([row])["cache_verification"]["uncached_requests"], 1)

    def test_failed_requests_do_not_contribute_backend_statistics(self):
        raw, native = pair()
        native["error"] = {"message": "failed"}
        row = latency.request_metrics(raw, native)
        self.assertIsNone(row["backend_prompt_ms"])
        self.assertEqual(row["backend_prompt_ms_unavailable_reason"], "request_not_successful")
        summary = latency.summarize([row])
        self.assertEqual(summary["backend_metrics"]["backend_prompt_ms"]["count"], 0)
        self.assertEqual(summary["cache_verification"]["successful_requests"], 0)

    def test_warmups_excluded_and_units_correct(self):
        measured = self.row()
        warmup = self.row({"cache_n": 16, "prompt_n": 1, "prompt_ms": 9999}, "warmup")
        warmup["request_id"] = "warmup"
        summary = latency.summarize([measured, warmup])
        self.assertEqual(summary["backend_metrics"]["backend_prompt_ms"]["p50"], 80)
        self.assertEqual(summary["backend_metrics"]["cached_input_tokens"]["unit"], "tokens")
        self.assertEqual(summary["backend_metrics"]["backend_decode_ms"]["unit"], "ms")
        self.assertEqual(summary["cache_verification"]["uncached_requests"], 1)
        self.assertEqual(summary["cache_verification"]["cached_requests"], 0)

    def test_conflicting_timings_are_not_silently_selected(self):
        raw, native = pair()
        raw["responses"].insert(-1, event(400, {"choices": [], "timings": {"cache_n": 3}}))
        row = latency.request_metrics(raw, native)
        self.assertIsNone(row["cached_input_tokens"])
        self.assertEqual(row["cached_input_tokens_unavailable_reason"], "conflicting_backend_timings")

    def test_invalid_timings_object(self):
        raw, native = pair()
        raw["responses"].insert(-1, event(400, {"choices": [], "timings": None}))
        row = latency.request_metrics(raw, native)
        self.assertEqual(row["cached_input_tokens_unavailable_reason"], "invalid_backend_timings")

    def test_post_done_events_do_not_change_backend_counters_or_sources(self):
        raw, native = pair()
        raw["responses"].append(event(600, {"choices": [], "timings": {"cache_n": 4}}))
        original = copy.deepcopy(raw)
        row = latency.request_metrics(raw, native)
        self.assertEqual(row["cached_input_tokens"], 0)
        self.assertEqual(raw, original)

    def test_partial_backend_coverage_is_counted(self):
        row = self.row()
        missing = self.row({})
        missing["request_id"] = "missing"
        summary = latency.summarize([row, missing])
        self.assertEqual(summary["backend_metrics"]["backend_prompt_ms"]["count"], 1)
        self.assertEqual(summary["cache_verification"]["unverified_requests"], 1)

    def test_jsonl_iterator_and_list_summaries_are_identical(self):
        rows = [self.row()]
        self.assertEqual(latency.summarize(iter(rows)), latency.summarize(rows))


if __name__ == "__main__":
    unittest.main()
