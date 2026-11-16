package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"llm-gateway-scheduler/gateway/internal/registry"
)

type fakeCounter struct {
	calls   atomic.Int64
	count   int
	failure error
}

func (counter *fakeCounter) CountInput(context.Context, registry.Model, any) (int, error) {
	counter.calls.Add(1)
	return counter.count, counter.failure
}

func registeredTest(testContext *testing.T, config registry.Config, counter InputCounter) *RegisteredHandler {
	testContext.Helper()
	handler, err := NewRegisteredHandler(userVerifier{}, config, counter)
	if err != nil {
		testContext.Fatal(err)
	}
	// These GATE-04 tests isolate admission; GATE-06 tests use the live constructor.
	handler.afterAdmission = nil
	handler.report = func(TimingRecord) {}
	testContext.Cleanup(handler.Close)
	return handler
}

func TestModelLookupAndBudgets(testContext *testing.T) {
	for _, testCase := range []struct {
		name, body, code string
		status, count    int
	}{
		{"unknown", strings.Replace(validBody, "local-llm", "unknown", 1), "model_not_found", 404, 5},
		{"output", strings.TrimSuffix(validBody, "}") + `,"max_tokens":1025}`, "output_limit_exceeded", 400, 5},
		{"context", validBody, "context_length_exceeded", 400, 4096},
		{"default", validBody, "not_implemented", 501, 5},
		{"alias", strings.Replace(validBody, "local-llm", "default", 1), "not_implemented", 501, 5},
	} {
		testContext.Run(testCase.name, func(testContext *testing.T) {
			config := registry.Default()
			counter := &fakeCounter{count: testCase.count}
			handler := registeredTest(testContext, config, counter)
			responseRecorder := quotaRequest(handler, "POST", "/v1/chat/completions", "alice-one", testCase.body)
			if responseRecorder.Code != testCase.status || !strings.Contains(responseRecorder.Body.String(), testCase.code) {
				testContext.Fatal(responseRecorder.Code, responseRecorder.Body)
			}
			if testCase.status == 404 || testCase.code == "output_limit_exceeded" {
				if counter.calls.Load() != 0 {
					testContext.Fatal("invalid request reached backend")
				}
			}
			// Even the successful validation stub must release its token hold.
			if _, quotaDecision := handler.quotas.ReserveTokens("alice", 40000); !quotaDecision.Allowed {
				testContext.Fatal("partial or leaked token charge", quotaDecision)
			}
		})
	}
}

func TestValidKeysCanListAndRequestEveryModel(testContext *testing.T) {
	config := registry.Default()
	second := config.Models[0]
	second.ID = "second"
	second.Aliases = nil
	second.Backend.RuntimeID = "second"
	second.Backend.BaseURL = "http://127.0.0.1:8082"
	config.Models = append(config.Models, second)
	handler := registeredTest(testContext, config, &fakeCounter{count: 5})
	for _, key := range []string{"alice-one", "bob"} {
		responseRecorder := quotaRequest(handler, "GET", "/v1/models", key, "")
		if responseRecorder.Code != 200 || !strings.Contains(responseRecorder.Body.String(), "local-llm") || strings.Contains(responseRecorder.Body.String(), "base_url") {
			testContext.Fatal(responseRecorder.Code, responseRecorder.Body)
		}
		if !strings.Contains(responseRecorder.Body.String(), "second") {
			testContext.Fatal("authenticated user cannot see all registrations")
		}
		for _, name := range []string{"local-llm", "default", "second"} {
			responseRecorder := quotaRequest(handler, "POST", ChatCompletionsPath, key, strings.Replace(validBody, "local-llm", name, 1))
			if responseRecorder.Code != 501 || !strings.Contains(responseRecorder.Body.String(), "not_implemented") {
				testContext.Fatal("valid key did not reach validated stub", name, responseRecorder.Code, responseRecorder.Body)
			}
		}
	}
	counter := &fakeCounter{count: 5}
	handler = registeredTest(testContext, config, counter)
	for _, key := range []string{"", "invalid"} {
		if responseRecorder := quotaRequest(handler, "GET", ModelsPath, key, ""); responseRecorder.Code != 401 {
			testContext.Fatal("missing/invalid key could list models", responseRecorder.Code)
		}
		for _, name := range []string{"local-llm", "second", "unknown"} {
			responseRecorder := quotaRequest(handler, "POST", ChatCompletionsPath, key, strings.Replace(validBody, "local-llm", name, 1))
			if responseRecorder.Code != 401 {
				testContext.Fatal("authentication did not precede model validation", responseRecorder.Code)
			}
		}
	}
	if counter.calls.Load() != 0 {
		testContext.Fatal("unauthenticated request reached runtime")
	}
}

func TestModelAliasAndReloadDoNotResetQuota(testContext *testing.T) {
	config := registry.Default()
	config.Models[0].Limits.Requests = 1
	second := config.Models[0]
	second.ID = "second"
	second.Aliases = nil
	second.Limits.Requests = 30
	second.Backend.RuntimeID = "second"
	second.Backend.BaseURL = "http://127.0.0.1:8082"
	config.Models = append(config.Models, second)
	handler := registeredTest(testContext, config, &fakeCounter{count: 5})
	if responseRecorder := quotaRequest(handler, "POST", "/v1/chat/completions", "alice-one", validBody); responseRecorder.Code != 501 {
		testContext.Fatal(responseRecorder.Code)
	}
	if err := handler.Reload(config); err != nil {
		testContext.Fatal(err)
	}
	responseRecorder := quotaRequest(handler, "POST", "/v1/chat/completions", "alice-two", strings.Replace(validBody, "local-llm", "default", 1))
	if responseRecorder.Code != 429 || !strings.Contains(responseRecorder.Body.String(), "model_request_limit") {
		testContext.Fatal(responseRecorder.Code, responseRecorder.Body)
	}
	config.Models[0].Backend.BaseURL = "http://127.0.0.1:8099"
	if err := handler.Reload(config); err == nil {
		testContext.Fatal("runtime change accepted")
	}
}

func TestTwoFakeRunningModels(testContext *testing.T) {
	var tokenCalls [2]atomic.Int64
	config := registry.Default()
	second := config.Models[0]
	second.ID = "second"
	second.Aliases = []string{"engineer"}
	second.Backend.RuntimeID = "second"
	second.Backend.ExpectedModel = "second"
	config.Models = append(config.Models, second)
	for index := range config.Models {
		index := index
		backend := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
			switch request.URL.Path {
			case "/health":
				json.NewEncoder(responseWriter).Encode(map[string]string{"status": "ok"})
			case "/v1/models":
				json.NewEncoder(responseWriter).Encode(map[string]any{"data": []map[string]string{{"id": config.Models[index].Backend.ExpectedModel}}})
			case "/slots":
				json.NewEncoder(responseWriter).Encode([]map[string]int{{"n_ctx": 4096}})
			case "/apply-template":
				json.NewEncoder(responseWriter).Encode(map[string]string{"prompt": "templated message"})
			case "/tokenize":
				tokenCalls[index].Add(1)
				json.NewEncoder(responseWriter).Encode(map[string]any{"tokens": []int{1, 2, 3}})
			default:
				testContext.Error("unexpected generation request")
				http.Error(responseWriter, "no inference", 500)
			}
		}))
		testContext.Cleanup(backend.Close)
		config.Models[index].Backend.BaseURL = backend.URL
	}
	handler := registeredTest(testContext, config, nil)
	for _, name := range []string{"default", "engineer"} {
		responseRecorder := quotaRequest(handler, "POST", "/v1/chat/completions", "alice-one", strings.Replace(validBody, "local-llm", name, 1))
		if responseRecorder.Code != 501 {
			testContext.Fatal(responseRecorder.Code, responseRecorder.Body)
		}
	}
	if tokenCalls[0].Load() != 1 || tokenCalls[1].Load() != 1 {
		testContext.Fatal("models not independently validated")
	}
}

func TestRuntimeFailureIsSanitized(testContext *testing.T) {
	handler := registeredTest(testContext, registry.Default(), &fakeCounter{failure: errors.New("private diagnostic")})
	responseRecorder := quotaRequest(handler, "POST", "/v1/chat/completions", "alice-one", validBody)
	if responseRecorder.Code != 503 || strings.Contains(responseRecorder.Body.String(), "private") {
		testContext.Fatal(responseRecorder.Code, responseRecorder.Body)
	}
}

func TestConcurrentRegistryReload(testContext *testing.T) {
	config := registry.Default()
	handler := registeredTest(testContext, config, &fakeCounter{count: 5})
	var wg sync.WaitGroup
	for index := 0; index < 30; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := handler.Reload(config); err != nil {
				testContext.Error(err)
			}
			responseRecorder := quotaRequest(handler, "GET", "/v1/models", "alice-one", "")
			if responseRecorder.Code != 200 {
				testContext.Error(responseRecorder.Code)
			}
		}()
	}
	wg.Wait()
}

func TestGlobalDenialBeforeModelLookup(testContext *testing.T) {
	config := registry.Default()
	config.Quotas.User.Requests = 1
	counter := &fakeCounter{count: 5}
	handler := registeredTest(testContext, config, counter)
	quotaRequest(handler, "POST", "/v1/chat/completions", "alice-one", validBody)
	responseRecorder := quotaRequest(handler, "POST", "/v1/chat/completions", "alice-two", strings.Replace(validBody, "local-llm", "unknown", 1))
	if responseRecorder.Code != 429 || counter.calls.Load() != 1 {
		testContext.Fatal("lookup preceded quota check")
	}
}
