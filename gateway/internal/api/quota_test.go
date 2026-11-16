package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-gateway-scheduler/gateway/internal/auth"
	"llm-gateway-scheduler/gateway/internal/quota"
)

type userVerifier struct{}

func (userVerifier) Verify(key string) (auth.Principal, error) {
	user := ""
	switch key {
	case "alice-one", "alice-two":
		user = "alice"
	case "bob":
		user = "bob"
	}
	if user == "" {
		return auth.Principal{}, auth.ErrInvalidKey
	}
	return auth.Principal{UserID: user, KeyID: key}, nil
}

func quotaRequest(handler http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	responseRecorder := httptest.NewRecorder()
	handler.ServeHTTP(responseRecorder, request)
	return responseRecorder
}

func TestQuotaOrderingAndSharedKeys(testContext *testing.T) {
	config := quota.DefaultConfig()
	config.User.Requests = 2
	quotaManager, err := quota.New(config, nil)
	if err != nil {
		testContext.Fatal(err)
	}
	called := 0
	handler := newHandler(userVerifier{}, quotaManager, func(responseWriter http.ResponseWriter, httpRequest *http.Request, request ChatRequest) {
		called++
		responseWriter.WriteHeader(204)
	})
	for _, testCase := range []struct {
		method, path, key, body string
		want                    int
	}{
		{"POST", "/v1/chat/completions", "bad", validBody, 401},
		{"POST", "/v1/chat/completions", "alice-one", "{", 400},
		{"GET", "/v1/chat/completions", "alice-one", "", 405},
		{"GET", "/health", "", "", 200},
		{"GET", "/v1/models", "alice-one", "", 501},
		{"POST", "/v1/chat/completions", "alice-one", validBody, 204},
		{"POST", "/v1/chat/completions", "alice-two", strings.TrimSuffix(validBody, "}") + `,"stream":true}`, 204},
		{"POST", "/v1/chat/completions", "alice-one", validBody, 429},
		{"POST", "/v1/chat/completions", "alice-two", validBody, 429},
		{"POST", "/v1/chat/completions", "bob", validBody, 204},
		{"GET", "/health", "", "", 200},
		{"GET", "/v1/models", "alice-one", "", 501},
	} {
		responseRecorder := quotaRequest(handler, testCase.method, testCase.path, testCase.key, testCase.body)
		if responseRecorder.Code != testCase.want {
			testContext.Fatalf("%s %s: %d want %d: %s", testCase.method, testCase.path, responseRecorder.Code, testCase.want, responseRecorder.Body)
		}
	}
	if called != 3 {
		testContext.Fatalf("rejected request reached downstream boundary: %d calls", called)
	}
}

type fixedDecision struct{ decision quota.Decision }

func (counter fixedDecision) CheckAndCount(string) quota.Decision { return counter.decision }

func TestQuotaErrors(testContext *testing.T) {
	for _, code := range []string{"user_request_limit", "global_request_limit", "user_token_limit", "global_token_limit"} {
		handler := newHandler(userVerifier{}, fixedDecision{quota.Decision{Code: code, RetryAfter: 1500 * time.Millisecond}}, func(http.ResponseWriter, *http.Request, ChatRequest) { testContext.Fatal("downstream called") })
		responseRecorder := quotaRequest(handler, "POST", "/v1/chat/completions", "alice-one", validBody)
		var body ErrorResponse
		if err := json.Unmarshal(responseRecorder.Body.Bytes(), &body); err != nil {
			testContext.Fatal(err)
		}
		if responseRecorder.Code != 429 || body.Error.Type != "rate_limit_error" || body.Error.Code != code || body.Error.Param != nil || responseRecorder.Header().Get("Retry-After") != "2" || responseRecorder.Header().Get("Cache-Control") != "no-store" {
			testContext.Fatalf("bad error: %d %s %v", responseRecorder.Code, responseRecorder.Body, responseRecorder.Header())
		}
	}
	for _, checker := range []QuotaChecker{nil, fixedDecision{quota.Decision{Code: "sensitive internal failure"}}} {
		responseRecorder := quotaRequest(NewHandler(userVerifier{}, checker), "POST", "/v1/chat/completions", "alice-one", validBody)
		if responseRecorder.Code != 503 || strings.Contains(responseRecorder.Body.String(), "sensitive") {
			testContext.Fatal("must fail closed and sanitized")
		}
	}
}

func TestExhaustedTokensRejectBeforeDownstream(testContext *testing.T) {
	quotaManager, err := quota.New(quota.DefaultConfig(), nil)
	if err != nil {
		testContext.Fatal(err)
	}
	if _, quotaDecision := quotaManager.ReserveTokens("alice", 40000); !quotaDecision.Allowed {
		testContext.Fatal(quotaDecision)
	}
	handler := newHandler(userVerifier{}, quotaManager, func(http.ResponseWriter, *http.Request, ChatRequest) {
		testContext.Fatal("token-exhausted request reached quotaManager stage")
	})
	responseRecorder := quotaRequest(handler, "POST", "/v1/chat/completions", "alice-one", validBody)
	if responseRecorder.Code != 429 || !strings.Contains(responseRecorder.Body.String(), "user_token_limit") {
		testContext.Fatal(responseRecorder.Code, responseRecorder.Body)
	}
}

func TestStubCountsRequestsButNotTokens(testContext *testing.T) {
	config := quota.DefaultConfig()
	config.User.Requests = 1
	quotaManager, err := quota.New(config, nil)
	if err != nil {
		testContext.Fatal(err)
	}
	handler := NewHandler(userVerifier{}, quotaManager)
	if responseRecorder := quotaRequest(handler, "POST", "/v1/chat/completions", "alice-one", validBody); responseRecorder.Code != 501 {
		testContext.Fatal(responseRecorder.Code)
	}
	if responseRecorder := quotaRequest(handler, "POST", "/v1/chat/completions", "alice-two", validBody); responseRecorder.Code != 429 {
		testContext.Fatal(responseRecorder.Code)
	}
	if _, quotaDecision := quotaManager.ReserveTokens("alice", 40000); !quotaDecision.Allowed {
		testContext.Fatal("stub spent tokens", quotaDecision)
	}
}
