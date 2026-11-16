"""Process ownership records and the per-port startup/shutdown lock.

A PID alone is not proof of ownership: Linux can reuse it. The saved start time,
executable and command fingerprint must still match before stopping a process.
"""

from contextlib import contextmanager
import fcntl
import hashlib
import json
import os
from pathlib import Path
import tempfile
import time
from urllib.parse import urlsplit

from . import server
from .model_config import local_url


def model_record_path(model):
    return (
        server.PROJECT_ROOT / ".runtime" / "models" / (model["backend"]["runtime_id"] + ".json")
    )


def capture_process_identity(process_id):
    process_directory = Path("/proc") / str(process_id)
    if process_directory.stat().st_uid != os.geteuid():
        raise RuntimeError("Process belongs to another user")
    # Fields after the final ')' begin with field 3; starttime is field 22.
    process_start_ticks = (process_directory / "stat").read_text().rsplit(")", 1)[1].split()[19]
    deadline = time.monotonic() + 1.0
    while True:
        command_line = (process_directory / "cmdline").read_bytes()
        executable = str((process_directory / "exe").resolve(strict=True))
        working_directory = str((process_directory / "cwd").resolve(strict=True))
        if (process_directory / "stat").read_text().rsplit(")", 1)[1].split()[
            19
        ] != process_start_ticks:
            raise RuntimeError("Process identity changed during capture")
        if command_line:
            return {
                "pid": process_id,
                "start_ticks": process_start_ticks,
                "executable": executable,
                "cwd": working_directory,
                "argv_sha256": hashlib.sha256(command_line).hexdigest(),
            }
        # WSL can briefly expose an empty cmdline immediately after exec.
        # Never persist that as a usable ownership fingerprint.
        if time.monotonic() >= deadline:
            raise RuntimeError("Process command line could not be verified")
        time.sleep(0.01)


def write_process_record(path, data):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    descriptor, temporary = tempfile.mkstemp(dir=path.parent, prefix=".model-")
    try:
        with os.fdopen(descriptor, "w") as output:
            json.dump(data, output)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


@contextmanager
def lock_runtime_port(address, timeout):
    port = urlsplit(local_url(address)).port
    path = server.PROJECT_ROOT / ".runtime" / f"llama-server-{port}.lock"
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a") as lock:
        deadline = time.monotonic() + timeout
        while True:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    raise RuntimeError("Timed out waiting for the model startup/stop lock")
                time.sleep(0.05)
        yield
