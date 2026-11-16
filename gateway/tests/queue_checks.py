"""Replay deterministic queue/cancellation tests against the Go implementation."""

import json
import shutil
import subprocess

from test_checks import CheckFailure


def run_checks(root, record):
    go = shutil.which("go") or "/usr/local/go/bin/go"
    pattern = "^(TestQueue.*|TestFIFOAndCapacityEligibility|TestPriorityAgingAndReload|TestBoundsCancellationExpiryAndClose|TestCancellationSelectionRaceDoesNotLeak|TestQuarantineDoesNotReleaseSlot|TestConfigDefaultsAndValidation)$"
    result = subprocess.run(
        [
            go,
            "test",
            "-race",
            "-count=1",
            "-json",
            "-run",
            pattern,
            "./internal/scheduler",
            "./internal/api",
        ],
        cwd=root / "gateway",
        capture_output=True,
        text=True,
        timeout=90,
    )
    passed = []
    failed = []
    for line in result.stdout.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        name = event.get("Test", "")
        if name and "/" not in name:
            if event.get("Action") == "pass":
                passed.append(name)
                record("queue/" + name)
            elif event.get("Action") == "fail":
                failed.append(name)
    if result.returncode or not passed:
        raise CheckFailure(
            "queue/" + (failed[0] if failed else "test_runner"),
            "replay queue tests",
            "Queue regression tests failed; see actual output.",
            "All selected Go tests pass",
            {"exit_code": result.returncode, "output": (result.stdout + result.stderr)[-4000:]},
        )
