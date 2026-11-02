import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from client.auth import saved_api_key
from client.llm import LLMClient


class ClientKeyTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name) / "credentials.json"
        self.token = "lgw_" + "1" * 16 + "_" + "A" * 43
        self.value = {
            "version": 1, "active_key_id": "1" * 16,
            "keys": {"1" * 16: {"user_id": "alice", "api_key": self.token,
                                  "base_url": "http://127.0.0.1:8000"}},
        }
        self.save()
        self.env = patch.dict(os.environ, {"LLM_GATEWAY_CREDENTIALS": str(self.path)})
        self.env.start()
        self.addCleanup(self.env.stop)

    def save(self):
        self.path.write_text(json.dumps(self.value), encoding="utf-8")
        self.path.chmod(0o600)

    def test_saved_key_loaded_for_matching_origin_only(self):
        for url in ("http://127.0.0.1:8000", "http://localhost:8000/"):
            with self.subTest(url=url):
                self.assertEqual(saved_api_key(url), self.token)
        for url in ("http://127.0.0.1:8080", "http://example.com:8000",
                    "http://127.0.0.1:8000/other", "http://user@localhost:8000",
                    "http://localhost:8000?x=1", "http://localhost:99999",
                    "https://localhost:8000"):
            with self.subTest(url=url):
                self.assertIsNone(saved_api_key(url))

    def test_missing_credentials_keeps_direct_client_working(self):
        self.path.unlink()
        self.assertIsNone(saved_api_key("http://127.0.0.1:8000"))

    def test_insecure_file_and_directory_rejected(self):
        self.path.chmod(0o644)
        with self.assertRaises(ValueError):
            saved_api_key("http://127.0.0.1:8000")
        self.path.chmod(0o600)
        self.path.parent.chmod(0o755)
        try:
            with self.assertRaises(ValueError):
                saved_api_key("http://127.0.0.1:8000")
        finally:
            self.path.parent.chmod(0o700)

    def test_corrupt_credentials_do_not_leak_their_content(self):
        self.path.write_text(self.token, encoding="utf-8")
        with self.assertRaises(ValueError) as raised:
            saved_api_key("http://127.0.0.1:8000")
        self.assertNotIn(self.token, str(raised.exception))

    def test_header_sent_and_redirects_disabled_for_gateway(self):
        with patch("client.llm.requests.post") as post:
            LLMClient(base_url="http://127.0.0.1:8000")._request(
                [{"role": "user", "content": "hello"}], stream=False)
        self.assertEqual(post.call_args.kwargs["headers"]["Authorization"],
                         "Bearer " + self.token)
        self.assertFalse(post.call_args.kwargs["allow_redirects"])

    def test_no_gateway_key_sent_to_llama_or_remote_server(self):
        for url in ("http://127.0.0.1:8080", "http://example.com:8000"):
            with self.subTest(url=url), patch("client.llm.requests.post") as post:
                LLMClient(base_url=url)._request([], stream=False)
                self.assertNotIn("headers", post.call_args.kwargs)

    def test_changed_selection_is_loaded_on_next_request(self):
        self.value["active_key_id"] = ""
        self.save()
        self.assertIsNone(saved_api_key("http://127.0.0.1:8000"))
