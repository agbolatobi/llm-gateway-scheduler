package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"llm-gateway-scheduler/gateway/internal/capacity"
	"llm-gateway-scheduler/gateway/internal/registry"
	"llm-gateway-scheduler/gateway/internal/scheduler"
)

func lifecycleBackend(testContext *testing.T, busy *atomic.Bool, reply http.HandlerFunc) registry.Config {
	testContext.Helper()
	testServer := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/slots" {
			fmt.Fprintf(responseWriter, `[{"id":0,"n_ctx":4096,"is_processing":%t}]`, busy.Load())
			return
		}
		if request.URL.Path == ChatCompletionsPath {
			reply(responseWriter, request)
			return
		}
		http.NotFound(responseWriter, request)
	}))
	testContext.Cleanup(func() { testServer.CloseClientConnections(); testServer.Close() })
	config := registry.Default()
	settings := scheduler.Default()
	settings.MaxWaiting = 0
	config.Gateway.Scheduler = &settings
	config.Models[0].Backend.BaseURL = testServer.URL
	return config
}
func lifecycleHandler(testContext *testing.T, config registry.Config, opts ProxyOptions) (*RegisteredHandler, <-chan TimingRecord) {
	handler, reports := liveProxy(testContext, config, opts)
	handler.counter = &fakeCounter{count: 3}
	return handler, reports
}
func withUsage(body, usage string) string {
	return strings.TrimSuffix(body, "}") + `,"usage":` + usage + "}"
}

const validUsage = `{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}`

func TestLifecycleActualUsageRefundsBudget(testContext *testing.T) {
	for _, stream := range []bool{false, true} {
		testContext.Run(fmt.Sprint(stream), func(testContext *testing.T) {
			var busy atomic.Bool
			config := lifecycleBackend(testContext, &busy, func(responseWriter http.ResponseWriter, httpRequest *http.Request) {
				if stream {
					var request ChatRequest
					json.NewDecoder(httpRequest.Body).Decode(&request)
					if request.StreamOptions == nil || !request.StreamOptions.IncludeUsage {
						testContext.Error("usage was not requested internally")
					}
					responseWriter.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(responseWriter, chunkFrame("hello", "")+chunkFrame("", "stop")+`data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"local-llm","choices":[],"usage":`+validUsage+"}\n\ndata: [DONE]\n\n")
				} else {
					responseWriter.Header().Set("Content-Type", "application/json")
					io.WriteString(responseWriter, withUsage(completion("hello"), validUsage))
				}
			})
			config.Quotas.User.Tokens = 140
			handler, reports := lifecycleHandler(testContext, config, DefaultProxyOptions())
			for index := 0; index < 2; index++ {
				responseRecorder := httptest.NewRecorder()
				handler.ServeHTTP(responseRecorder, proxyRequest("default", stream))
				if responseRecorder.Code != 200 {
					testContext.Fatal("actual usage was not refunded", responseRecorder.Code, responseRecorder.Body)
				}
				if stream && strings.Contains(responseRecorder.Body.String(), `"usage"`) {
					testContext.Fatal("unsolicited usage event leaked")
				}
				timingRecord := getReport(testContext, reports)
				if timingRecord.ReservedTokens != 131 || timingRecord.ChargedTokens != 5 || timingRecord.UsageSource != "backend" || timingRecord.CapacityState != "released" || timingRecord.UserID != "alice" || timingRecord.Model != "local-llm" || timingRecord.RuntimeID == "" {
					testContext.Fatalf("bad settlement: %+v", timingRecord)
				}
			}
			responseRecorder := httptest.NewRecorder()
			handler.ServeHTTP(responseRecorder, proxyRequest("default", stream))
			if responseRecorder.Code != 429 {
				testContext.Fatal("spent tokens were not retained", responseRecorder.Code, responseRecorder.Body)
			}
		})
	}
}

func TestLifecycleUsageFallback(testContext *testing.T) {
	for _, usage := range []string{"null", `{}`, `{"prompt_tokens":3,"completion_tokens":2,"total_tokens":6}`, `{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}`, `{"prompt_tokens":3,"completion_tokens":129,"total_tokens":132}`, `{"prompt_tokens":3,"completion_tokens":-1,"total_tokens":2}`, `{"prompt_tokens":3,"completion_tokens":2.5,"total_tokens":5.5}`} {
		var busy atomic.Bool
		config := lifecycleBackend(testContext, &busy, func(responseWriter http.ResponseWriter, request *http.Request) {
			responseWriter.Header().Set("Content-Type", "application/json")
			io.WriteString(responseWriter, withUsage(completion("hello"), usage))
		})
		config.Quotas.User.Tokens = 140
		handler, reports := lifecycleHandler(testContext, config, DefaultProxyOptions())
		responseRecorder := httptest.NewRecorder()
		handler.ServeHTTP(responseRecorder, proxyRequest("local-llm", false))
		timingRecord := getReport(testContext, reports)
		if responseRecorder.Code != 200 || timingRecord.ChargedTokens != 131 || timingRecord.UsageSource != "fallback" || timingRecord.Usage != nil {
			testContext.Fatal(usage, responseRecorder.Code, timingRecord)
		}
		responseRecorder = httptest.NewRecorder()
		handler.ServeHTTP(responseRecorder, proxyRequest("local-llm", false))
		if responseRecorder.Code != 429 {
			testContext.Fatal("fallback undercharged", responseRecorder.Code)
		}
	}
}

func TestLifecycleBusyAliasRefundAndRecovery(testContext *testing.T) {
	var busy atomic.Bool
	started, release := make(chan struct{}), make(chan struct{})
	config := lifecycleBackend(testContext, &busy, func(responseWriter http.ResponseWriter, request *http.Request) {
		busy.Store(true)
		close(started)
		select {
		case <-release:
		case <-request.Context().Done():
		}
		busy.Store(false)
		responseWriter.Header().Set("Content-Type", "application/json")
		io.WriteString(responseWriter, withUsage(completion("hello"), validUsage))
	})
	config.Quotas.User.Tokens = 400
	handler, reports := lifecycleHandler(testContext, config, DefaultProxyOptions())
	done := make(chan struct{})
	go func() { defer close(done); handler.ServeHTTP(httptest.NewRecorder(), proxyRequest("local-llm", false)) }()
	<-started
	responseRecorder := httptest.NewRecorder()
	handler.ServeHTTP(responseRecorder, proxyRequest("default", false))
	if responseRecorder.Code != 503 || !strings.Contains(responseRecorder.Body.String(), "capacity_exceeded") {
		testContext.Fatal(responseRecorder.Code, responseRecorder.Body)
	}
	timingRecord := getReport(testContext, reports)
	if timingRecord.ChargedTokens != 0 || timingRecord.UsageSource != "not_dispatched" {
		testContext.Fatal(timingRecord)
	}
	next := config.Clone()
	next.Models = nil
	if handler.Reload(next) == nil {
		testContext.Fatal("removed occupied model")
	}
	close(release)
	<-done
	getReport(testContext, reports)
	// Reservation state is observable without adding a production debug endpoint.
	reservation, decision := handler.quotas.ReserveForModel("alice", "local-llm", 395)
	if !decision.Allowed {
		testContext.Fatal("busy rejection leaked tokens", decision)
	}
	handler.quotas.SettleTokens(reservation, 0)
	base, _ := registry.LocalURL(config.Models[0].Backend.BaseURL)
	if handler.capacity.Occupied(base) {
		testContext.Fatal("completed request leaked capacity")
	}
}

func TestLifecycleCancellationWaitsForBackendIdle(testContext *testing.T) {
	var busy atomic.Bool
	started, cancelled := make(chan struct{}), make(chan struct{})
	config := lifecycleBackend(testContext, &busy, func(responseWriter http.ResponseWriter, request *http.Request) {
		busy.Store(true)
		responseWriter.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(responseWriter, chunkFrame("hello", ""))
		responseWriter.(http.Flusher).Flush()
		close(started)
		<-request.Context().Done()
		close(cancelled)
	})
	handler, reports := lifecycleHandler(testContext, config, DefaultProxyOptions())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	responseRecorder := &observedWriter{ResponseRecorder: httptest.NewRecorder(), ready: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if panicValue := recover(); panicValue != nil && panicValue != http.ErrAbortHandler {
				testContext.Errorf("panic: %v", panicValue)
			}
		}()
		handler.ServeHTTP(responseRecorder, proxyRequest("local-llm", true).WithContext(ctx))
	}()
	<-started
	// Wait for the gateway to observe a backend event before cancelling.
	<-responseRecorder.ready
	cancel()
	<-cancelled
	base, _ := registry.LocalURL(config.Models[0].Backend.BaseURL)
	if !handler.capacity.Occupied(base) {
		testContext.Fatal("capacity released on HTTP disconnect")
	}
	<-done
	timingRecord := getReport(testContext, reports)
	if timingRecord.CapacityState != "quarantined_pending_idle" || timingRecord.ChargedTokens != 131 || timingRecord.UsageSource != "fallback" {
		testContext.Fatal(timingRecord)
	}
	probe := httptest.NewRecorder()
	handler.ServeHTTP(probe, proxyRequest("default", false))
	if probe.Code != 503 || !strings.Contains(probe.Body.String(), "capacity_unconfirmed") {
		testContext.Fatal(probe.Code, probe.Body)
	}
	getReport(testContext, reports)
	busy.Store(false)
	if !handler.capacity.Recover(context.Background(), base, func(ctx context.Context) (bool, error) { return handler.execution.Idle(ctx, config.Models[0]) }) {
		testContext.Fatal("idle runtime did not recover")
	}
	lease, err := handler.capacity.Acquire(base)
	if err != nil {
		testContext.Fatal(err)
	}
	lease.Release()
}

type observedWriter struct {
	*httptest.ResponseRecorder
	ready chan struct{}
	once  sync.Once
}

func (writer *observedWriter) Write(responseBody []byte) (int, error) {
	count, err := writer.ResponseRecorder.Write(responseBody)
	writer.once.Do(func() { close(writer.ready) })
	return count, err
}

func TestLifecycleUnknownDispatchDoesNotTrustIdle(testContext *testing.T) {
	var busy atomic.Bool
	config := lifecycleBackend(testContext, &busy, func(responseWriter http.ResponseWriter, request *http.Request) { responseWriter.WriteHeader(500) })
	handler, reports := lifecycleHandler(testContext, config, DefaultProxyOptions())
	responseRecorder := httptest.NewRecorder()
	handler.ServeHTTP(responseRecorder, proxyRequest("local-llm", false))
	timingRecord := getReport(testContext, reports)
	if timingRecord.CapacityState != "quarantined_unknown_dispatch" || timingRecord.ChargedTokens != 131 {
		testContext.Fatal(timingRecord)
	}
	responseRecorder = httptest.NewRecorder()
	handler.ServeHTTP(responseRecorder, proxyRequest("local-llm", false))
	if responseRecorder.Code != 503 || !strings.Contains(responseRecorder.Body.String(), "capacity_unconfirmed") {
		testContext.Fatal(responseRecorder.Code, responseRecorder.Body)
	}
}

func TestLifecycleFinishExactlyOnce(testContext *testing.T) {
	handler := registeredTest(testContext, registry.Default(), &fakeCounter{count: 3})
	reservation, quotaDecision := handler.quotas.ReserveForModel("alice", "local-llm", 131)
	if !quotaDecision.Allowed {
		testContext.Fatal(quotaDecision)
	}
	prepared, err := newPreparedRequest(registry.Default().Models[0], ChatRequest{Messages: []Message{}}, 128, handler.quotas, reservation, 131)
	if err != nil {
		testContext.Fatal(err)
	}
	capacityManager := capacity.New(1)
	prepared.lease, _ = capacityManager.Acquire("backend")
	if err := prepared.settleUsageAndReleaseCapacity(); err != nil {
		testContext.Fatal(err)
	}
	if err := prepared.settleUsageAndReleaseCapacity(); err != nil {
		testContext.Fatal(err)
	}
	lease, err := capacityManager.Acquire("backend")
	if err != nil {
		testContext.Fatal(err)
	}
	prepared.settleUsageAndReleaseCapacity()
	if _, err = capacityManager.Acquire("other"); err != capacity.ErrFull {
		testContext.Fatal("duplicate cleanup freed another request", err)
	}
	lease.Release()
}
