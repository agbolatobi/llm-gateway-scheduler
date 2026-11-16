"""Download private GCS weights using Workload Identity; never write credentials."""
import hashlib
import json
import os
from pathlib import Path
import re
import time
import urllib.error
import urllib.parse
import urllib.request


def checksum(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def download():
    model_uri = urllib.parse.urlsplit(os.environ["MODEL_URI"])
    expected = os.environ["MODEL_SHA256"]
    if model_uri.scheme != "gs" or not model_uri.netloc or not model_uri.path.strip("/") or model_uri.query or model_uri.fragment:
        raise ValueError("MODEL_URI must identify one gs://bucket/object")
    if not re.fullmatch(r"[a-f0-9]{64}", expected):
        raise ValueError("MODEL_SHA256 must contain 64 lowercase hexadecimal characters")
    destination = Path("/models/model.gguf")
    if destination.exists() and checksum(destination) == expected:
        print("Verified existing model; download skipped.")
        return
    bucket = urllib.parse.quote(model_uri.netloc, safe="")
    object_name = urllib.parse.quote(model_uri.path.lstrip("/"), safe="")
    download_url = f"https://storage.googleapis.com/download/storage/v1/b/{bucket}/o/{object_name}?alt=media"
    metadata_url = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"
    temporary = destination.with_suffix(".partial")
    # The GKE metadata service and IAM grants can take time to become ready.
    for attempt in range(12):
        try:
            metadata = urllib.request.Request(metadata_url, headers={"Metadata-Flavor": "Google"})
            with urllib.request.urlopen(metadata, timeout=10) as response:
                token = json.load(response)["access_token"]
            request = urllib.request.Request(download_url, headers={"Authorization": "Bearer " + token})
            digest = hashlib.sha256()
            with urllib.request.urlopen(request, timeout=60) as response, temporary.open("wb") as output:
                while chunk := response.read(1024 * 1024):
                    digest.update(chunk)
                    output.write(chunk)
            if digest.hexdigest() != expected:
                raise ValueError("Downloaded model checksum does not match MODEL_SHA256")
            temporary.chmod(0o644)
            temporary.replace(destination)
            print("Model downloaded and SHA-256 verified.")
            return
        except (urllib.error.URLError, TimeoutError):
            if attempt == 11:
                raise RuntimeError("Model download failed; check Workload Identity, bucket access and connectivity") from None
            time.sleep(10)
        finally:
            temporary.unlink(missing_ok=True)


if __name__ == "__main__":
    download()
