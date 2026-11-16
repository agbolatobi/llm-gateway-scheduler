"""Gracefully stop this project's llama-server on Linux/WSL."""

import argparse
import fcntl
import math
import os
from pathlib import Path
import select
import signal
import sys
import time

from client import server


def _explicit_port(args):
    """Require one explicit port; never guess from a default or environment."""
    values = []
    for index, arg in enumerate(args[1:], 1):
        if arg == "--port":
            if index + 1 >= len(args):
                return None
            values.append(args[index + 1])
        elif arg.startswith("--port="):
            values.append(arg.split("=", 1)[1])
    if len(values) != 1:
        return None
    try:
        return int(values[0])
    except ValueError:
        return None


def _matches(pid, port):
    """Inspect the process, not a substring match against its command line."""
    process = Path("/proc") / str(pid)
    try:
        if process.stat().st_uid != os.geteuid():
            return False
        if (process / "exe").resolve(strict=True) != server.SERVER_BINARY.resolve():
            return False
        if (process / "cwd").resolve(strict=True) != server.PROJECT_ROOT.resolve():
            return False
        args = [os.fsdecode(arg) for arg in (process / "cmdline").read_bytes().split(b"\0") if arg]
        return _explicit_port(args) == port
    except (OSError, RuntimeError):
        # Vanished or inaccessible processes must never be signalled.
        return False


def _find_server(port):
    matches = [
        int(entry.name)
        for entry in Path("/proc").iterdir()
        if entry.name.isdecimal() and _matches(int(entry.name), port)
    ]
    if len(matches) > 1:
        raise RuntimeError(f"Multiple matching servers: {matches}. No process was stopped.")
    return matches[0] if matches else None


def _terminate(pid, port, timeout):
    # A pidfd refers to this process even if Linux later reuses its numeric PID.
    if not hasattr(os, "pidfd_open") or not hasattr(signal, "pidfd_send_signal"):
        raise RuntimeError("Safe stopping requires Python/Linux pidfd support. No signal sent.")
    try:
        descriptor = os.pidfd_open(pid)
    except ProcessLookupError:
        return f"Server PID {pid} already exited."
    try:
        poller = select.poll()
        poller.register(descriptor, select.POLLIN)
        if poller.poll(0):
            return f"Server PID {pid} already exited."
        if not _matches(pid, port):
            raise RuntimeError("Server identity changed. No signal sent; run --dry-run again.")
        try:
            signal.pidfd_send_signal(descriptor, signal.SIGTERM)
        except ProcessLookupError:
            return f"Server PID {pid} already exited."
        if not poller.poll(math.ceil(timeout * 1000)):
            raise RuntimeError(
                f"PID {pid} did not exit within {timeout:g}s after SIGTERM. "
                "It was not force-killed; inspect the server logs."
            )
        return f"Stopped project server PID {pid} on port {port}."
    finally:
        os.close(descriptor)


def stop_server(port=8080, *, timeout=10, dry_run=False):
    if not 1 <= port <= 65535:
        raise ValueError("--port must be between 1 and 65535")
    if not math.isfinite(timeout) or not 0 < timeout <= 3600:
        raise ValueError("--timeout must be positive and at most 3600 seconds")

    def result():
        pid = _find_server(port)
        if pid is None:
            return f"No matching project server on port {port}; no process was stopped."
        if dry_run:
            return f"Would stop project server PID {pid} on port {port}: {server.SERVER_BINARY}"
        return _terminate(pid, port, timeout)

    if dry_run:
        return result()  # Preview does not create files or send signals.

    lock_path = server.PROJECT_ROOT / ".runtime" / f"llama-server-{port}.lock"
    lock_path.parent.mkdir(parents=True, exist_ok=True)
    with lock_path.open("a") as lock:
        # Coordinate with --start-server and other stop commands. Never unlink it.
        deadline = time.monotonic() + timeout
        while True:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise RuntimeError("Timed out waiting for startup/stop lock. No signal sent.")
                time.sleep(min(0.1, remaining))
        return result()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--port", type=int, default=8080, help="Server port (default: 8080)")
    parser.add_argument("--dry-run", action="store_true", help="Preview without changing anything")
    parser.add_argument(
        "--timeout",
        type=float,
        default=10,
        help="Seconds allowed for locking and, separately, graceful exit (default: 10)",
    )
    args = parser.parse_args()
    try:
        print(stop_server(args.port, timeout=args.timeout, dry_run=args.dry_run))
        return 0
    except KeyboardInterrupt:
        print("Stop interrupted; check whether the server is still running.", file=sys.stderr)
        return 130
    except (OSError, RuntimeError, ValueError) as error:
        print(f"Stop failed: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
