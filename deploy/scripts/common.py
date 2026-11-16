"""Shared paths and subprocess helpers; personal inputs are never executed as code."""
import json
from pathlib import Path
import shutil
import subprocess

ROOT = Path(__file__).resolve().parents[2]
VARIABLES = ROOT / "infra/variables"
DEFAULT_ENV = VARIABLES / "workload.env"


def executable(name):
    installed = shutil.which(name)
    local = ROOT / ".runtime/infra-tools" / name
    if installed:
        return installed
    if local.is_file():
        return str(local)
    raise SystemExit(f"Install {name} and add it to PATH.")


def run(arguments, *, capture=False, **options):
    return subprocess.run([str(argument) for argument in arguments], check=True,
                          text=True, capture_output=capture, **options)


def read_settings(path=DEFAULT_ENV):
    values = {}
    for number, line in enumerate(Path(path).read_text().splitlines(), start=1):
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        if "=" not in line:
            raise ValueError(f"{path}:{number}: expected KEY=value")
        name, value = line.split("=", 1)
        if name in values:
            raise ValueError(f"Duplicate setting: {name}")
        values[name] = value.strip()
    return values


def load_json(path):
    return json.loads(Path(path).read_text())


def save_json(path, value):
    Path(path).write_text(json.dumps(value, indent=2) + "\n")
