package evaluation

import (
	"bytes"
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
	"time"

	"llm-gateway-scheduler/gateway/internal/registry"
)

const textEvent = `data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"local-llm","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}` + "\n\n"
const finishEvent = `data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"local-llm","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}` + "\n\n"

func TestStreamEvidence(testContext *testing.T) {
	for _, example := range []struct {
		name, body string
		success    bool
		tokens     bool
	}{
		{"complete", textEvent + finishEvent + "data: [DONE]\n\n", true, true},
		{"missing_done", textEvent + finishEvent, false, true},
		{"no_finish", textEvent + "data: [DONE]\n\n", false, false},
		{"data_after_done", textEvent + finishEvent + "data: [DONE]\n\n" + textEvent, false, true},
		{"usage_missing", textEvent + strings.Replace(finishEvent, `,"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}`, "", 1) + "data: [DONE]\n\n", true, false},
		{"whitespace", strings.Replace(textEvent, "hello", " ", 1) + finishEvent + "data: [DONE]\n\n", true, true},
	} {
		testContext.Run(example.name, func(testContext *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(writer, example.body)
			}))
			defer server.Close()
			var evidence bytes.Buffer
			result := Perform(context.Background(), server.Client(), server.URL, "local-llm", "", "trial", "request", Job{ID: "job", Class: "interactive", Prompt: "hi", MaxTokens: 8}, time.Now(), NewRecorder(&evidence))
			if (result.Outcome == "completed") != example.success {
				testContext.Fatalf("%+v", result)
			}
			if example.success && (result.TTFTMS == nil || result.E2EMS == nil || *result.E2EMS < *result.TTFTMS) {
				testContext.Fatalf("bad timing: %+v", result)
			}
			if !example.success && result.E2EMS != nil {
				testContext.Fatal("failed response retained success latency")
			}
			if (result.OutputTokens != nil) != example.tokens {
				testContext.Fatalf("bad usage: %+v", result)
			}
			if !strings.Contains(evidence.String(), `"event":"sse"`) {
				testContext.Fatal("missing raw evidence")
			}
		})
	}
}

func TestTimeoutAndBrokenTransport(testContext *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		if request.URL.Query().Get("unused") == "" {
			writer.WriteHeader(200)
			writer.(http.Flusher).Flush()
			<-request.Context().Done()
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	result := Perform(ctx, server.Client(), server.URL, "local-llm", "", "trial", "request", Job{Prompt: "hi", MaxTokens: 8}, time.Now(), NewRecorder(io.Discard))
	if result.Outcome != "timed_out" {
		testContext.Fatalf("%+v", result)
	}
	broken := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Content-Length", "999999")
		io.WriteString(writer, textEvent+finishEvent+"data: [DONE]\n\n")
	}))
	defer broken.Close()
	result = Perform(context.Background(), broken.Client(), broken.URL, "local-llm", "", "trial", "request", Job{Prompt: "hi", MaxTokens: 8}, time.Now(), NewRecorder(io.Discard))
	if result.Outcome == "completed" || result.E2EMS != nil {
		testContext.Fatalf("broken transport reported success: %+v", result)
	}
}

func TestThreeModesUseRealGatewayAndQueueEvidence(testContext *testing.T) {
	var busy atomic.Bool
	var slot sync.Mutex
	var erased atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/health":
			io.WriteString(writer, `{"status":"ok"}`)
		case "/v1/models":
			io.WriteString(writer, `{"data":[{"id":"local-llm"}]}`)
		case "/props":
			io.WriteString(writer, `{"model_path":"missing-test-model"}`)
		case "/slots":
			fmt.Fprintf(writer, `[{"id":0,"n_ctx":4096,"is_processing":%t}]`, busy.Load())
		case "/slots/0":
			erased.Add(1)
			io.WriteString(writer, `{"id_slot":0,"n_erased":0}`)
		case "/apply-template":
			io.WriteString(writer, `{"prompt":"test"}`)
		case "/tokenize":
			io.WriteString(writer, `{"tokens":[1,2,3]}`)
		case "/v1/chat/completions":
			slot.Lock()
			defer slot.Unlock()
			busy.Store(true)
			writer.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(writer, textEvent)
			writer.(http.Flusher).Flush()
			time.Sleep(80 * time.Millisecond)
			busy.Store(false)
			io.WriteString(writer, finishEvent+"data: [DONE]\n\n")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer backend.Close()
	config := registry.Default()
	config.Models[0].Backend.BaseURL = backend.URL
	plan := Plan{Version: 1, Config: config, Model: "local-llm", Repetitions: 1, TimeoutSeconds: 5, MaxInFlight: 8, Modes: []string{"direct", "fifo", "interactive_priority_aging"}, Traces: []Trace{{Name: "contended", Jobs: []Job{
		{ID: "first", AtSeconds: 0, Class: "background", Prompt: "first", MaxTokens: 8},
		{ID: "second", AtSeconds: .02, Class: "background", Prompt: "second", MaxTokens: 8},
		{ID: "third", AtSeconds: .04, Class: "interactive", Prompt: "third", MaxTokens: 8},
	}}}}
	var output bytes.Buffer
	if err := Run(context.Background(), plan, NewRecorder(&output)); err != nil {
		testContext.Fatal(err)
	}
	requests, records := 0, 0
	requestClasses := map[string]string{}
	gatewayClasses := map[string]string{}
	sequences := map[string]map[string]float64{}
	queued := false
	decoder := json.NewDecoder(&output)
	for decoder.More() {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			testContext.Fatal(err)
		}
		if event["event"] == "request" {
			requests++
			result := event["result"].(map[string]any)
			requestClasses[result["request_id"].(string)] = result["class"].(string)
			if result["outcome"] != "completed" {
				testContext.Errorf("request failed: %+v", result)
			}
		}
		if event["event"] == "gateway" {
			records++
			record := event["record"].(map[string]any)
			requestID := record["request_id"].(string)
			gatewayClasses[requestID] = record["request_class"].(string)
			trialID := event["trial_id"].(string)
			if sequences[trialID] == nil {
				sequences[trialID] = map[string]float64{}
			}
			jobIndex := requestID[strings.LastIndex(requestID, "-")+1:]
			sequences[trialID][jobIndex] = record["selection_sequence"].(float64)
			if event["record"].(map[string]any)["queue_wait_ms"].(float64) > 10 {
				queued = true
			}
		}
	}
	for requestID, actual := range gatewayClasses {
		if actual != requestClasses[requestID] {
			testContext.Errorf("class mismatch %s: planned=%s gateway=%s", requestID, requestClasses[requestID], actual)
		}
	}
	if sequences["r1-t1-fifo"]["1"] != 2 || sequences["r1-t1-interactive_priority_aging"]["2"] != 2 {
		testContext.Fatalf("expected FIFO to dispatch background next and priority to dispatch interactive next: %+v", sequences)
	}
	if requests != 9 || records != 6 || !queued || erased.Load() != 3 {
		testContext.Fatalf("requests=%d records=%d queued=%t erases=%d", requests, records, queued, erased.Load())
	}
}

func TestRequestClassSentOnlyInGatewayJSON(testContext *testing.T) {
	for _, gatewayKey := range []string{"", "test-key"} {
		for _, requestClass := range []string{"background", "interactive"} {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var payload map[string]any
				if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
					testContext.Error(err)
				}
				if gatewayKey == "" {
					if _, present := payload["request_class"]; present {
						testContext.Error("gateway extension leaked to direct runtime")
					}
				} else if payload["request_class"] != requestClass {
					testContext.Errorf("missing JSON class: %+v", payload)
				}
				if request.Header.Get("X-Request-Class") != "" {
					testContext.Error("obsolete class header")
				}
				writer.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(writer, textEvent+finishEvent+"data: [DONE]\n\n")
			}))
			result := Perform(context.Background(), server.Client(), server.URL, "local-llm", gatewayKey, "trial", "request", Job{Prompt: "hi", Class: requestClass, MaxTokens: 8}, time.Now(), NewRecorder(io.Discard))
			server.Close()
			if result.Outcome != "completed" {
				testContext.Fatalf("%+v", result)
			}
		}
	}
}

func TestExperimentDeadlineValidation(testContext *testing.T) {
	config := registry.Default()
	policy := config.SchedulerConfig()
	policy.MaxWaitSeconds = 180
	policy.AgingSeconds = 60
	config.Gateway.Scheduler = &policy
	plan := Plan{Version: 1, Config: config, Model: "local-llm", Repetitions: 1, TimeoutSeconds: 250, GatewayTimeoutSeconds: 240, MaxInFlight: 4, Modes: []string{"fifo"}, Traces: []Trace{{Name: "trace", Jobs: []Job{{ID: "job", Class: "background", Prompt: "hi", MaxTokens: 8}}}}}
	if err := plan.Validate(); err != nil {
		testContext.Fatal(err)
	}
	plan.GatewayTimeoutSeconds = 180
	if plan.Validate() == nil {
		testContext.Error("queue must leave time for generation")
	}
	plan.GatewayTimeoutSeconds = 250
	if plan.Validate() == nil {
		testContext.Error("client deadline must exceed gateway deadline")
	}
}
