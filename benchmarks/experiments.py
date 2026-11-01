"""Small controlled concurrency/context sweeps, using the CLI's server lifecycle."""

import argparse
from contextlib import contextmanager
import fcntl
import json
import math
from pathlib import Path
import signal
import socket
import subprocess
import sys
import time

import requests

from client import server
from stop_server import _terminate
from . import memory, run
from .experiment_results import export_results, summarize_stage
from .prepare import DEFAULT_INPUT, PROJECT_ROOT, load_workload, new_name, positive_integer

DEFAULT_CONFIG = Path(__file__).with_name("experiments.json")


def load_plan(path, source):
    plan = json.loads(Path(path).read_text(encoding="utf-8"))
    if not isinstance(plan, dict):
        raise ValueError("Experiment config must be an object")
    for key in ("repetitions", "warmup_requests"):
        positive_integer(plan[key], key)
    if plan["repetitions"] < 8:
        raise ValueError("Use at least eight measured requests (two waves at concurrency four)")
    for key in ("concurrency", "context"):
        levels = plan[key]
        if not isinstance(levels, list) or levels != sorted(set(levels)):
            raise ValueError(f"{key} must be an increasing list without duplicates")
        for value in levels:
            positive_integer(value, key)
    if plan["concurrency"] != [1, 2, 4] or len(plan["context"]) != 2:
        raise ValueError("Plan requires concurrency [1, 2, 4] and two context limits")
    options = plan["server"]
    server.launch_options(options)
    if options["parallel"] < 4 or options.get("cont_batching") is not True or options.get("kv_unified") is not False:
        raise ValueError("Use at least four fixed slots, continuous batching and non-unified KV")
    required = {"ctx_size", "parallel", "threads", "threads_batch", "batch_size", "ubatch_size", "log_verbosity"}
    if required - options.keys():
        raise ValueError("Explicit context/slot/thread/batch/log settings are required")
    for context in plan["context"]:
        if context < 512 * options["parallel"] or context % options["parallel"]:
            raise ValueError("Each context must divide evenly into at least 512 tokens per slot")
    if options["ctx_size"] not in plan["context"]:
        raise ValueError("The concurrency context must also be tested in the context experiment")
    for key, low, high in (("memory_interval", 0.1, 1), ("idle_seconds", 1, 30)):
        value = plan[key]
        if type(value) not in (int, float) or not math.isfinite(value) or not low <= value <= high:
            raise ValueError(f"{key} must be between {low} and {high}")
    for key in ("max_e2e_seconds", "max_ttft_seconds", "max_ram_mib",
                "min_available_ram_mib", "request_timeout_seconds"):
        value = plan["limits"][key]
        if type(value) not in (int, float) or not math.isfinite(value) or value <= 0:
            raise ValueError(f"{key} must be positive and finite")
    load_workload(source, case_ids=[plan["case"]], repetitions=plan["repetitions"])
    return plan


def available_ram_mib():
    for line in Path("/proc/meminfo").read_text().splitlines():
        if line.startswith("MemAvailable:"):
            return int(line.split()[1]) / 1024
    raise RuntimeError("Available host RAM cannot be verified")


def require_headroom(plan):
    if available_ram_mib() < plan["limits"]["min_available_ram_mib"]:
        raise RuntimeError("Host available RAM is below the experiment safety floor")


def require_free_port(port):
    if port == 8080 or not 1024 <= port <= 65535:
        raise ValueError("Use a dedicated experiment port from 1024-65535, not the project's 8080")
    with socket.socket() as probe:
        # Permit our previous server's closed TCP connections, not a live listener.
        probe.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        probe.bind(("127.0.0.1", port))  # Never reuse/reconfigure an existing service.


def check_input_budget(address, plan, source):
    workload = load_workload(source, case_ids=[plan["case"]])
    case = workload["cases"][0]
    prompt = workload["prompts"][case["prompt_id"]]["prompt"]
    with requests.Session() as session:
        session.trust_env = False
        def query(method, endpoint, **kwargs):
            response = session.request(method, address + endpoint, timeout=5, allow_redirects=False, **kwargs)
            response.raise_for_status()
            return response.json()
        slots = query("GET", "/slots")
        if not isinstance(slots, list) or not slots or any(item.get("is_processing") for item in slots):
            raise RuntimeError("Experiment server is not idle")
        formatted = query("POST", "/apply-template", json={"messages": [{"role": "user", "content": prompt}]})
        tokens = query("POST", "/tokenize", json={"content": formatted["prompt"],
                                                "add_special": True, "parse_special": True})["tokens"]
        if not isinstance(tokens, list) or not tokens or any(type(token) is not int for token in tokens):
            raise ValueError("Model tokenizer did not return token IDs")
        contexts = [item["n_ctx"] for item in slots]
        if any(type(value) is not int or len(tokens) + case["max_tokens"] > value for value in contexts):
            raise ValueError("Chat-templated input plus output cap exceeds per-slot context")
        return {"input_tokens": len(tokens), "output_cap": case["max_tokens"],
                "slot_contexts": contexts, "method": "/apply-template then /tokenize, add_special/parse_special true"}


class PhaseMemoryMonitor(memory.MemoryMonitor):
    def __init__(self, address, interval, phase):
        super().__init__(address, interval)
        self.phase = phase

    def summary(self):
        value = super().summary()
        value.update(scope="server_process_" + self.phase,
                     window=("first verified process observation through readiness; earliest allocations may be missed"
                             if self.phase == "startup" else "post-readiness idle interval before experiment requests"),
                     baseline_kind="first observation in this phase; not allocator-reserved memory")
        return value


class OwnedServer:
    """Observe startup/idle memory, then gracefully stop only our own child server."""

    def __init__(self, root, plan, port, context):
        self.root, self.plan, self.port, self.context = root, plan, port, context
        self.address = f"http://127.0.0.1:{port}"
        self.process = self.monitor = None
        self.ticks = None

    def started(self, process, log):
        self.process = process
        self.ticks = memory.process_start(process.pid)
        self.monitor = PhaseMemoryMonitor(self.address, self.plan["memory_interval"], "startup")
        if self.monitor.pid != process.pid or self.monitor.start_ticks != self.ticks:
            raise RuntimeError("Startup sampler could not verify the owned server")
        self.monitor.__enter__()

    def close(self):
        try:
            if self.monitor and self.monitor.thread:
                self.monitor.__exit__()
                self.monitor.save(self.root / "startup")
                self.monitor = None
        finally:
            if self.process and self.process.poll() is None:
                if memory.process_start(self.process.pid) != self.ticks:
                    raise RuntimeError("Owned server identity changed; no signal sent")
                _terminate(self.process.pid, self.port, 10)
                self.process.wait(timeout=2)

    def __enter__(self):
        require_free_port(self.port)
        require_headroom(self.plan)
        self.root.mkdir(parents=True, exist_ok=False)
        log = PROJECT_ROOT / ".runtime" / f"llama-server-{self.port}.log"
        offset = log.stat().st_size if log.exists() else 0
        try:
            started = time.perf_counter_ns()
            path = server.ensure_server_ready(
                self.address, timeout=120,
                server_options={**self.plan["server"], "ctx_size": self.context}, on_start=self.started,
            )
            ready = time.perf_counter_ns()
            if path is None or self.process is None:
                raise RuntimeError("Another server appeared; it was not adopted or stopped")
            if self.monitor:
                self.monitor.__exit__()
                self.monitor.save(self.root / "startup")
                self.monitor = None
            with path.open("rb") as stream:
                stream.seek(offset)
                text = stream.read().decode(errors="replace")
            (self.root / "startup.log").write_text(text, encoding="utf-8")
            allocations = [line for line in text.splitlines()
                           if "buffer size" in line or "n_ctx" in line or "n_batch" in line
                           or "kv_unified" in line or "n_ubatch" in line]
            run.save_run(self.root / "startup.json", {
                "launch_to_readiness_seconds": (ready - started) / 1e9,
                "allocation_log_lines": allocations,
                "allocation_unavailable_reason": None if allocations else "backend_allocations_not_logged",
                "scope": "fresh process; filesystem cache is not flushed; sampled process RSS is not KV allocation",
            })
            idle = PhaseMemoryMonitor(self.address, self.plan["memory_interval"], "idle")
            with idle:
                time.sleep(self.plan["idle_seconds"])
            idle.save(self.root / "idle")
            require_headroom(self.plan)
            return self
        except BaseException:
            self.close()
            raise

    def __exit__(self, *_):
        self.close()


def command(plan, source, root, port, concurrency):
    return [sys.executable, "-m", "benchmarks.run", "--input", str(source),
            "--case", plan["case"], "--concurrency", str(concurrency),
            "--repetitions", str(plan["repetitions"]), "--warmup-requests", str(plan["warmup_requests"]),
            "--memory-interval", str(plan["memory_interval"]), "--temperature", "0", "--prompt-cache", "off",
            "--request-timeout", str(plan["limits"]["request_timeout_seconds"]),
            "--base-url", f"http://127.0.0.1:{port}", "--no-start-server", "--output", str(root / "run")]


def stop_profile(process):
    """Signal only the process group created for this benchmark; keep its exports."""
    import os
    if process.poll() is None:
        for sig, timeout in ((signal.SIGINT, 15), (signal.SIGTERM, 5), (signal.SIGKILL, 5)):
            try:
                os.killpg(process.pid, sig)
                process.wait(timeout=timeout)
                return
            except ProcessLookupError:
                process.wait(timeout=5)
                return
            except subprocess.TimeoutExpired:
                continue
        raise RuntimeError("Owned benchmark process did not exit after cleanup signals")


def profile_stage(arguments, root, plan, owned):
    with (root / "runner.log").open("w", encoding="utf-8") as log:
        with subprocess.Popen(arguments, cwd=PROJECT_ROOT, stdout=log, stderr=subprocess.STDOUT,
                              start_new_session=True) as process:
            try:
                deadline = time.monotonic() + plan["repetitions"] * plan["limits"]["request_timeout_seconds"] + 120
                while process.poll() is None:
                    if time.monotonic() > deadline:
                        raise RuntimeError("Experiment condition exceeded its wall-clock safety timeout")
                    require_headroom(plan)
                    if (memory.process_start(owned.process.pid) != owned.ticks
                            or memory.rss_mib(owned.process.pid) > plan["limits"]["max_ram_mib"]):
                        raise RuntimeError("Server identity/RSS safety limit failed; profile stopped")
                    time.sleep(0.2)
                if process.returncode:
                    raise RuntimeError(f"Benchmark exited {process.returncode}; inspect {root / 'runner.log'}")
            finally:
                stop_profile(process)


@contextmanager
def experiment_lock(port):
    path = PROJECT_ROOT / ".runtime" / f"benchmark-experiment-{port}.lock"
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise RuntimeError("Another experiment owns this port") from error
        yield


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("experiment", choices=("concurrency", "context", "all"))
    parser.add_argument("--config", type=Path, default=DEFAULT_CONFIG)
    parser.add_argument("--input", type=Path, default=DEFAULT_INPUT)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--port", type=int, default=8081)
    parser.add_argument("--dry-run", action="store_true", help="Print the plan only; no files/processes/network")
    args = parser.parse_args(argv)
    record = root = None
    try:
        plan = load_plan(args.config, args.input)
        if args.port == 8080 or not 1024 <= args.port <= 65535:
            raise ValueError("Use a dedicated port, not 8080")
        kinds = ["concurrency", "context"] if args.experiment == "all" else [args.experiment]
        conditions = [(kind, level if kind == "concurrency" else 1,
                       plan["server"]["ctx_size"] if kind == "concurrency" else level)
                      for kind in kinds for level in plan[kind]]
        root = (args.output or PROJECT_ROOT / "benchmarks/artifacts" / ("experiments-" + new_name())).resolve()
        if root.exists():
            raise FileExistsError("Experiment output already exists; choose a new directory")
        if args.dry_run:
            print(json.dumps({"plan": plan, "conditions": conditions, "port": args.port,
                              "output": str(root)}, indent=2))
            return 0
        run.installed_executable()  # Fail before starting a model if dependencies are absent.
        require_free_port(args.port)
        require_headroom(plan)
        with experiment_lock(args.port):
            root.mkdir(parents=True, exist_ok=False)
            record = {"schema_version": 1, "status": "running", "plan": plan,
                      "experiments": kinds, "input": str(args.input.resolve()), "port": args.port,
                      "stages": [], "errors": []}
            run.save_run(root / "experiment.json", record)
            for kind, concurrency, context in conditions:
                name = f"{kind}-c{concurrency}-ctx{context}"
                directory = root / name
                record["active_stage"] = name
                run.save_run(root / "experiment.json", record)
                print(f"Experiment {name}...", flush=True)
                with OwnedServer(directory, plan, args.port, context) as owned:
                    budget = check_input_budget(owned.address, plan, args.input)
                    run.save_run(directory / "input_budget.json", budget)
                    profile_stage(command(plan, args.input.resolve(), directory, args.port, concurrency),
                                  directory, plan, owned)
                    result = summarize_stage(directory, plan, concurrency, context)
                record["stages"].append({"experiment": kind, "path": name, "result": result})
                run.save_run(root / "experiment.json", record)
                export_results(root, record)
                if not result["passed"]:
                    raise RuntimeError(f"Stopping after unacceptable condition: {result['gaps']}")
            record["status"] = "completed"
            data = export_results(root, record)
            if data["gaps"]:
                raise RuntimeError(f"Comparison controls failed: {data['gaps']}")
            if args.experiment == "all":
                run.save_run(root / "frozen_config.json", {
                    "server_options": plan["server"], "concurrency": data["selected_concurrency"],
                    "temperature": 0, "prompt_cache": "off", "memory_interval": plan["memory_interval"],
                    "evidence": str(root), "scope": "Initial CPU short-prompt baseline; revalidate for other workloads",
                })
            run.save_run(root / "experiment.json", record)
            print(f"Experiment reports: {root / 'summary.md'}", flush=True)
            return 0
    except (OSError, ValueError, RuntimeError, KeyError, TypeError, subprocess.SubprocessError,
            requests.RequestException) as error:
        if record is not None:
            record["status"] = "failed"
            record["errors"].append(str(error))
            run.save_run(root / "experiment.json", record)
            export_results(root, record)
        print(f"Experiment failed: {error}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        if record is not None:
            record["status"] = "interrupted"
            run.save_run(root / "experiment.json", record)
            export_results(root, record)
        print("Experiment interrupted; partial evidence retained and owned server cleanup attempted.", file=sys.stderr)
        return 130


if __name__ == "__main__":
    sys.exit(main())
