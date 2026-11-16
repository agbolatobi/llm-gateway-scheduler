"""Read-only shared configuration. Registration is an explicit file edit."""

import json
from pathlib import Path
import re
from urllib.parse import urlsplit

PROJECT_ROOT = Path(__file__).resolve().parent.parent
DEFAULT_CONFIG = PROJECT_ROOT / "gateway.config.json"
ID = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}\Z")


def local_url(raw):
    if not isinstance(raw, str):
        raise ValueError("Backend URL must be text")
    parsed_url = urlsplit(raw)
    if (
        parsed_url.scheme != "http"
        or parsed_url.hostname not in {"127.0.0.1", "localhost"}
        or parsed_url.username is not None
        or parsed_url.password is not None
        or parsed_url.path not in {"", "/"}
        or parsed_url.query
        or parsed_url.fragment
        or "?" in raw
        or "#" in raw
        or parsed_url.port is None
        or not 1 <= parsed_url.port <= 65535
    ):
        raise ValueError(
            "Use a loopback HTTP URL with an explicit port and no path or credentials"
        )
    return f"http://127.0.0.1:{parsed_url.port}"


def object_fields(value, required, optional=()):
    if (
        not isinstance(value, dict)
        or set(value) - set(required) - set(optional)
        or set(required) - set(value)
    ):
        raise ValueError("Missing or unsupported configuration fields")


def integer(value, low, high):
    if type(value) is not int or not low <= value <= high:
        raise ValueError(f"Configuration integer must be between {low} and {high}")


def limits(value):
    object_fields(value, ("requests", "tokens"))
    for number in value.values():
        integer(number, 1, 10**15)


def names(values, *, identifiers=False):
    if not isinstance(values, list) or any(
        not isinstance(name, str) or not name or name.strip() != name for name in values
    ):
        raise ValueError("Names must be nonempty trimmed strings")
    if len(set(values)) != len(values) or (
        identifiers and any(not ID.fullmatch(name) for name in values)
    ):
        raise ValueError("Duplicate or invalid names")


def validate_scheduler(settings):
    object_fields(
        settings,
        (),
        (
            "policy",
            "max_waiting_requests",
            "max_waiting_per_runtime",
            "max_queue_wait_seconds",
            "aging_threshold_seconds",
        ),
    )
    if settings.get("policy", "fifo") not in ("fifo", "interactive_priority_aging"):
        raise ValueError("Unsupported scheduler policy")
    bounds = (
        ("max_waiting_requests", 0, 1024),
        ("max_waiting_per_runtime", 1, 1024),
        ("max_queue_wait_seconds", 1, 1800),
        ("aging_threshold_seconds", 1, 1800),
    )
    for field, minimum, maximum in bounds:
        if field in settings:
            integer(settings[field], minimum, maximum)


def validate_gateway(settings):
    object_fields(settings, ("listen",), ("max_active_requests", "scheduler"))
    if "max_active_requests" in settings:
        integer(settings["max_active_requests"], 1, 64)
    if "scheduler" in settings:
        validate_scheduler(settings["scheduler"])
    names([settings["listen"]])
    return local_url("http://" + settings["listen"])


def validate_quotas(settings):
    object_fields(
        settings, ("request_window_seconds", "token_window_seconds", "user", "global_ceiling")
    )
    integer(settings["request_window_seconds"], 1, 86400)
    integer(settings["token_window_seconds"], 1, 2592000)
    limits(settings["user"])
    limits(settings["global_ceiling"])


def validate_launch(settings):
    object_fields(settings, ("executable", "model_file", "device", "parallel", "threads"))
    for field in ("executable", "model_file", "device"):
        names([settings[field]])
    integer(settings["parallel"], 1, 1)
    integer(settings["threads"], 1, 4096)


def validate(data):
    """Validate sections in admission order, then enforce registry uniqueness."""
    object_fields(data, ("version", "gateway", "quotas", "models"))
    integer(data["version"], 1, 1)
    gateway_address = validate_gateway(data["gateway"])
    validate_quotas(data["quotas"])

    if not isinstance(data["models"], list) or not data["models"]:
        raise ValueError("At least one canonical model is required")

    registered_names = set()
    runtime_ids = set()
    backend_addresses = set()
    for model in data["models"]:
        object_fields(
            model,
            (
                "id",
                "context_limit",
                "default_output_tokens",
                "max_output_tokens",
                "limits",
                "backend",
            ),
            ("aliases", "launch", "max_active_requests"),
        )
        if "max_active_requests" in model:
            integer(model["max_active_requests"], 1, 1)
        if model.get("aliases") is not None:
            names(model["aliases"], identifiers=True)
        model_names = [model["id"], *(model.get("aliases") or [])]
        names(model_names, identifiers=True)
        if registered_names.intersection(model_names):
            raise ValueError("Model IDs and aliases must be globally unique")
        registered_names.update(model_names)

        integer(model["context_limit"], 2, 1 << 24)
        integer(model["default_output_tokens"], 1, model["context_limit"] - 1)
        integer(
            model["max_output_tokens"],
            model["default_output_tokens"],
            model["context_limit"] - 1,
        )
        limits(model["limits"])

        backend = model["backend"]
        object_fields(backend, ("runtime_id", "engine", "base_url", "expected_model"))
        names([backend["runtime_id"]], identifiers=True)
        names([backend["expected_model"]])
        if backend["engine"] != "llama.cpp":
            raise ValueError("Only llama.cpp is supported in this version")
        address = local_url(backend["base_url"])
        if (
            address == gateway_address
            or address in backend_addresses
            or backend["runtime_id"] in runtime_ids
        ):
            raise ValueError("Duplicate/conflicting backend address or runtime ID")
        backend_addresses.add(address)
        runtime_ids.add(backend["runtime_id"])

        if model.get("launch") is not None:
            validate_launch(model["launch"])
    return data


def no_duplicates(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("Duplicate configuration field")
        result[key] = value
    return result


class ModelConfig:
    def __init__(self, path=DEFAULT_CONFIG):
        self.path = Path(path).resolve()
        with self.path.open("rb") as source:
            raw = source.read((1 << 20) + 1)
        if len(raw) > 1 << 20:
            raise ValueError("Configuration exceeds 1 MiB")
        self.data = validate(json.loads(raw, object_pairs_hook=no_duplicates))

    @property
    def models(self):
        return self.data["models"]

    def find(self, name):
        for model in self.models:
            if name in [model["id"], *(model.get("aliases") or [])]:
                return model
        raise ValueError(f"Model {name!r} is not registered; edit {self.path} explicitly")

    def resolve_path(self, value):
        path = Path(value)
        return (self.path.parent / path).resolve() if not path.is_absolute() else path.resolve()

    def global_limits(self):
        return {
            key: min(
                sum(model["limits"][key] for model in self.models),
                self.data["quotas"]["global_ceiling"][key],
            )
            for key in ("requests", "tokens")
        }
