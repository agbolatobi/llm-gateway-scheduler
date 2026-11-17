"""Offline checks for the standalone model downloader."""

import importlib.util
import io
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("model_download", Path(__file__).resolve().parents[1] / "models/download.py")
downloader = importlib.util.module_from_spec(spec)
spec.loader.exec_module(downloader)


class ModelDownloadTests(unittest.TestCase):
    def test_download_validation_and_cleanup(self):
        for data, size, succeeds in [(b"GGUFtest", "8", True), (b"GGUFtest", "20", False), (b"html", "4", False)]:
            with self.subTest(data=data, size=size), tempfile.TemporaryDirectory() as directory:
                destination = Path(directory) / "model.gguf"
                response = io.BytesIO(data)
                response.headers = {"Content-Length": size}
                with patch.object(downloader.urllib.request, "urlopen", return_value=response):
                    if succeeds:
                        downloader.download(destination)
                        self.assertEqual(destination.read_bytes(), data)
                    else:
                        with self.assertRaises(ValueError):
                            downloader.download(destination)
                        self.assertFalse(destination.exists())
                self.assertEqual(list(Path(directory).glob("*.partial")), [])

    def test_existing_model_is_preserved_without_network(self):
        with tempfile.TemporaryDirectory() as directory:
            destination = Path(directory) / "model.gguf"
            destination.write_bytes(b"existing")
            with patch.object(downloader.urllib.request, "urlopen") as request:
                downloader.download(destination)
            request.assert_not_called()
            self.assertEqual(destination.read_bytes(), b"existing")
