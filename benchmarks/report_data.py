"""Load and validate saved benchmark artifacts for report generation.

This module reads existing results only; it never starts a model or a benchmark.
"""

import json
import math
from pathlib import Path

from .prepare import identifier
from .results import request_outcomes
from .latency import read_jsonl, summarize

METRICS = (
    ("time_to_first_token", "Time to first token"),
    ("request_latency", "AIPerf last-content latency"),
    ("inter_token_latency", "Inter-token latency"),
    ("request_throughput", "Request throughput"),
    ("output_token_throughput", "Output token throughput"),
    ("input_sequence_length", "Input tokens"),
    ("output_sequence_length", "Output tokens"),
    ("request_count", "Request count"),
    ("error_request_count", "Error requests"),
)
STATS = ("avg", "p50", "p95", "p99")


def read_object(path):
    value = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(value, dict):
        raise ValueError(f"Expected a JSON object in {path}")
    return value


def contained_path(root, relative):
    path = (root / relative).resolve()
    if not path.is_relative_to(root):
        raise ValueError(f"Report input escapes the run directory: {relative}")
    return path


def metric_rows(summary):
    """Use native top-level profiling aggregates, never warmup_metrics."""
    rows = []
    for tag, label in METRICS:
        metric = summary.get(tag)
        if metric is None:
            metric = {}
        if not isinstance(metric, dict):
            raise ValueError(f"Invalid AIPerf metric: {tag}")
        rows.append([label, metric.get("unit", "N/A"), *(metric.get(stat) for stat in STATS)])
    return rows


def memory_rows(summary):
    """Case peaks are scalar observations, never request percentiles."""
    rows = []
    for name, label in (("ram", "Server RSS"), ("vram", "Server VRAM")):
        metric = (summary or {}).get(name, {})
        if not isinstance(metric, dict):
            raise ValueError("Invalid memory counter summary")
        for field in ("baseline_mib", "peak_mib"):
            value = metric.get(field)
            if value is not None and (
                type(value) not in (int, float) or not math.isfinite(value) or value < 0
            ):
                raise ValueError("Invalid memory value")
        status = metric.get("status", "unavailable")
        if status not in {"available", "partial", "unavailable"}:
            raise ValueError("Invalid memory status")
        if status == "unavailable" and metric.get("peak_mib") is not None:
            raise ValueError("Unavailable memory must have a null peak")
        rows.append(
            [
                label,
                metric.get("baseline_mib"),
                metric.get("peak_mib"),
                metric.get("sample_count"),
                status,
                metric.get("unavailable_reason")
                or (
                    ", ".join(metric.get("unavailable_reasons", {}))
                    if status == "partial"
                    else None
                ),
            ]
        )
    return rows


def load_report(run_directory):
    root = Path(run_directory).resolve()
    run = read_object(contained_path(root, "run.json"))
    if run.get("schema_version") != 1 or not isinstance(run.get("settings"), dict):
        raise ValueError("Unsupported or invalid run.json")
    if run.get("status") in {"dry_run", "prepared", "starting_server"}:
        raise ValueError("This run has no benchmark results to export")
    if not isinstance(run.get("cases"), list) or not run["cases"]:
        raise ValueError("run.json must contain cases")

    manifest_path = contained_path(root, "inputs/manifest.json")
    manifest = read_object(manifest_path) if manifest_path.is_file() else {}
    specs = {}
    for spec in manifest.get("cases", []):
        if not isinstance(spec, dict):
            raise ValueError("Invalid case in inputs/manifest.json")
        key = identifier(spec.get("id"), "Manifest case id")
        if key in specs:
            raise ValueError(f"Duplicate manifest case: {key}")
        specs[key] = spec
    if (
        manifest.get("source_sha256")
        and run.get("source_sha256")
        and manifest["source_sha256"] != run["source_sha256"]
    ):
        raise ValueError("Run and input manifest source hashes disagree")

    cases = []
    seen = set()
    has_results = False
    for case in run["cases"]:
        if not isinstance(case, dict):
            raise ValueError("Invalid case in run.json")
        key = identifier(case.get("id"), "Case id")
        if key in seen:
            raise ValueError(f"Duplicate run case: {key}")
        seen.add(key)
        summary_path = contained_path(root, f"{key}/profile_export_aiperf.json")
        records_path = contained_path(root, f"{key}/profile_export.jsonl")
        summary = read_object(summary_path) if summary_path.is_file() else {}
        native_outcomes = request_outcomes(records_path) if records_path.is_file() else None
        completion_path = contained_path(root, f"{key}/request_metrics.jsonl")
        completion = (
            summarize(read_jsonl(completion_path)) if completion_path.is_file() else None
        )
        memory_path = contained_path(root, f"{key}/memory_summary.json")
        memory = read_object(memory_path) if memory_path.is_file() else None
        if memory is not None and (
            memory.get("schema_version") != 1
            or memory.get("scope") != "server_process_case"
            or memory.get("unit") != "MiB"
            or not all(
                isinstance(memory.get(field_name), dict) for field_name in ("ram", "vram")
            )
        ):
            raise ValueError("Invalid memory summary")
        outcomes = completion["outcomes"] if completion else native_outcomes
        has_results |= summary_path.is_file() or records_path.is_file()
        warnings = []
        if completion is None:
            warnings.append(
                "Completion-verified timings unavailable; first-text and E2E metrics are N/A."
            )
        elif completion["outcomes"]["errors"]:
            warnings.append(
                "Completion verification failed for measured requests; inspect request_metrics.jsonl."
            )
        if not summary_path.is_file():
            warnings.append("Aggregate JSON unavailable; metric values are N/A.")
        elif not any(isinstance(summary.get(tag), dict) for tag, _ in METRICS):
            raise ValueError(f"No recognized AIPerf metrics in {summary_path}")
        if outcomes is None:
            warnings.append("Per-request JSONL unavailable; request coverage is unverified.")
        else:
            expected = specs.get(key, {}).get("request_count")
            if expected is not None and outcomes["measured"] != expected:
                warnings.append(
                    f"Measured record count {outcomes['measured']} differs from expected {expected}."
                )
            if outcomes["errors"]:
                warnings.append(
                    "Failed, cancelled or context-skipped measured requests are present."
                )
            if outcomes["warmup_errors"]:
                warnings.append("Warm-up errors are present; inspect the raw records.")
        if summary.get("is_complete") is False or summary.get("was_cancelled"):
            warnings.append("AIPerf marked these results incomplete or cancelled.")
        if case.get("status") != "completed":
            warnings.append("This case was not marked completed; results may be partial.")
        completion_rows = [
            [
                label,
                "ms",
                *(completion[metric].get(stat) if completion else None for stat in STATS),
            ]
            for metric, label in (
                ("text_ttft", "First generated text"),
                ("e2e_latency", "Completion E2E latency"),
            )
        ]
        cases.append(
            {
                "id": key,
                "status": case.get("status", "unknown"),
                "spec": specs.get(key, {}),
                "outcomes": outcomes,
                "completion": completion,
                "backend": completion["backend_metrics"] if completion else {},
                "cache_verification": completion["cache_verification"] if completion else None,
                "memory": memory,
                "memory_rows": memory_rows(memory),
                "rows": completion_rows + metric_rows(summary),
                "warnings": warnings,
            }
        )
    if not has_results:
        raise ValueError("No saved AIPerf results found; run a benchmark first")
    return {"root": root, "run": run, "manifest": manifest, "cases": cases}
