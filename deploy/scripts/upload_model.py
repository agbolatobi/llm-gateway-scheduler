"""Upload a licensed GGUF and print its checksum; no model is downloaded implicitly."""
import argparse
import hashlib
from pathlib import Path
from common import DEFAULT_ENV, executable, read_settings, run

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("model", type=Path)
parser.add_argument("--env", default=DEFAULT_ENV)
arguments = parser.parse_args()
settings = read_settings(arguments.env)
uri = settings["MODEL_URI"]
if not uri.startswith("gs://") or "YOUR_" in uri:
    parser.error("Fill in MODEL_URI with a gs://bucket/object path")
with arguments.model.open("rb") as model:
    checksum = hashlib.file_digest(model, "sha256").hexdigest()
run([executable("gcloud"), "storage", "cp", "--no-clobber", arguments.model, uri])
print(f"MODEL_SHA256={checksum}\nUse a new object path when changing weights; existing objects are not overwritten.")
