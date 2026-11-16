"""Failure reports remain useful through HTTP, persistence and Postman reuse."""

import json
from pathlib import Path
import tempfile
import threading
import unittest
from http.server import ThreadingHTTPServer
from types import SimpleNamespace
from unittest.mock import patch

import requests

import gate07_lab as fixtures
import postman_lab as console
from test_checks import CheckFailure, Checks


def response(status=200, body="{}", request_id="test-request"):
    result = requests.Response()
    result.status_code = status
    result._content = body.encode()
    result.headers["X-Request-ID"] = request_id
    result.request = requests.Request(
        "POST",
        "http://127.0.0.1/v1/chat/completions",
        headers={"Authorization": "Bearer header-secret"},
    ).prepare()
    return result


class FailureTests(unittest.TestCase):
    def test_actual_runner_names_http_failure(self):
        checks = Checks()
        http_response = response(
            503, '{"error":{"code":"capacity_exceeded","message":"Runtime busy"}}'
        )
        with patch("test_checks.requests.post", return_value=http_response):
            with self.assertRaises(CheckFailure) as caught:
                fixtures.run_checks(
                    "http://gateway",
                    "http://backend",
                    {"actual_key": "test-key"},
                    Path("unused"),
                )
        detail = caught.exception.details
        self.assertEqual(detail["check"], "actual_usage_refunds_and_retained_charges")
        self.assertEqual(detail["expected"], 200)
        self.assertEqual(detail["actual"], 503)
        self.assertEqual(detail["response"]["request_id"], "test-request")
        self.assertIn("Runtime busy", detail["response"]["body"])
        self.assertNotIn("header-secret", json.dumps(detail))
        checks.close()

    def test_settlement_reports_expected_and_observed_tokens(self):
        with tempfile.TemporaryDirectory() as directory:
            logs = Path(directory) / "gateway.log"
            logs.write_text(
                json.dumps(
                    {
                        "event": "gateway_request",
                        "request_id": "test-request",
                        "charged_tokens": 131,
                        "usage_source": "fallback",
                    }
                )
            )
            with self.assertRaises(CheckFailure) as caught:
                Checks().settlement(response(), logs, charged_tokens=5, usage_source="backend")
            detail = caught.exception.details
            self.assertEqual(
                detail["expected"], {"charged_tokens": 5, "usage_source": "backend"}
            )
            self.assertEqual(
                detail["actual"], {"charged_tokens": 131, "usage_source": "fallback"}
            )

    def test_transport_bad_json_missing_id_and_stream_failures(self):
        checks = Checks()
        with patch("test_checks.requests.post", side_effect=requests.Timeout("Timed out")):
            with self.assertRaises(CheckFailure) as caught:
                checks.request("send chat", "http://gateway", timeout=1)
        self.assertEqual(caught.exception.details["actual"]["error_type"], "Timeout")
        with self.assertRaises(CheckFailure) as caught:
            checks.json("read rejection", response(503, "not JSON"))
        self.assertEqual(caught.exception.details["reason"], "The response was not valid JSON.")
        with self.assertRaises(CheckFailure) as caught:
            checks.settlement(response(request_id=""), Path("unused"), charged_tokens=5)
        self.assertIn("no request ID", caught.exception.details["reason"])
        http_response = response()
        with patch.object(http_response, "iter_lines", return_value=iter(())):
            with self.assertRaises(CheckFailure) as caught:
                checks.first_event(http_response)
        self.assertEqual(caught.exception.details["step"], "receive first streaming event")

    def test_http_failure_is_saved_redacted_and_reuses_collection(self):
        with tempfile.TemporaryDirectory() as directory:
            lab = console.TestLab(Path(directory), "http://127.0.0.1:8005")
            lab.gateway = "http://unused"
            lab.servers = [SimpleNamespace(server_port=8095)]
            lab.values = {"actual_key": "gateway-secret"}
            before = lab.collection.read_bytes()
            server = ThreadingHTTPServer(("127.0.0.1", 0), console.handler_for(lab))
            threading.Thread(target=server.serve_forever, daemon=True).start()
            base = f"http://127.0.0.1:{server.server_port}"
            headers = {"Authorization": "Bearer " + lab.key}

            def fail_suite(_base, _backend, _values, _logs, complete):
                complete("previous_check")
                checks = Checks()
                checks.name = "forced_http_failure"
                checks.status("accept chat", response(503, "gateway-secret " + lab.key), 200)

            try:
                with patch.object(lab, "reset"), patch.object(
                    fixtures, "run_checks", side_effect=fail_suite
                ):
                    reply = requests.post(base + "/tests/run", headers=headers, timeout=5)
                self.assertEqual(reply.status_code, 500)
                result = reply.json()
                self.assertEqual(result["failure"]["expected"], 200)
                self.assertEqual(result["failure"]["actual"], 503)
                self.assertEqual(
                    result["checks"][0], {"name": "previous_check", "passed": True}
                )
                self.assertFalse(result["checks"][1]["passed"])
                self.assertEqual(result["checks"][1]["failure"], result["failure"])
                self.assertEqual(json.loads(lab.report.read_text()), result)
                latest = requests.get(base + "/tests/status", headers=headers, timeout=5).json()
                self.assertEqual(latest["last_run"], result)
                for secret in (lab.key, "gateway-secret", "header-secret"):
                    self.assertNotIn(secret, json.dumps(latest))
                self.assertIn("[redacted]", result["failure"]["response"]["body"])
                self.assertEqual(lab.collection.read_bytes(), before)

                with patch.object(
                    lab, "reset", side_effect=RuntimeError("Backend did not drain " + lab.key)
                ):
                    reply = requests.post(base + "/tests/reset", headers=headers, timeout=5)
                self.assertEqual(reply.status_code, 500)
                self.assertEqual(reply.json()["failure"]["step"], "reset test state")
                self.assertIn("Backend did not drain", reply.json()["error"])
                self.assertNotIn(lab.key, reply.text)
                self.assertFalse(lab.run_lock.locked())

                with patch.object(
                    lab, "reset", side_effect=RuntimeError("Gateway startup timed out")
                ):
                    reply = requests.post(base + "/tests/run", headers=headers, timeout=5)
                self.assertEqual(reply.status_code, 500)
                self.assertEqual(reply.json()["failure"]["check"], "test_setup")
                self.assertEqual(reply.json()["failure"]["step"], "prepare test gateway")
            finally:
                server.shutdown()
                server.server_close()


if __name__ == "__main__":
    unittest.main()
