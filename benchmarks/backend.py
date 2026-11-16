"""llama.cpp phase timings and observed cross-request cache verification."""

import json
import math

FIELDS = {
    "backend_prompt_ms": ("prompt_ms", "ms", False),
    "backend_decode_ms": ("predicted_ms", "ms", False),
    "backend_prompt_tokens": ("prompt_n", "tokens", True),
    "cached_input_tokens": ("cache_n", "tokens", True),
    "backend_output_tokens": ("predicted_n", "tokens", True),
}


def valid(value, integer=False):
    return (type(value) is int if integer else type(value) in (int, float)) and (
        math.isfinite(value) and value >= 0
    )


def extract(raw, status, *, input_tokens=None):
    """Use the final SSE timings object before DONE; never infer from client latency."""
    timings, problem = None, None
    if status != "success":
        problem = "request_not_successful"
    else:
        for message in (raw or {}).get("responses", []):
            data = [
                packet.get("value", "")
                for packet in message.get("packets", [])
                if packet.get("name") == "data"
            ]
            payload = "\n".join(data).strip()
            if payload == "[DONE]":
                break
            if not payload:
                continue
            try:
                event = json.loads(payload)
            except ValueError:
                continue
            if not isinstance(event, dict) or "timings" not in event:
                continue
            candidate = event["timings"]
            if not isinstance(candidate, dict):
                problem = "invalid_backend_timings"
                break
            if timings is not None and candidate != timings:
                problem = "conflicting_backend_timings"
                break
            timings = candidate
    result = {}
    for name, (source, _, integer) in FIELDS.items():
        value = (timings or {}).get(source)
        reason = problem
        if reason is None and value is None:
            reason = "backend_counter_not_reported"
        elif reason is None and not valid(value, integer):
            reason = "invalid_backend_counter"
        result[name] = value if reason is None else None
        result[name + "_unavailable_reason"] = reason
    # Usage describes the whole prompt; prompt_n is only work newly evaluated.
    prompt, cached = result["backend_prompt_tokens"], result["cached_input_tokens"]
    if (
        prompt is not None
        and cached is not None
        and valid(input_tokens)
        and prompt + cached != input_tokens
    ):
        for name in ("backend_prompt_tokens", "cached_input_tokens"):
            result[name] = None
            result[name + "_unavailable_reason"] = "backend_input_count_mismatch"
    return result


def summarize_backend(rows, statistics):
    measured = [
        row
        for row in rows
        if row.get("benchmark_phase") == "profiling" and row.get("status") == "success"
    ]
    verification = {
        "successful_requests": len(measured),
        "uncached_requests": 0,
        "cached_requests": 0,
        "unverified_requests": 0,
    }
    for row in measured:
        cached, prompt, total = (
            row.get(key)
            for key in ("cached_input_tokens", "backend_prompt_tokens", "input_tokens")
        )
        if (
            not valid(cached, True)
            or not valid(prompt, True)
            or not valid(total)
            or cached + prompt != total
        ):
            verification["unverified_requests"] += 1
        elif cached == 0:
            verification["uncached_requests"] += 1
        else:
            verification["cached_requests"] += 1
    metrics = {}
    for name, (_, unit, integer) in FIELDS.items():
        values = [row[name] for row in measured if valid(row.get(name), integer)]
        metrics[name] = statistics(values, unit=unit)
    for name in ("input_tokens", "output_tokens"):
        metrics[name] = statistics(
            [row[name] for row in measured if valid(row.get(name))], unit="tokens"
        )
    return metrics, verification
