"""Completion-verified metrics from AIPerf 0.13.0 raw SSE exports.

No requests or clocks are started here: all durations use the original transport's
perf_counter_ns timestamps. Native AIPerf artifacts are never modified.
"""

from collections import Counter
import csv
import json
import math

from .memory import uncollected_fields
from .backend import extract as extract_backend, summarize_backend


def read_jsonl(path):
    with path.open(encoding="utf-8") as stream:
        for number, line in enumerate(stream, 1):
            if not line.strip():
                continue
            row = json.loads(line)
            if not isinstance(row, dict):
                raise ValueError(f"Expected an object at {path}:{number}")
            yield row


def identity(row):
    metadata = row.get("metadata")
    if not isinstance(metadata, dict):
        raise ValueError("AIPerf record has no metadata")
    key = metadata.get("x_request_id")
    if not isinstance(key, str) or not key:
        raise ValueError("AIPerf record has no unique x_request_id")
    if metadata.get("benchmark_phase") not in {"profiling", "warmup"}:
        raise ValueError("Unexpected AIPerf benchmark phase")
    return key


def finite_number(value):
    return type(value) in (int, float) and math.isfinite(value) and value >= 0


def native_metric(record, name, unit):
    metric = (record or {}).get("metrics", {}).get(name, {})
    value = metric.get("value")
    return value if metric.get("unit") == unit and finite_number(value) else None


def stream_timing(raw):
    """Return observed text/DONE timestamps, rejecting malformed timing/streams."""
    start = raw.get("start_perf_ns")
    if type(start) is not int or start < 0:
        return None, None, "invalid_timing"
    previous, first, done = start, None, None
    responses = raw.get("responses")
    if not isinstance(responses, list):
        return None, None, "invalid_stream"
    for message in responses:
        if not isinstance(message, dict) or not isinstance(message.get("packets"), list):
            return first, done, "invalid_stream"
        timestamp = message.get("perf_ns")
        if type(timestamp) is not int or timestamp < previous:
            return first, done, "invalid_timing"
        previous = timestamp
        data = []
        for packet in message["packets"]:
            if not isinstance(packet, dict):
                return first, done, "invalid_stream"
            if packet.get("name") == "event" and packet.get("value") == "error":
                return first, done, "stream_error"
            if packet.get("name") == "data":
                value = packet.get("value")
                if not isinstance(value, str):
                    return first, done, "invalid_stream"
                data.append(value)
        payload = "\n".join(data).strip()
        if not payload:  # SSE comments/heartbeats are not generated text.
            continue
        if payload == "[DONE]":
            return first, timestamp, None
        try:
            event = json.loads(payload)
        except ValueError:
            return first, done, "invalid_stream"
        if not isinstance(event, dict):
            return first, done, "invalid_stream"
        if event.get("error"):
            return first, done, "stream_error"
        choices = event.get("choices", [])
        if not isinstance(choices, list):
            return first, done, "invalid_stream"
        for choice in choices:
            if not isinstance(choice, dict) or not isinstance(choice.get("delta", {}), dict):
                return first, done, "invalid_stream"
            content = choice.get("delta", {}).get("content")
            if isinstance(content, str) and content and first is None:
                first = timestamp
    return first, done, "incomplete_stream"


def request_metrics(raw, native):
    source = raw if raw is not None else native
    key = identity(source)
    metadata = source["metadata"]
    if raw is not None and native is not None:
        if identity(native) != key or any(
            native["metadata"].get(field) != metadata.get(field)
            for field in ("benchmark_phase", "conversation_id")
        ):
            raise ValueError(f"Raw and metric identities disagree for {key}")
    start = (raw or {}).get("start_perf_ns")
    first, done, problem = (
        stream_timing(raw) if raw is not None else (None, None, "missing_raw")
    )
    metadata_sources = [row["metadata"] for row in (raw, native) if row is not None]
    http_status = (raw or {}).get("status")
    if any(meta.get("was_cancelled") for meta in metadata_sources):
        status = "cancelled"
    elif any(meta.get("context_overflow_skip") for meta in metadata_sources):
        status = "context_skipped"
    elif any(row.get("error") for row in (raw, native) if row is not None):
        status = "error"
    elif raw is None:
        status = "missing_raw"
    elif native is None:
        status = "missing_metrics"
    elif type(http_status) is not int or not 200 <= http_status < 300:
        status = "http_error"
    else:
        status = problem or "success"
    ttft = (first - start) / 1_000_000 if first is not None else None
    result = {
        "schema_version": 1,
        "request_id": key,
        "conversation_id": metadata.get("conversation_id"),
        "benchmark_phase": metadata["benchmark_phase"],
        "status": status,
        "http_status": http_status,
        "request_start_perf_ns": start,
        "first_text_perf_ns": first,
        "completion_perf_ns": done,
        "completion_received": done is not None,
        "ttft_ms": ttft,
        "ttft_unavailable_reason": (
            None if ttft is not None else (problem or "no_generated_text")
        ),
        "e2e_latency_ms": (done - start) / 1_000_000 if status == "success" else None,
        "e2e_unavailable_reason": None if status == "success" else status,
        "aiperf_request_latency_ms": native_metric(native, "request_latency", "ms"),
        # Read backend usage only, never estimate from the builtin tokenizer.
        "input_tokens": native_metric(native, "usage_prompt_tokens", "tokens"),
        "output_tokens": native_metric(native, "usage_completion_tokens", "tokens"),
        "input_tokens_unavailable_reason": (
            None
            if native_metric(native, "usage_prompt_tokens", "tokens") is not None
            else "server_usage_not_reported"
        ),
        "output_tokens_unavailable_reason": (
            None
            if native_metric(native, "usage_completion_tokens", "tokens") is not None
            else "server_usage_not_reported"
        ),
    }
    result.update(extract_backend(raw, status, input_tokens=result["input_tokens"]))
    return result


def derive_records(directory):
    """Join by x_request_id, not file order or repeated warm-up conversation IDs."""
    native = {}
    metric_path = directory / "profile_export.jsonl"
    metric_rows = read_jsonl(metric_path) if metric_path.is_file() else ()
    for row in metric_rows:
        key = identity(row)
        if key in native:
            raise ValueError(f"Duplicate metric request ID: {key}")
        native[key] = row
    seen, rows = set(), []
    raw_path = directory / "profile_export_raw.jsonl"
    # AIPerf can exit on all-failed runs before aggregating worker raw files.
    raw_paths = (
        [raw_path]
        if raw_path.is_file()
        else sorted((directory / "raw_records").glob("raw_records_*.jsonl"))
    )
    # Stream large raw records; retain only the small derived records.
    for path in raw_paths:
        for raw in read_jsonl(path):
            key = identity(raw)
            if key in seen:
                raise ValueError(f"Duplicate raw request ID: {key}")
            seen.add(key)
            rows.append(request_metrics(raw, native.pop(key, None)))
    rows.extend(request_metrics(None, row) for row in native.values())
    if not rows:
        raise ValueError("AIPerf exports contain no requests")
    return rows


def statistics(values, *, unit="ms"):
    values = sorted(values)
    result = {
        "unit": unit,
        "count": len(values),
        "avg": None,
        "min": None,
        "max": None,
        "p50": None,
        "p95": None,
        "p99": None,
    }
    if values:
        result.update(avg=sum(values) / len(values), min=values[0], max=values[-1])
        for percentile in (50, 95, 99):
            index = (len(values) - 1) * percentile / 100
            low, high = math.floor(index), math.ceil(index)
            result[f"p{percentile}"] = values[low] + (values[high] - values[low]) * (
                index - low
            )
    return result


def summarize(rows):
    # PDF loading supplies a JSONL iterator; both summary passes need the rows.
    rows = list(rows)
    counts = {"measured": 0, "errors": 0, "warmup": 0, "warmup_errors": 0}
    statuses, values, first_text, seen = Counter(), [], [], set()
    for row in rows:
        key = row.get("request_id")
        if row.get("schema_version") != 1 or not isinstance(key, str) or not key or key in seen:
            raise ValueError("Invalid or duplicate completion-metric request")
        seen.add(key)
        phase, status = row.get("benchmark_phase"), row.get("status")
        if phase not in {"profiling", "warmup"} or not isinstance(status, str) or not status:
            raise ValueError("Invalid completion-metric phase or status")
        value = row.get("e2e_latency_ms")
        if status == "success" and not finite_number(value):
            raise ValueError("Successful request has no valid completion latency")
        if status == "success" and row.get("completion_received") is not True:
            raise ValueError("Successful request has no verified completion marker")
        if status != "success" and value is not None:
            raise ValueError("Unsuccessful request must not have a completion latency")
        if phase == "warmup":
            counts["warmup"] += 1
            counts["warmup_errors"] += status != "success"
            continue
        counts["measured"] += 1
        counts["errors"] += status != "success"
        statuses[status] += 1
        if status == "success":
            values.append(value)
            if finite_number(row.get("ttft_ms")):
                first_text.append(row["ttft_ms"])
    backend, verification = summarize_backend(rows, statistics)
    return {
        "schema_version": 1,
        "clock": "AIPerf transport perf_counter_ns",
        "completion_boundary": "first SSE data: [DONE]",
        "percentile_method": "linear interpolation at (n - 1) * p",
        "outcomes": counts,
        "measured_status_counts": dict(statuses),
        "e2e_latency": statistics(values),
        "text_ttft": statistics(first_text),
        "backend_metrics": backend,
        "cache_verification": verification,
    }


def export_request_metrics(directory, *, memory=None):
    rows = derive_records(directory)
    for row in rows:
        row.update(memory.request_fields(row) if memory is not None else uncollected_fields())
    summary = summarize(rows)
    paths = [
        directory / name
        for name in ("request_metrics.jsonl", "request_metrics.csv", "latency_summary.json")
    ]
    if any(path.exists() for path in paths):
        raise FileExistsError("Refusing to overwrite completion-metric exports")
    with paths[0].open("x", encoding="utf-8") as stream:
        for row in rows:
            stream.write(json.dumps(row, allow_nan=False) + "\n")
    with paths[1].open("x", encoding="utf-8", newline="") as stream:
        writer = csv.DictWriter(stream, fieldnames=list(rows[0]))
        writer.writeheader()
        writer.writerows(rows)
    with paths[2].open("x", encoding="utf-8") as stream:
        json.dump(summary, stream, indent=2, allow_nan=False)
        stream.write("\n")
    return summary
