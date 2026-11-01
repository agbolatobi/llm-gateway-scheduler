"""Create evidence-based MEAS-02 comparisons from saved results; no inference."""

import argparse
import csv
import hashlib
import json
from pathlib import Path
import sys

from .latency import read_jsonl, summarize
from .prepare import identifier
from .runtime import digest


def read_object(path):
    value = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(value, dict):
        raise ValueError(f"Expected an object: {path}")
    return value


def inside(root, relative):
    path = (root / relative).resolve()
    if not path.is_relative_to(root):
        raise ValueError("Comparison input escapes run directory")
    return path


def analyze(directory):
    root = Path(directory).resolve()
    run = read_object(root / "run.json")
    manifest = read_object(inside(root, "inputs/manifest.json"))
    if run.get("schema_version") != 1 or not isinstance(run.get("settings"), dict):
        raise ValueError("Invalid run manifest")
    settings, gaps, rows, snapshots = run["settings"], [], [], []
    if run.get("status") != "completed":
        gaps.append("Benchmark run is not completed.")
    if settings.get("prompt_cache") != "off":
        gaps.append("Uncached prompt policy was not explicitly selected.")
    if manifest.get("concurrency") != 1:
        gaps.append("Sequential concurrency 1 is required.")
    if manifest.get("repetitions", 0) < 3:
        gaps.append("At least three measured repetitions per case are required.")
    if run.get("warmup_requests_per_case", 0) < 1:
        gaps.append("At least one successful warm-up per case is required.")
    if not run.get("source_sha256") or run["source_sha256"] != manifest.get("source_sha256"):
        gaps.append("Workload source hashes are missing or disagree.")
    reference_path = inside(root, "runtime.json")
    reference = read_object(reference_path) if reference_path.is_file() else {}
    if reference.get("status") != "available" or not reference.get("fingerprint"):
        gaps.append("The actual runtime configuration was not captured.")
    elif digest(reference.get("controls")) != reference["fingerprint"]:
        gaps.append("Runtime reference fingerprint does not match its controls.")
    specs = {identifier(case["id"], "Case id"): case for case in manifest["cases"]}
    seen = set()
    for case in run["cases"]:
        key = identifier(case["id"], "Case id")
        if key in seen or key not in specs:
            raise ValueError("Duplicate or unknown run case")
        seen.add(key)
        spec = specs[key]
        path = inside(root, f"{key}/request_metrics.jsonl")
        if not path.is_file():
            gaps.append(f"{key}: request metrics are missing.")
            continue
        records = list(read_jsonl(path))
        summary = summarize(records)
        counts, backend, cache = (summary[name] for name in
                                  ("outcomes", "backend_metrics", "cache_verification"))
        n = counts["measured"]
        if (case.get("status") != "completed" or n != spec.get("request_count")
                or n < 3 or counts["errors"] or counts["warmup_errors"]
                or counts["warmup"] != run.get("warmup_requests_per_case")):
            gaps.append(f"{key}: measured/warm-up coverage is incomplete or unsuccessful.")
        if cache["uncached_requests"] != n or cache["cached_requests"] or cache["unverified_requests"]:
            gaps.append(f"{key}: uncached input accounting was not verified for every request.")
        for name in ("backend_prompt_ms", "backend_decode_ms", "backend_prompt_tokens",
                     "cached_input_tokens", "backend_output_tokens", "input_tokens", "output_tokens"):
            if backend[name]["count"] != n:
                gaps.append(f"{key}: {name} does not cover every measured request.")
        inputs = list(read_jsonl(inside(root, "inputs/" + spec["input_file"])))
        texts = {item.get("text_input") for item in inputs}
        if len(texts) != 1 or not all(isinstance(text, str) for text in texts):
            raise ValueError("A case must replay one exact prompt")
        text_hash = hashlib.sha256(next(iter(texts)).encode()).hexdigest()
        snapshot_path = inside(root, f"{key}/runtime_snapshot.json")
        snapshot = read_object(snapshot_path) if snapshot_path.is_file() else {}
        valid_runtime = snapshot.get("unchanged") is True
        for side in ("before", "after"):
            point = snapshot.get(side, {})
            valid_runtime &= (point.get("status") == "available"
                              and point.get("fingerprint") == reference.get("fingerprint")
                              and digest(point.get("controls")) == point.get("fingerprint"))
        if not valid_runtime:
            gaps.append(f"{key}: runtime settings/process/files changed or could not be verified.")
        snapshots.append({"case": key, "unchanged": bool(valid_runtime)})
        rows.append({
            "case": key, "prompt_id": spec["prompt_id"], "prompt_sha256": text_hash,
            "output_cap": spec["max_tokens"], "requests": n,
            "input_tokens_p50": backend["input_tokens"]["p50"],
            "output_tokens_p50": backend["output_tokens"]["p50"],
            "new_input_tokens_p50": backend["backend_prompt_tokens"]["p50"],
            "cached_input_tokens_p50": backend["cached_input_tokens"]["p50"],
            "ttft_ms_p50": summary["text_ttft"]["p50"],
            "e2e_ms_p50": summary["e2e_latency"]["p50"],
            "prompt_ms_p50": backend["backend_prompt_ms"]["p50"],
            "decode_ms_p50": backend["backend_decode_ms"]["p50"],
        })
    if seen != set(specs):
        gaps.append("Run and workload case sets differ.")
    by_cap, by_prompt = {}, {}
    for row in rows:
        by_cap.setdefault(row["output_cap"], []).append(row)
        by_prompt.setdefault(row["prompt_sha256"], []).append(row)
    input_groups = [group for group in by_cap.values()
                    if len({row["prompt_id"] for row in group}) >= 3
                    and len({row["input_tokens_p50"] for row in group
                             if row["input_tokens_p50"] is not None}) >= 3]
    output_groups = [group for group in by_prompt.values()
                     if len({row["output_cap"] for row in group}) >= 2]
    input_group = sorted(min(input_groups, key=lambda g: g[0]["output_cap"]),
                         key=lambda row: row["input_tokens_p50"]) if input_groups else []
    output_group = sorted(min(output_groups, key=lambda g: g[0]["input_tokens_p50"] or 0),
                          key=lambda row: row["output_cap"]) if output_groups else []
    if not input_group:
        gaps.append("Need three distinct prompt sizes at one fixed output cap.")
    if not output_group:
        gaps.append("Need two output caps for the exact same prompt.")
    elif len({row["input_tokens_p50"] for row in output_group}) != 1:
        gaps.append("Actual input length changed in the fixed-prompt comparison.")
    return {
        "schema_version": 1, "meas_02_complete": not gaps, "gaps": gaps,
        "run_directory": str(root), "source_sha256": run.get("source_sha256"),
        "runtime_fingerprint": reference.get("fingerprint"),
        "runtime_controls": reference.get("controls"),
        "thread_log_evidence": reference.get("thread_log_evidence"),
        "settings": settings, "statistic": "P50; successful measured requests; warm-ups excluded",
        "cases": rows, "input_comparison": [row["case"] for row in input_group],
        "output_comparison": [row["case"] for row in output_group], "runtime_checks": snapshots,
    }


def fmt(value):
    return "N/A" if value is None else f"{value:,.3f}" if isinstance(value, float) else str(value)


def markdown(data):
    lines = ["# MEAS-02: prompt and output length", "",
             "Acceptance: " + ("complete" if data["meas_02_complete"] else "incomplete"), "",
             f"Run: {data['run_directory']}", f"Workload SHA-256: {data['source_sha256']}",
             f"Runtime fingerprint: {data['runtime_fingerprint']}", "",
             "## Measurements", "", data["statistic"] + ". Times are milliseconds.", "",
             "| Case | n | Input | Output | New | Cached | TTFT | E2E | Prompt | Decode |",
             "| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |"]
    keys = ("case", "requests", "input_tokens_p50", "output_tokens_p50", "new_input_tokens_p50",
            "cached_input_tokens_p50", "ttft_ms_p50", "e2e_ms_p50", "prompt_ms_p50", "decode_ms_p50")
    for row in data["cases"]:
        lines.append("| " + " | ".join(fmt(row[key]) for key in keys) + " |")
    lookup = {row["case"]: row for row in data["cases"]}
    lines += ["", "## Observations", ""]
    for kind, title in (("input_comparison", "Input-length comparison"),
                        ("output_comparison", "Output-length comparison")):
        group = [lookup[key] for key in data[kind]]
        if len(group) < 2:
            lines.append(f"- {title}: insufficient comparable cases.")
            continue
        first, last = group[0], group[-1]
        if kind == "input_comparison":
            lines.append(
                f"- {title}: fixed output cap {first['output_cap']}; actual input "
                f"{fmt(first['input_tokens_p50'])} to {fmt(last['input_tokens_p50'])} tokens. "
                f"Backend prompt time changed from {fmt(first['prompt_ms_p50'])} to "
                f"{fmt(last['prompt_ms_p50'])} ms; TTFT from {fmt(first['ttft_ms_p50'])} to "
                f"{fmt(last['ttft_ms_p50'])} ms. Decode time changed from "
                f"{fmt(first['decode_ms_p50'])} to {fmt(last['decode_ms_p50'])} ms at actual "
                f"outputs {fmt(first['output_tokens_p50'])} and {fmt(last['output_tokens_p50'])}.")
        else:
            lines.append(
                f"- {title}: exact fixed prompt {first['prompt_id']}, caps "
                f"{first['output_cap']} to {last['output_cap']}; actual outputs "
                f"{fmt(first['output_tokens_p50'])} to {fmt(last['output_tokens_p50'])} tokens. "
                f"Backend generation time changed from {fmt(first['decode_ms_p50'])} to "
                f"{fmt(last['decode_ms_p50'])} ms; E2E from {fmt(first['e2e_ms_p50'])} to "
                f"{fmt(last['e2e_ms_p50'])} ms. Backend prompt times were "
                f"{fmt(first['prompt_ms_p50'])} and {fmt(last['prompt_ms_p50'])} ms.")
    lines += ["", "## Interpretation and limits", "",
              "- Backend prompt_ms describes newly evaluated input work; cache counters identify reuse.",
              "- Backend predicted_ms describes generation. TTFT also includes transport, queueing, "
              "first-token work and other overhead; it is not pure prefill.",
              "- Input processing and generation are observed separately. Longer context can also "
              "affect generation work; do not attribute every E2E change to prefill.",
              "- Output caps are limits, not guaranteed lengths; compare actual output counts.",
              "- Three repetitions are a small baseline, not reliable p95/p99 evidence. These are "
              "per-case repetitions, not three independent full-run trials.",
              "- Fixed case order, different prompt content, background load and CPU frequency can "
              "affect results. No isolated-machine or linear-scaling claim is made.",
              "- Runtime snapshots check process identity, effective server properties and file hashes "
              "before/after each case. They cannot detect transient changes between observations.",
              "- Model SHA-256 identifies local bytes; no unverified upstream revision is invented.",
              "", "## Runtime record", "",
              "Full effective controls, binary/model fingerprints, context, slots, launch options, "
              "sampling defaults, selected thread environment and hardware are in runtime.json. "
              "Per-case before/after evidence is in runtime_snapshot.json. Request temperature, "
              "cache policy and caps override relevant server defaults; see run.json and inputs/manifest.json."]
    if data["thread_log_evidence"]:
        lines.append("Startup-log thread evidence (qualified, not PID-correlated): "
                     + json.dumps(data["thread_log_evidence"], sort_keys=True))
    if data["gaps"]:
        lines += ["", "## Remaining gaps", "", *("- " + gap for gap in data["gaps"])]
    return "\n".join(lines) + "\n"


def export_comparison(directory):
    root = Path(directory).resolve()
    data = analyze(root)
    paths = [root / ("comparison." + suffix) for suffix in ("json", "csv", "md")]
    if any(path.exists() for path in paths):
        raise FileExistsError("Refusing to overwrite comparison exports")
    with paths[0].open("x", encoding="utf-8") as stream:
        json.dump(data, stream, indent=2, allow_nan=False)
        stream.write("\n")
    with paths[1].open("x", encoding="utf-8", newline="") as stream:
        fields = list(data["cases"][0]) if data["cases"] else ["case"]
        writer = csv.DictWriter(stream, fieldnames=fields)
        writer.writeheader()
        writer.writerows(data["cases"])
    with paths[2].open("x", encoding="utf-8") as stream:
        stream.write(markdown(data))
    return data


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("run_directory", type=Path)
    args = parser.parse_args(argv)
    try:
        data = export_comparison(args.run_directory)
        print(f"Comparison: {args.run_directory / 'comparison.md'}")
        return 0 if data["meas_02_complete"] else 1
    except (OSError, ValueError, KeyError, TypeError) as error:
        print(f"Comparison failed: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
