"""Explicit test failures with bounded HTTP evidence; no outgoing headers."""
import json
import time

import requests


class CheckFailure(Exception):
    def __init__(self, check, step, reason, expected=None, actual=None, response=None):
        super().__init__(reason)
        self.details = {"check": check, "step": step, "reason": reason,
                        "expected": expected, "actual": actual}
        if response is not None:
            request = response.request
            raw = response.content if response._content is not False else None
            self.details["response"] = {
                "status": response.status_code,
                "request_id": response.headers.get("X-Request-ID"),
                "method": request.method if request is not None else None,
                "url": request.url if request is not None else None,
                "body": raw.decode("utf-8", errors="replace")[:4096] if raw is not None else None,
                "body_truncated": raw is not None and len(raw) > 4096,
            }


class Checks:
    def __init__(self):
        self.name = "test_setup"
        self.responses = []

    def close(self):
        for response in self.responses:
            response.close()

    def equal(self, step, actual, expected, response=None):
        if actual != expected:
            raise CheckFailure(self.name, step, "The observed result did not match the expected result.", expected, actual, response)

    def request(self, step, url, **kwargs):
        try:
            response = requests.post(url, **kwargs)
            self.responses.append(response)
            return response
        except requests.RequestException as error:
            raise CheckFailure(self.name, step, "The HTTP request could not complete.",
                               {"method": "POST", "url": url, "result": "HTTP response"},
                               {"error_type": type(error).__name__, "message": str(error)}) from error

    def text(self, step, response):
        try:
            return response.text
        except requests.RequestException as error:
            raise CheckFailure(self.name, step, "The response body ended unexpectedly or timed out.",
                               "Complete response body", {"error_type": type(error).__name__, "message": str(error)}, response) from error

    def json(self, step, response):
        raw = self.text(step, response)
        try:
            return json.loads(raw)
        except ValueError as error:
            raise CheckFailure(self.name, step, "The response was not valid JSON.", "JSON response", raw[:4096], response) from error

    def status(self, step, response, expected):
        if response.status_code != expected:
            # Error responses are finite JSON; read them for diagnostics. Never
            # consume an open successful SSE stream just to inspect its status.
            if response.status_code >= 400:
                self.text(step, response)
            raise CheckFailure(self.name, step, f"Expected HTTP {expected}, received HTTP {response.status_code}.",
                               expected, response.status_code, response)

    def first_event(self, response):
        try:
            line = next(response.iter_lines(chunk_size=1))
        except (StopIteration, requests.RequestException) as error:
            raise CheckFailure(self.name, "receive first streaming event", "The stream ended before its first event.",
                               "An SSE data event", type(error).__name__, response) from error
        self.equal("receive first streaming event", line.startswith(b"data:"), True, response)

    def settlement(self, response, logs, **expected):
        request_id = response.headers.get("X-Request-ID")
        if not request_id:
            raise CheckFailure(self.name, "find settlement log", "The response has no request ID for log correlation.",
                               "X-Request-ID header", None, response)
        end = time.monotonic() + 5
        while time.monotonic() < end:
            for line in logs.read_text().splitlines():
                pos = line.find('{"event"')
                if pos < 0:
                    continue
                try:
                    record = json.loads(line[pos:])
                except ValueError:
                    continue
                if record.get("request_id") == request_id:
                    self.equal("verify token/capacity settlement", {k: record.get(k) for k in expected}, expected, response)
                    return record
            time.sleep(0.02)
        raise CheckFailure(self.name, "find settlement log", "No matching settlement log appeared within five seconds.",
                           {"request_id": request_id, "log_record": "gateway_request"}, "No matching record", response)
