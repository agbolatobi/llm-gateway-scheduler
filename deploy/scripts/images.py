"""Build pinned CPU/GPU images locally; publish only with --push."""
import argparse
import re
from common import ROOT, VARIABLES, DEFAULT_ENV, executable, load_json, read_settings, run

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--env", default=DEFAULT_ENV)
parser.add_argument("--tag", required=True, help="A unique release tag, e.g. test-001")
parser.add_argument("--push", action="store_true")
arguments = parser.parse_args()
if not re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}", arguments.tag):
    parser.error("Invalid image tag")
settings = read_settings(arguments.env)
repository = settings["IMAGE_REPOSITORY"].rstrip("/")
if "YOUR_" in repository:
    parser.error("Fill in IMAGE_REPOSITORY")
pins = load_json(ROOT / "docker/images.lock.json")
docker = executable("docker")
images = []
for name in ("gateway", "runtime"):
    image = f"{repository}/{name}:{arguments.tag}"
    command = [docker, "build", "--platform", "linux/amd64", "-f", ROOT / f"docker/{name}.Dockerfile", "-t", image]
    if name == "runtime":
        command += ["--build-arg", "LLAMA_IMAGE=" + pins["gpu" if settings.get("GPU") == "true" else "cpu"]]
    run([*command, ROOT])
    if arguments.push:
        run([docker, "push", image])
        inspection = run([docker, "buildx", "imagetools", "inspect", image], capture=True).stdout
        digest = re.search(r"^Digest:\s+(sha256:[a-f0-9]{64})", inspection, re.MULTILINE)
        if not digest:
            raise RuntimeError("Registry did not return an immutable digest")
        images.append(f"{name.upper()}_IMAGE={repository}/{name}@{digest.group(1)}")
if images:
    VARIABLES.mkdir(parents=True, exist_ok=True)
    output = VARIABLES / "images.env"
    output.write_text("\n".join(images) + "\n")
    print(f"Copy these two values into your workload.env:\n{output.read_text()}")
