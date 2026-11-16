import json
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "scripts"))
from render import render, validate_settings
from common import ROOT, read_settings


class RenderTests(unittest.TestCase):
    def settings(self, target="gke"):
        settings = read_settings(ROOT / "infra/examples/workload.env.example")
        settings.update(TARGET=target, GATEWAY_IMAGE="registry.example/gateway@sha256:" + "a"*64,
                        RUNTIME_IMAGE="registry.example/runtime@sha256:" + "b"*64,
                        MODEL_SHA256="c"*64, MODEL_URI="gs://example-models/model.gguf",
                        MODEL_SERVICE_ACCOUNT="models@example-project.iam.gserviceaccount.com")
        return settings

    def test_cpu_and_portable_render(self):
        for target in ("base", "gke"):
            with self.subTest(target=target), tempfile.TemporaryDirectory() as directory:
                destination = Path(directory)
                manifest = render(self.settings(target), destination)
                content = manifest.read_text()
                self.assertNotIn("REPLACE", content)
                self.assertIn("restartPolicy: Always", content)
                self.assertIn("terminationGracePeriodSeconds: 160", content)
                self.assertIn("readOnlyRootFilesystem: true", content)
                self.assertIn("type: Recreate", content)
                self.assertNotIn('"launch"', content)
                self.assertEqual("name: download-model" in content, target == "gke")
                config = json.loads((destination / "base/gateway.config.json").read_text())
                self.assertEqual(config["models"][0]["context_limit"], 4096)
                self.assertEqual(config["models"][0]["default_output_tokens"], 1024)
                self.assertTrue((destination / "release.json").is_file())

    def test_gpu_render(self):
        settings = self.settings()
        settings["GPU"] = "true"
        with tempfile.TemporaryDirectory() as directory:
            content = render(settings, Path(directory)).read_text()
            self.assertIn("nvidia.com/gpu:", content)
            self.assertIn("workload: gpu", content)
            self.assertIn('- "99"', content)

    def test_missing_pins_and_invalid_budget_are_rejected(self):
        for name, value in [("GATEWAY_IMAGE", "gateway:latest"), ("MODEL_SHA256", "bad"),
                            ("MAX_OUTPUT_TOKENS", "4096"), ("RUNTIME_THREADS", "0"), ("GPU", "yes")]:
            with self.subTest(setting=name):
                settings = self.settings()
                settings[name] = value
                with self.assertRaises(ValueError):
                    validate_settings(settings)


if __name__ == "__main__":
    unittest.main()
