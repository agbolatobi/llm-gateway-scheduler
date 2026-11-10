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
func TestProxyRunningModel(t *testing.T) {
	path := os.Getenv("GATEWAY_LIVE_CONFIG")
	if path == "" {
		t.Skip("set GATEWAY_LIVE_CONFIG to smoke-test an existing model")
	}
	c, err := registry.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	h, reports := liveProxy(t, c, DefaultProxyOptions())
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			r := proxyRequest(c.Models[0].ID, stream)
			body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"Say hello."}],"max_tokens":8,"stream":%t}`, c.Models[0].ID, stream)
			if stream {
				body = strings.TrimSuffix(body, "}") + `,"stream_options":{"include_usage":true}}`
			}
			r = httptest.NewRequest("POST", ChatCompletionsPath, strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer alice-one")
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 || strings.Contains(w.Body.String(), `"error":`) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			if stream && !strings.Contains(w.Body.String(), "data: [DONE]") {
				t.Fatal("missing stream completion")
			}
			rpt := getReport(t, reports)
			if rpt.Outcome != "completed" || rpt.FirstTextMS == nil || rpt.CompletionMS == nil {
				t.Fatalf("bad smoke timing: %+v", rpt)
			}
			t.Logf("stream=%t request=%s first_text_ms=%.2f completion_ms=%.2f", stream, rpt.RequestID, *rpt.FirstTextMS, *rpt.CompletionMS)
		})
	}
}
