"""Read-only provenance for the verified local llama.cpp process."""

from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import platform
import re

import requests

from . import memory
from client.server import _local_address, PROJECT_ROOT

SAMPLING_KEYS = {
    "seed", "temperature", "dynatemp_range", "dynatemp_exponent", "top_k", "top_p",
    "min_p", "top_n_sigma", "xtc_probability", "xtc_threshold", "typical_p",
    "repeat_last_n", "repeat_penalty", "presence_penalty", "frequency_penalty",
    "dry_multiplier", "dry_base", "dry_allowed_length", "dry_penalty_last_n",
    "mirostat", "mirostat_tau", "mirostat_eta", "adaptive_target", "adaptive_decay",
    "n_keep", "n_discard", "ignore_eos", "min_keep", "samplers",
    "reasoning_format", "reasoning_in_content", "speculative.types", "backend_sampling",
}
OPTIONS = {"--model", "-m", "--alias", "--host", "--port", "--ctx-size", "-c",
           "--parallel", "-np", "--threads", "-t", "--threads-batch", "-tb",
           "--device", "-dev", "--gpu-layers", "-ngl", "--batch-size", "-b",
           "--ubatch-size", "-ub", "--cache-type-k", "--cache-type-v", "--log-verbosity"}
BOOL_OPTIONS = {"--cont-batching", "--no-cont-batching", "--kv-unified", "--no-kv-unified"}
ENV_KEYS = {"LLAMA_ARG_THREADS", "LLAMA_ARG_THREADS_BATCH", "LLAMA_ARG_CTX_SIZE",
            "LLAMA_ARG_N_PARALLEL", "LLAMA_ARG_BATCH", "LLAMA_ARG_UBATCH",
            "LLAMA_ARG_DEVICE", "LLAMA_ARG_N_GPU_LAYERS", "OMP_NUM_THREADS"}


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True).encode()).hexdigest()


def launch_options(args):
    result = {}
    for index, arg in enumerate(args):
        key, separator, value = arg.partition("=")
        if key in OPTIONS:
            result[key] = value if separator else (args[index + 1] if index + 1 < len(args) else None)
        elif key in BOOL_OPTIONS:
            result[key] = True
    return result


class RuntimeRecorder:
    def __init__(self, base_url):
        self.base_url = base_url
        self.hashes = {}

    def fingerprint_file(self, path):
        path = Path(path)
        before = path.stat()
        key = (str(path), before.st_dev, before.st_ino, before.st_size,
               before.st_mtime_ns, before.st_ctime_ns)
        if key not in self.hashes:
            with path.open("rb") as stream:
                value = hashlib.file_digest(stream, "sha256").hexdigest()
            after = path.stat()
            if (after.st_ino, after.st_size, after.st_mtime_ns, after.st_ctime_ns) != (
                    before.st_ino, before.st_size, before.st_mtime_ns, before.st_ctime_ns):
                raise RuntimeError("File changed while fingerprinting")
            self.hashes[key] = value
        return {"size_bytes": before.st_size, "sha256": self.hashes[key]}

    def capture(self):
        result = {"schema_version": 1, "captured_at": datetime.now(timezone.utc).isoformat(),
                  "status": "unavailable", "unavailable_reason": None}
        pid, reason = memory.find_target(self.base_url)
        if reason:
            result["unavailable_reason"] = reason
            return result
        try:
            process = Path("/proc") / str(pid)
            ticks = memory.process_start(pid)
            raw_args = (process / "cmdline").read_bytes()
            args = [os.fsdecode(arg) for arg in raw_args.split(b"\0") if arg]
            env = {}
            for item in (process / "environ").read_bytes().split(b"\0"):
                key, separator, value = item.partition(b"=")
                if separator and os.fsdecode(key) in ENV_KEYS:
                    env[os.fsdecode(key)] = os.fsdecode(value)
            address, _ = _local_address(self.base_url)
            with requests.Session() as session:
                session.trust_env = False
                response = session.get(address + "/props", timeout=3, allow_redirects=False)
                response.raise_for_status()
                props = response.json()
            if not isinstance(props, dict):
                raise ValueError("Invalid server properties")
            generation = props.get("default_generation_settings", {})
            params = generation.get("params", {})
            if not isinstance(params, dict) or not isinstance(props.get("model_path"), str):
                raise ValueError("Missing runtime properties")
            model_path = Path(props["model_path"])
            if not model_path.is_absolute():
                model_path = (process / "cwd").resolve() / model_path
            executable = (process / "exe").resolve(strict=True)
            cpu = next((line.split(":", 1)[1].strip()
                        for line in Path("/proc/cpuinfo").read_text().splitlines()
                        if line.startswith("model name")), "unavailable")
            ram = next(int(line.split()[1]) / 1024
                       for line in Path("/proc/meminfo").read_text().splitlines()
                       if line.startswith("MemTotal:"))
            controls = {
                "process": {"pid": pid, "start_ticks": ticks, "argv_sha256": hashlib.sha256(raw_args).hexdigest(),
                            "executable": str(executable), "launch_options": launch_options(args),
                            "thread_environment": env},
                "runtime_binary": self.fingerprint_file(process / "exe"),
                "model": {"path": str(model_path.resolve()), **self.fingerprint_file(model_path),
                          "quantization": props.get("model_ftype"), "alias": props.get("model_alias")},
                "build_info": props.get("build_info"),
                "context_size": generation.get("n_ctx"), "slots": props.get("total_slots"),
                "sampling_defaults": {key: value for key, value in params.items() if key in SAMPLING_KEYS},
                "chat_template_sha256": hashlib.sha256(str(props.get("chat_template", "")).encode()).hexdigest(),
                "hardware": {"cpu": cpu, "visible_cpus": os.cpu_count(),
                             "cpu_affinity": sorted(os.sched_getaffinity(pid)),
                             "total_ram_mib": ram, "kernel": platform.release()},
            }
            # This is supporting evidence, not a PID-correlated server API counter.
            thread_evidence = None
            stdout = process / "fd/1"
            log = (PROJECT_ROOT / ".runtime" / "llama-server-8080.log").resolve()
            if stdout.resolve() == log and log.is_file():
                with log.open("rb") as stream:
                    stream.seek(max(0, log.stat().st_size - 65536))
                    text = stream.read().decode(errors="replace")
                values = re.findall(r"threadpool init, n_threads = (\d+)", text)
                if values:
                    thread_evidence = {"threads": int(values[-1]),
                                       "source": "latest threadpool-init line in process stdout log",
                                       "limitation": "append-only log; line is not PID-correlated"}
            if memory.process_start(pid) != ticks:
                raise RuntimeError("Server process identity changed")
            result.update(status="available", controls=controls, fingerprint=digest(controls),
                          thread_log_evidence=thread_evidence,
                          limitations=["Hashes identify files observed on disk, not a model source revision.",
                                       "Snapshots cannot detect a temporary change between observations.",
                                       "No claim of an isolated or frequency-locked machine."])
        except (OSError, ValueError, RuntimeError, IndexError, StopIteration, requests.RequestException):
            result["unavailable_reason"] = "runtime_configuration_unavailable_or_changed"
        return result


def save_snapshot(directory, before, after):
    value = {"schema_version": 1, "before": before, "after": after,
             "unchanged": (before.get("status") == after.get("status") == "available"
                           and before.get("fingerprint") == after.get("fingerprint"))}
    with (directory / "runtime_snapshot.json").open("x", encoding="utf-8") as stream:
        json.dump(value, stream, indent=2, allow_nan=False)
        stream.write("\n")
    return value
