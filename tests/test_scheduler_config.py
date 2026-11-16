import copy
import json
from pathlib import Path
import unittest

from client.model_config import validate


class SchedulerConfigTests(unittest.TestCase):
    def test_defaults_partial_settings_and_invalid_fields(self):
        base = json.loads((Path(__file__).resolve().parents[1] / "gateway.config.json").read_text())
        for settings in ({}, {"policy":"fifo"}, {"policy":"interactive_priority_aging", "max_waiting_requests":0}):
            data = copy.deepcopy(base)
            data["gateway"]["scheduler"] = settings
            validate(data)
        for settings in (None, {"policy":None}, {"policy":"random"}, {"max_waiting_requests":True},
                         {"max_waiting_requests":-1}, {"max_waiting_per_runtime":0},
                         {"max_queue_wait_seconds":0}, {"aging_threshold_seconds":0}, {"unknown":1}):
            data = copy.deepcopy(base)
            data["gateway"]["scheduler"] = settings
            with self.assertRaises(ValueError):
                validate(data)


if __name__ == "__main__":
    unittest.main()
