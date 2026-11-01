"""Stop tests use isolated, owned Python subprocesses; never a model server."""

import fcntl
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import stop_server


class StopServerTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        for name, value in (
            ("PROJECT_ROOT", self.root),
            ("SERVER_BINARY", Path(sys.executable).resolve()),
        ):
            patcher = patch.object(stop_server.server, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)

    def spawn(self, *, port="8080", cwd=None, ignore_term=False):
        program = "import signal,time; "
        if ignore_term:
            program += "signal.signal(signal.SIGTERM, signal.SIG_IGN); "
        program += "print('ready', flush=True); time.sleep(60)"
        command = [sys.executable, "-u", "-c", program]
        if port is not None:
            command += ["--port", port]
        child = subprocess.Popen(
            command, cwd=cwd or self.root, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, text=True,
        )

        def cleanup():
            if child.poll() is None:
                child.kill()
            child.communicate(timeout=5)

        self.addCleanup(cleanup)
        self.assertEqual(child.stdout.readline().strip(), "ready")
        return child

    def test_port_parsing_requires_one_explicit_value(self):
        for args, expected in (
            (["server", "--port", "8080"], 8080),
            (["server", "--port=8081"], 8081),
            (["server"], None),
            (["server", "--port"], None),
            (["server", "--port", "bad"], None),
            (["server", "--port", "8080", "--port=8081"], None),
        ):
            with self.subTest(args=args):
                self.assertEqual(stop_server._explicit_port(args), expected)

    def test_invalid_arguments_are_rejected_before_process_discovery(self):
        with patch.object(stop_server, "_find_server") as find:
            for port in (0, 65536):
                with self.assertRaises(ValueError):
                    stop_server.stop_server(port)
            for timeout in (0, -1, float("nan"), float("inf"), 3601):
                with self.assertRaises(ValueError):
                    stop_server.stop_server(timeout=timeout)
            find.assert_not_called()

    def test_dry_run_does_not_stop_or_create_runtime_files(self):
        child = self.spawn()
        self.assertIn(f"Would stop project server PID {child.pid}", stop_server.stop_server(dry_run=True))
        self.assertIsNone(child.poll())
        self.assertFalse((self.root / ".runtime").exists())

    def test_stops_only_matching_process_and_keeps_files(self):
        selected = self.spawn()
        other_port = self.spawn(port="8081")
        runtime = self.root / ".runtime"
        runtime.mkdir()
        log = runtime / "llama-server-8080.log"
        log.write_text("keep logs")
        self.assertIn(f"Stopped project server PID {selected.pid}", stop_server.stop_server())
        self.assertEqual(selected.wait(timeout=5), -signal.SIGTERM)
        self.assertIsNone(other_port.poll())
        self.assertEqual(log.read_text(), "keep logs")
        self.assertTrue((runtime / "llama-server-8080.lock").exists())

    def test_wrong_project_is_not_stopped(self):
        other_directory = self.root / "another-project"
        other_directory.mkdir()
        child = self.spawn(cwd=other_directory)
        self.assertIn("No matching", stop_server.stop_server())
        self.assertIsNone(child.poll())

    def test_wrong_executable_is_not_stopped(self):
        child = self.spawn()
        with patch.object(stop_server.server, "SERVER_BINARY", self.root / "other-server"):
            self.assertIn("No matching", stop_server.stop_server())
        self.assertIsNone(child.poll())

    def test_missing_explicit_port_is_not_guessed(self):
        child = self.spawn(port=None)
        self.assertIn("No matching", stop_server.stop_server())
        self.assertIsNone(child.poll())

    def test_no_server_is_a_successful_noop(self):
        self.assertIn("No matching", stop_server.stop_server())

    def test_multiple_matches_are_refused(self):
        first, second = self.spawn(), self.spawn()
        with self.assertRaisesRegex(RuntimeError, "Multiple matching"):
            stop_server.stop_server()
        self.assertIsNone(first.poll())
        self.assertIsNone(second.poll())

    def test_shutdown_timeout_does_not_force_kill(self):
        child = self.spawn(ignore_term=True)
        with self.assertRaisesRegex(RuntimeError, "not force-killed"):
            stop_server.stop_server(timeout=0.1)
        self.assertIsNone(child.poll())

    def test_active_startup_lock_prevents_signalling(self):
        child = self.spawn()
        runtime = self.root / ".runtime"
        runtime.mkdir()
        with (runtime / "llama-server-8080.lock").open("a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            with self.assertRaisesRegex(RuntimeError, "No signal sent"):
                stop_server.stop_server(timeout=0.05)
        self.assertIsNone(child.poll())

    def test_identity_is_checked_again_after_opening_pidfd(self):
        child = self.spawn()
        with patch.object(stop_server, "_matches", return_value=False):
            with self.assertRaisesRegex(RuntimeError, "identity changed"):
                stop_server._terminate(child.pid, 8080, 1)
        self.assertIsNone(child.poll())

    def test_unavailable_pidfd_does_not_fall_back_to_kill(self):
        child = self.spawn()
        with patch.object(stop_server.os, "pidfd_open", side_effect=OSError("not supported")):
            with self.assertRaises(OSError):
                stop_server.stop_server()
        self.assertIsNone(child.poll())


if __name__ == "__main__":
    unittest.main()
