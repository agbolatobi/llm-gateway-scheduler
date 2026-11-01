"""Prepare benchmark cases, start/reuse llama.cpp through the CLI, then run AIPerf."""

import argparse
from datetime import datetime, timezone
from importlib.metadata import PackageNotFoundError, version
import json
import math
from pathlib import Path
import shlex
import subprocess
import sys

from .prepare import PROJECT_ROOT, add_workload_arguments, load_workload, new_name, write_prepared
from .report import export_pdf, require_pdf_support
from .results import request_outcomes
from .latency import export_request_metrics
from .memory import MemoryMonitor
from .runtime import RuntimeRecorder, save_snapshot
from .compare import export_comparison

AIPERF_VERSION = "0.13.0"
# Exact text is replayed, and measured token counts come only from backend usage.
# AIPerf still requires a tokenizer for internal plumbing; builtin avoids downloads.
DEFAULT_TOKENIZER = "builtin"


def startup_command(args):
    return [
        sys.executable, str(PROJECT_ROOT / "cli.py"),
        "--start-server", "--server-only",
        "--base-url", args.base_url, "--model", args.model,
        "--startup-timeout", str(args.startup_timeout),
    ]


def profile_command(args, executable, input_file, output_dir, workload):
    command = [
        str(executable), "profile",
        "--model", args.model, "--url", args.base_url,
        "--endpoint-type", "chat", "--endpoint", "/v1/chat/completions",
        "--streaming", "--input-file", str(input_file),
        "--custom-dataset-type", "mooncake_trace",
        "--tokenizer", args.tokenizer,
        "--use-server-token-count", "--use-legacy-max-tokens",
        "--concurrency", str(workload["concurrency"]),
        "--request-count", str(workload["repetitions"]),
        "--extra-inputs", json.dumps({
            "temperature": args.temperature, "cache_prompt": args.prompt_cache == "reuse",
        }),
        "--request-timeout-seconds", str(args.request_timeout),
        "--export-level", "raw",
        "--artifact-dir", str(output_dir),
        "--ui-type", "none",
        "--no-gpu-telemetry", "--no-server-metrics",
    ]
    if args.warmup_requests:
        command += ["--warmup-request-count", str(args.warmup_requests)]
    return command


def installed_executable():
    try:
        installed = version("aiperf")
    except PackageNotFoundError as error:
        raise RuntimeError(
            "AIPerf is missing from this Python environment. Run "
            ".venv/bin/python -m pip install -r requirements.txt "
            "and use .venv/bin/python -m benchmarks.run"
        ) from error
    if installed != AIPERF_VERSION:
        raise RuntimeError(f"Expected AIPerf {AIPERF_VERSION}, found {installed}; reinstall requirements")
    executable = Path(sys.executable).with_name("aiperf")
    if not executable.is_file():
        raise RuntimeError(f"AIPerf command not found beside this Python: {executable}")
    return executable


def save_run(path, record):
    path.write_text(json.dumps(record, indent=2) + "\n", encoding="utf-8")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    add_workload_arguments(parser)
    parser.add_argument("--base-url", default="http://127.0.0.1:8080")
    parser.add_argument("--model", default="local-llm")
    parser.add_argument("--tokenizer", default=DEFAULT_TOKENIZER,
                        help="AIPerf dataset tokenizer (default: offline builtin; metrics use server usage)")
    parser.add_argument("--temperature", type=float, default=0)
    parser.add_argument("--prompt-cache", choices=("off", "reuse"), default="off",
                        help="Cross-request prompt reuse: off (verified baseline) or reuse (allowed, not guaranteed)")
    parser.add_argument("--startup-timeout", type=float, default=120)
    parser.add_argument("--request-timeout", type=float, default=180)
    parser.add_argument("--memory-interval", type=float, default=1.0,
                        help="Server memory polling interval in seconds (0.1-60; default: 1)")
    parser.add_argument("--warmup-requests", type=int, default=1,
                        help="Excluded warm-up requests per case (default: 1; 0 disables)")
    parser.add_argument("--output", type=Path, help="New run directory; must not already exist")
    parser.add_argument("--no-start-server", action="store_true",
                        help="Skip CLI startup; use an already-running server")
    parser.add_argument("--dry-run", action="store_true",
                        help="Prepare inputs and print commands without startup, download, or inference")
    parser.add_argument("--pdf", action="store_true",
                        help="Also export report.pdf after a successful benchmark (ignored in dry-run)")
    parser.add_argument("--compare", action="store_true",
                        help="Require a sequential warmed uncached baseline and export verified comparisons")
    args = parser.parse_args(argv)
    if not math.isfinite(args.temperature) or not 0 <= args.temperature <= 2:
        parser.error("--temperature must be between 0 and 2")
    for name in ("startup_timeout", "request_timeout"):
        if not math.isfinite(getattr(args, name)) or getattr(args, name) <= 0:
            parser.error(f"--{name.replace('_', '-')} must be positive and finite")
    if args.warmup_requests < 0:
        parser.error("--warmup-requests cannot be negative")
    if not math.isfinite(args.memory_interval) or not 0.1 <= args.memory_interval <= 60:
        parser.error("--memory-interval must be between 0.1 and 60 seconds")
    if not args.tokenizer.strip() or not args.model.strip():
        parser.error("--tokenizer and --model must not be empty")

    record = None
    record_path = None
    try:
        workload = load_workload(
            args.input, case_ids=args.case_ids, repetitions=args.repetitions,
            concurrency=args.concurrency,
        )
        if args.compare and (workload["concurrency"] != 1 or workload["repetitions"] < 3
                             or args.warmup_requests < 1 or args.prompt_cache != "off"):
            raise ValueError("--compare requires concurrency 1, at least 3 repetitions, warm-ups, and --prompt-cache off")
        if args.pdf and not args.dry_run:
            require_pdf_support()
        executable = (Path(sys.executable).with_name("aiperf")
                      if args.dry_run else installed_executable())
        # Validate auto-start addresses even in dry-run, before creating output.
        if not args.no_start_server:
            from client.server import _local_address
            _local_address(args.base_url)
        output = (args.output or PROJECT_ROOT / "benchmarks/artifacts" / new_name()).resolve()
        # The run directory itself must be new, not just its inputs subdirectory.
        output.mkdir(parents=True, exist_ok=False)
        prepared = write_prepared(workload, output / "inputs")
        cases = [
            {
                "id": case["id"],
                "status": "pending",
                "command": profile_command(
                    args, executable, output / "inputs" / case["input_file"],
                    output / case["id"], workload,
                ),
            }
            for case in prepared["cases"]
        ]
        record = {
            "schema_version": 1,
            "created_at": datetime.now(timezone.utc).isoformat(),
            "status": "prepared",
            "aiperf_version": AIPERF_VERSION,
            "source_sha256": workload["source_sha256"],
            "settings": {key: value for key, value in vars(args).items()
                         if key not in {"input", "output"}},
            "measured_request_count": prepared["request_count"],
            "warmup_requests_per_case": args.warmup_requests,
            "startup_command": None if args.no_start_server else startup_command(args),
            "cases": cases,
        }
        record_path = output / "run.json"
        save_run(record_path, record)
        print(f"Run directory: {output}", flush=True)
        if args.dry_run:
            if record["startup_command"]:
                print(shlex.join(record["startup_command"]))
            for case in cases:
                print(shlex.join(case["command"]))
            record["status"] = "dry_run"
            save_run(record_path, record)
            print("Dry run: no server started, tokenizer downloaded, or requests sent.")
            return 0

        if record["startup_command"]:
            record["status"] = "starting_server"
            save_run(record_path, record)
            subprocess.run(record["startup_command"], cwd=PROJECT_ROOT, check=True)
        record["status"] = "running"
        save_run(record_path, record)
        runtime = RuntimeRecorder(args.base_url)
        save_run(output / "runtime.json", runtime.capture())
        for case in cases:
            print(f"Benchmarking {case['id']}...", flush=True)
            case["status"] = "running"
            save_run(record_path, record)
            try:
                profile_error = None
                case_dir = output / case["id"]
                memory = MemoryMonitor(args.base_url, args.memory_interval)
                before = runtime.capture()
                try:
                    with memory:
                        try:
                            result = subprocess.run(case["command"], cwd=PROJECT_ROOT, check=True)
                            case["exit_code"] = result.returncode
                        except subprocess.CalledProcessError as error:
                            # Preserve usable failure records even when AIPerf exits nonzero.
                            case["exit_code"] = error.returncode
                            profile_error = error
                finally:
                    # Stop polling and retain samples on failure/interrupt too.
                    case["memory"] = memory.save(case_dir)
                    snapshot = save_snapshot(case_dir, before, runtime.capture())
                    case["runtime_unchanged"] = snapshot["unchanged"]
                completion = export_request_metrics(case_dir, memory=memory)
                case["completion_outcomes"] = completion["outcomes"]
                case["cache_verification"] = completion["cache_verification"]
                outcomes = request_outcomes(case_dir / "profile_export.jsonl")
                case["request_outcomes"] = outcomes
                if profile_error:
                    raise profile_error
                verification = completion["cache_verification"]
                if args.prompt_cache == "off" and (
                        verification["cached_requests"] or verification["unverified_requests"]):
                    raise RuntimeError(
                        f"Uncached prompt policy could not be verified for {case['id']}: "
                        f"{verification}. Results retained; inspect backend counters."
                    )
                if (outcomes["measured"] != workload["repetitions"] or outcomes["errors"]
                        or completion["outcomes"]["measured"] != workload["repetitions"]
                        or completion["outcomes"]["errors"]):
                    raise RuntimeError(
                        f"Incomplete or failed measured requests for {case['id']}: "
                        f"native={outcomes}, completion={completion['outcomes']}"
                    )
                case["status"] = "completed"
            except KeyboardInterrupt:
                case["status"] = "interrupted"
                raise
            except (OSError, ValueError, RuntimeError, subprocess.CalledProcessError):
                case["status"] = "failed"
                raise
            finally:
                save_run(record_path, record)
        record["status"] = "completed"
        save_run(record_path, record)
        if args.compare:
            try:
                comparison = export_comparison(output)
            except (OSError, ValueError, KeyError, TypeError) as error:
                record["comparison"] = {"status": "failed", "error": str(error)}
                save_run(record_path, record)
                print(f"Inference completed, but comparison export failed: {error}", file=sys.stderr)
                return 1
            record["comparison"] = {
                "status": "completed" if comparison["meas_02_complete"] else "incomplete",
                "gaps": comparison["gaps"], "path": "comparison.md",
            }
            save_run(record_path, record)
            if not comparison["meas_02_complete"]:
                print(f"Controlled comparison is incomplete: {comparison['gaps']}", file=sys.stderr)
                return 1
            print(f"Controlled comparison: {output / 'comparison.md'}")
        if args.pdf:
            try:
                pdf_path = export_pdf(output)
            except Exception as error:
                # Report failure must not relabel successful inference as failed.
                record["pdf_export"] = {"status": "failed", "error": str(error)}
                save_run(record_path, record)
                print(f"Benchmark completed, but PDF export failed: {error}", file=sys.stderr)
                print("JSON/CSV results are retained. Retry with python -m benchmarks.report RUN_DIRECTORY.",
                      file=sys.stderr)
                return 1
            record["pdf_export"] = {"status": "completed", "path": str(pdf_path)}
            save_run(record_path, record)
            print(f"PDF report: {pdf_path}")
        print(f"AIPerf finished. Inspect per-request outcomes in: {output}")
        print("The model remains running; stop it with the existing stop_server.py command.")
        return 0
    except KeyboardInterrupt:
        if record is not None:
            if record["status"] != "completed":
                record["status"] = "interrupted"
            elif args.pdf:
                record["pdf_export"] = {"status": "interrupted"}
            save_run(record_path, record)
        print("Benchmark interrupted. Existing results are retained.", file=sys.stderr)
        return 130
    except (OSError, ValueError, RuntimeError, subprocess.CalledProcessError) as error:
        if record is not None:
            record["status"] = "failed"
            record["error"] = str(error)
            save_run(record_path, record)
        print(f"Benchmark failed: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
