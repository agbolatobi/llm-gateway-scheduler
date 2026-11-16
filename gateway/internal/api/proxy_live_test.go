package api

import (
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"llm-gateway-scheduler/gateway/internal/registry"
)

// Explicit opt-in only: this sends two short requests to an already-running
// registered model. It never starts/stops runtimes or uses the user's key store.
func TestProxyRunningModel(testContext *testing.T) {
	path := os.Getenv("GATEWAY_LIVE_CONFIG")
	if path == "" {
		testContext.Skip("set GATEWAY_LIVE_CONFIG to smoke-test an existing model")
	}
	config, err := registry.Load(path)
	if err != nil {
		testContext.Fatal(err)
	}
	handler, reports := liveProxy(testContext, config, DefaultProxyOptions())
	for _, stream := range []bool{false, true} {
		testContext.Run(fmt.Sprint(stream), func(testContext *testing.T) {
			request := proxyRequest(config.Models[0].ID, stream)
			body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"Say hello."}],"max_tokens":8,"stream":%t}`, config.Models[0].ID, stream)
			if stream {
				body = strings.TrimSuffix(body, "}") + `,"stream_options":{"include_usage":true}}`
			}
			request = httptest.NewRequest("POST", ChatCompletionsPath, strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer alice-one")
			request.Header.Set("Content-Type", "application/json")
			responseRecorder := httptest.NewRecorder()
			handler.ServeHTTP(responseRecorder, request)
			if responseRecorder.Code != 200 || strings.Contains(responseRecorder.Body.String(), `"error":`) {
				testContext.Fatalf("status=%d body=%s", responseRecorder.Code, responseRecorder.Body)
			}
			if stream && !strings.Contains(responseRecorder.Body.String(), "data: [DONE]") {
				testContext.Fatal("missing stream completion")
			}
			rpt := getReport(testContext, reports)
			if rpt.Outcome != "completed" || rpt.FirstTextMS == nil || rpt.CompletionMS == nil {
				testContext.Fatalf("bad smoke timing: %+v", rpt)
			}
			testContext.Logf("stream=%t request=%s first_text_ms=%.2f completion_ms=%.2f", stream, rpt.RequestID, *rpt.FirstTextMS, *rpt.CompletionMS)
		})
	}
}
