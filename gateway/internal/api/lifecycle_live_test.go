package api

import (
	"bufio"
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

// Opt-in test using the registered runtime. It never starts/stops the runtime.
func TestLifecycleRunningModel(testContext *testing.T) {
	path := os.Getenv("GATEWAY_LIVE_CONFIG")
	if path == "" {
		testContext.Skip("set GATEWAY_LIVE_CONFIG for live usage and cancellation checks")
	}
	config, err := registry.Load(path)
	if err != nil {
		testContext.Fatal(err)
	}
	handler, reports := liveProxy(testContext, config, DefaultProxyOptions())
	testServer := httptest.NewServer(handler)
	defer testServer.Close()
	send := func(ctx context.Context, stream bool, cap int) (*http.Response, error) {
		body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"Count upwards from one, explaining each number."}],"max_tokens":%d,"temperature":0,"stream":%t}`, config.Models[0].ID, cap, stream)
		request, err := http.NewRequestWithContext(ctx, "POST", testServer.URL+ChatCompletionsPath, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Authorization", "Bearer alice-one")
		request.Header.Set("Content-Type", "application/json")
		return (&http.Client{Timeout: 125 * time.Second}).Do(request)
	}
	for _, stream := range []bool{false, true} {
		request, err := send(context.Background(), stream, 8)
		if err != nil {
			testContext.Fatal(err)
		}
		responseBody, err := io.ReadAll(request.Body)
		request.Body.Close()
		if err != nil || request.StatusCode != 200 {
			testContext.Fatal(request.StatusCode, string(responseBody), err)
		}
		record := getReport(testContext, reports)
		if record.UsageSource != "backend" || record.CapacityState != "released" {
			testContext.Fatalf("live settlement: %+v", record)
		}
		testContext.Logf("stream=%t reserved=%d charged=%d source=%s capacity=%s", stream, record.ReservedTokens, record.ChargedTokens, record.UsageSource, record.CapacityState)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := send(ctx, true, 256)
	if err != nil {
		testContext.Fatal(err)
	}
	reader := bufio.NewReader(request.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			testContext.Fatal(err)
		}
		if strings.HasPrefix(line, "data: {") {
			break
		}
	}
	cancel()
	request.Body.Close()
	record := getReport(testContext, reports)
	if record.UsageSource != "fallback" || (record.CapacityState != "released" && record.CapacityState != "quarantined_pending_idle") || record.CompletionMS != nil {
		testContext.Fatalf("live cancellation: %+v", record)
	}
	testContext.Logf("cancel outcome=%s charged=%d capacity=%s", record.Outcome, record.ChargedTokens, record.CapacityState)
	if record.CapacityState == "quarantined_pending_idle" {
		wait, cancelWait := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancelWait()
		for {
			idle, err := handler.execution.Idle(wait, config.Models[0])
			if err == nil && idle {
				break
			}
			if wait.Err() != nil {
				testContext.Fatal("backend did not become idle")
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	request, err = send(context.Background(), false, 8)
	if err != nil {
		testContext.Fatal(err)
	}
	responseBody, err := io.ReadAll(request.Body)
	request.Body.Close()
	if err != nil || request.StatusCode != 200 {
		testContext.Fatal("request after cancellation failed", request.StatusCode, string(responseBody), err)
	}
	if getReport(testContext, reports).CapacityState != "released" {
		testContext.Fatal("capacity leaked after recovery")
	}
}
