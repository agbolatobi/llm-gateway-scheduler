package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"llm-gateway-scheduler/gateway/internal/registry"
)

func TestQueueRunningModelBurst(testContext *testing.T) {
	path := os.Getenv("GATEWAY_LIVE_CONFIG")
	if path == "" {
		testContext.Skip("set GATEWAY_LIVE_CONFIG for a real-model burst")
	}
	config, err := registry.Load(path)
	if err != nil {
		testContext.Fatal(err)
	}
	handler, reports := liveProxy(testContext, config, DefaultProxyOptions())
	testServer := httptest.NewServer(handler)
	defer testServer.Close()
	start := make(chan struct{})
	finished := make(chan error, 3)
	for count := 0; count < 3; count++ {
		go func(count int) {
			<-start
			body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"Explain what a CPU cache does."}],"max_tokens":32,"temperature":0}`, config.Models[0].ID)
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			request, _ := http.NewRequestWithContext(ctx, "POST", testServer.URL+ChatCompletionsPath, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer alice-one")
			request.Header.Set("X-Request-ID", fmt.Sprintf("burst-%d", count))
			response, err := http.DefaultClient.Do(request)
			if err == nil {
				_, err = io.Copy(io.Discard, response.Body)
				response.Body.Close()
				if response.StatusCode != 200 {
					err = fmt.Errorf("burst status %d", response.StatusCode)
				}
			}
			finished <- err
		}(count)
	}
	close(start)
	for count := 0; count < 3; count++ {
		if err := <-finished; err != nil {
			testContext.Error(err)
		}
	}
	queued := false
	for count := 0; count < 3; count++ {
		timingRecord := getReport(testContext, reports)
		if timingRecord.Outcome != "completed" || timingRecord.CapacityState != "released" || timingRecord.SchedulerPolicy != "fifo" {
			testContext.Errorf("burst result: %+v", timingRecord)
		}
		if timingRecord.QueueWaitMS > 1 {
			queued = true
		}
		testContext.Logf("%s queue_wait_ms=%.2f charged=%d source=%s", timingRecord.RequestID, timingRecord.QueueWaitMS, timingRecord.ChargedTokens, timingRecord.UsageSource)
	}
	if !queued {
		testContext.Error("burst did not exercise waiting")
	}
}
