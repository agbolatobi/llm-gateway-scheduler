"""Offline metric, integrity, planning and Kubernetes transport checks."""

import json
import subprocess
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

from benchmarks.evaluation.plan import resolve
from benchmarks.evaluation.audit import audit_classes
from benchmarks.evaluation.report import p95, seal, summarize, verify
from benchmarks.evaluation.transport import GKE


class MetricTests(unittest.TestCase):
    def setUp(self):
        self.jobs = [{"id": "a", "class": "background"}, {"id": "b", "class": "interactive"}]
        self.start = {"trial_id": "trial", "trace": "contended", "mode": "fifo"}
        self.requests = [dict(job_id="a", request_id="a", **{"class": "background"}, outcome="completed",
                              sent_ms=0, ttft_ms=10, e2e_ms=20, output_tokens=5),
                         dict(job_id="b", request_id="b", **{"class": "interactive"}, outcome="rejected",
                              sent_ms=1, ttft_ms=None, e2e_ms=None, output_tokens=None)]
        self.gateway = {"a": {"scheduler_policy": "fifo", "outcome": "completed", "queue_wait_ms": 7}}

    def summary(self, **changes):
        arguments = dict(start=self.start, end={"elapsed_ms": 2000}, requests=self.requests,
                         gateway_records=self.gateway, jobs=self.jobs, request_class="all")
        arguments.update(changes)
        return summarize(**arguments)

    def test_failed_work_stays_in_denominator_not_latency(self):
        summary = self.summary()
        self.assertEqual(summary["completion_percent"]["value"], 50)
        self.assertEqual(summary["p95_e2e_ms"]["value"], 20)
        self.assertEqual(summary["output_tokens_per_second"]["value"], 2.5)
        self.assertEqual(summary["max_background_queue_wait_ms"]["value"], 7)

    def test_missing_usage_is_not_zero(self):
        self.requests[0]["output_tokens"] = None
        self.assertIsNone(self.summary()["output_tokens_per_second"]["value"])

    def test_incomplete_and_missing_queue_evidence(self):
        self.assertIsNone(self.summary(end=None)["completion_percent"]["value"])
        self.assertIsNone(self.summary(end=None)["output_tokens_per_second"]["value"])
        self.assertIsNone(self.summary(gateway_records={})["max_background_queue_wait_ms"]["value"])
        summary = self.summary(requests=self.requests[:1])
        self.assertEqual(summary["outcomes"]["unfinished"], 1)
        self.assertIsNone(summary["output_tokens_per_second"]["value"])

    def test_background_timeout_is_in_wait_maximum(self):
        self.requests[0]["outcome"] = "timed_out"
        self.gateway["a"].update(outcome="queue_timeout", queue_wait_ms=30000)
        self.assertEqual(self.summary()["max_background_queue_wait_ms"]["value"], 30000)

    def test_direct_queue_unavailable_and_percentile_is_nearest_rank(self):
        self.start["mode"] = "direct"
        self.assertIsNone(self.summary()["max_background_queue_wait_ms"]["value"])
        self.assertEqual(p95(range(1, 101)), 95)
        self.assertIsNone(p95([]))

    def test_integrity_detects_evidence_edits(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            for name in ("plan.json", "events.jsonl", "provenance.json", "source.zip", "worker"):
                (directory / name).write_text("evidence")
            seal(directory)
            verify(directory)
            (directory / "events.jsonl").write_text("changed")
            with self.assertRaisesRegex(ValueError, "integrity"):
                verify(directory)


class PlanningTests(unittest.TestCase):
    def test_launch_paths_removed_without_mutating_original(self):
        config = {"gateway": {}, "models": [{"id": "local-llm", "launch": {"model_file": "local.gguf"}}]}
        prompts = {"prompts": [{"id": "p", "prompt": "hello"}], "test_cases": [{"id": "c", "prompt_id": "p", "max_tokens": 64}]}
        workload = {"version": 1, "traces": [{"name": "quiet", "jobs": [{"id": "j", "case": "c", "at_seconds": 0, "class": "interactive"}]}]}
        options = SimpleNamespace(trace=None, mode=None, model="local-llm", repetitions=3, timeout=180, max_in_flight=64)
        plan = resolve(config, prompts, workload, options)
        self.assertNotIn("launch", plan["gateway_config"]["models"][0])
        self.assertIn("launch", config["models"][0])
        self.assertEqual(plan["traces"][0]["jobs"][0]["prompt"], "hello")
        options.aging_seconds = 60
        options.queue_wait = 180
        options.gateway_timeout = 240
        options.timeout = 250
        tuned = resolve(config, prompts, workload, options)
        self.assertEqual(tuned["gateway_config"]["gateway"]["scheduler"]["aging_threshold_seconds"], 60)
        self.assertEqual(tuned["gateway_config"]["gateway"]["scheduler"]["max_queue_wait_seconds"], 180)
        self.assertEqual(tuned["gateway_timeout_seconds"], 240)
        self.assertEqual(config["gateway"], {})
        options.queue_wait = 240
        with self.assertRaisesRegex(ValueError, "Queue deadline"):
            resolve(config, prompts, workload, options)
        options.queue_wait = 180
        options.trace = ["missing"]
        with self.assertRaisesRegex(ValueError, "Unknown trace"):
            resolve(config, prompts, workload, options)


class KubernetesTransportTests(unittest.TestCase):
    def setUp(self):
        self.pod = {"metadata": {"name": "gateway-test", "uid": "uid-1"},
                    "spec": {"nodeName": "node-1", "containers": [{"name": "gateway", "image": "gateway:test"}],
                             "initContainers": [{"name": "runtime", "image": "runtime:test"}],
                             "volumes": [{"name": "config", "configMap": {"name": "gateway-config-123"}}]},
                    "status": {"conditions": [{"type": "Ready", "status": "True"}],
                               "initContainerStatuses": [{"name": "runtime", "imageID": "sha256:test", "restartCount": 0}]}}

    def output(self, command, **kwargs):
        if command[-2:] == ["uname", "-m"]:
            return "x86_64\n"
        if "pods" in command:
            return json.dumps({"items": [self.pod]})
        if "configmap" in command:
            return json.dumps({"data": {"gateway.config.json": '{"version":1}'}})
        return json.dumps(self.pod)

    def test_explicit_context_runtime_exec_cleanup_and_provenance(self):
        with patch("subprocess.check_output", side_effect=self.output), patch("subprocess.run") as run:
            run.return_value.returncode = 0
            transport = GKE("test-context", "llm-gateway")
            self.assertEqual(transport.architecture, "amd64")
            with tempfile.TemporaryDirectory() as temporary:
                binary = Path(temporary) / "worker"
                binary.write_bytes(b"binary")
                with (Path(temporary) / "events").open("wb") as events:
                    self.assertEqual(transport.execute(binary, b"{}", events, events), 0)
            commands = [call.args[0] for call in run.call_args_list]
            self.assertEqual(len(commands), 3)
            for command in commands:
                self.assertIn("test-context", command)
                self.assertIn("runtime", command)
            self.assertIn("cat > /tmp/llm-eval-", commands[0][-1])
            self.assertIn("rm -f /tmp/llm-eval-", commands[-1][-1])
            self.assertEqual(transport.provenance["pod_uid"], "uid-1")

    def test_replaced_pod_refused(self):
        with patch("subprocess.check_output", side_effect=self.output):
            transport = GKE("test-context", "llm-gateway")
            self.pod["metadata"]["uid"] = "uid-2"
            with self.assertRaisesRegex(RuntimeError, "replaced"):
                transport.status()

    def test_remote_shell_delivers_plan_to_background_worker(self):
        real_run = subprocess.run

        def local_shell(command, **kwargs):
            return real_run(command[-3:], **kwargs)

        with patch("subprocess.check_output", side_effect=self.output):
            transport = GKE("test-context", "llm-gateway")
            with tempfile.TemporaryDirectory() as temporary:
                binary = Path(temporary) / "worker"
                binary.write_bytes(b"#!/bin/sh\ncat\n")
                plan = b'{"traces": ["quiet", "contended"]}'
                with (Path(temporary) / "events").open("w+b") as events:
                    with patch("subprocess.run", side_effect=local_shell):
                        self.assertEqual(transport.execute(binary, plan, events, events), 0)
                    events.seek(0)
                    self.assertEqual(events.read(), plan)


class ClassificationAuditTests(unittest.TestCase):
    def test_mismatch_missing_and_correct_gateway_evidence(self):
        start = {"trial_id": "trial", "mode": "fifo"}
        jobs = [{"id": "bg", "class": "background"}]
        requests = [{"job_id": "bg", "request_id": "r", "class": "background", "sent_ms": 0}]
        self.assertEqual(len(audit_classes(start, requests, {"r": {"request_class": "interactive"}}, jobs)), 1)
        self.assertEqual(len(audit_classes(start, requests, {}, jobs)), 1)
        self.assertEqual(audit_classes(start, requests, {"r": {"request_class": "background"}}, jobs), [])
        start["mode"] = "direct"
        self.assertEqual(audit_classes(start, requests, {}, jobs), [])


if __name__ == "__main__":
    unittest.main()
