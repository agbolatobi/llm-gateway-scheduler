"""Convert the project workload to exact-prompt AIPerf JSONL files."""

import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import re
import sys
from uuid import uuid4

PROJECT_ROOT = Path(__file__).resolve().parent.parent
DEFAULT_INPUT = PROJECT_ROOT / "benchmark_prompts.json"


def positive_integer(value, name):
    if type(value) is not int or value < 1:
        raise ValueError(f"{name} must be a positive integer")
    return value


def identifier(value, name):
    if not isinstance(value, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{0,79}", value):
        raise ValueError(f"{name} must contain 1-80 letters, digits, underscores or hyphens")
    return value


def new_name():
    return datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S%fZ") + "-" + uuid4().hex[:6]


def load_workload(source, *, case_ids=None, repetitions=None, concurrency=None):
    source = Path(source).resolve()
    raw = source.read_bytes()
    data = json.loads(raw.decode("utf-8-sig"))
    if not isinstance(data, dict):
        raise ValueError("The workload must be a JSON object")
    prompts = data.get("prompts")
    cases = data.get("test_cases")
    config = data.get("test_config", {})
    if not isinstance(prompts, list) or not prompts:
        raise ValueError("prompts must be a non-empty list")
    if not isinstance(cases, list) or not cases:
        raise ValueError("test_cases must be a non-empty list")
    if not isinstance(config, dict):
        raise ValueError("test_config must be an object")
    prompt_map = {}
    for prompt in prompts:
        if not isinstance(prompt, dict):
            raise ValueError("Every prompt must be an object")
        key = identifier(prompt.get("id"), "Prompt id")
        if key in prompt_map:
            raise ValueError(f"Duplicate prompt id: {key}")
        text = prompt.get("prompt")
        if not isinstance(text, str) or not text.strip():
            raise ValueError(f"Prompt {key} must contain non-empty text")
        prompt_map[key] = prompt
    seen = set()
    for case in cases:
        if not isinstance(case, dict):
            raise ValueError("Every test case must be an object")
        key = identifier(case.get("id"), "Case id")
        if key in seen:
            raise ValueError(f"Duplicate case id: {key}")
        seen.add(key)
        prompt_id = case.get("prompt_id")
        if not isinstance(prompt_id, str) or prompt_id not in prompt_map:
            raise ValueError(f"Case {key} references an unknown prompt_id")
        positive_integer(case.get("max_tokens"), f"{key}.max_tokens")
    selected = set(case_ids or [])
    unknown = selected - seen
    if unknown:
        raise ValueError(f"Unknown case(s): {', '.join(sorted(unknown))}")
    if selected:
        cases = [case for case in cases if case["id"] in selected]
    repetitions = positive_integer(
        config.get("repetitions", 3) if repetitions is None else repetitions, "repetitions"
    )
    concurrency = positive_integer(
        config.get("concurrency", 1) if concurrency is None else concurrency, "concurrency"
    )
    return {
        "source": str(source),
        "source_sha256": hashlib.sha256(raw).hexdigest(),
        "prompts": prompt_map,
        "cases": cases,
        "repetitions": repetitions,
        "concurrency": concurrency,
    }


def write_prepared(workload, output):
    """Write a new directory only; never overwrite an earlier prepared workload."""
    output = Path(output).resolve()
    output.mkdir(parents=True, exist_ok=False)
    manifest = {
        "schema_version": 1,
        "source": workload["source"],
        "source_sha256": workload["source_sha256"],
        "dataset_type": "mooncake_trace",
        "concurrency": workload["concurrency"],
        "repetitions": workload["repetitions"],
        "request_count": len(workload["cases"]) * workload["repetitions"],
        "cases": [],
    }
    for case in workload["cases"]:
        prompt = workload["prompts"][case["prompt_id"]]
        filename = case["id"] + ".jsonl"
        records = [
            {
                "session_id": f"{case['id']}-r{index}",
                "text_input": prompt["prompt"],
                "output_length": case["max_tokens"],
            }
            for index in range(1, workload["repetitions"] + 1)
        ]
        with (output / filename).open("x", encoding="utf-8") as stream:
            for record in records:
                stream.write(json.dumps(record, ensure_ascii=False) + "\n")
        manifest["cases"].append(
            {
                **case,
                "input_file": filename,
                "target_input_tokens": prompt.get("target_input_tokens"),
                "request_count": len(records),
                "requests": [
                    {"line": index, "repetition": index, "id": f"{case['id']}-r{index}"}
                    for index in range(1, len(records) + 1)
                ],
            }
        )
    (output / "manifest.json").write_text(
        json.dumps(manifest, indent=2, ensure_ascii=False) + "\n", encoding="utf-8"
    )
    return manifest


def add_workload_arguments(parser):
    parser.add_argument(
        "--input",
        type=Path,
        default=DEFAULT_INPUT,
        help="Source JSON workload (default: project benchmark_prompts.json)",
    )
    parser.add_argument(
        "--case",
        action="append",
        dest="case_ids",
        help="Select a case ID; repeat to select several",
    )
    parser.add_argument("--repetitions", type=int, help="Override the workload repetitions")
    parser.add_argument("--concurrency", type=int, help="Override the workload concurrency")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    add_workload_arguments(parser)
    parser.add_argument(
        "--output", type=Path, help="New output directory; must not already exist"
    )
    args = parser.parse_args(argv)
    try:
        workload = load_workload(
            args.input,
            case_ids=args.case_ids,
            repetitions=args.repetitions,
            concurrency=args.concurrency,
        )
        output = args.output or PROJECT_ROOT / "benchmarks/generated" / new_name()
        manifest = write_prepared(workload, output)
        print(f"Prepared {len(manifest['cases'])} cases / {manifest['request_count']} requests")
        print(f"Inputs and manifest: {output.resolve()}")
        print("No inference was run. Original prompts were not changed.")
        return 0
    except (OSError, ValueError) as error:
        print(f"Preparation failed: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
