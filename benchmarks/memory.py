"""Sample the verified local server process, never the benchmark client or host totals."""

from collections import Counter
import csv
import io
import json
import math
from pathlib import Path
import platform
import shutil
import subprocess
import threading
import time

from client.server import _local_address
from stop_server import _find_server


def process_start(pid):
    # Field 22, after the parenthesized comm field (which may contain spaces).
    text = (Path("/proc") / str(pid) / "stat").read_text()
    return int(text[text.rfind(")") + 2 :].split()[19])


def find_target(base_url):
    try:
        _, port = _local_address(base_url)
    except ValueError:
        return None, "nonlocal_or_unsupported_endpoint"
    try:
        pid = _find_server(port)
        return (pid, None) if pid else (None, "project_server_not_found")
    except (OSError, RuntimeError):
        return None, "server_identity_unavailable"


def cpu_only(args):
    # Do not infer CPU-only from a missing NVIDIA process record.
    devices = []
    for index, arg in enumerate(args):
        if arg == "--device" and index + 1 < len(args):
            devices.append(args[index + 1])
        elif arg.startswith("--device="):
            devices.append(arg.split("=", 1)[1])
    return devices == ["none"]


def rss_mib(pid):
    for line in (Path("/proc") / str(pid) / "status").read_text().splitlines():
        fields = line.split()
        if fields and fields[0] == "VmRSS:":
            if len(fields) != 3 or fields[2] != "kB" or not fields[1].isdigit():
                raise ValueError("Invalid VmRSS")
            return int(fields[1]) / 1024
    raise ValueError("VmRSS not reported")


def gpu_memory_mib(executable, pid):
    """Sum only this PID's reported used memory across distinct GPU UUIDs."""
    if executable is None:
        return None, "nvidia_smi_not_found"
    try:
        result = subprocess.run(
            [
                executable,
                "--query-compute-apps=pid,gpu_uuid,used_gpu_memory",
                "--format=csv,noheader,nounits",
            ],
            capture_output=True,
            text=True,
            timeout=1,
            check=False,
        )
    except subprocess.TimeoutExpired:
        return None, "gpu_query_timeout"
    except (OSError, UnicodeError):
        return None, "gpu_query_failed"
    if result.returncode:
        return None, "gpu_query_failed_or_unsupported"
    devices = {}
    for fields in csv.reader(io.StringIO(result.stdout)):
        if not fields or not any(field.strip() for field in fields):
            continue
        if len(fields) != 3:
            return None, "gpu_counter_unavailable"
        process, device, value = (field.strip() for field in fields)
        if not process.isdecimal():
            return None, "gpu_counter_unavailable"
        if int(process) != pid:
            continue
        try:
            amount = float(value)
        except ValueError:
            return None, "gpu_counter_unavailable"
        if not device or device in devices or not math.isfinite(amount) or amount < 0:
            return None, "gpu_counter_unavailable"
        devices[device] = amount
    if not devices:
        return None, "gpu_process_not_reported"
    return sum(devices.values()), None


def counter_summary(samples, name):
    values = [sample[name + "_mib"] for sample in samples if sample[name + "_mib"] is not None]
    reasons = Counter(
        sample[name + "_unavailable_reason"]
        for sample in samples
        if sample[name + "_unavailable_reason"]
    )
    return {
        "baseline_mib": samples[0][name + "_mib"] if samples else None,
        "peak_mib": max(values) if values else None,
        "sample_count": len(values),
        "unavailable_sample_count": len(samples) - len(values),
        "status": ("partial" if reasons else "available") if values else "unavailable",
        "unavailable_reason": None if values else next(iter(reasons), "no_samples"),
        "unavailable_reasons": dict(reasons),
    }


class MemoryMonitor:
    """Poll around one AIPerf case; startup is excluded, warm-up/setup are included."""

    def __init__(self, base_url, interval=1.0):
        if not math.isfinite(interval) or not 0.1 <= interval <= 60:
            raise ValueError("Memory interval must be between 0.1 and 60 seconds")
        self.interval = interval
        self.samples = []
        self.pid, self.target_reason = find_target(base_url)
        self.start_ticks = None
        self.cpu_reason = None
        # WSL's NVIDIA process namespace/counters cannot reliably attribute this PID.
        self.gpu_reason = (
            "wsl_process_vram_unsupported"
            if "microsoft" in platform.release().lower()
            else None
        )
        if self.pid is not None:
            try:
                self.start_ticks = process_start(self.pid)
                args = (Path("/proc") / str(self.pid) / "cmdline").read_bytes().split(b"\0")
                if cpu_only([arg.decode(errors="replace") for arg in args]):
                    self.cpu_reason = "cpu_only_server"
            except (OSError, ValueError, IndexError):
                self.target_reason = "server_identity_unavailable"
        self.gpu_executable = shutil.which("nvidia-smi")
        wsl_gpu = Path("/usr/lib/wsl/lib/nvidia-smi")
        if self.gpu_executable is None and wsl_gpu.is_file():
            self.gpu_executable = str(wsl_gpu)
        self.stop = threading.Event()
        self.thread = None

    def _same_process(self):
        try:
            return self.start_ticks is not None and process_start(self.pid) == self.start_ticks
        except (OSError, ValueError, IndexError):
            return False

    def sample(self):
        begin = time.perf_counter_ns()
        ram, vram = None, None
        ram_reason = vram_reason = self.target_reason
        if ram_reason is None:
            if not self._same_process():
                ram_reason = vram_reason = "server_exited_or_identity_changed"
            else:
                try:
                    ram = rss_mib(self.pid)
                except (OSError, ValueError):
                    ram_reason = "rss_counter_unavailable"
                if self.cpu_reason:
                    vram_reason = self.cpu_reason
                elif self.gpu_reason:
                    vram_reason = self.gpu_reason
                else:
                    vram, vram_reason = gpu_memory_mib(self.gpu_executable, self.pid)
                # Reject readings if the PID was recycled or exited during collection.
                if not self._same_process():
                    ram = vram = None
                    ram_reason = vram_reason = "server_exited_or_identity_changed"
        self.samples.append(
            {
                "start_perf_ns": begin,
                "end_perf_ns": time.perf_counter_ns(),
                "ram_mib": ram,
                "vram_mib": vram,
                "ram_unavailable_reason": ram_reason,
                "vram_unavailable_reason": vram_reason,
            }
        )

    def _loop(self):
        while not self.stop.wait(self.interval):
            self.sample()

    def __enter__(self):
        self.sample()
        self.thread = threading.Thread(target=self._loop, name="server-memory")
        self.thread.start()
        return self

    def __exit__(self, *_):
        self.stop.set()
        self.thread.join()
        self.sample()

    def summary(self):
        return {
            "schema_version": 1,
            "unit": "MiB",
            "scope": "server_process_case",
            "server_pid": self.pid,
            "server_start_ticks": self.start_ticks,
            "clock": "perf_counter_ns",
            "sampling_interval_seconds": self.interval,
            "window": "around AIPerf case, including setup, warm-up and teardown; excludes model startup",
            "ram_source": "/proc/PID/status VmRSS (resident, includes shared/file-backed pages)",
            "vram_source": "nvidia-smi per-PID used_gpu_memory, summed across GPU UUIDs",
            "peak_kind": "maximum observed sample; may miss spikes",
            "baseline_kind": "first pre-case observation; not guaranteed idle",
            "ram": counter_summary(self.samples, "ram"),
            "vram": counter_summary(self.samples, "vram"),
        }

    def save(self, directory):
        directory.mkdir(parents=True, exist_ok=True)
        summary = self.summary()
        with (directory / "memory_samples.jsonl").open("x", encoding="utf-8") as stream:
            for sample in self.samples:
                stream.write(json.dumps(sample, allow_nan=False) + "\n")
        with (directory / "memory_summary.json").open("x", encoding="utf-8") as stream:
            json.dump(summary, stream, indent=2, allow_nan=False)
            stream.write("\n")
        return summary

    def request_fields(self, row):
        start, end = row.get("request_start_perf_ns"), row.get("completion_perf_ns")
        valid = type(start) is int and type(end) is int and 0 <= start <= end
        samples = [
            sample
            for sample in self.samples
            if valid and start <= sample["start_perf_ns"] <= sample["end_perf_ns"] <= end
        ]
        result = {
            "memory_scope": "server_process_request_window",
            "memory_server_pid": self.pid,
            "memory_sampling_interval_seconds": self.interval,
        }
        summary = self.summary()
        for name in ("ram", "vram"):
            metric = counter_summary(samples, name)
            reason = metric["unavailable_reason"]
            if not metric["sample_count"]:
                if summary[name]["sample_count"] == 0:
                    reason = summary[name]["unavailable_reason"]
                elif not valid:
                    reason = "request_window_unavailable"
                elif not samples:
                    reason = "no_samples_in_request_window"
            result.update(
                {
                    f"peak_{name}_mib": metric["peak_mib"],
                    f"{name}_sample_count": metric["sample_count"],
                    f"{name}_unavailable_sample_count": metric["unavailable_sample_count"],
                    f"{name}_measurement_status": metric["status"],
                    f"{name}_unavailable_reason": reason,
                }
            )
        return result


def uncollected_fields():
    result = {
        "memory_scope": "server_process_request_window",
        "memory_server_pid": None,
        "memory_sampling_interval_seconds": None,
    }
    for name in ("ram", "vram"):
        result.update(
            {
                f"peak_{name}_mib": None,
                f"{name}_sample_count": 0,
                f"{name}_unavailable_sample_count": 0,
                f"{name}_measurement_status": "unavailable",
                f"{name}_unavailable_reason": "memory_not_collected",
            }
        )
    return result
