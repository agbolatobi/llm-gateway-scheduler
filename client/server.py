"""Start and reuse the single local llama.cpp server on Linux/WSL."""

import errno
import fcntl
import math
import shlex
from pathlib import Path
import subprocess
import time
from urllib.parse import urlsplit

import requests

PROJECT_ROOT = Path(__file__).resolve().parent.parent
SERVER_BINARY = PROJECT_ROOT / "runtime/llama-b11669/llama-server"
MODEL_FILE = PROJECT_ROOT / "models/qwen2.5-1.5b-instruct-q4_k_m.gguf"


def launch_options(options=None):
    """Validated overrides for a new server; existing servers are never reconfigured."""
    options = dict(options or {})
    integers = {
        "ctx_size",
        "parallel",
        "threads",
        "threads_batch",
        "batch_size",
        "ubatch_size",
        "log_verbosity",
    }
    booleans = {"cont_batching", "kv_unified"}
    if options.keys() - integers - booleans:
        raise ValueError("Unknown server startup option")
    for key, value in options.items():
        if key in integers and (
            type(value) is not int or value < (0 if key == "log_verbosity" else 1)
        ):
            raise ValueError(f"{key} must be a positive integer (verbosity may be zero)")
        if key in booleans and type(value) is not bool:
            raise ValueError(f"{key} must be boolean")
    if options.get("ubatch_size", 512) > options.get("batch_size", 2048):
        raise ValueError("ubatch_size must not exceed batch_size")
    if options.get("ctx_size", 4096) < options.get("parallel", 1):
        raise ValueError("ctx_size must be at least parallel")
    values = {"ctx_size": 4096, "parallel": 1, **options}
    result = []
    for key, value in values.items():
        flag = key.replace("_", "-")
        if key in booleans:
            result.append("--" + ("" if value else "no-") + flag)
        else:
            result += ["--" + flag, str(value)]
    return result


def _local_address(base_url):
    url = urlsplit(base_url)
    if (
        url.scheme != "http"
        or url.hostname not in {"127.0.0.1", "localhost"}
        or url.path not in {"", "/"}
        or url.username is not None
        or url.password is not None
        or url.query
        or url.fragment
    ):
        raise ValueError(
            "--start-server requires a local HTTP URL such as "
            "http://127.0.0.1:8080, without a path or credentials."
        )
    port = url.port if url.port is not None else 80
    if port == 0:
        raise ValueError("The server port must be between 1 and 65535.")
    return f"http://127.0.0.1:{port}", port


def _remaining(deadline):
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise RuntimeError("Timed out waiting for the local model to become ready.")
    return remaining


def _is_connection_refused(error):
    # Requests wraps the socket error in urllib3 exceptions.
    seen = set()
    while error is not None and id(error) not in seen:
        seen.add(id(error))
        if isinstance(error, OSError) and error.errno == errno.ECONNREFUSED:
            return True
        error = error.__cause__ or error.__context__
    return False


def _health(session, address, deadline):
    try:
        with session.get(
            f"{address}/health",
            timeout=min(2, _remaining(deadline)),
            allow_redirects=False,
        ) as response:
            # Only the documented loading response is safe to wait on.
            if response.status_code in {200, 503}:
                body = response.json()
                if isinstance(body, dict):
                    if response.status_code == 200 and body.get("status") == "ok":
                        return "ready"
                    error = body.get("error")
                    if (
                        response.status_code == 503
                        and isinstance(error, dict)
                        and error.get("message") == "Loading model"
                    ):
                        return "loading"
            response.raise_for_status()
            raise RuntimeError("Unexpected health response; no server was started.")
    except requests.ConnectionError as error:
        if _is_connection_refused(error):
            return "missing"
        raise


def _stop_owned_process(process):
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)


def ensure_server_ready(
    base_url,
    *,
    model="local-llm",
    timeout=120,
    server_options=None,
    on_start=None,
    model_file=None,
    server_binary=None,
    device="none",
    verify_ready=None,
):
    """Return the log path if started, or None if an existing server was reused.

    Successfully started servers persist after the CLI exits. A failed or
    interrupted startup cleans up only the process created by this call.
    server_options apply only on launch. on_start observes the owned Popen and
    log path immediately after launch, before readiness (used by experiments).
    """
    if not math.isfinite(timeout) or timeout <= 0:
        raise ValueError("Startup timeout must be a positive, finite number.")
    address, port = _local_address(base_url)
    binary = Path(server_binary) if server_binary is not None else SERVER_BINARY
    weights = Path(model_file) if model_file is not None else MODEL_FILE
    extra_options = launch_options(server_options)
    deadline = time.monotonic() + timeout
    process = None
    log_path = PROJECT_ROOT / ".runtime" / f"llama-server-{port}.log"

    with requests.Session() as session:
        session.trust_env = False  # Local readiness checks must not use a proxy.
        if _health(session, address, deadline) == "ready":
            if verify_ready is not None:
                verify_ready()
            return None

        log_path.parent.mkdir(parents=True, exist_ok=True)
        # localhost and 127.0.0.1 share the same per-port startup lock.
        lock_path = log_path.with_suffix(".lock")
        with lock_path.open("a") as lock:
            while True:
                _remaining(deadline)
                try:
                    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    break
                except BlockingIOError:
                    time.sleep(min(0.2, _remaining(deadline)))

            try:
                while True:
                    _remaining(deadline)
                    if process is not None and process.poll() is not None:
                        raise RuntimeError(
                            f"llama-server exited with code {process.returncode}. "
                            f"See {log_path}"
                        )
                    state = _health(session, address, deadline)
                    if state == "ready":
                        if verify_ready is not None:
                            verify_ready()
                        return log_path if process is not None else None

                    if state == "missing" and process is None:
                        if not weights.is_file():
                            guidance = f"See {PROJECT_ROOT / 'models/README.md'} for download instructions."
                            if weights.resolve() == MODEL_FILE.resolve():
                                guidance += (
                                    " Download the default model by running: python3 "
                                    + shlex.quote(str(PROJECT_ROOT / "models/download.py"))
                                )
                            else:
                                guidance += " Download the configured model from its publisher to this path."
                            raise RuntimeError(f"Model file not found: {weights}. {guidance}")
                        if not binary.is_file():
                            raise RuntimeError(f"Server executable not found: {binary}")
                        command = [
                            str(binary),
                            "--model",
                            str(weights),
                            "--alias",
                            model,
                            "--host",
                            "127.0.0.1",
                            "--port",
                            str(port),
                            "--device",
                            device,
                            *extra_options,
                        ]
                        with log_path.open("ab") as log:
                            process = subprocess.Popen(
                                command,
                                cwd=PROJECT_ROOT,
                                stdin=subprocess.DEVNULL,
                                stdout=log,
                                stderr=subprocess.STDOUT,
                                start_new_session=True,
                            )
                        if on_start is not None:
                            on_start(process, log_path)
                    time.sleep(min(0.2, _remaining(deadline)))
            except BaseException as error:
                if process is not None:
                    _stop_owned_process(process)
                if isinstance(error, (OSError, RuntimeError)):
                    raise RuntimeError(f"{error} Startup log: {log_path}") from error
                raise
