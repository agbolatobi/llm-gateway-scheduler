"""Replay real-model container smoke, cancellation, key persistence and shutdown tests.

Build the two llm-*-infra-test:local images first. Uses isolated containers and
an ephemeral loopback port. Existing local gateway/model processes are untouched.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
import uuid

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "scripts"))
from common import ROOT, run
from smoke import request, verify


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--model", type=Path, required=True)
    arguments = parser.parse_args()
    model_file = arguments.model.resolve(strict=True)
    identity = "llm-infra-" + uuid.uuid4().hex[:10]
    gateway_name, runtime_name = identity + "-gateway", identity + "-runtime"
    gateway_image, runtime_image = "llm-gateway-infra-test:local", "llm-runtime-infra-test:local"
    def docker(*arguments):
        return run(["docker", *arguments], capture=True).stdout.strip()
    def stop(name):
        docker("stop", "--time", "25", name)
        code = docker("inspect", "--format", "{{.State.ExitCode}}", name)
        assert code != "137", f"{name} required SIGKILL"
    def wait_until(probe, timeout=90):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                if probe():
                    return
            except (OSError, subprocess.CalledProcessError):
                pass
            time.sleep(0.2)
        raise AssertionError("Timed out waiting for container condition")
    def healthy():
        return docker("exec", runtime_name, "curl", "-fsS", "--max-time", "2", "http://127.0.0.1:8080/health")
    def busy():
        slots = json.loads(docker("exec", runtime_name, "curl", "-fsS", "--max-time", "2", "http://127.0.0.1:8080/slots"))
        return any(slot["is_processing"] for slot in slots)
    def start_runtime():
        docker("run", "-d", "--name", runtime_name, "--network", "container:" + gateway_name,
               "--read-only", "--cap-drop=ALL", "--security-opt", "no-new-privileges", "--tmpfs", "/tmp",
               "--mount", f"type=bind,src={model_file},dst=/models/model.gguf,readonly",
               runtime_image, "--model", "/models/model.gguf", "--alias", "local-llm",
               "--host", "127.0.0.1", "--port", "8080", "--ctx-size", "4096", "--parallel", "1",
               "--threads", "2", "--slots", "--no-webui", "--n-gpu-layers", "0")
        wait_until(healthy)
    with tempfile.TemporaryDirectory(prefix="llm-infra-") as temporary:
        directory = Path(temporary)
        directory.chmod(0o755)
        keys = directory / "keys"
        keys.mkdir(mode=0o700)
        os.chown(keys, 10001, 10001)
        config = json.loads((ROOT / "deploy/base/gateway.config.json").read_text())
        config["gateway"]["scheduler"]["max_waiting_requests"] = 1
        config["gateway"]["scheduler"]["max_waiting_per_runtime"] = 1
        config["quotas"]["user"]["requests"] = 100
        config["quotas"]["global_ceiling"]["requests"] = 200
        config["models"][0]["limits"]["requests"] = 200
        configuration = directory / "gateway.json"
        configuration.write_text(json.dumps(config))
        configuration.chmod(0o644)
        try:
            docker("run", "-d", "--name", gateway_name, "--read-only", "--cap-drop=ALL",
                   "--security-opt", "no-new-privileges", "--tmpfs", "/tmp", "-p", "127.0.0.1::8000",
                   "--mount", f"type=bind,src={keys},dst=/data/keys",
                   "--mount", f"type=bind,src={configuration},dst=/config/gateway.json,readonly",
                   gateway_image, "--config", "/config/gateway.json", "--config-dir", "/data/keys",
                   "--listen", "0.0.0.0:8000", "--allow-remote-listen", "--shutdown-timeout", "20s")
            port = docker("port", gateway_name, "8000/tcp").split(":")[-1]
            base_url = "http://127.0.0.1:" + port
            start_runtime()
            created = docker("exec", gateway_name, "/gateway", "keys", "create", "--user", "infra-test", "--config-dir", "/data/keys")
            token = re.search(r"API token: (\S+)", created).group(1)
            key_id = re.search(r"Created key (\S+)", created).group(1)
            verify(base_url, token, "local-llm")
            body = {"model":"local-llm", "messages":[{"role":"user", "content":"Count from one to one hundred, explaining every number."}], "max_completion_tokens":256, "stream":True}
            # Closing the HTTP response must cancel generation and recover the execution slot.
            response = request(base_url, token, "/v1/chat/completions", body)
            response.readline()
            response.close()
            wait_until(lambda: not busy(), 20)
            print("PASS: client cancellation releases runtime capacity.")
            # Fill one active and one waiting slot, then assert explicit overflow.
            with ThreadPoolExecutor(max_workers=3) as executor:
                def consume():
                    with request(base_url, token, "/v1/chat/completions", body) as response:
                        return response.read().decode()
                first = executor.submit(consume)
                wait_until(busy)
                second = executor.submit(consume)
                time.sleep(0.5)
                try:
                    with request(base_url, token, "/v1/chat/completions", body):
                        raise AssertionError("Queue overflow was accepted")
                except urllib.error.HTTPError as error:
                    failure = json.load(error)
                    assert error.code == 503 and failure["error"]["code"] == "queue_full", failure
                assert "[DONE]" in first.result(timeout=90)
                assert "[DONE]" in second.result(timeout=90)
                print("PASS: queued requests complete; overflow is rejected.")
                # Gateway-first shutdown drains an active stream before it exits.
                shorter = dict(body, max_completion_tokens=64)
                def short_reply():
                    with request(base_url, token, "/v1/chat/completions", shorter) as response:
                        return response.read().decode()
                active = executor.submit(short_reply)
                wait_until(busy)
                stop(gateway_name)
                assert "[DONE]" in active.result(timeout=25)
                stop(runtime_name)
                docker("rm", runtime_name)
                docker("start", gateway_name)
                port = docker("port", gateway_name, "8000/tcp").split(":")[-1]
                base_url = "http://127.0.0.1:" + port
                start_runtime()
                with request(base_url, token, "/v1/models") as response:
                    assert response.status == 200
                print("PASS: graceful gateway-first shutdown and persisted API key after restart.")
                # Unexpected runtime-first shutdown must not hang gateway termination.
                active = executor.submit(consume)
                wait_until(busy)
                docker("kill", "--signal", "SIGTERM", runtime_name)
                try:
                    active.result(timeout=20)
                except (OSError, urllib.error.HTTPError):
                    pass
                docker("exec", gateway_name, "/gateway", "keys", "revoke", "--id", key_id, "--config-dir", "/data/keys")
                try:
                    request(base_url, token, "/v1/models")
                    raise AssertionError("Revoked key accepted")
                except urllib.error.HTTPError as error:
                    assert error.code == 401
                stop(gateway_name)
                print("PASS: runtime-first shutdown, key revocation and bounded gateway exit.")
        except Exception:
            for name in (gateway_name, runtime_name):
                subprocess.run(["docker", "logs", "--tail", "15", name], check=False)
            raise
        finally:
            for name in (runtime_name, gateway_name):
                subprocess.run(["docker", "rm", "-f", name], check=False, capture_output=True)


if __name__ == "__main__":
    main()
