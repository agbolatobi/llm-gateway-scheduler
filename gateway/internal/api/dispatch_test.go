package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"llm-gateway-scheduler/gateway/internal/quota"
	"llm-gateway-scheduler/gateway/internal/registry"
	"llm-gateway-scheduler/gateway/internal/routing"
)

// Test-only reply consumer. This is deliberately not a production proxy:
// GATE-06 must implement response validation, streaming, timeouts and timing.
func dispatchHarness(client *routing.Client) func(http.ResponseWriter, *http.Request, *preparedRequest) {
	return func(responseWriter http.ResponseWriter, request *http.Request, prepared *preparedRequest) {
		response, err := prepared.sendToBackend(request.Context(), client)
		if err != nil {
			writeDispatchError(responseWriter, err)
			return
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, 65536))
		if err != nil {
			writeDispatchError(responseWriter, err)
			return
		}
		responseWriter.Header().Set("Content-Type", "application/json")
		responseWriter.WriteHeader(response.StatusCode)
		responseWriter.Write(body)
	}
}

func fakeRoutes(testContext *testing.T, generate func(int, http.ResponseWriter, *http.Request)) registry.Config {
	testContext.Helper()
	config := registry.Default()
	second := config.Models[0]
	second.ID, second.Aliases = "second", []string{"engineer"}
	second.Backend.RuntimeID, second.Backend.ExpectedModel = "second-runtime", "second-backend"
	config.Models = append(config.Models, second)
	for index := range config.Models {
		index, identity := index, config.Models[index].Backend.ExpectedModel
		backend := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
			switch request.URL.Path {
			case "/health":
				json.NewEncoder(responseWriter).Encode(map[string]string{"status": "ok"})
			case "/v1/models":
				json.NewEncoder(responseWriter).Encode(map[string]any{"data": []map[string]string{{"id": identity}}})
			case "/slots":
				responseWriter.Write([]byte(`[{"id":0,"n_ctx":4096,"is_processing":false}]`))
			case "/apply-template":
				responseWriter.Write([]byte(`{"prompt":"<chat>hello</chat>"}`))
			case "/tokenize":
				responseWriter.Write([]byte(`{"tokens":[1,2,3]}`))
			case ChatCompletionsPath:
				generate(index, responseWriter, request)
			default:
				testContext.Error("unexpected backend path", request.URL.Path)
				http.NotFound(responseWriter, request)
			}
		}))
		testContext.Cleanup(func() { backend.CloseClientConnections(); backend.Close() })
		config.Models[index].Backend.BaseURL = backend.URL
	}
	return config
}

func routingClient(testContext *testing.T) *routing.Client {
	testContext.Helper()
	client := routing.NewClient()
	testContext.Cleanup(client.CloseIdleConnections)
	return client
}

func TestDispatchTwoBackendsAndPayload(testContext *testing.T) {
	var calls [2]atomic.Int64
	config := fakeRoutes(testContext, func(index int, responseWriter http.ResponseWriter, request *http.Request) {
		calls[index].Add(1)
		for _, name := range []string{"Authorization", "Cookie", "Idempotency-Key", "X-Idempotency-Key", "X-Forwarded-For"} {
			if request.Header.Get(name) != "" {
				testContext.Error("private/client header forwarded", name)
			}
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			testContext.Error(err)
		}
		expected := []string{"local-llm", "second-backend"}[index]
		if body["model"] != expected {
			testContext.Error("wrong backend identity", body)
		}
		if body["max_tokens"] != float64(128) && body["max_tokens"] != float64(64) {
			testContext.Error("effective cap missing", body)
		}
		if _, ok := body["request_class"]; ok {
			testContext.Error("request class leaked")
		}
		if _, ok := body["max_completion_tokens"]; ok {
			testContext.Error("cap was not normalized")
		}
		json.NewEncoder(responseWriter).Encode(map[string]string{"served_by": expected})
	})
	handler := registeredTest(testContext, config, nil)
	handler.afterAdmission = dispatchHarness(routingClient(testContext))
	for _, testCase := range []struct{ name, key, extra, expected string }{
		{"default", "alice-one", ``, "local-llm"},
		{"engineer", "bob", `,"max_completion_tokens":64`, "second-backend"},
		{"local-llm", "bob", `,"max_tokens":64`, "local-llm"},
		{"second", "alice-two", ``, "second-backend"},
	} {
		body := `{"model":"` + testCase.name + `","messages":[{"role":"user","content":"hello"}],"request_class":"background"` + testCase.extra + `}`
		request := httptest.NewRequest("POST", ChatCompletionsPath, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+testCase.key)
		for _, name := range []string{"Cookie", "Idempotency-Key", "X-Idempotency-Key", "X-Forwarded-For"} {
			request.Header.Set(name, "do-not-forward")
		}
		responseRecorder := httptest.NewRecorder()
		handler.ServeHTTP(responseRecorder, request)
		if responseRecorder.Code != 200 || !strings.Contains(responseRecorder.Body.String(), testCase.expected) {
			testContext.Fatal(responseRecorder.Code, responseRecorder.Body)
		}
	}
	if calls[0].Load() != 2 || calls[1].Load() != 2 {
		testContext.Fatal("wrong routing", calls[0].Load(), calls[1].Load())
	}
	for _, testCase := range []struct {
		key, body string
		status    int
	}{
		{"", validBody, 401}, {"invalid", validBody, 401},
		{"alice-one", strings.Replace(validBody, "local-llm", "unknown", 1), 404},
		{"alice-one", strings.TrimSuffix(validBody, "}") + `,"max_tokens":1025}`, 400},
	} {
		responseRecorder := quotaRequest(handler, "POST", ChatCompletionsPath, testCase.key, testCase.body)
		if responseRecorder.Code != testCase.status {
			testContext.Fatal(responseRecorder.Code, responseRecorder.Body)
		}
	}
	if calls[0].Load() != 2 || calls[1].Load() != 2 {
		testContext.Fatal("rejected request generated work")
	}
}

func TestValidationOnlySeamDoesNotGenerate(testContext *testing.T) {
	var generations atomic.Int64
	config := fakeRoutes(testContext, func(_ int, responseWriter http.ResponseWriter, request *http.Request) {
		generations.Add(1)
		responseWriter.Write([]byte(`{}`))
	})
	handler := registeredTest(testContext, config, nil) // Explicitly isolated validation-only test seam.
	responseRecorder := quotaRequest(handler, "POST", ChatCompletionsPath, "alice-one", validBody)
	if responseRecorder.Code != 501 || generations.Load() != 0 {
		testContext.Fatal("stub performed generation", responseRecorder.Code, generations.Load())
	}
	reservation, quotaDecision := handler.quotas.ReserveTokens("alice", 40000)
	if !quotaDecision.Allowed {
		testContext.Fatal("stub leaked reservation", quotaDecision)
	}
	handler.quotas.SettleTokens(reservation, 0)
}

func TestDispatchCancellationFromHTTPClient(testContext *testing.T) {
	started, cancelled, handled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	config := fakeRoutes(testContext, func(_ int, responseWriter http.ResponseWriter, request *http.Request) {
		io.Copy(io.Discard, request.Body)
		close(started)
		<-request.Context().Done()
		close(cancelled)
	})
	handler := registeredTest(testContext, config, nil)
	harness := dispatchHarness(routingClient(testContext))
	handler.afterAdmission = func(responseWriter http.ResponseWriter, request *http.Request, prepared *preparedRequest) {
		defer close(handled)
		harness(responseWriter, request, prepared)
		// Finish before notifying the test; the handler's deferred finish is a no-op.
		if err := prepared.settleUsageAndReleaseCapacity(); err != nil {
			testContext.Error(err)
		}
	}
	gateway := httptest.NewServer(handler)
	defer gateway.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", gateway.URL+ChatCompletionsPath, strings.NewReader(validBody))
	req.Header.Set("Authorization", "Bearer alice-one")
	req.Header.Set("Content-Type", "application/json")
	result := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(req)
		if response != nil {
			response.Body.Close()
		}
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		cancel()
		testContext.Fatal("backend did not start")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		testContext.Fatal("backend did not observe cancellation")
	}
	select {
	case <-handled:
	case <-time.After(3 * time.Second):
		testContext.Fatal("gateway did not clean up")
	}
	if err := <-result; err == nil {
		testContext.Fatal("client did not report cancellation")
	}
	// 3 input + 128 output are conservatively charged, not refunded as zero.
	reservation, quotaDecision := handler.quotas.ReserveTokens("alice", 40000-131)
	if !quotaDecision.Allowed {
		testContext.Fatal("reservation leak", quotaDecision)
	}
	if _, quotaDecision := handler.quotas.ReserveTokens("alice", 1); quotaDecision.Allowed {
		testContext.Fatal("ambiguous generation was refunded")
	}
	handler.quotas.SettleTokens(reservation, 0)
}

func TestReloadDuringSlowDispatchUsesSnapshot(testContext *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	config := fakeRoutes(testContext, func(index int, responseWriter http.ResponseWriter, request *http.Request) {
		io.Copy(io.Discard, request.Body)
		if index == 0 {
			close(started)
			select {
			case <-release:
			case <-request.Context().Done():
				return
			}
		}
		json.NewEncoder(responseWriter).Encode(map[string]int{"backend": index})
	})
	handler := registeredTest(testContext, config, nil)
	handler.afterAdmission = dispatchHarness(routingClient(testContext))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest("POST", ChatCompletionsPath, strings.NewReader(strings.Replace(validBody, "local-llm", "default", 1))).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer alice-one")
	request.Header.Set("Content-Type", "application/json")
	responseRecorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { handler.ServeHTTP(responseRecorder, request); close(done) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		testContext.Fatal("dispatch not started")
	}
	next := config.Clone()
	next.Models[0].Aliases = nil
	next.Models[1].Aliases = []string{"engineer", "default"}
	reloaded := make(chan error, 1)
	go func() { reloaded <- handler.Reload(next) }()
	select {
	case err := <-reloaded:
		if err != nil {
			testContext.Fatal(err)
		}
	case <-time.After(time.Second):
		testContext.Fatal("slow backend blocked reload")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		testContext.Fatal("dispatch stuck")
	}
	if responseRecorder.Code != 200 || !strings.Contains(responseRecorder.Body.String(), `"backend":0`) {
		testContext.Fatal("in-flight route changed", responseRecorder.Code, responseRecorder.Body)
	}
	second := quotaRequest(handler, "POST", ChatCompletionsPath, "alice-one", strings.Replace(validBody, "local-llm", "default", 1))
	if second.Code != 200 || !strings.Contains(second.Body.String(), `"backend":1`) {
		testContext.Fatal("new route not applied", second.Code, second.Body)
	}
}

func TestPreparedSnapshotAndCleanup(testContext *testing.T) {
	for _, scenario := range []string{"unused", "missing route", "refused connection", "backend error", "success"} {
		testContext.Run(scenario, func(testContext *testing.T) {
			config := registry.Default()
			config.Quotas.User.Tokens = 200
			config.Quotas.GlobalCeiling.Tokens = 200
			config.Models[0].Limits.Tokens = 200
			quotaManager, err := quota.New(config.QuotaConfig(), nil)
			if err != nil {
				testContext.Fatal(err)
			}
			reservation, quotaDecision := quotaManager.ReserveForModel("alice", "local-llm", 133)
			if !quotaDecision.Allowed {
				testContext.Fatal(quotaDecision)
			}
			content := "original"
			temperature := 0.5
			request := ChatRequest{Messages: []Message{{Role: "user", Content: &content}}, Temperature: &temperature}
			if scenario == "missing route" {
				config.Models[0].Backend.BaseURL = ""
			}
			if scenario == "refused connection" {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					testContext.Fatal(err)
				}
				config.Models[0].Backend.BaseURL = "http://" + listener.Addr().String()
				listener.Close()
			}
			if scenario == "backend error" || scenario == "success" {
				backend := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, httpRequest *http.Request) {
					io.Copy(io.Discard, httpRequest.Body)
					if scenario == "backend error" {
						responseWriter.WriteHeader(500)
					}
					responseWriter.Write([]byte(`{}`))
				}))
				defer backend.Close()
				config.Models[0].Backend.BaseURL = backend.URL
			}
			prepared, err := newPreparedRequest(config.Models[0], request, 128, quotaManager, reservation, 133)
			if err != nil {
				testContext.Fatal(err)
			}
			content = "modified"
			temperature = 1
			config.Models[0].Backend.ExpectedModel = "changed"
			config.Models[0].Aliases[0] = "changed"
			if strings.Contains(string(prepared.payload.Messages), "modified") || *prepared.payload.Temperature != 0.5 || prepared.model.Backend.ExpectedModel != "local-llm" || prepared.model.Aliases[0] != "default" {
				testContext.Fatal("prepared request shared mutable input")
			}
			client := routingClient(testContext)
			if scenario != "unused" {
				_, err = prepared.sendToBackend(context.Background(), client)
				if (err == nil) != (scenario == "success") {
					testContext.Fatal(scenario, err)
				}
				if _, err := prepared.sendToBackend(context.Background(), client); err == nil {
					testContext.Fatal("double dispatch accepted")
				}
			}
			if err := prepared.settleUsageAndReleaseCapacity(); err != nil {
				testContext.Fatal(err)
			}
			if err := prepared.settleUsageAndReleaseCapacity(); err != nil {
				testContext.Fatal("cleanup not idempotent", err)
			}
			if _, err := prepared.sendToBackend(context.Background(), client); err == nil {
				testContext.Fatal("dispatch after cleanup")
			}
			remaining := int64(200)
			if scenario == "backend error" || scenario == "success" {
				remaining -= 133
			}
			remainingBudgetReservation, quotaDecision := quotaManager.ReserveForModel("alice", "local-llm", remaining)
			if !quotaDecision.Allowed {
				testContext.Fatal("hold leaked", quotaDecision)
			}
			if _, quotaDecision := quotaManager.ReserveForModel("alice", "local-llm", 1); quotaDecision.Allowed {
				testContext.Fatal("incorrect token accounting")
			}
			quotaManager.SettleTokens(remainingBudgetReservation, 0)
		})
	}
}
