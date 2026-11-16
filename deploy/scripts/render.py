"""Render portable or GKE manifests from ignored personal inputs."""
import argparse
import hashlib
import re
import shutil
from common import ROOT, VARIABLES, DEFAULT_ENV, executable, load_json, read_settings, run, save_json


def validate_settings(settings):
    required = ["TARGET", "GATEWAY_IMAGE", "RUNTIME_IMAGE", "MODEL_SHA256", "MODEL_ID"]
    if settings.get("TARGET") == "gke":
        required += ["MODEL_URI", "MODEL_SERVICE_ACCOUNT"]
    if settings.get("TARGET") not in ("base", "gke"):
        raise ValueError("TARGET must be base or gke")
    for name in required:
        if not settings.get(name) or "REPLACE" in settings[name] or "YOUR_" in settings[name]:
            raise ValueError(f"Fill in {name} in the workload input file")
    for name in ("GATEWAY_IMAGE", "RUNTIME_IMAGE"):
        if not re.fullmatch(r"[A-Za-z0-9._/:-]+@sha256:[a-f0-9]{64}", settings[name]):
            raise ValueError(f"{name} must be an immutable image@sha256:digest")
    if not re.fullmatch(r"[a-f0-9]{64}", settings["MODEL_SHA256"]):
        raise ValueError("MODEL_SHA256 must contain 64 lowercase hexadecimal characters")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}", settings["MODEL_ID"]):
        raise ValueError("MODEL_ID must be a valid gateway model identifier")
    if settings.get("GPU", "false") not in ("true", "false"):
        raise ValueError("GPU must be true or false")
    context = int(settings.get("CONTEXT_TOKENS", 4096))
    default = int(settings.get("DEFAULT_OUTPUT_TOKENS", 1024))
    maximum = int(settings.get("MAX_OUTPUT_TOKENS", 2048))
    if not 1 <= default <= maximum < context <= 1 << 24:
        raise ValueError("Require 1 <= default output <= maximum output < context <= 16777216")
    if not 1 <= int(settings.get("RUNTIME_THREADS", 2)) <= 4096:
        raise ValueError("RUNTIME_THREADS must be 1-4096")


def render(settings, destination=None):
    validate_settings(settings)
    destination = destination or VARIABLES / "rendered"
    destination.mkdir(parents=True, exist_ok=True)
    # Copy only committed templates. Personal inputs and credentials never enter manifests.
    for directory in ("base", "overlays"):
        shutil.copytree(ROOT / "deploy" / directory, destination / directory, dirs_exist_ok=True)
    base = destination / "base"
    config = load_json(base / "gateway.config.json")
    model = config["models"][0]
    model["id"] = settings["MODEL_ID"]
    model["backend"]["expected_model"] = settings["MODEL_ID"]
    model["aliases"] = [] if model["id"] == "default" else ["default"]
    model["context_limit"] = int(settings.get("CONTEXT_TOKENS", 4096))
    model["default_output_tokens"] = int(settings.get("DEFAULT_OUTPUT_TOKENS", 1024))
    model["max_output_tokens"] = int(settings.get("MAX_OUTPUT_TOKENS", 2048))
    save_json(base / "gateway.config.json", config)
    deployment = load_json(base / "deployment.json")
    pod = deployment["spec"]["template"]["spec"]
    gateway = pod["containers"][0]
    initialize, verify, runtime = pod["initContainers"]
    gateway["image"] = initialize["image"] = settings["GATEWAY_IMAGE"]
    verify["image"] = runtime["image"] = settings["RUNTIME_IMAGE"]
    verify["env"][0]["value"] = settings["MODEL_SHA256"]
    for flag, value in {"--alias": model["id"], "--ctx-size": model["context_limit"], "--threads": settings.get("RUNTIME_THREADS", "2")}.items():
        runtime["args"][runtime["args"].index(flag) + 1] = str(value)
    for category in ("requests", "limits"):
        suffix = "REQUEST" if category == "requests" else "LIMIT"
        for resource in ("cpu", "memory"):
            setting = f"RUNTIME_{resource.upper()}_{suffix}"
            if settings.get(setting):
                runtime["resources"][category][resource] = settings[setting]
    gpu = settings.get("GPU", "false") == "true"
    if gpu:
        runtime["args"][runtime["args"].index("--n-gpu-layers") + 1] = "99"
        runtime["resources"]["requests"]["nvidia.com/gpu"] = "1"
        runtime["resources"]["limits"]["nvidia.com/gpu"] = "1"
        pod["tolerations"] = [{"key":"nvidia.com/gpu", "operator":"Exists", "effect":"NoSchedule"}]
    save_json(base / "deployment.json", deployment)
    for name in ("keys", "models"):
        path = base / f"{name}-pvc.json"
        claim = load_json(path)
        setting = "KEY_STORAGE" if name == "keys" else "MODEL_STORAGE"
        if settings.get(setting):
            claim["spec"]["resources"]["requests"]["storage"] = settings[setting]
        if settings.get("STORAGE_CLASS"):
            claim["spec"]["storageClassName"] = settings["STORAGE_CLASS"]
        save_json(path, claim)
    target = base
    if settings["TARGET"] == "gke":
        target = destination / "overlays/gke"
        patch = load_json(target / "deployment-patch.json")
        downloader = patch[0]["value"]
        for environment in downloader["env"]:
            environment["value"] = settings[environment["name"]]
        patch[2]["value"]["workload"] = "gpu" if gpu else "cpu"
        save_json(target / "deployment-patch.json", patch)
        account = load_json(target / "account-patch.json")
        account["metadata"]["annotations"]["iam.gke.io/gcp-service-account"] = settings["MODEL_SERVICE_ACCOUNT"]
        save_json(target / "account-patch.json", account)
    manifest = destination / "deployment.yaml"
    result = run([executable("kubectl"), "kustomize", target], capture=True)
    manifest.write_text(result.stdout)
    save_json(destination / "release.json", {"gateway_image":settings["GATEWAY_IMAGE"], "runtime_image":settings["RUNTIME_IMAGE"], "model_sha256":settings["MODEL_SHA256"], "manifest_sha256":hashlib.sha256(result.stdout.encode()).hexdigest()})
    return manifest


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--env", type=str, default=DEFAULT_ENV)
    arguments = parser.parse_args()
    print(render(read_settings(arguments.env)))
