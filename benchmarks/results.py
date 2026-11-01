"""Read request outcomes from native AIPerf JSONL exports."""

import json


def request_outcomes(path):
    """Count native AIPerf records without mixing warm-up into measurements."""
    counts = {"measured": 0, "errors": 0, "warmup": 0, "warmup_errors": 0}
    if not path.is_file():
        raise RuntimeError(f"AIPerf produced no per-request JSONL: {path}")
    with path.open(encoding="utf-8") as stream:
        for line in stream:
            row = json.loads(line)
            if not isinstance(row, dict) or not isinstance(row.get("metadata"), dict):
                raise ValueError(f"Invalid AIPerf record in {path}")
            metadata = row["metadata"]
            phase = metadata.get("benchmark_phase")
            failed = bool(row.get("error") or metadata.get("was_cancelled")
                          or metadata.get("context_overflow_skip"))
            if phase == "profiling":
                counts["measured"] += 1
                counts["errors"] += int(failed)
            elif phase == "warmup":
                counts["warmup"] += 1
                counts["warmup_errors"] += int(failed)
            else:
                raise ValueError(f"Unexpected AIPerf record phase: {phase!r}")
    return counts
