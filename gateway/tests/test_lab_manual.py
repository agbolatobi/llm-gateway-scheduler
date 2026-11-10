"""Exercise manual requests through the public lab, including real cancellation."""
import json
from pathlib import Path
import tempfile
import threading
import time
import unittest
from http.server import ThreadingHTTPServer

import requests
import postman_lab as console


class ManualTests(unittest.TestCase):
    def test_manual_gateway_controls_streams_and_suite_coexist(self):
        with tempfile.TemporaryDirectory() as directory:
            lab = console.TestLab(Path(directory), "http://127.0.0.1:8005")
            server = ThreadingHTTPServer(("127.0.0.1", 0), console.handler_for(lab))
            threading.Thread(target=server.serve_forever, daemon=True).start()
            base = f"http://127.0.0.1:{server.server_port}"
            headers = {"Authorization": "Bearer " + lab.key}
            opened = []

            def post(path, body=None, user="actual", stream=False):
                r = requests.post(base + path, headers={**headers, "X-Test-User": user},
                                  json=body, stream=stream, timeout=8)
                opened.append(r)
                return r

            def chat(user="actual", cap=128, stream=False, model="local-llm"):
                return post("/v1/chat/completions",
                            {"model": model, "messages": [{"role": "user", "content": "hello"}],
                             "max_tokens": cap, "stream": stream,
                             **({"stream_options": {"include_usage": True}} if stream else {})}, user, stream)

            def control(mode):
                r = post("/__test/control", {"mode": mode})
                self.assertEqual(r.status_code, 200, r.text)

            def wait_manual():
                end = time.monotonic() + 5
                while lab.manual_requests and time.monotonic() < end:
                    time.sleep(0.02)
                self.assertEqual(lab.manual_requests, 0)

            try:
                lab.start()
                exported = lab.environment.read_bytes()
                self.assertEqual(requests.get(base + "/v1/models", timeout=3).status_code, 401)
                models = requests.get(base + "/v1/models", headers=headers, timeout=3)
                self.assertEqual(models.status_code, 200)
                self.assertEqual({m["id"] for m in models.json()["data"]}, {"local-llm", "second"})
                for code in (200, 200, 429):
                    r = chat()
                    self.assertEqual(r.status_code, code, r.text)
                self.assertIn("X-Request-ID", r.headers)
                self.assertEqual(chat(user="unknown").status_code, 400)
                self.assertEqual(post("/__test/control", {"mode": []}).status_code, 400)
                self.assertEqual(post("/__test/control", {"mode": "normal", "model": "unknown"}).status_code, 400)

                wait_manual()
                self.assertEqual(post("/tests/reset").status_code, 200)
                control("missing_usage")
                self.assertEqual(chat("fallback").status_code, 200)
                self.assertEqual(chat("fallback").status_code, 429)
                control("normal")
                r = chat("stream", cap=32, stream=True)
                self.assertEqual(r.status_code, 200)
                self.assertIn("data: [DONE]", r.text)

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
                    records = requests.get(base + "/tests/requests", headers=headers, timeout=3).json()["requests"]
                    matching = [r for r in records if r["request_id"] == held.headers["X-Request-ID"]]
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
                r = chat("timeout", cap=32, stream=True)
                self.assertIn("backend_idle_timeout", r.text)
                self.assertNotIn("[DONE]", r.text)
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
                for r in opened:
                    r.close()
                for state in lab.states:
                    state.release.set()
                server.shutdown()
                server.server_close()
                lab.close()


if __name__ == "__main__":
    unittest.main()
