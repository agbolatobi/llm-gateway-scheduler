import json

import requests

from .auth import saved_api_key


class LLMClient:
    def __init__(
        self,
        base_url="http://127.0.0.1:8080",
        model="local-llm",
        timeout=(5, 180),
    ):
        self.base_url = base_url.rstrip("/")
        self.model = model
        self.timeout = timeout

    def _request(self, messages, *, stream, **options):
        """Send a request; the caller closes the returned response."""
        token = saved_api_key(self.base_url)
        authentication = {}
        if token:
            authentication = {
                "headers": {"Authorization": f"Bearer {token}"},
                "allow_redirects": False,
            }
        return requests.post(
            f"{self.base_url}/v1/chat/completions",
            json={
                **options,
                "model": self.model,
                "messages": messages,
                "stream": stream,
            },
            stream=stream,
            timeout=self.timeout,
            **authentication,
        )

    def chat(self, messages, **options):
        """Return the complete response, including token usage."""
        with self._request(messages, stream=False, **options) as response:
            response.raise_for_status()
            return response.json()

    def stream(self, messages, **options):
        """Yield parsed streaming events as they arrive."""
        with self._request(messages, stream=True, **options) as response:
            response.raise_for_status()
            response.encoding = "utf-8"

            for line in response.iter_lines(
                chunk_size=1,
                decode_unicode=True,
            ):
                if not line.startswith("data:"):
                    continue

                data = line[5:].strip()

                if not data:
                    continue
                if data == "[DONE]":
                    return

                event = json.loads(data)

                if "error" in event:
                    raise RuntimeError(str(event["error"]))

                yield event

            raise RuntimeError("The stream ended before completion.")
