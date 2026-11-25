"""Resolve saved arrival traces to the project's existing engineering prompts."""

import copy
import json


def read_json(path):
    def unique_fields(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError(f"Duplicate JSON field: {key}")
            result[key] = value
        return result
    return json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=unique_fields)


def resolve(config, prompts, workload, options):
    if workload.get("version") != 1:
        raise ValueError("Evaluation workload must have version 1")
    prompt_by_id = {item["id"]: item["prompt"] for item in prompts["prompts"]}
    case_by_id = {item["id"]: item for item in prompts["test_cases"]}
    if len(prompt_by_id) != len(prompts["prompts"]) or len(case_by_id) != len(prompts["test_cases"]):
        raise ValueError("Duplicate prompt or case IDs")
    traces = []
    for trace in workload["traces"]:
        if options.trace and trace["name"] not in options.trace:
            continue
        jobs = []
        for entry in trace["jobs"]:
            case = case_by_id[entry["case"]]
            jobs.append({"id": entry["id"], "at_seconds": entry["at_seconds"], "class": entry["class"],
                         "prompt": prompt_by_id[case["prompt_id"]], "max_tokens": case["max_tokens"]})
        # An interval is measured from the first scheduled arrival, not idle lead-in.
        if jobs and min(job["at_seconds"] for job in jobs) != 0:
            raise ValueError("Each trace must have its first arrival at zero")
        traces.append({"name": trace["name"], "jobs": jobs})
    if options.trace and set(options.trace) != {trace["name"] for trace in traces}:
        raise ValueError("Unknown trace selected")
    copied_config = copy.deepcopy(config)
    scheduler = copied_config["gateway"].setdefault("scheduler", {})
    if getattr(options, "aging_seconds", None) is not None:
        scheduler["aging_threshold_seconds"] = options.aging_seconds
    if getattr(options, "queue_wait", None) is not None:
        scheduler["max_queue_wait_seconds"] = options.queue_wait
    gateway_timeout = getattr(options, "gateway_timeout", 120)
    if scheduler.get("max_queue_wait_seconds", 30) >= gateway_timeout:
        raise ValueError("Queue deadline must be shorter than the gateway request deadline")
    for model in copied_config["models"]:
        # Launch paths are host-specific. The worker verifies runtime identity,
        # tokenizer, slot context and hashes the running model file separately.
        model.pop("launch", None)
    return {"version": 1, "gateway_config": copied_config, "model": options.model,
            "traces": traces, "modes": options.mode or ["direct", "fifo", "interactive_priority_aging"],
            "repetitions": options.repetitions, "timeout_seconds": options.timeout,
            "max_in_flight": options.max_in_flight, "gateway_timeout_seconds": gateway_timeout}
