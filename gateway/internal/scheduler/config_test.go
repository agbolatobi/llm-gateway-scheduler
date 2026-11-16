package scheduler

import (
	"encoding/json"
	"testing"
)

func TestConfigDefaultsAndValidation(testContext *testing.T) {
	var config Config
	if err := json.Unmarshal([]byte(`{}`), &config); err != nil || config != Default() {
		testContext.Fatal(config, err)
	}
	for _, raw := range []string{
		`null`, `{"policy":null}`, `{"policy":"other"}`, `{"max_waiting_requests":-1}`,
		`{"max_waiting_per_runtime":0}`, `{"max_queue_wait_seconds":0}`,
		`{"aging_threshold_seconds":0}`, `{"unknown":1}`,
	} {
		if err := json.Unmarshal([]byte(raw), &config); err == nil {
			testContext.Errorf("accepted %s", raw)
		}
	}
}
