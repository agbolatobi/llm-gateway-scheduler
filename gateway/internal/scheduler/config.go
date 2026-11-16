package scheduler

import (
	"bytes"
	"encoding/json"
	"fmt"
)

const FIFO = "fifo"
const Priority = "interactive_priority_aging"

type Config struct {
	Policy         string `json:"policy"`
	MaxWaiting     int    `json:"max_waiting_requests"`
	MaxPerRuntime  int    `json:"max_waiting_per_runtime"`
	MaxWaitSeconds int    `json:"max_queue_wait_seconds"`
	AgingSeconds   int    `json:"aging_threshold_seconds"`
}

func Default() Config { return Config{FIFO, 16, 8, 30, 5} }

func (config *Config) UnmarshalJSON(raw []byte) error {
	type plain Config
	value := plain(Default())
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if fields == nil {
		return fmt.Errorf("scheduler must be an object")
	}
	for _, fieldValue := range fields {
		if bytes.Equal(bytes.TrimSpace(fieldValue), []byte("null")) {
			return fmt.Errorf("scheduler fields cannot be null")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	*config = Config(value)
	return config.Validate()
}

func (config Config) Validate() error {
	if config.Policy != FIFO && config.Policy != Priority {
		return fmt.Errorf("scheduler policy must be fifo or interactive_priority_aging")
	}
	if config.MaxWaiting < 0 || config.MaxWaiting > 1024 || config.MaxPerRuntime < 1 || config.MaxPerRuntime > 1024 {
		return fmt.Errorf("waiting limits must be 0..1024 globally and 1..1024 per runtime")
	}
	if config.MaxWaitSeconds < 1 || config.MaxWaitSeconds > 1800 || config.AgingSeconds < 1 || config.AgingSeconds > 1800 {
		return fmt.Errorf("queue wait and aging must be 1..1800 seconds")
	}
	return nil
}
