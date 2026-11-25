"""Run the same compiled worker locally or beside a model in a selected GKE Pod."""

import json
import subprocess
import uuid
from pathlib import Path


def command_json(command):
    return json.loads(subprocess.check_output(command, text=True, timeout=60))


class Local:
    architecture = None
    provenance = {"target": "local", "client_location": "same host as runtime"}

    def execute(self, binary, plan_bytes, events, errors):
        return subprocess.run([str(binary)], input=plan_bytes, stdout=events, stderr=errors).returncode


class GKE:
    def __init__(self, context, namespace, pod_name=None, container="runtime"):
        self.prefix = ["kubectl", "--context", context, "--namespace", namespace]
        pods = command_json(self.prefix + ["get", "pods", "-l", "app=llm-gateway", "-o", "json"])["items"]
        ready = [pod for pod in pods if not pod["metadata"].get("deletionTimestamp") and
                 any(condition["type"] == "Ready" and condition["status"] == "True"
                     for condition in pod.get("status", {}).get("conditions", []))]
        if pod_name:
            ready = [pod for pod in ready if pod["metadata"]["name"] == pod_name]
        if len(ready) != 1:
            raise ValueError("Select exactly one Ready gateway Pod using --pod; no Pod was chosen implicitly.")
        self.pod = ready[0]
        self.pod_name = self.pod["metadata"]["name"]
        containers = self.pod["spec"].get("containers", []) + self.pod["spec"].get("initContainers", [])
        if container not in [item["name"] for item in containers]:
            raise ValueError(f"Pod has no {container!r} container")
        self.container = container
        self.exec_prefix = self.prefix + ["exec", "-i", self.pod_name, "-c", container, "--"]
        machine = subprocess.check_output(self.exec_prefix + ["uname", "-m"], text=True, timeout=30).strip()
        self.architecture = {"x86_64": "amd64", "aarch64": "arm64"}.get(machine)
        if not self.architecture:
            raise ValueError(f"Unsupported Pod architecture: {machine}")
        config_volume = next(item for item in self.pod["spec"]["volumes"] if item["name"] == "config")
        config_map = command_json(self.prefix + ["get", "configmap", config_volume["configMap"]["name"], "-o", "json"])
        self.config = json.loads(config_map["data"]["gateway.config.json"])
        self.provenance = {
            "target": "gke", "context": context, "namespace": namespace,
            "pod": self.pod_name, "pod_uid": self.pod["metadata"]["uid"],
            "node": self.pod["spec"].get("nodeName"), "container": container,
            "client_location": "runtime container, Pod loopback",
            "containers": [{"name": item["name"], "image": item["image"], "resources": item.get("resources", {})} for item in containers],
            "container_status": self.status(),
        }

    def status(self):
        pod = command_json(self.prefix + ["get", "pod", self.pod_name, "-o", "json"])
        if pod["metadata"]["uid"] != self.pod["metadata"]["uid"]:
            raise RuntimeError("Selected Pod was replaced during evaluation")
        statuses = pod.get("status", {}).get("containerStatuses", []) + pod.get("status", {}).get("initContainerStatuses", [])
        return [{key: item.get(key) for key in ("name", "imageID", "containerID", "restartCount", "ready")} for item in statuses]

    def execute(self, binary, plan_bytes, events, errors):
        # Only generated hexadecimal names enter the shell; user values remain argv.
        remote = "/tmp/llm-eval-" + uuid.uuid4().hex
        upload = f"umask 077; mkdir {remote} && cat > {remote}/worker && chmod 700 {remote}/worker"
        cleanup = f"rm -f {remote}/worker {remote}/pid; rmdir {remote} 2>/dev/null || true"
        # On a disconnected exec session the worker may outlive kubectl. Only
        # stop the PID whose executable still matches this unique upload path.
        stop_and_cleanup = (
            f'if test -f {remote}/pid; then read worker_pid < {remote}/pid; '
            'case "$worker_pid" in ""|*[!0-9]*) ;; *) '
            f'if test "$(readlink /proc/$worker_pid/exe)" = "{remote}/worker"; then '
            'kill -TERM "$worker_pid" 2>/dev/null; fi ;; esac; fi; ' + cleanup)
        try:
            with binary.open("rb") as binary_file:
                subprocess.run(self.exec_prefix + ["/bin/sh", "-c", upload], stdin=binary_file,
                               stdout=errors, stderr=errors, timeout=120, check=True)
            # Keep the shell alive to reap the worker and clean its private directory.
            # Preserve stdin before backgrounding: noninteractive shells redirect
            # asynchronous commands' fd 0 to /dev/null before applying <&0.
            run = (f"exec 3<&0; trap '{cleanup}' EXIT; "
                   "trap 'kill -TERM \"$worker_pid\" 2>/dev/null; wait \"$worker_pid\"; exit 130' HUP INT TERM; "
                   f"{remote}/worker <&3 3<&- & worker_pid=$!; exec 3<&-; echo \"$worker_pid\" > {remote}/pid; wait \"$worker_pid\"")
            result = subprocess.run(self.exec_prefix + ["/bin/sh", "-c", run], input=plan_bytes,
                                    stdout=events, stderr=errors)
            after = self.status()
            self.provenance["container_status_after"] = after
            if after != self.provenance["container_status"]:
                raise RuntimeError("Pod container identity/readiness/restarts changed during evaluation")
            return result.returncode
        finally:
            subprocess.run(self.exec_prefix + ["/bin/sh", "-c", stop_and_cleanup], stdout=errors, stderr=errors, timeout=30, check=True)
