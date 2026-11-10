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
    u = urlsplit(raw)
    if (u.scheme != "http" or u.hostname not in {"127.0.0.1", "localhost"}
            or u.username is not None or u.password is not None
            or u.path not in {"", "/"} or u.query or u.fragment or "?" in raw or "#" in raw
            or u.port is None or not 1 <= u.port <= 65535):
        raise ValueError("Use a loopback HTTP URL with an explicit port and no path or credentials")
    return f"http://127.0.0.1:{u.port}"

def object_fields(value, required, optional=()):
    if not isinstance(value, dict) or set(value) - set(required) - set(optional) or set(required) - set(value):
        raise ValueError("Missing or unsupported configuration fields")

def integer(value, low, high):
    if type(value) is not int or not low <= value <= high:
        raise ValueError(f"Configuration integer must be between {low} and {high}")

def limits(value):
    object_fields(value, ("requests", "tokens"))
    for number in value.values():
        integer(number, 1, 10**15)

def names(values, *, identifiers=False):
    if not isinstance(values, list) or any(not isinstance(v, str) or not v or v.strip() != v for v in values):
        raise ValueError("Names must be nonempty trimmed strings")
    if len(set(values)) != len(values) or (identifiers and any(not ID.fullmatch(v) for v in values)):
        raise ValueError("Duplicate or invalid names")

def validate(data):
    object_fields(data, ("version", "gateway", "quotas", "models"))
    integer(data["version"], 1, 1)
    object_fields(data["gateway"], ("listen",), ("max_active_requests",))
    if "max_active_requests" in data["gateway"]:
        integer(data["gateway"]["max_active_requests"], 1, 64)
    names([data["gateway"]["listen"]])
    listen = local_url("http://" + data["gateway"]["listen"])
    q = data["quotas"]
    object_fields(q, ("request_window_seconds", "token_window_seconds", "user", "global_ceiling"))
    integer(q["request_window_seconds"], 1, 86400)
    integer(q["token_window_seconds"], 1, 2592000)
    limits(q["user"]); limits(q["global_ceiling"])
    if not isinstance(data["models"], list) or not data["models"]:
        raise ValueError("At least one canonical model is required")
    all_names, runtimes, addresses = set(), set(), set()
    for m in data["models"]:
        object_fields(m, ("id", "context_limit", "default_output_tokens", "max_output_tokens", "limits", "backend"), ("aliases", "launch", "max_active_requests"))
        if "max_active_requests" in m:
            integer(m["max_active_requests"], 1, 1)
        if m.get("aliases") is not None:
            names(m["aliases"], identifiers=True)
        model_names = [m["id"], *(m.get("aliases") or [])]
        names(model_names, identifiers=True)
        if all_names.intersection(model_names):
            raise ValueError("Model IDs and aliases must be globally unique")
        all_names.update(model_names)
        integer(m["context_limit"], 2, 1 << 24)
        integer(m["default_output_tokens"], 1, m["context_limit"] - 1)
        integer(m["max_output_tokens"], m["default_output_tokens"], m["context_limit"] - 1)
        limits(m["limits"])
        b = m["backend"]
        object_fields(b, ("runtime_id", "engine", "base_url", "expected_model"))
        names([b["runtime_id"]], identifiers=True); names([b["expected_model"]])
        if b["engine"] != "llama.cpp":
            raise ValueError("Only llama.cpp is supported in this version")
        address = local_url(b["base_url"])
        if address == listen or address in addresses or b["runtime_id"] in runtimes:
            raise ValueError("Duplicate/conflicting backend address or runtime ID")
        addresses.add(address); runtimes.add(b["runtime_id"])
        launch = m.get("launch")
        if launch is not None:
            object_fields(launch, ("executable", "model_file", "device", "parallel", "threads"))
            for field in ("executable", "model_file", "device"):
                names([launch[field]])
            integer(launch["parallel"], 1, 1)
            integer(launch["threads"], 1, 4096)
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
        return {key: min(sum(m["limits"][key] for m in self.models), self.data["quotas"]["global_ceiling"][key]) for key in ("requests", "tokens")}
