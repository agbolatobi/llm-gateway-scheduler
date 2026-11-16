import hashlib
import importlib.util
import io
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import urllib.error

SCRIPT = Path(__file__).resolve().parents[1] / "overlays/gke/download-model.py"
specification = importlib.util.spec_from_file_location("model_downloader", SCRIPT)
downloader = importlib.util.module_from_spec(specification)
specification.loader.exec_module(downloader)


class DownloadTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.destination = Path(self.temporary.name) / "model.gguf"
        self.content = b"verified model bytes"
        self.settings = {"MODEL_URI":"gs://example-bucket/model.gguf",
                         "MODEL_SHA256":hashlib.sha256(self.content).hexdigest()}

    def run_download(self):
        with patch.dict(os.environ, self.settings), patch.object(downloader, "Path", return_value=self.destination):
            downloader.download()

    def test_verified_cache_does_not_request_credentials(self):
        self.destination.write_bytes(self.content)
        with patch.object(downloader.urllib.request, "urlopen") as fetch:
            self.run_download()
            fetch.assert_not_called()

    def test_download_is_verified_before_replacing_model(self):
        replies = [io.BytesIO(b'{"access_token":"test-token"}'), io.BytesIO(self.content)]
        with patch.object(downloader.urllib.request, "urlopen", side_effect=replies) as fetch:
            self.run_download()
            self.assertEqual(fetch.call_args_list[0].args[0].get_header("Metadata-flavor"), "Google")
            self.assertEqual(fetch.call_args_list[1].args[0].get_header("Authorization"), "Bearer test-token")
        self.assertEqual(self.destination.read_bytes(), self.content)
        self.assertFalse(self.destination.with_suffix(".partial").exists())

    def test_checksum_failure_preserves_previous_model(self):
        self.destination.write_bytes(b"old model")
        replies = [io.BytesIO(b'{"access_token":"test-token"}'), io.BytesIO(b"corrupt model")]
        with patch.object(downloader.urllib.request, "urlopen", side_effect=replies):
            with self.assertRaisesRegex(ValueError, "checksum"):
                self.run_download()
        self.assertEqual(self.destination.read_bytes(), b"old model")
        self.assertFalse(self.destination.with_suffix(".partial").exists())

    def test_metadata_failure_retries_with_a_bound(self):
        with patch.object(downloader.urllib.request, "urlopen", side_effect=urllib.error.URLError("unavailable")) as fetch, patch.object(downloader.time, "sleep"):
            with self.assertRaisesRegex(RuntimeError, "Model download failed"):
                self.run_download()
            self.assertEqual(fetch.call_count, 12)
