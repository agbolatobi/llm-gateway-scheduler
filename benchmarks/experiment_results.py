"""Saved-result checks for the concurrency and context experiments; no inference."""

import copy
import csv
import json

from .compare import read_object
from .latency import read_jsonl, statistics
from .runtime import digest


def measurement_window(rows):
    """First measured send through last DONE, not a sum/mean of request rates."""
    events = []
    for row in rows:
        start, end = row.get("request_start_perf_ns"), row.get("completion_perf_ns")
        if type(start) is not int or type(end) is not int or not 0 <= start < end:
            return None, None
        events.extend(((start, 1), (end, -1)))
    if not events:
        return None, None
    active = peak = 0
    for _, change in sorted(events):  # Ends precede sends at the same timestamp.
        active += change
        peak = max(peak, active)
    return (max(time for time, _ in events) - min(time for time, _ in events)) / 1e9, peak


def normalized_controls(controls, *, context=False):
    value = copy.deepcopy(controls)
    process = value.setdefault("process", {})
    for key in ("pid", "start_ticks", "argv_sha256"):
        process.pop(key, None)
    if context:
        process.setdefault("launch_options", {}).pop("--ctx-size", None)
        value.pop("context_size", None)
    return value


def summarize_stage(root, plan, concurrency, context):
    case_id = plan["case"]
    run = read_object(root / "run/run.json")
    manifest = read_object(root / "run/inputs/manifest.json")
    directory = root / "run" / case_id
    records = list(read_jsonl(directory / "request_metrics.jsonl"))
    measured = [row for row in records if row["benchmark_phase"] == "profiling"]
    successful = [row for row in measured if row["status"] == "success"]
    warmup = [row for row in records if row["benchmark_phase"] == "warmup"]
    point = read_object(root / "run/runtime.json")
    snapshot = read_object(directory / "runtime_snapshot.json")
    controls = point.get("controls", {})
    gaps = []
    budget = read_object(root / "input_budget.json")
    if any(row.get("input_tokens") != budget["input_tokens"] for row in successful):
        gaps.append("Actual input usage differs from the chat-templated tokenizer budget")
    if (
        run["status"] != "completed"
        or len(measured) != plan["repetitions"]
        or len(successful) != len(measured)
        or len(warmup) != plan["warmup_requests"]
        or any(row["status"] != "success" for row in warmup)
    ):
        gaps.append("Request/warm-up coverage is incomplete or unsuccessful")
    if (
        run["settings"].get("prompt_cache") != "off"
        or run["settings"].get("temperature") != 0
        or manifest.get("concurrency") != concurrency
    ):
        gaps.append("Request settings differ from the experiment")
    for row in measured:
        if (
            row.get("cached_input_tokens") != 0
            or row.get("input_tokens") is None
            or row.get("backend_prompt_tokens") != row.get("input_tokens")
            or row.get("output_tokens") is None
        ):
            gaps.append("Uncached token accounting is missing or inconsistent")
            break
    if point.get("status") != "available" or digest(controls) != point.get("fingerprint"):
        gaps.append("Runtime reference is unavailable or inconsistent")
    for side in ("before", "after"):
        item = snapshot.get(side, {})
        if (
            not snapshot.get("unchanged")
            or item.get("status") != "available"
            or item.get("fingerprint") != point.get("fingerprint")
            or digest(item.get("controls")) != item.get("fingerprint")
        ):
            gaps.append("Runtime changed during the case or could not be verified")
            break
    expected = {**plan["server"], "ctx_size": context}
    launch = controls.get("process", {}).get("launch_options", {})
    for key, value in expected.items():
        flag = "--" + key.replace("_", "-")
        if type(value) is bool:
            flag = "--" + ("" if value else "no-") + key.replace("_", "-")
            matches = launch.get(flag) is True
        else:
            matches = launch.get(flag) == str(value)
        if not matches:
            gaps.append(f"Server did not use requested {flag}")
    if controls.get("slots") != expected["parallel"]:
        gaps.append("Server slot count differs from the plan")
    duration, overlap = measurement_window(measured)
    if duration is None or overlap != concurrency:
        gaps.append(
            "Measured timing window is unavailable or target concurrency was not reached"
        )
    metrics = {}
    for name in ("ttft_ms", "e2e_latency_ms"):
        values = [row[name] for row in successful if row.get(name) is not None]
        if len(values) != len(measured):
            gaps.append(f"Missing {name}")
        stat = statistics(values)
        for label in ("p50", "p95", "max"):
            metrics[name + "_" + label] = stat[label]
    memory = read_object(directory / "memory_summary.json")
    idle = read_object(root / "idle/memory_summary.json")
    startup = read_object(root / "startup/memory_summary.json")
    if any(value["ram"]["status"] != "available" for value in (memory, idle, startup)):
        gaps.append("Complete process-RSS samples are required")
    limits = plan["limits"]
    for name, limit in (
        ("ttft_ms_max", limits["max_ttft_seconds"] * 1000),
        ("e2e_latency_ms_max", limits["max_e2e_seconds"] * 1000),
    ):
        if metrics[name] is not None and metrics[name] > limit:
            gaps.append(f"Safety/acceptance threshold exceeded: {name}")
    peaks = [value["ram"]["peak_mib"] for value in (memory, idle, startup)]
    if any(value is not None and value > limits["max_ram_mib"] for value in peaks):
        gaps.append("Safety/acceptance threshold exceeded: process RSS")
    output = sum(
        row["output_tokens"] for row in successful if row.get("output_tokens") is not None
    )
    return {
        "concurrency": concurrency,
        "total_context": context,
        "context_per_slot": controls.get("context_size"),
        "server_slots": controls.get("slots"),
        "requests": len(measured),
        "successful": len(successful),
        "warmups": len(warmup),
        "duration_seconds": duration,
        "max_observed_inflight": overlap,
        "output_tokens": output,
        "output_tokens_per_second": output / duration if duration else None,
        "requests_per_second": len(successful) / duration if duration else None,
        **metrics,
        "actual_input_tokens": sorted(
            {row["input_tokens"] for row in successful if row.get("input_tokens") is not None}
        ),
        "actual_output_tokens": sorted(
            {row["output_tokens"] for row in successful if row.get("output_tokens") is not None}
        ),
        "idle_ram_mib": idle["ram"]["baseline_mib"],
        "startup_peak_ram_mib": startup["ram"]["peak_mib"],
        "peak_ram_mib": memory["ram"]["peak_mib"],
        "peak_vram_mib": memory["vram"]["peak_mib"],
        "vram_unavailable_reason": memory["vram"]["unavailable_reason"],
        "source_sha256": manifest["source_sha256"],
        "runtime_controls": controls,
        "gaps": sorted(set(gaps)),
        "passed": not gaps,
    }


def export_results(root, record):
    """Update only this new suite's own summaries; never alter child run evidence."""
    stages = record["stages"]
    groups = {
        kind: [stage for stage in stages if stage["experiment"] == kind]
        for kind in record["experiments"]
    }
    gaps = list(record.get("errors", []))
    for kind, group in groups.items():
        if len(group) != len(record["plan"][kind]) or any(
            not item["result"]["passed"] for item in group
        ):
            gaps.append(f"{kind}: not all planned conditions passed")
        if group:
            reference = group[0]["result"]
            for stage in group[1:]:
                row = stage["result"]
                if (
                    normalized_controls(row["runtime_controls"], context=kind == "context")
                    != normalized_controls(
                        reference["runtime_controls"], context=kind == "context"
                    )
                    or row["source_sha256"] != reference["source_sha256"]
                    or row["actual_input_tokens"] != reference["actual_input_tokens"]
                ):
                    gaps.append(f"{kind}: controlled settings/workload changed")
    reliable = [
        stage["result"]["concurrency"]
        for stage in groups.get("concurrency", [])
        if stage["result"]["passed"]
    ]
    selected = max(reliable) if reliable else None
    data = {
        "schema_version": 1,
        "status": record["status"],
        "gaps": sorted(set(gaps)),
        "selected_concurrency": selected,
        "stages": stages,
        "plan": record["plan"],
        "interpretation": "Small CPU-only closed-loop workload; no reliable tail, GPU, or general-capacity claim.",
    }
    (root / "summary.json").write_text(json.dumps(data, indent=2, allow_nan=False) + "\n")
    rows = [
        {
            "experiment": stage["experiment"],
            "path": stage["path"],
            **{
                key: value
                for key, value in stage["result"].items()
                if key != "runtime_controls"
            },
        }
        for stage in stages
    ]
    with (root / "summary.csv").open("w", newline="", encoding="utf-8") as stream:
        writer = csv.DictWriter(stream, fieldnames=list(rows[0]) if rows else ["experiment"])
        writer.writeheader()
        writer.writerows(rows)

    def number(value):
        return "N/A" if value is None else f"{value:.3f}"

    lines = [
        "# Concurrency and context experiments",
        "",
        f"Status: {record['status']}",
        f"Highest tested concurrency within declared limits: {selected}",
        "",
        "| Experiment | Client concurrency | Total context | Context/slot | Output tok/s | P50 TTFT ms | P50 E2E ms | Idle RSS MiB | Startup peak RSS MiB | Case peak RSS MiB |",
        "| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |",
    ]
    for item in rows:
        keys = (
            "concurrency",
            "total_context",
            "context_per_slot",
            "output_tokens_per_second",
            "ttft_ms_p50",
            "e2e_latency_ms_p50",
            "idle_ram_mib",
            "startup_peak_ram_mib",
            "peak_ram_mib",
        )
        lines.append(
            "| "
            + item["experiment"]
            + " | "
            + " | ".join(number(item[key]) for key in keys)
            + " |"
        )
    lines += [
        "",
        "## Interpretation",
        "",
        "Concurrency changes client outstanding requests; server slots, context and batch settings stay fixed. "
        "Throughput is successful output tokens / (last measured DONE - first measured send), excluding warm-up, "
        "startup and AIPerf setup. Observed overlap verifies the requested concurrency was reached.",
        "",
        "Context settings are total server context. Read context/slot from /props; do not assume the total is per request. "
        "Context comparisons use fresh servers with identical short inputs, output cap, slots, concurrency and CPU settings.",
        "",
        "RAM is sampled process used/resident RSS, not allocated KV size or unique physical memory. Startup and idle "
        "samples are retained separately. Allocation lines, when exposed, are in startup.log and are not substituted for RSS. "
        "CPU VRAM is unavailable, not zero. Similar RSS does not prove identical KV allocation; mmap, residency, "
        "work buffers and sampling affect observations.",
        "",
        "One fixed-order sweep with eight requests per condition is exploratory. P95 is descriptive only. "
        "No isolated-machine, GPU or universal safe-concurrency claim is made. Snapshots miss temporary changes.",
        "",
        "## Evidence",
        "",
        *[
            f"- [{stage['path']}]({stage['path']}/): run metrics, runtime, idle/startup memory and startup log."
            for stage in stages
        ],
    ]
    if gaps:
        lines += ["", "## Gaps", "", *("- " + gap for gap in sorted(set(gaps)))]
    (root / "summary.md").write_text("\n".join(lines) + "\n", encoding="utf-8")
    return data
