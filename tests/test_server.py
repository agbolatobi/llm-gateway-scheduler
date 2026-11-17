"""Lifecycle checks that do not load a model or touch an existing server."""

import errno
from pathlib import Path
import tempfile
import time
import unittest
from unittest.mock import Mock, patch

import requests

from client import server


class ServerStartupTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        binary = self.root / "llama-server"
        model = self.root / "model.gguf"
        binary.touch()
        model.touch()
        for name, value in (
            ("PROJECT_ROOT", self.root),
            ("SERVER_BINARY", binary),
            ("MODEL_FILE", model),
        ):
            patcher = patch.object(server, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)
        self.process = Mock()
        self.process.poll.return_value = None
        launcher = patch.object(server.subprocess, "Popen", return_value=self.process)
        self.launch = launcher.start()
        self.addCleanup(launcher.stop)

    def test_ready_server_is_reused_without_creating_runtime_files(self):
        with patch.object(server, "_health", return_value="ready"):
            self.assertIsNone(server.ensure_server_ready("http://127.0.0.1:8080"))
        self.launch.assert_not_called()
        self.assertFalse((self.root / ".runtime").exists())

    def test_existing_loading_server_is_waited_for(self):
        with patch.object(server, "_health", side_effect=["loading", "loading", "ready"]):
            self.assertIsNone(server.ensure_server_ready("http://localhost:8080"))
        self.launch.assert_not_called()

    def test_health_is_rechecked_after_acquiring_lock(self):
        with patch.object(server, "_health", side_effect=["missing", "ready"]):
            self.assertIsNone(server.ensure_server_ready("http://127.0.0.1:8080"))
        self.launch.assert_not_called()

    def test_missing_server_starts_once_and_persists_after_readiness(self):
        with patch.object(server, "_health", side_effect=["missing", "missing", "ready"]):
            log = server.ensure_server_ready("http://localhost:8081", model="test-model")
        self.assertEqual(log, self.root / ".runtime/llama-server-8081.log")
        self.launch.assert_called_once()
        command = self.launch.call_args.args[0]
        self.assertEqual(command[command.index("--port") + 1], "8081")
        self.assertEqual(command[command.index("--alias") + 1], "test-model")
        self.assertTrue(self.launch.call_args.kwargs["start_new_session"])
        self.assertEqual(self.launch.call_args.kwargs["stderr"], server.subprocess.STDOUT)
        self.process.terminate.assert_not_called()

    def test_startup_timeout_terminates_only_the_spawned_process(self):
        with patch.object(server, "_health", return_value="missing"):
            with self.assertRaisesRegex(RuntimeError, "Timed out"):
                server.ensure_server_ready("http://127.0.0.1:8080", timeout=0.05)
        self.launch.assert_called_once()
        self.process.terminate.assert_called_once()
        self.process.wait.assert_called_once()

    def test_existing_loading_server_is_not_terminated_on_timeout(self):
        with patch.object(server, "_health", return_value="loading"):
            with self.assertRaisesRegex(RuntimeError, "Timed out"):
                server.ensure_server_ready("http://127.0.0.1:8080", timeout=0.05)
        self.launch.assert_not_called()
        self.process.terminate.assert_not_called()

    def test_failed_process_reports_exit_code_and_log(self):
        self.process.poll.return_value = 1
        self.process.returncode = 1
        with patch.object(server, "_health", return_value="missing"):
            with self.assertRaisesRegex(RuntimeError, "exited with code 1.*llama-server-8080.log"):
                server.ensure_server_ready("http://127.0.0.1:8080")
        self.launch.assert_called_once()

    def test_unknown_network_error_does_not_start_a_server(self):
        with patch.object(server, "_health", side_effect=requests.Timeout("read timed out")):
            with self.assertRaises(requests.Timeout):
                server.ensure_server_ready("http://127.0.0.1:8080")
        self.launch.assert_not_called()

    def test_missing_model_has_a_useful_error(self):
        with patch.object(server, "MODEL_FILE", self.root / "absent.gguf"):
            with patch.object(server, "_health", return_value="missing"):
                with self.assertRaisesRegex(RuntimeError, "Model file not found.*models/README.md.*python3 .*models/download.py"):
                    server.ensure_server_ready("http://127.0.0.1:8080")
        self.launch.assert_not_called()

    def test_missing_custom_model_points_to_publisher(self):
        with patch.object(server, "_health", return_value="missing"):
            with self.assertRaisesRegex(RuntimeError, "Download the configured model from its publisher") as caught:
                server.ensure_server_ready("http://127.0.0.1:8080", model_file=self.root / "custom.gguf")
        self.assertNotIn("download.py", str(caught.exception))
        self.launch.assert_not_called()

    def test_remote_urls_and_invalid_timeouts_are_rejected(self):
        for address in ("https://localhost:8080", "http://example.com", "http://127.0.0.1:8080/v1"):
            with self.subTest(address=address), self.assertRaises(ValueError):
                server.ensure_server_ready(address)
        for timeout in (0, -1, float("inf"), float("nan")):
            with self.subTest(timeout=timeout), self.assertRaises(ValueError):
                server.ensure_server_ready("http://127.0.0.1:8080", timeout=timeout)
        self.launch.assert_not_called()


class HealthCheckTests(unittest.TestCase):
    def response_session(self, status, body):
        response = Mock()
        response.status_code = status
        response.json.return_value = body
        if status >= 400:
            response.raise_for_status.side_effect = requests.HTTPError(str(status))
        session = Mock()
        session.get.return_value.__enter__ = Mock(return_value=response)
        session.get.return_value.__exit__ = Mock(return_value=False)
        return session

    def test_health_distinguishes_loading_from_other_service_errors(self):
        ready = self.response_session(200, {"status": "ok"})
        loading = self.response_session(503, {"error": {"message": "Loading model"}})
        busy = self.response_session(503, {"error": {"message": "Unavailable"}})
        self.assertEqual(server._health(ready, "http://127.0.0.1:8080", time.monotonic() + 5), "ready")
        self.assertEqual(server._health(loading, "http://127.0.0.1:8080", time.monotonic() + 5), "loading")
        with self.assertRaises(requests.HTTPError):
            server._health(busy, "http://127.0.0.1:8080", time.monotonic() + 5)

    def test_only_connection_refusal_means_missing(self):
        session = Mock()
        wrapped = requests.ConnectionError("wrapped connection refusal")
        wrapped.__cause__ = ConnectionRefusedError(errno.ECONNREFUSED, "refused")
        session.get.side_effect = wrapped
        self.assertEqual(server._health(session, "http://127.0.0.1:8080", time.monotonic() + 5), "missing")
        session.get.side_effect = requests.ConnectionError("connection reset")
        with self.assertRaises(requests.ConnectionError):
            server._health(session, "http://127.0.0.1:8080", time.monotonic() + 5)


if __name__ == "__main__":
    unittest.main()
