"""One command for local/GKE evaluation; see benchmarks/README.evaluation.md."""

import argparse
import datetime
import json
import os
import platform
import subprocess
import sys
import uuid
import zipfile
from pathlib import Path

from benchmarks.evaluation.plan import read_json, resolve
from benchmarks.evaluation.report import build_report, seal, sha256, verify, write_json
from benchmarks.evaluation.transport import GKE, Local

ROOT = Path(__file__).resolve().parents[1]


def parser():
    arguments = argparse.ArgumentParser(description=__doc__)
    arguments.add_argument("--target", choices=("local", "gke"), default="local")
    arguments.add_argument("--config", type=Path, help="Local gateway config; GKE defaults to the selected Pod's ConfigMap")
    arguments.add_argument("--context", help="Explicit kubectl context (required for GKE)")
    arguments.add_argument("--namespace", default="llm-gateway")
    arguments.add_argument("--pod", help="Required when more than one gateway Pod is Ready")
    arguments.add_argument("--container", default="runtime")
    arguments.add_argument("--model", default="local-llm")
    arguments.add_argument("--prompts", type=Path, default=ROOT / "benchmark_prompts.json")
    arguments.add_argument("--workload", type=Path, default=ROOT / "benchmarks/evaluation.json")
    arguments.add_argument("--trace", action="append", help="Select trace; repeat flag to select several")
    arguments.add_argument("--mode", action="append", choices=("direct", "fifo", "interactive_priority_aging"))
    arguments.add_argument("--repetitions", type=int, default=3)
    arguments.add_argument("--timeout", type=int, default=180, help="Per-client-request deadline in seconds")
    arguments.add_argument("--aging-seconds", type=int, help="Override aging only in temporary evaluation gateways")
    arguments.add_argument("--queue-wait", type=int, help="Override temporary gateway queue deadline, in seconds")
    arguments.add_argument("--gateway-timeout", type=int, default=120, help="Temporary gateway total request deadline, in seconds")
    arguments.add_argument("--max-in-flight", type=int, default=64, help="Client safety bound; excess offered work is recorded unsent")
    arguments.add_argument("--output", type=Path, help="New run directory; existing paths refused")
    arguments.add_argument("--dry-run", action="store_true", help="Save resolved plan, without building or sending inference requests")
    arguments.add_argument("--report-only", type=Path, help="Verify hashes and rebuild reports without contacting a server")
    return arguments


def capture_source(directory):
    hashes = {}
    with zipfile.ZipFile(directory / "source.zip", "w", zipfile.ZIP_DEFLATED) as archive:
        candidates = list((ROOT / "gateway").rglob("*")) + list((ROOT / "benchmarks/evaluation").glob("*.py"))
        candidates += [ROOT / "benchmarks/evaluate.py"]
        for path in sorted(candidates):
            if not path.is_file() or any(part in ("__pycache__", "artifacts", ".git") for part in path.parts):
                continue
            is_go_source = path.suffix == ".go" or path.name in ("go.mod", "go.sum")
            is_ui_asset = "chatui" in path.parts and "static" in path.parts
            is_evaluator = path.suffix == ".py" and "benchmarks" in path.parts
            if not (is_go_source or is_ui_asset or is_evaluator):
                continue
            name = path.relative_to(ROOT).as_posix()
            hashes[name] = sha256(path)
            archive.write(path, name)
    return hashes


def main(argv=None):
    arguments = parser()
    options = arguments.parse_args(argv)
    if options.report_only:
        directory = options.report_only.resolve()
        verify(directory)
        report = build_report(directory)
        seal(directory)
        print(f"Verified evidence; report: {directory / 'report.html'}")
        return 0 if report["complete"] and report["classification_valid"] else 1
    if options.target == "gke" and not options.context:
        arguments.error("--context is required for GKE")
    if not 1 <= options.repetitions <= 10 or not 1 <= options.timeout <= 1800 or not 1 <= options.max_in_flight <= 128:
        arguments.error("repetitions: 1..10; timeout: 1..1800; max-in-flight: 1..128")
    for option_name in ("aging_seconds", "queue_wait", "gateway_timeout"):
        value = getattr(options, option_name)
        if value is not None and not 1 <= value <= 1800:
            arguments.error(f"{option_name} must be 1..1800 seconds")
    if options.gateway_timeout >= options.timeout:
        arguments.error("--timeout must exceed --gateway-timeout")
    run_id = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    directory = (options.output or ROOT / "benchmarks/artifacts" / ("eval-" + run_id)).resolve()
    directory.mkdir(parents=True, exist_ok=False)
    provenance = {"started_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
                  "target": options.target, "controller_platform": platform.platform(), "exit_code": None,
                  "invocation": [sys.executable, "-m", "benchmarks.evaluate", *(argv if argv is not None else sys.argv[1:])]}
    # These exist even when setup/build fails, preserving useful failed-run evidence.
    (directory / "events.jsonl").touch()
    print(f"Run evidence: {directory}", flush=True)
    return_code = 1
    try:
        transport = Local() if options.target == "local" else GKE(options.context, options.namespace, options.pod, options.container)
        provenance.update(transport.provenance)
        config = read_json(options.config or ROOT / "gateway.config.json") if options.target == "local" or options.config else transport.config
        prompts, workload = read_json(options.prompts), read_json(options.workload)
        plan = resolve(config, prompts, workload, options)
        write_json(directory / "gateway-config.original.json", config)
        write_json(directory / "prompts.json", prompts)
        write_json(directory / "workload.json", workload)
        write_json(directory / "plan.json", plan)
        if options.dry_run:
            provenance["dry_run"] = True
            return_code = 0
        else:
            provenance["source_files_sha256"] = capture_source(directory)
            build_env = os.environ.copy()
            build_env["CGO_ENABLED"] = "0"
            if options.target == "gke":
                build_env.update(GOOS="linux", GOARCH=transport.architecture)
            binary = directory / "worker"
            build_command = ["go", "build", "-trimpath", "-buildvcs=false", "-o", str(binary), "./cmd/evaluate"]
            provenance["build_command"] = build_command
            provenance["go_version"] = subprocess.check_output(["go", "version"], text=True, timeout=30).strip()
            provenance["build_environment"] = json.loads(subprocess.check_output(
                ["go", "env", "-json", "GOOS", "GOARCH", "CGO_ENABLED", "GOFLAGS", "GOTOOLCHAIN"],
                env=build_env, text=True, timeout=30))
            with (directory / "build.log").open("wb") as build_log:
                subprocess.run(build_command, cwd=ROOT / "gateway", env=build_env, stdout=build_log, stderr=build_log, check=True, timeout=300)
            if any(sha256(ROOT / name) != expected for name, expected in provenance["source_files_sha256"].items()):
                raise RuntimeError("Source changed while building; repeat from a stable checkout")
            provenance["worker_sha256"] = sha256(binary)
            print("Running fixed arrivals; keep this runtime free of other generation traffic.", flush=True)
            with (directory / "events.jsonl").open("wb") as events, (directory / "worker.stderr.log").open("wb") as errors:
                return_code = transport.execute(binary, json.dumps(plan).encode(), events, errors)
            provenance.update(transport.provenance)
    except KeyboardInterrupt:
        provenance["error"] = "Interrupted by operator; preserve partial evidence."
        return_code = 130
    except (OSError, ValueError, KeyError, RuntimeError, subprocess.SubprocessError) as error:
        provenance["error"] = str(error)
        print(f"Evaluation failed: {error}", file=sys.stderr)
    finally:
        provenance["exit_code"] = return_code
        provenance["finished_utc"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
        write_json(directory / "provenance.json", provenance)
        if (directory / "plan.json").exists() and not options.dry_run:
            report = build_report(directory)
            if not report["classification_valid"]:
                return_code = 1
            print(f"Report: {directory / 'report.html'}", flush=True)
        seal(directory)
    return return_code


if __name__ == "__main__":
    raise SystemExit(main())
