"""Check a port-forwarded deployment with a temporary key, revoked on completion."""
import argparse
import json
import re
import urllib.error
import urllib.request
from common import executable, run


def request(base_url, key, path, body=None):
    payload = None if body is None else json.dumps(body).encode()
    headers = {"Authorization": "Bearer " + key}
    if body is not None:
        headers["Content-Type"] = "application/json"
    return urllib.request.urlopen(urllib.request.Request(base_url + path, data=payload, headers=headers), timeout=150)


def verify(base_url, key, model):
    with request(base_url, key, "/v1/models") as response:
        assert model in [entry["id"] for entry in json.load(response)["data"]]
    try:
        request(base_url, "invalid-smoke-key", "/v1/models")
        raise AssertionError("Invalid key was accepted")
    except urllib.error.HTTPError as error:
        assert error.code == 401
    body = {"model":model, "messages":[{"role":"user", "content":"Explain a gateway in one sentence."}], "max_completion_tokens":32}
    with request(base_url, key, "/v1/chat/completions", body) as response:
        assert json.load(response)["choices"][0]["message"]["content"]
    body["stream"] = True
    with request(base_url, key, "/v1/chat/completions", body) as response:
        stream = response.read().decode()
        assert "data: [DONE]" in stream and '"delta"' in stream
    with urllib.request.urlopen(base_url + "/chat/", timeout=5) as response:
        assert response.status == 200
    print("PASS: UI, authentication, model discovery, JSON chat and streaming.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--context", required=True)
    parser.add_argument("--base-url", default="http://127.0.0.1:8000")
    parser.add_argument("--model", default="local-llm")
    parser.add_argument("--restart", action="store_true", help="Also restart the dedicated test Deployment and verify key persistence; restart port-forward if it exits")
    arguments = parser.parse_args()
    control = [executable("kubectl"), "--context", arguments.context, "-n", "llm-gateway"]
    key_command = [*control, "exec", "deployment/gateway", "-c", "gateway", "--", "/gateway", "keys"]
    created = run([*key_command, "create", "--user", "deployment-smoke", "--config-dir", "/data/keys"], capture=True).stdout
    token = re.search(r"API token: (\S+)", created).group(1)
    key_id = re.search(r"Created key (\S+)", created).group(1)
    try:
        verify(arguments.base_url, token, arguments.model)
        if arguments.restart:
            run([*control, "rollout", "restart", "deployment/gateway"])
            run([*control, "rollout", "status", "deployment/gateway", "--timeout=20m"])
            input("Restart port-forward if needed, then press Enter to verify the same key: ")
            verify(arguments.base_url, token, arguments.model)
    finally:
        run([*key_command, "revoke", "--id", key_id, "--config-dir", "/data/keys"], capture=True)
    try:
        request(arguments.base_url, token, "/v1/models")
        raise AssertionError("Revoked key was accepted")
    except urllib.error.HTTPError as error:
        assert error.code == 401
    print("PASS: temporary key revoked and rejected.")


if __name__ == "__main__":
    main()
