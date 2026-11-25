// Package evaluation runs repeatable, timed workloads beside a llama.cpp runtime.
package evaluation

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sync"

	"llm-gateway-scheduler/gateway/internal/registry"
)

type Job struct {
	ID        string  `json:"id"`
	AtSeconds float64 `json:"at_seconds"`
	Class     string  `json:"class"`
	Prompt    string  `json:"prompt"`
	MaxTokens int     `json:"max_tokens"`
}
type Trace struct {
	Name string `json:"name"`
	Jobs []Job  `json:"jobs"`
}
type Plan struct {
	Version               int             `json:"version"`
	Config                registry.Config `json:"gateway_config"`
	Model                 string          `json:"model"`
	Traces                []Trace         `json:"traces"`
	Modes                 []string        `json:"modes"`
	Repetitions           int             `json:"repetitions"`
	TimeoutSeconds        int             `json:"timeout_seconds"`
	GatewayTimeoutSeconds int             `json:"gateway_timeout_seconds,omitempty"`
	MaxInFlight           int             `json:"max_in_flight"`
}

func (plan Plan) Validate() error {
	if plan.Version != 1 || plan.Repetitions < 1 || plan.Repetitions > 10 || plan.TimeoutSeconds < 1 || plan.TimeoutSeconds > 1800 || plan.MaxInFlight < 1 || plan.MaxInFlight > 128 {
		return fmt.Errorf("invalid version, repeats (1..10), timeout (1..1800), or client concurrency (1..128)")
	}
	if plan.GatewayTimeoutSeconds < 0 || plan.GatewayTimeoutSeconds > 1800 {
		return fmt.Errorf("gateway timeout must be 1..1800, or omitted for the 120-second default")
	}
	if plan.GatewayTimeoutSeconds > 0 && (plan.TimeoutSeconds <= plan.GatewayTimeoutSeconds || plan.Config.SchedulerConfig().MaxWaitSeconds >= plan.GatewayTimeoutSeconds) {
		return fmt.Errorf("deadlines must satisfy queue wait < gateway request < client request")
	}
	if err := plan.Config.Validate(); err != nil {
		return err
	}
	model, found := plan.Config.Lookup(plan.Model)
	if !found {
		return fmt.Errorf("model is not registered")
	}
	seenModes := map[string]bool{}
	if len(plan.Modes) < 1 || len(plan.Modes) > 3 {
		return fmt.Errorf("select 1..3 modes")
	}
	for _, mode := range plan.Modes {
		if (mode != "direct" && mode != "fifo" && mode != "interactive_priority_aging") || seenModes[mode] {
			return fmt.Errorf("invalid or duplicate mode")
		}
		seenModes[mode] = true
	}
	if len(plan.Traces) < 1 || len(plan.Traces) > 8 {
		return fmt.Errorf("select 1..8 traces")
	}
	seenTraces := map[string]bool{}
	for _, trace := range plan.Traces {
		if trace.Name == "" || seenTraces[trace.Name] || len(trace.Jobs) < 1 || len(trace.Jobs) > 200 {
			return fmt.Errorf("invalid trace")
		}
		seenTraces[trace.Name] = true
		seenJobs := map[string]bool{}
		startsAtZero := false
		for _, job := range trace.Jobs {
			if job.ID == "" || seenJobs[job.ID] || math.IsNaN(job.AtSeconds) || math.IsInf(job.AtSeconds, 0) || job.AtSeconds < 0 || job.AtSeconds > 1800 || len(job.Prompt) < 1 || len(job.Prompt) > 200000 || job.MaxTokens < 1 || job.MaxTokens > model.MaxOutputTokens || (job.Class != "interactive" && job.Class != "background") {
				return fmt.Errorf("invalid job %q", job.ID)
			}
			seenJobs[job.ID] = true
			startsAtZero = startsAtZero || job.AtSeconds == 0
		}
		if !startsAtZero {
			return fmt.Errorf("trace %s must start at zero", trace.Name)
		}
	}
	return nil
}

// Recorder serializes concurrent evidence without relying on wall-clock ordering.
type Recorder struct {
	mutex   sync.Mutex
	encoder *json.Encoder
	err     error
}

func NewRecorder(writer io.Writer) *Recorder { return &Recorder{encoder: json.NewEncoder(writer)} }
func (recorder *Recorder) Emit(event string, fields map[string]any) {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	fields["event"] = event
	if recorder.err == nil {
		recorder.err = recorder.encoder.Encode(fields)
	}
}
func (recorder *Recorder) Err() error {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	return recorder.err
}
