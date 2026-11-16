from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


class SetupTests(unittest.TestCase):
    def test_fresh_checkout_creates_inputs_without_overwriting(self):
        with tempfile.TemporaryDirectory() as directory:
            checkout = Path(directory)
            shutil.copytree(ROOT / "deploy/scripts", checkout / "deploy/scripts")
            shutil.copytree(ROOT / "infra/examples", checkout / "infra/examples")
            command = ["python3", str(checkout / "deploy/scripts/setup.py")]
            subprocess.run(command, check=True, capture_output=True)
            personal = checkout / "infra/variables/workload.env"
            self.assertTrue(personal.exists())
            self.assertEqual(personal.stat().st_mode & 0o777, 0o600)
            personal.write_text("keep my personal settings\n")
            subprocess.run(command, check=True, capture_output=True)
            self.assertEqual(personal.read_text(), "keep my personal settings\n")
