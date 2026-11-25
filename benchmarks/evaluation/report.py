"""Compute five metrics from saved request evidence; preserve unavailable reasons."""

import csv
import hashlib
import html
import json
import math
from collections import Counter
from pathlib import Path

from benchmarks.evaluation.audit import audit_classes


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2, ensure_ascii=False, allow_nan=False) + "\n", encoding="utf-8")


def sha256(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def seal(directory):
    # Integrity checks detect accidental edits, not malicious replacement of both
    # the evidence and manifest. Keep a separately trusted copy for that threat.
    entries = {path.relative_to(directory).as_posix(): sha256(path)
               for path in sorted(directory.rglob("*")) if path.is_file() and path.name != "manifest.json"}
    write_json(directory / "manifest.json", {"algorithm": "sha256", "files": entries})


def verify(directory):
    manifest = json.loads((directory / "manifest.json").read_text(encoding="utf-8"))
    for name, expected in manifest["files"].items():
        path = (directory / name).resolve()
        if not path.is_relative_to(directory.resolve()) or not path.is_file() or sha256(path) != expected:
            raise ValueError(f"Evidence integrity check failed: {name}")
    required = {"plan.json", "events.jsonl", "provenance.json"}
    if not required.issubset(manifest["files"]):
        raise ValueError("Manifest is missing required evidence")


def p95(values):
    values = sorted(value for value in values if value is not None)
    return values[math.ceil(len(values) * 0.95) - 1] if values else None


def metric(value, reason=None):
    return {"value": value, "reason": reason}


def summarize(start, end, requests, gateway_records, jobs, request_class):
    selected_jobs = [job for job in jobs if request_class == "all" or job["class"] == request_class]
    job_ids = {job["id"] for job in selected_jobs}
    selected = [request for request in requests if request["job_id"] in job_ids]
    completed = [request for request in selected if request["outcome"] == "completed"]
    observed = {request["job_id"] for request in selected}
    unfinished = len(job_ids - observed)
    outcomes = dict(Counter(request["outcome"] for request in selected))
    if unfinished:
        outcomes["unfinished"] = unfinished
    finished_trial = end is not None and unfinished == 0
    ttft_values = [request["ttft_ms"] for request in completed if request["ttft_ms"] is not None]
    e2e_values = [request["e2e_ms"] for request in completed if request["e2e_ms"] is not None]
    latency_reason = None if finished_trial else "partial trial; observed successes only"
    result = {
        "trial_id": start["trial_id"], "trace": start["trace"], "mode": start["mode"], "class": request_class,
        "offered": len(selected_jobs), "observed": len(selected), "completed": len(completed),
        "outcomes": outcomes, "ttft_samples": len(ttft_values), "e2e_samples": len(e2e_values),
        "p95_ttft_ms": metric(p95(ttft_values), latency_reason if ttft_values else "no successful text samples"),
        "p95_e2e_ms": metric(p95(e2e_values), latency_reason if e2e_values else "no successful complete streams"),
        "completion_percent": metric(100 * len(completed) / len(selected_jobs) if selected_jobs and finished_trial else None,
                                     None if selected_jobs and finished_trial else "no offered work or unfinished trial"),
        "output_tokens_per_second": metric(None, "reported for whole workload only"),
        "max_background_queue_wait_ms": metric(None, "reported for whole workload only"),
    }
    if request_class != "all":
        return result
    elapsed_ms = end.get("elapsed_ms") if end else None
    if not finished_trial or not elapsed_ms or elapsed_ms <= 0:
        result["output_tokens_per_second"] = metric(None, "unfinished trial or unavailable measured interval")
    elif any(request["output_tokens"] is None for request in completed):
        result["output_tokens_per_second"] = metric(None, "successful request missing valid runtime token usage")
    else:
        result["output_tokens_per_second"] = metric(sum(request["output_tokens"] for request in completed) / (elapsed_ms / 1000))
    background = [request for request in selected if request["class"] == "background" and request["sent_ms"] is not None]
    if start["mode"] == "direct":
        result["max_background_queue_wait_ms"] = metric(None, "direct runtime has no gateway queue")
    elif not finished_trial:
        result["max_background_queue_wait_ms"] = metric(None, "unfinished trial; remaining waits unknown")
    elif any(request["request_id"] not in gateway_records for request in background):
        result["max_background_queue_wait_ms"] = metric(None, "missing background gateway timing record")
    else:
        # Overflow never entered the waiting queue; early admission rejects did
        # not reach the scheduler. Immediate dispatch is a valid zero wait.
        waits = [gateway_records[request["request_id"]]["queue_wait_ms"] for request in background
                 if gateway_records[request["request_id"]].get("scheduler_policy")
                 and gateway_records[request["request_id"]]["outcome"] not in ("queue_full", "capacity_exceeded")]
        result["max_background_queue_wait_ms"] = metric(max(waits) if waits else None,
                                                        None if waits else "no background jobs entered the queue")
    return result


METRICS = {
    "p95_ttft_ms": ("p95 first text (ms)", "How long slower requests wait before a reply starts."),
    "p95_e2e_ms": ("p95 full reply (ms)", "How long slower requests wait for the whole response."),
    "output_tokens_per_second": ("Useful output (tokens/s)", "Completed output over the full workload duration; measures useful capacity."),
    "completion_percent": ("Completion (%)", "Shows failures and rejections that could otherwise make latency look better."),
    "max_background_queue_wait_ms": ("Max background wait (ms)", "Shows the observed cost of priority to background work; does not prove no starvation."),
}


def build_report(directory):
    plan = json.loads((directory / "plan.json").read_text(encoding="utf-8"))
    provenance = json.loads((directory / "provenance.json").read_text(encoding="utf-8"))
    starts, ends, requests, gateway = {}, {}, {}, {}
    warnings, environment, run_ended = [], {}, False
    flat_requests = []
    for number, line in enumerate((directory / "events.jsonl").read_text(encoding="utf-8").splitlines(), 1):
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            warnings.append(f"Invalid/truncated evidence line {number}; report is incomplete.")
            continue
        trial = event.get("trial_id")
        kind = event["event"]
        if kind == "environment":
            environment = event
        elif kind == "trial_start":
            starts[trial] = event
        elif kind == "trial_end":
            ends[trial] = event
        elif kind == "request":
            requests.setdefault(trial, []).append(event["result"])
            flat_requests.append({"trial_id": trial, **event["result"]})
        elif kind == "gateway":
            gateway.setdefault(trial, {})[event["record"]["request_id"]] = event["record"]
        elif kind == "run_error":
            warnings.append(event["error"])
        elif kind == "run_end":
            run_ended = event.get("complete", False)
    expected = plan["repetitions"] * len(plan["traces"]) * len(plan["modes"])
    traces = {trace["name"]: trace["jobs"] for trace in plan["traces"]}
    summaries = []
    classification_issues = []
    first_props = environment.get("snapshot", {}).get("/props")
    for trial, start in starts.items():
        trial_requests = requests.get(trial, [])
        seen = [request["job_id"] for request in trial_requests]
        expected_ids = {job["id"] for job in traces[start["trace"]]}
        if len(seen) != len(set(seen)) or not set(seen).issubset(expected_ids):
            raise ValueError(f"Duplicate or unknown request jobs in {trial}")
        trial_issues = audit_classes(start, trial_requests, gateway.get(trial, {}), traces[start["trace"]])
        classification_issues.extend(trial_issues)
        warnings.extend(trial_issues)
        trial_summaries = []
        for request_class in ("all", "interactive", "background"):
            trial_summaries.append(summarize(start, ends.get(trial), trial_requests, gateway.get(trial, {}),
                                             traces[start["trace"]], request_class))
        if trial_issues:
            for row in trial_summaries:
                row["max_background_queue_wait_ms"] = metric(None, "request classification audit failed")
        summaries.extend(trial_summaries)
        # Label what actually happened instead of assuming a trace name proves load.
        if start["trace"] == "quiet" and any(record["queue_wait_ms"] > 10 for record in gateway.get(trial, {}).values()):
            warnings.append(f"{trial}: quiet workload queued; increase arrival spacing and repeat every mode.")
        if start["trace"] == "contended" and start["mode"] != "direct" and not any(record["queue_wait_ms"] > 10 for record in gateway.get(trial, {}).values()):
            warnings.append(f"{trial}: no meaningful contention observed; shorten spacing or increase work.")
        if any(request["sent_ms"] is not None and request["sent_ms"] - request["scheduled_ms"] > 100 for request in trial_requests):
            warnings.append(f"{trial}: client arrival delay exceeded 100 ms; inspect requests.csv.")
        before_props = start.get("snapshot", {}).get("/props")
        after_props = ends.get(trial, {}).get("snapshot", {}).get("/props")
        if before_props != after_props:
            warnings.append(f"{trial}: runtime properties changed or end snapshot is missing.")
        if before_props != first_props:
            warnings.append(f"{trial}: runtime properties differ from initial fingerprint; comparison is not controlled.")
    expected_ids = {f"r{repetition + 1}-t{trace_index + 1}-{mode}"
                    for repetition in range(plan["repetitions"])
                    for trace_index in range(len(plan["traces"])) for mode in plan["modes"]}
    execution_complete = (run_ended and set(starts) == expected_ids and set(ends) == expected_ids
                          and provenance.get("exit_code") == 0
                          and all(row["observed"] == row["offered"] for row in summaries)
                          and not any("Invalid/truncated evidence" in warning for warning in warnings))
    if not execution_complete:
        warnings.append("Run incomplete or worker failed; do not claim a final policy comparison.")
    if any(row["class"] != "all" and row["e2e_samples"] < 20 for row in summaries):
        warnings.append("Fewer than 20 successful samples per class/trial: p95 is exploratory, not stable tail evidence.")
    if plan["repetitions"] < 3:
        warnings.append("Fewer than three repeats: smoke test only.")
    if not environment.get("snapshot", {}).get("model_sha256"):
        warnings.append("Model file hash unavailable; model identity is not fully reproducible.")
    if provenance.get("error"):
        warnings.append(provenance["error"])
    report = {"schema_version": 1, "complete": execution_complete,
              "classification_valid": not classification_issues,
              "classification_issues": classification_issues,
              "comparison_cautions": "Review warnings, sample sizes and workload outcomes before drawing conclusions; complete means execution finished, not that a performance target passed.",
              "expected_trials": expected, "finished_trials": len(ends), "warnings": warnings,
              "definitions": METRICS, "environment": environment, "provenance": provenance, "summaries": summaries}
    write_json(directory / "summary.json", report)
    columns = ["trial_id", "trace", "mode", "class", "offered", "observed", "completed", "ttft_samples", "e2e_samples"]
    for name in METRICS:
        columns.extend([name, name + "_reason"])
    with (directory / "summary.csv").open("w", newline="", encoding="utf-8") as output:
        writer = csv.DictWriter(output, fieldnames=columns); writer.writeheader()
        for row in summaries:
            flat = {key: row[key] for key in columns if key in row and key not in METRICS}
            for name in METRICS:
                flat[name] = row[name]["value"]
                flat[name + "_reason"] = row[name]["reason"]
            writer.writerow(flat)
    with (directory / "requests.csv").open("w", newline="", encoding="utf-8") as output:
        fields = ["trial_id", "request_id", "job_id", "class", "scheduled_ms", "sent_ms", "terminal_ms", "ttft_ms", "e2e_ms", "output_tokens", "usage_reason", "http_status", "outcome", "error"]
        writer = csv.DictWriter(output, fieldnames=fields); writer.writeheader(); writer.writerows(flat_requests)
    escaped = html.escape
    rows = []
    for row in summaries:
        cells = [row["trial_id"], row["class"], f'{row["completed"]}/{row["offered"]}']
        cells.extend(f'{row[name]["value"]:.2f}' if row[name]["value"] is not None else "N/A: " + str(row[name]["reason"]) for name in METRICS)
        rows.append("<tr>" + "".join("<td>" + escaped(str(cell)) + "</td>" for cell in cells) + "</tr>")
    headers = ["Trial", "Class", "Completed/offered"] + [definition[0] for definition in METRICS.values()]
    document = """<!doctype html><html lang="en"><meta charset="utf-8"><title>Gateway evaluation</title>
<style>body{font:15px system-ui;margin:2rem;color:#17202a}table{border-collapse:collapse;font-size:13px}td,th{border:1px solid #bbb;padding:.5rem;text-align:left}th{background:#edf1f5}pre{white-space:pre-wrap}h1{font-size:24px}@media print{body{margin:0}table{font-size:9px}}</style>
<h1>Gateway evaluation</h1>"""
    document += "<p>" + ("All planned trials finished." if report["complete"] else "INCOMPLETE RUN — retain for diagnosis, not final comparison.") + "</p>"
    if classification_issues:
        document += "<p><strong>INVALID POLICY COMPARISON: request classes did not match gateway evidence.</strong></p>"
    document += "<p>Loopback measurements beside the runtime; excludes public ingress, TLS and browser/network delay. Temporary gateways use this checkout's code and fresh quota counters. Cache is erased, then identically warmed before each trial; reuse is allowed within the trial.</p>"
    document += "<ul>" + "".join("<li>" + escaped(warning) + "</li>" for warning in warnings) + "</ul>"
    document += "<table><tr>" + "".join("<th>" + escaped(header) + "</th>" for header in headers) + "</tr>" + "".join(rows) + "</table>"
    document += "<h2>Why these five</h2><ul>" + "".join("<li><b>" + escaped(title) + "</b>: " + escaped(reason) + "</li>" for title, reason in METRICS.values()) + "</ul>"
    document += "<p>Nearest-rank p95 uses successful requests only; completion counts all offered work. Throughput uses actual successful output tokens over the full scheduled-to-terminal interval. Background wait includes queued failures. Missing evidence stays N/A. Compare policies within one environment; cloud/local hardware differs.</p>"
    document += "<p>Audit: plan.json, events.jsonl, requests.csv, provenance.json, source.zip and manifest.json. Print this page to PDF if needed. Raw streams contain generated text.</p>"
    document += "<details><summary>Provenance</summary><pre>" + escaped(json.dumps(provenance, indent=2)) + "</pre></details></html>"
    (directory / "report.html").write_text(document, encoding="utf-8")
    return report
