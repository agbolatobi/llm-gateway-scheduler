package evaluation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"llm-gateway-scheduler/gateway/internal/api"
	"llm-gateway-scheduler/gateway/internal/auth"
	"llm-gateway-scheduler/gateway/internal/registry"
)

type trialAuthenticator struct{ token string }

func (verifier trialAuthenticator) Verify(token string) (auth.Principal, error) {
	if token != verifier.token {
		return auth.Principal{}, auth.ErrInvalidKey
	}
	return auth.Principal{KeyID: "evaluation", UserID: "evaluation"}, nil
}

func Run(ctx context.Context, plan Plan, recorder *Recorder) error {
	if err := plan.Validate(); err != nil {
		return err
	}
	model, _ := plan.Config.Lookup(plan.Model)
	client := newClient()
	defer client.CloseIdleConnections()
	if err := idle(ctx, client, model); err != nil {
		return err
	}
	environment, err := snapshot(ctx, client, model, true)
	if err != nil {
		return err
	}
	recorder.Emit("environment", map[string]any{"snapshot": environment, "cache_policy": "erase before each trial; identical 8-token warmup; runtime-default reuse within measured workload", "scope": "worker and temporary gateway beside a dedicated single-slot runtime; no ingress or live gateway measurement"})
	inspector := registry.NewInspector()
	for _, trace := range plan.Traces {
		for _, job := range trace.Jobs {
			count, err := inspector.CountInput(ctx, model, []map[string]string{{"role": "user", "content": job.Prompt}})
			if err != nil {
				return fmt.Errorf("token preflight %s: %w", job.ID, err)
			}
			if count+job.MaxTokens > model.ContextLimit {
				return fmt.Errorf("job %s exceeds model context", job.ID)
			}
			recorder.Emit("input_tokens", map[string]any{"trace": trace.Name, "job_id": job.ID, "tokens": count, "max_tokens": job.MaxTokens})
		}
	}
	// Rotate ordering to reduce systematic warm-machine/order bias.
	for repetition := 0; repetition < plan.Repetitions; repetition++ {
		for traceIndex, trace := range plan.Traces {
			for modeIndex := range plan.Modes {
				mode := plan.Modes[(modeIndex+repetition)%len(plan.Modes)]
				trialID := fmt.Sprintf("r%d-t%d-%s", repetition+1, traceIndex+1, mode)
				if err := runTrial(ctx, plan, model, client, trace, mode, trialID, recorder); err != nil {
					return fmt.Errorf("%s: %w", trialID, err)
				}
			}
		}
	}
	recorder.Emit("run_end", map[string]any{"complete": true})
	return recorder.Err()
}

func runTrial(ctx context.Context, plan Plan, model registry.Model, client *http.Client, trace Trace, mode, trialID string, recorder *Recorder) error {
	if err := idle(ctx, client, model); err != nil {
		return err
	}
	erased, err := resetCache(ctx, client, model)
	if err != nil {
		return fmt.Errorf("cache reset: %w", err)
	}
	warmup := trace.Jobs[0]
	warmup.MaxTokens = min(8, warmup.MaxTokens)
	warmCtx, cancelWarm := context.WithTimeout(ctx, time.Duration(plan.TimeoutSeconds)*time.Second)
	warmResult := Perform(warmCtx, client, model.Backend.BaseURL, model.Backend.ExpectedModel, "", "warmup:"+trialID, "warmup", warmup, time.Now(), recorder)
	cancelWarm()
	recorder.Emit("warmup", map[string]any{"trial_id": trialID, "cache_reset": erased, "result": warmResult})
	if warmResult.Outcome != "completed" {
		return fmt.Errorf("warmup failed: %s", warmResult.Error)
	}
	if err := idle(ctx, client, model); err != nil {
		return err
	}
	before, err := snapshot(ctx, client, model, false)
	if err != nil {
		return err
	}
	base, requestModel, key := model.Backend.BaseURL, model.Backend.ExpectedModel, ""
	config := plan.Config.Clone()
	options := api.DefaultProxyOptions()
	if plan.GatewayTimeoutSeconds > 0 {
		options.RequestTimeout = time.Duration(plan.GatewayTimeoutSeconds) * time.Second
	}
	var handler *api.RegisteredHandler
	var server *http.Server
	if mode != "direct" {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		config.Gateway.Listen = listener.Addr().String()
		policy := config.SchedulerConfig()
		policy.Policy = mode
		config.Gateway.Scheduler = &policy
		random := make([]byte, 32)
		if _, err := rand.Read(random); err != nil {
			listener.Close()
			return err
		}
		key = hex.EncodeToString(random)
		handler, err = api.NewRegisteredHandlerWithReporter(trialAuthenticator{key}, config, nil, options, func(record api.TimingRecord) {
			recorder.Emit("gateway", map[string]any{"trial_id": trialID, "record": record})
		})
		if err != nil {
			listener.Close()
			return err
		}
		defer handler.Close()
		server = &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
		defer server.Close()
		go server.Serve(listener)
		base = "http://" + listener.Addr().String()
		requestModel = model.ID
	}
	recorder.Emit("trial_start", map[string]any{"trial_id": trialID, "trace": trace.Name, "mode": mode, "jobs": len(trace.Jobs), "gateway_config": config, "snapshot": before, "gateway_timeouts_seconds": map[string]int{"request": int(options.RequestTimeout / time.Second), "idle": int(options.IdleTimeout / time.Second), "write": int(options.WriteTimeout / time.Second)}})
	started := time.Now()
	var pending sync.WaitGroup
	var terminalMutex sync.Mutex
	lastTerminalMS := 0.0
	inFlight := make(chan struct{}, plan.MaxInFlight)
	for jobIndex, job := range trace.Jobs {
		pending.Add(1)
		go func(jobIndex int, job Job) {
			defer pending.Done()
			requestID := fmt.Sprintf("eval-%d-%d", started.UnixNano(), jobIndex)
			result := Result{RequestID: requestID, JobID: job.ID, Class: job.Class, ScheduledMS: job.AtSeconds * 1000, Outcome: "not_sent", UsageReason: "request_not_sent"}
			defer func() {
				terminalMutex.Lock()
				lastTerminalMS = max(lastTerminalMS, result.TerminalMS)
				terminalMutex.Unlock()
				recorder.Emit("request", map[string]any{"trial_id": trialID, "result": result})
			}()
			timer := time.NewTimer(time.Until(started.Add(time.Duration(job.AtSeconds * float64(time.Second)))))
			defer timer.Stop()
			select {
			case <-ctx.Done():
				result.Error = "run_cancelled"
				result.TerminalMS = milliseconds(time.Since(started))
				return
			case <-timer.C:
			}
			select {
			case inFlight <- struct{}{}:
				defer func() { <-inFlight }()
			default:
				result.Error = "client_in_flight_limit"
				result.TerminalMS = milliseconds(time.Since(started))
				return
			}
			requestCtx, cancel := context.WithTimeout(ctx, time.Duration(plan.TimeoutSeconds)*time.Second)
			defer cancel()
			result = Perform(requestCtx, client, base, requestModel, key, trialID, requestID, job, started, recorder)
		}(jobIndex, job)
	}
	pending.Wait()
	elapsed := lastTerminalMS // Excludes writing the final evidence and cleanup.
	// Wait for handler cleanup so rejected/timed-out queue records are retained.
	if server != nil {
		handler.StopWaiting()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = server.Shutdown(shutdownCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("gateway cleanup: %w", err)
		}
	}
	after, snapshotErr := snapshot(ctx, client, model, false)
	recorder.Emit("trial_end", map[string]any{"trial_id": trialID, "elapsed_ms": elapsed, "snapshot": after})
	if snapshotErr != nil {
		return snapshotErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return recorder.Err()
}
