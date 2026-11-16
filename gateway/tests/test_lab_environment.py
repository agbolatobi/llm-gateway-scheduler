"""Persistence checks use temporary private state and the existing gateway binary."""

import json
from pathlib import Path
import tempfile
import unittest
import threading
from http.server import ThreadingHTTPServer

import requests

import gate07_lab as lab
import postman_lab as console


class EnvironmentTests(unittest.TestCase):
    def test_environment_created_reused_updated_and_recreated(self):
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory)
            path, status = console.export_environment(
                work, "http://127.0.0.1:8005", "test-secret"
            )
            self.assertEqual(status, "created")
            original = path.read_bytes()
            modified = path.stat().st_mtime_ns
            identity = json.loads(original)["id"]
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            _, status = console.export_environment(work, "http://127.0.0.1:8005", "test-secret")
            self.assertEqual(status, "unchanged")
            self.assertEqual(path.read_bytes(), original)
            self.assertEqual(path.stat().st_mtime_ns, modified)
            for base, key in [
                ("http://127.0.0.1:8007", "test-secret"),
                ("http://127.0.0.1:8007", "replacement-secret"),
            ]:
                _, status = console.export_environment(work, base, key)
                self.assertEqual(status, "changed")
                environment = json.loads(path.read_text())
                self.assertEqual(environment["id"], identity)
                self.assertEqual(
                    {variable["key"]: variable["value"] for variable in environment["values"]},
                    {"gateway_test_url": base, "gateway_test_key": key},
                )
            path.unlink()
            _, status = console.export_environment(work, "http://127.0.0.1:8005", "test-secret")
            self.assertEqual(status, "created")
            self.assertEqual(path.read_bytes(), original)
            path.write_text("{broken")
            _, status = console.export_environment(work, "http://127.0.0.1:8005", "test-secret")
            self.assertEqual(status, "changed")
            self.assertEqual(path.read_bytes(), original)

    def test_restart_retains_keys_and_export(self):
        with tempfile.TemporaryDirectory() as directory:
            runtime = Path(directory)
            binary = lab.ROOT / ".runtime/gateway"
            with lab.lab_directory(runtime=runtime) as work:
                control = lab.saved_control_key(work)
                keys = lab.lab_keys(binary, work / "keys", "http://127.0.0.1:8005")
                path, status = console.export_collection(work, "http://127.0.0.1:8005", control)
                self.assertEqual(status, "created")
                before, modified = path.read_bytes(), path.stat().st_mtime_ns
                self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            with lab.lab_directory(runtime=runtime) as work:
                self.assertTrue(lab.saved_control_key(work) == control, "control key changed")
                reused = lab.lab_keys(binary, work / "keys", "http://127.0.0.1:8005")
                self.assertTrue(reused == keys, "gateway keys changed")
                path, status = console.export_collection(work, "http://127.0.0.1:8005", control)
                self.assertEqual(status, "unchanged")
                self.assertTrue(path.read_bytes() == before, "export changed")
                self.assertEqual(path.stat().st_mtime_ns, modified)
                records = json.loads((work / "keys/api-keys.json").read_text())
                self.assertEqual(len(records["keys"]), len(lab.USERS))
                identity = json.loads(before)["info"]["_postman_id"]
                path, status = console.export_collection(work, "http://127.0.0.1:8007", control)
                self.assertEqual(status, "changed")
                self.assertEqual(json.loads(path.read_text())["info"]["_postman_id"], identity)

    def test_verification_workspace_does_not_change_saved_environment(self):
        with tempfile.TemporaryDirectory() as directory:
            runtime = Path(directory)
            with lab.lab_directory(runtime=runtime) as work:
                persistent, _ = console.export_collection(
                    work, "http://127.0.0.1:8005", "test-placeholder"
                )
                before = persistent.read_bytes()
                with self.assertRaisesRegex(RuntimeError, "already running"):
                    with lab.lab_directory(runtime=runtime):
                        pass
            with lab.lab_directory(verify=True, runtime=runtime) as temporary:
                self.assertNotEqual(temporary, persistent.parent)
                self.assertFalse(temporary.is_relative_to(runtime))
                self.assertEqual(list(runtime.iterdir()), [persistent.parent])
                console.export_collection(
                    temporary, "http://127.0.0.1:8007", "test-placeholder"
                )
            self.assertFalse(temporary.exists())
            self.assertTrue(
                persistent.read_bytes() == before, "verification changed saved environment"
            )

    def test_http_runs_are_repeatable_and_show_current_files(self):
        with tempfile.TemporaryDirectory() as directory:
            controller = console.TestLab(Path(directory), "http://127.0.0.1:8005")
            server = ThreadingHTTPServer(("127.0.0.1", 0), console.handler_for(controller))
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            base = f"http://127.0.0.1:{server.server_port}"
            headers = {"Authorization": "Bearer " + controller.key}
            try:
                controller.start()
                before = controller.collection.read_bytes()
                environment_before = controller.environment.read_bytes()
                self.assertEqual(
                    requests.get(base + "/tests/status", timeout=3).status_code, 401
                )
                status = requests.get(base + "/tests/status", headers=headers, timeout=3).json()
                self.assertEqual(
                    status["files"]["postman_collection"], str(controller.collection)
                )
                self.assertEqual(
                    status["files"]["postman_environment"], str(controller.environment)
                )
                self.assertTrue(status["files"]["postman_environment_uri"].startswith("file:"))
                self.assertTrue(
                    controller.key not in json.dumps(status), "status exposed credentials"
                )
                controller.run_lock.acquire()
                try:
                    self.assertEqual(
                        requests.post(
                            base + "/tests/reset", headers=headers, timeout=3
                        ).status_code,
                        409,
                    )
                finally:
                    controller.run_lock.release()
                ids = []
                for _ in range(2):
                    response = requests.post(base + "/tests/run", headers=headers, timeout=60)
                    self.assertEqual(response.status_code, 200, response.text)
                    result = response.json()
                    self.assertEqual(result["status"], "passed")
                    self.assertGreaterEqual(len(result["checks"]), 15)
                    self.assertTrue(
                        any(check["name"].startswith("queue/") for check in result["checks"])
                    )
                    self.assertTrue(all(check["passed"] for check in result["checks"]))
                    ids.append(result["run_id"])
                    self.assertTrue(
                        controller.collection.read_bytes() == before,
                        "test run changed import details",
                    )
                    self.assertEqual(controller.environment.read_bytes(), environment_before)
                self.assertNotEqual(ids[0], ids[1])
                self.assertEqual(json.loads(controller.report.read_text())["run_id"], ids[1])
                controller.environment.unlink()
                self.assertEqual(
                    requests.post(
                        base + "/tests/reset", headers=headers, timeout=15
                    ).status_code,
                    200,
                )
                self.assertEqual(controller.environment.read_bytes(), environment_before)
                restarted = console.TestLab(Path(directory), "http://127.0.0.1:8005")
                self.assertTrue(restarted.key == controller.key, "restart changed test key")
                self.assertEqual(restarted.export_status, "unchanged")
                self.assertEqual(restarted.environment_status, "unchanged")
            finally:
                server.shutdown()
                server.server_close()
                controller.close()


if __name__ == "__main__":
    unittest.main()
