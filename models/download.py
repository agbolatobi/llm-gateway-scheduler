"""Download the default local model using only the Python standard library."""

from pathlib import Path
import shutil
import sys
import tempfile
import urllib.error
import urllib.request

MODEL_NAME = "qwen2.5-1.5b-instruct-q4_k_m.gguf"
MODEL_URL = (
    "https://huggingface.co/Qwen/Qwen2.5-1.5B-Instruct-GGUF/resolve/main/"
    + MODEL_NAME
)


def download(destination=None):
    destination = Path(destination) if destination is not None else Path(__file__).resolve().parent / MODEL_NAME
    if destination.is_file():
        print(f"Model already exists: {destination}")
        return
    destination.parent.mkdir(parents=True, exist_ok=True)
    temporary = None
    try:
        print(f"Downloading {MODEL_NAME} (about 1.12 GB) to {destination}...", flush=True)
        with urllib.request.urlopen(MODEL_URL, timeout=60) as response:
            with tempfile.NamedTemporaryFile(dir=destination.parent, suffix=".partial", delete=False) as output:
                temporary = Path(output.name)
                shutil.copyfileobj(response, output, length=1024 * 1024)
            expected_size = response.headers.get("Content-Length")
        if expected_size is not None and temporary.stat().st_size != int(expected_size):
            raise ValueError("Incomplete model download; run the script again.")
        with temporary.open("rb") as source:
            if source.read(4) != b"GGUF":
                raise ValueError("Downloaded file is not a GGUF model; run the script again.")
        temporary.replace(destination)
        print(f"Model downloaded: {destination}")
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)


if __name__ == "__main__":
    try:
        download()
    except (OSError, ValueError, urllib.error.URLError) as error:
        print(f"Model download failed: {error}. See models/README.md for manual download instructions.", file=sys.stderr)
        sys.exit(1)
    except KeyboardInterrupt:
        print("Model download interrupted. Run the script again to retry.", file=sys.stderr)
        sys.exit(130)
