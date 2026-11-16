package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"llm-gateway-scheduler/gateway/internal/registry"
)

func TestChatPageDoesNotBypassAPIAuthentication(testContext *testing.T) {
	handler := registeredTest(testContext, registry.Default(), &fakeCounter{count: 3})
	defer handler.Close()
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest("GET", "/chat/", nil))
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Paste your full API token") {
		testContext.Fatal("chat page was not served")
	}
	for _, path := range []string{ModelsPath, ChatCompletionsPath} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != 401 {
			testContext.Fatalf("UI integration bypassed authentication for %s", path)
		}
	}
}
