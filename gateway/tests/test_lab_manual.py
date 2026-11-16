"""Exercise manual requests through the public lab, including real cancellation."""

import json
from pathlib import Path
import tempfile
import threading
import time
import socket
import unittest
from http.server import ThreadingHTTPServer

import requests
import postman_lab as console


class ManualTests(unittest.TestCase):
    def test_queued_manual_disconnect_and_policy_reload(self):
        with tempfile.TemporaryDirectory() as directory:
            lab = console.TestLab(Path(directory), "http://127.0.0.1:8005")
            server = ThreadingHTTPServer(("127.0.0.1", 0), console.handler_for(lab))
            threading.Thread(target=server.serve_forever, daemon=True).start()
            base = f"http://127.0.0.1:{server.server_port}"
            headers = {"Authorization": "Bearer " + lab.key, "X-Test-User": "busy"}
            held = None
            raw = None
            try:
                lab.start()
                before = lab.environment.read_bytes()
                settings = requests.post(
                    base + "/tests/scheduler",
                    headers=headers,
                    json={"policy": "fifo", "max_waiting_requests": 2},
                    timeout=5,
                )
                self.assertEqual(settings.status_code, 200, settings.text)
                self.assertEqual(
                    requests.post(
                        base + "/__test/control",
                        headers=headers,
                        json={"mode": "hold"},
                        timeout=5,
                    ).status_code,
                    200,
                )
                body = {
                    "model": "local-llm",
                    "messages": [{"role": "user", "content": "hello"}],
                    "max_tokens": 8,
                    "stream": True,
                }
                held = requests.post(
                    base + "/v1/chat/completions",
                    headers=headers,
                    json=body,
                    stream=True,
                    timeout=5,
                )
                self.assertEqual(held.status_code, 200)
                next(held.iter_lines(chunk_size=1))
                payload = json.dumps({**body, "stream": False}).encode()
                raw = socket.create_connection(("127.0.0.1", server.server_port), timeout=3)
                wire = (
                    f"POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\n"
                    f"Authorization: Bearer {lab.key}\r\nX-Test-User: busy\r\n"
                    f"X-Request-ID: manual-wait-cancel\r\nContent-Type: application/json\r\n"
                    f"Content-Length: {len(payload)}\r\n\r\n"
                ).encode() + payload
                raw.sendall(wire)
                raw.settimeout(0.2)
                with self.assertRaises(socket.timeout):
                    raw.recv(1)  # Waiting: no premature HTTP/SSE success response.
                changed = requests.post(
                    base + "/tests/scheduler",
                    headers=headers,
                    json={"policy": "interactive_priority_aging"},
                    timeout=5,
                )
                self.assertEqual(changed.status_code, 200, changed.text)
                raw.shutdown(socket.SHUT_RDWR)
                raw.close()
                raw = None
                matching = []
                end = time.monotonic() + 5
                while time.monotonic() < end:
                    records = requests.get(
                        base + "/tests/requests", headers=headers, timeout=3
                    ).json()["requests"]
                    matching = [
                        response
                        for response in records
                        if response["request_id"] == "manual-wait-cancel"
                    ]
                    if matching:
                        break
                    time.sleep(0.05)
                self.assertTrue(matching, "Queued manual disconnect was not propagated")
                record = matching[0]
                self.assertEqual(record["outcome"], "request_cancelled")
                self.assertEqual(record["charged_tokens"], 0)
                self.assertIsNone(record["dispatch_ms"])
                self.assertGreater(record["queue_wait_ms"], 0)
                self.assertEqual(lab.states[0].snapshot()["generation_calls"], 1)
                self.assertEqual(lab.environment.read_bytes(), before)
                self.assertEqual(
                    requests.post(
                        base + "/tests/scheduler",
                        headers=headers,
                        json={"max_waiting_requests": -1},
                        timeout=5,
                    ).status_code,
                    400,
                )
            finally:
                if raw:
                    raw.close()
                if held:
                    held.close()
                for state in lab.states:
                    state.release.set()
                server.shutdown()
                server.server_close()
                lab.close()

    def test_manual_gateway_controls_streams_and_suite_coexist(self):
        with tempfile.TemporaryDirectory() as directory:
            lab = console.TestLab(Path(directory), "http://127.0.0.1:8005")
            server = ThreadingHTTPServer(("127.0.0.1", 0), console.handler_for(lab))
            threading.Thread(target=server.serve_forever, daemon=True).start()
            base = f"http://127.0.0.1:{server.server_port}"
            headers = {"Authorization": "Bearer " + lab.key}
            opened = []

            def post(path, body=None, user="actual", stream=False):
                response = requests.post(
                    base + path,
                    headers={**headers, "X-Test-User": user},
                    json=body,
                    stream=stream,
                    timeout=8,
                )
                opened.append(response)
                return response

            def chat(user="actual", cap=128, stream=False, model="local-llm"):
                return post(
                    "/v1/chat/completions",
                    {
                        "model": model,
                        "messages": [{"role": "user", "content": "hello"}],
                        "max_tokens": cap,
                        "stream": stream,
                        **({"stream_options": {"include_usage": True}} if stream else {}),
                    },
                    user,
                    stream,
                )

            def control(mode):
                response = post("/__test/control", {"mode": mode})
                self.assertEqual(response.status_code, 200, response.text)

            def wait_manual():
                end = time.monotonic() + 5
                while lab.manual_requests and time.monotonic() < end:
                    time.sleep(0.02)
                self.assertEqual(lab.manual_requests, 0)

            try:
                lab.start()
                lab.configure_scheduler({"max_waiting_requests": 0})
                exported = lab.environment.read_bytes()
                self.assertEqual(requests.get(base + "/v1/models", timeout=3).status_code, 401)
                models = requests.get(base + "/v1/models", headers=headers, timeout=3)
                self.assertEqual(models.status_code, 200)
                self.assertEqual(
                    {token_limit["id"] for token_limit in models.json()["data"]},
                    {"local-llm", "second"},
                )
                for code in (200, 200, 429):
                    response = chat()
                    self.assertEqual(response.status_code, code, response.text)
                self.assertIn("X-Request-ID", response.headers)
                self.assertEqual(chat(user="unknown").status_code, 400)
                self.assertEqual(post("/__test/control", {"mode": []}).status_code, 400)
                self.assertEqual(
                    post("/__test/control", {"mode": "normal", "model": "unknown"}).status_code,
                    400,
                )

                wait_manual()
                self.assertEqual(post("/tests/reset").status_code, 200)
                control("missing_usage")
                self.assertEqual(chat("fallback").status_code, 200)
                self.assertEqual(chat("fallback").status_code, 429)
                control("normal")
                response = chat("stream", cap=32, stream=True)
                self.assertEqual(response.status_code, 200)
                self.assertIn("data: [DONE]", response.text)

                wait_manual()
                self.assertEqual(post("/tests/reset").status_code, 200)
                control("stubborn")
                held = chat("cancel", cap=32, stream=True)
                self.assertEqual(held.status_code, 200)
                self.assertTrue(next(held.iter_lines(chunk_size=1)).startswith(b"data:"))
                for model in ("local-llm", "second"):
                    busy = chat("busy", cap=32, model=model)
                    self.assertEqual(busy.status_code, 503, busy.text)
                    self.assertEqual(busy.json()["error"]["code"], "capacity_exceeded")
                self.assertEqual(post("/tests/run").status_code, 409)
                self.assertEqual(post("/tests/reset").status_code, 409)
                held.close()
                wait_manual()
                # The proxy must propagate cancellation to the actual gateway.
                end = time.monotonic() + 5
                records = []
                while time.monotonic() < end:
                    records = requests.get(
                        base + "/tests/requests", headers=headers, timeout=3
                    ).json()["requests"]
                    matching = [
                        response
                        for response in records
                        if response["request_id"] == held.headers["X-Request-ID"]
                    ]
                    if matching:
                        break
                    time.sleep(0.05)
                self.assertTrue(matching, "Cancelled stream never settled")
                self.assertEqual(matching[0]["capacity_state"], "quarantined_pending_idle")
                self.assertEqual(matching[0]["charged_tokens"], 35)
                blocked = chat("cancel", cap=32)
                self.assertEqual(blocked.json()["error"]["code"], "capacity_unconfirmed")
                control("normal")
                time.sleep(0.15)
                self.assertEqual(chat("cancel", cap=32).status_code, 200)
                state = requests.get(base + "/__test/state", headers=headers, timeout=3).json()
                self.assertIn("local-llm", state["models"])
                self.assertNotIn(lab.key, json.dumps(records))

                wait_manual()
                self.assertEqual(post("/tests/reset").status_code, 200)
                control("stall")
                response = chat("timeout", cap=32, stream=True)
                self.assertIn("backend_idle_timeout", response.text)
                self.assertNotIn("[DONE]", response.text)
                control("normal")
                wait_manual()
                self.assertEqual(post("/tests/reset").status_code, 200)
                self.assertEqual(lab.environment.read_bytes(), exported)
                lab.run_lock.acquire()
                try:
                    self.assertEqual(chat().status_code, 409)
                finally:
                    lab.run_lock.release()
            finally:
                for response in opened:
                    response.close()
                for state in lab.states:
                    state.release.set()
                server.shutdown()
                server.server_close()
                lab.close()


if __name__ == "__main__":
    unittest.main()
