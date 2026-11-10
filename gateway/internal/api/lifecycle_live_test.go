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
func TestLifecycleRunningModel(t *testing.T) {
	path := os.Getenv("GATEWAY_LIVE_CONFIG")
	if path == "" {
		t.Skip("set GATEWAY_LIVE_CONFIG for live usage and cancellation checks")
	}
	c, err := registry.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	h, reports := liveProxy(t, c, DefaultProxyOptions())
	s := httptest.NewServer(h)
	defer s.Close()
	send := func(ctx context.Context, stream bool, cap int) (*http.Response, error) {
		body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"Count upwards from one, explaining each number."}],"max_tokens":%d,"temperature":0,"stream":%t}`, c.Models[0].ID, cap, stream)
		r, err := http.NewRequestWithContext(ctx, "POST", s.URL+ChatCompletionsPath, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		r.Header.Set("Authorization", "Bearer alice-one")
		r.Header.Set("Content-Type", "application/json")
		return (&http.Client{Timeout: 125 * time.Second}).Do(r)
	}
	for _, stream := range []bool{false, true} {
		r, err := send(context.Background(), stream, 8)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil || r.StatusCode != 200 {
			t.Fatal(r.StatusCode, string(b), err)
		}
		record := getReport(t, reports)
		if record.UsageSource != "backend" || record.CapacityState != "released" {
			t.Fatalf("live settlement: %+v", record)
		}
		t.Logf("stream=%t reserved=%d charged=%d source=%s capacity=%s", stream, record.ReservedTokens, record.ChargedTokens, record.UsageSource, record.CapacityState)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := send(ctx, true, 256)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(r.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "data: {") {
			break
		}
	}
	cancel()
	r.Body.Close()
	record := getReport(t, reports)
	if record.UsageSource != "fallback" || (record.CapacityState != "released" && record.CapacityState != "quarantined_pending_idle") || record.CompletionMS != nil {
		t.Fatalf("live cancellation: %+v", record)
	}
	t.Logf("cancel outcome=%s charged=%d capacity=%s", record.Outcome, record.ChargedTokens, record.CapacityState)
	if record.CapacityState == "quarantined_pending_idle" {
		wait, cancelWait := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancelWait()
		for {
			idle, err := h.execution.Idle(wait, c.Models[0])
			if err == nil && idle {
				break
			}
			if wait.Err() != nil {
				t.Fatal("backend did not become idle")
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	r, err = send(context.Background(), false, 8)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil || r.StatusCode != 200 {
		t.Fatal("request after cancellation failed", r.StatusCode, string(b), err)
	}
	if getReport(t, reports).CapacityState != "released" {
		t.Fatal("capacity leaked after recovery")
	}
}
